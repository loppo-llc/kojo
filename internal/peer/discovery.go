// Package peer's discovery.go drives the auto-pairing flow described
// in docs/peer-onboarding-plan.md.
//
// Sequence (peer side):
//
//  1. Resolve Hub URL: --hub CLI flag → KOJO_HUB_URL env → MagicDNS
//     default `https://kojo.<tailnet>.ts.net:<port>`. The MagicDNS
//     default reads the OS tailscaled's tailnet name via
//     `tailscale status --json`. KOJO_HUB_PORT (default 8080)
//     supplies the port.
//  2. GET <hub>/api/v1/peers/hub-info — learns Hub's
//     {deviceId, name, publicKey, url}.
//  3. Write the Hub row into local peer_registry (trusted=true) so
//     the local PeerAuth middleware accepts Hub-signed requests on
//     the inter-peer surface.
//  4. POST <hub>/api/v1/peers/join-request — sends our own identity.
//     Hub answers state="approved" (already paired) or state="pending"
//     (parked, waiting for Owner Approve).
//  5. On "pending", loop step 4 every 60s until Hub returns "approved".
//
// Errors at any step are logged at Warn and the loop retries with
// fixed cadence — `kojo --peer` must never crash because the Hub
// is briefly unreachable.

package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// JoinHeartbeat is the polling cadence while a join-request sits
// in `pending`. Matches the plan's "60s heartbeat".
const JoinHeartbeat = 60 * time.Second

// HubInfo is the response shape of GET /api/v1/peers/hub-info.
type HubInfo struct {
	DeviceID  string `json:"deviceId"`
	Name      string `json:"name"`
	PublicKey string `json:"publicKey"`
	URL       string `json:"url"`
	Version   string `json:"version"`
}

// JoinResponse is the response shape of POST/GET
// /api/v1/peers/join-request.
type JoinResponse struct {
	State string   `json:"state"`
	Hub   *HubInfo `json:"hub,omitempty"`
}

// DiscoveryConfig parameterises NewDiscovery.
type DiscoveryConfig struct {
	// HubURLOverride is the --hub CLI flag value. Empty falls back
	// to KOJO_HUB_URL env and then MagicDNS.
	HubURLOverride string
	// DefaultHubPort is the port appended to the MagicDNS form
	// when KOJO_HUB_PORT is unset. Pass the binary's --port flag
	// value (8080 by default).
	DefaultHubPort int
	// PeerPublicURL is the dial address we advertise in
	// join-request bodies. The main loop fills this in once the
	// peer listener has bound.
	PeerPublicURL string
}

// Discovery is the long-running auto-pairing coordinator.
type Discovery struct {
	cfg      DiscoveryConfig
	identity *Identity
	store    *store.Store
	logger   *slog.Logger
	client   *http.Client
}

// NewDiscovery wires a Discovery. Identity / store / logger must be
// non-nil.
func NewDiscovery(cfg DiscoveryConfig, identity *Identity, st *store.Store, logger *slog.Logger) (*Discovery, error) {
	if identity == nil {
		return nil, errors.New("peer.NewDiscovery: nil identity")
	}
	if st == nil {
		return nil, errors.New("peer.NewDiscovery: nil store")
	}
	if logger == nil {
		return nil, errors.New("peer.NewDiscovery: nil logger")
	}
	if cfg.DefaultHubPort == 0 {
		cfg.DefaultHubPort = 8080
	}
	return &Discovery{
		cfg:      cfg,
		identity: identity,
		store:    st,
		logger:   logger,
		client:   NoKeepAliveHTTPClient(10 * time.Second),
	}, nil
}

// Run blocks until ctx is cancelled. It executes the discovery flow
// repeatedly: resolve hub → fetch hub-info → register hub in registry
// → POST join-request → if pending poll every JoinHeartbeat until
// approved. Once approved, it keeps a heartbeat going so a future
// Reject + re-approve cycle is observed without operator intervention.
func (d *Discovery) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		hubURL, err := d.resolveHubURL(ctx)
		if err != nil {
			d.logger.Warn("peer discovery: could not resolve Hub URL; retrying",
				"err", err)
			d.sleep(ctx, JoinHeartbeat)
			continue
		}
		d.logger.Info("peer discovery: Hub URL resolved", "hub", hubURL)
		hub, err := d.fetchHubInfo(ctx, hubURL)
		if err != nil {
			d.logger.Warn("peer discovery: hub-info fetch failed; retrying",
				"hub", hubURL, "err", err)
			d.sleep(ctx, JoinHeartbeat)
			continue
		}
		if err := d.upsertHubIntoRegistry(ctx, hub, hubURL); err != nil {
			d.logger.Warn("peer discovery: write Hub into peer_registry failed",
				"err", err)
			d.sleep(ctx, JoinHeartbeat)
			continue
		}
		d.joinLoop(ctx, hubURL)
		// joinLoop only returns on ctx cancellation OR a fatal
		// error worth re-resolving the Hub URL for. Loop back.
	}
}

// joinLoop POSTs the join-request and polls until approved. On
// approval it switches to a slower keepalive (still JoinHeartbeat —
// the Hub uses it for last_seen tracking).
func (d *Discovery) joinLoop(ctx context.Context, hubURL string) {
	announced := false
	for {
		if ctx.Err() != nil {
			return
		}
		resp, err := d.postJoinRequest(ctx, hubURL)
		if err != nil {
			d.logger.Warn("peer discovery: join-request failed; retrying",
				"hub", hubURL, "err", err)
			d.sleep(ctx, JoinHeartbeat)
			continue
		}
		switch resp.State {
		case "approved":
			if !announced {
				d.logger.Info("peer discovery: join approved by Hub",
					"hub", hubURL)
				announced = true
			}
			if resp.Hub != nil {
				if err := d.upsertHubIntoRegistry(ctx, resp.Hub, hubURL); err != nil {
					d.logger.Warn("peer discovery: refresh Hub row failed",
						"err", err)
				}
			}
		case "pending":
			if announced {
				// Owner Rejected after a prior approval — fall
				// back to "waiting" state.
				announced = false
			}
			d.logger.Info("peer discovery: awaiting Owner approval on Hub",
				"hub", hubURL,
				"device_id", d.identity.DeviceID)
		default:
			d.logger.Warn("peer discovery: unexpected join state",
				"state", resp.State)
		}
		d.sleep(ctx, JoinHeartbeat)
	}
}

// resolveHubURL returns the Hub base URL (`scheme://host:port`).
// Order: --hub flag, KOJO_HUB_URL env, MagicDNS default.
func (d *Discovery) resolveHubURL(ctx context.Context) (string, error) {
	if v := strings.TrimSpace(d.cfg.HubURLOverride); v != "" {
		return d.canonicalHubURL(v)
	}
	if v := strings.TrimSpace(os.Getenv("KOJO_HUB_URL")); v != "" {
		return d.canonicalHubURL(v)
	}
	// MagicDNS default.
	tailnet, err := readTailnetName(ctx)
	if err != nil {
		return "", fmt.Errorf("tailnet name: %w", err)
	}
	port := d.cfg.DefaultHubPort
	if v := strings.TrimSpace(os.Getenv("KOJO_HUB_PORT")); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			port = p
		}
	}
	return fmt.Sprintf("https://kojo.%s:%d", tailnet, port), nil
}

// canonicalHubURL accepts "host:port" or "scheme://host:port" and
// returns a canonical base URL with no path.
func (d *Discovery) canonicalHubURL(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	if u.Host == "" {
		return "", errors.New("missing host")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q", scheme)
	}
	return scheme + "://" + u.Host, nil
}

// fetchHubInfo GETs /api/v1/peers/hub-info.
func (d *Discovery) fetchHubInfo(ctx context.Context, hubURL string) (*HubInfo, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		hubURL+"/api/v1/peers/hub-info", nil)
	if err != nil {
		return nil, fmt.Errorf("build: %w", err)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	var info HubInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if err := ValidateDeviceID(info.DeviceID); err != nil {
		return nil, fmt.Errorf("hub deviceId: %w", err)
	}
	if err := ValidateName(info.Name); err != nil {
		return nil, fmt.Errorf("hub name: %w", err)
	}
	if err := ValidatePublicKey(info.PublicKey); err != nil {
		return nil, fmt.Errorf("hub publicKey: %w", err)
	}
	return &info, nil
}

// upsertHubIntoRegistry writes the Hub row into peer_registry with
// trusted=true. If URL is empty (Hub hasn't bound its listener yet),
// fall back to the dialing URL we already used to fetch hub-info.
//
// RegisterPeerMetadata preserves public_key on conflict (identity
// immutability — see store/peer_registry.go). If the Hub returned a
// key that disagrees with the stored row, the silent preserve would
// leave us trusting the OLD key while talking to the NEW Hub; refuse
// loudly so the operator notices and runs `kojo --peer-remove <hub>`
// to re-pair.
func (d *Discovery) upsertHubIntoRegistry(ctx context.Context, hub *HubInfo, fallbackURL string) error {
	if hub == nil {
		return errors.New("nil hub")
	}
	rowURL := hub.URL
	if rowURL == "" {
		rowURL = fallbackURL
	}
	if !IsDialAddress(rowURL) {
		return fmt.Errorf("hub URL not dialable: %q", rowURL)
	}
	// Check existing row's public_key BEFORE writing — RegisterPeerMetadata
	// preserves public_key on conflict but updates name/url, so calling it
	// first then comparing would already have written the new dial URL
	// against a row we no longer trust to be the same identity.
	if existing, err := d.store.GetPeer(ctx, hub.DeviceID); err == nil && existing != nil {
		if existing.PublicKey != "" && existing.PublicKey != hub.PublicKey {
			return fmt.Errorf("hub public_key mismatch: stored row has a different key (deviceId=%s); operator must run `kojo --peer-remove %s` to re-pair", hub.DeviceID, hub.DeviceID)
		}
	}
	rec, err := d.store.RegisterPeerMetadata(ctx, &store.PeerRecord{
		DeviceID:  hub.DeviceID,
		Name:      hub.Name,
		URL:       rowURL,
		PublicKey: hub.PublicKey,
	})
	if err != nil {
		return err
	}
	// trusted=true unconditionally — operator opted in by running
	// `kojo --peer`, the Hub it auto-discovered must be admitted on
	// the privileged surface for register-push / session-proxy /
	// agent-sync to land.
	if rec == nil || !rec.Trusted {
		if err := d.store.UpdatePeerTrust(ctx, hub.DeviceID, true); err != nil {
			return fmt.Errorf("trust apply: %w", err)
		}
	}
	return nil
}

// postJoinRequest sends our identity to Hub and returns the parsed
// response.
func (d *Discovery) postJoinRequest(ctx context.Context, hubURL string) (*JoinResponse, error) {
	body, err := json.Marshal(map[string]string{
		"deviceId":  d.identity.DeviceID,
		"name":      d.identity.Name,
		"url":       d.cfg.PeerPublicURL,
		"publicKey": d.identity.PublicKeyBase64(),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		hubURL+"/api/v1/peers/join-request", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if resp.StatusCode == http.StatusConflict {
		return nil, fmt.Errorf("409 conflict: %s", strings.TrimSpace(string(respBody)))
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var jr JoinResponse
	if err := json.Unmarshal(respBody, &jr); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &jr, nil
}

// SetPeerPublicURL lets main.go update the advertised URL once the
// listener has bound.
func (d *Discovery) SetPeerPublicURL(u string) {
	d.cfg.PeerPublicURL = u
}

func (d *Discovery) sleep(ctx context.Context, dur time.Duration) {
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// readTailnetName shells out to `tailscale status --json` and pulls
// MagicDNSSuffix out of the response. Failure paths propagate the
// error so the caller can fall back to retry / log.
func readTailnetName(parent context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
	if err != nil {
		return "", fmt.Errorf("tailscale status: %w", err)
	}
	// MagicDNSSuffix is `<tailnet>.ts.net` (no leading dot). Pull
	// it via a minimal partial decode so we don't pay the full
	// status struct's surface area.
	var parsed struct {
		MagicDNSSuffix string `json:"MagicDNSSuffix"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return "", fmt.Errorf("decode tailscale status: %w", err)
	}
	suffix := strings.TrimSpace(parsed.MagicDNSSuffix)
	suffix = strings.TrimSuffix(suffix, ".")
	if suffix == "" {
		return "", errors.New("tailscale status: empty MagicDNSSuffix")
	}
	return suffix, nil
}
