// Package peer's discovery.go drives the auto-pairing flow described
// in docs/peer-tsnet-identity.md.
//
// Sequence (peer side):
//
//  1. Resolve Hub URL: --hub CLI flag → KOJO_HUB_URL env → MagicDNS
//     default `https://kojo.<tailnet>.ts.net:<port>`.
//  2. GET <hub>/api/v1/peers/hub-info — learns Hub's
//     {deviceId, name, url}.
//  3. Write the Hub row into local peer_registry so the local
//     tsnet identity middleware can resolve Hub-inbound requests.
//  4. POST <hub>/api/v1/peers/join-request — sends our own
//     {device_id, name, url}. Hub reads our NodeKey from the
//     inbound HTTP request via tsnet WhoIs; we do NOT send it.
//  5. Hub answers state="approved" (already paired) or state=
//     "pending" (parked, awaiting Owner Approve). On "pending",
//     poll GET /join-request/{deviceId} every JoinHeartbeat.
//  6. On approved, log + return. The discovery loop EXITS — there
//     is nothing to refresh; the Registrar's heartbeat keeps
//     last_seen current.

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
// in `pending`.
const JoinHeartbeat = 60 * time.Second

// HubInfo is the response shape of GET /api/v1/peers/hub-info.
//
// NodeKey is the Hub's Tailscale stable NodeKey. The peer stamps it
// onto its local peer_registry row for the Hub so the tsnet
// identity middleware can later resolve inbound Hub requests
// (Subscriber WS, blob push, agent-sync) to RolePeer. Empty until
// the Hub's tsnet has finished its login handshake; the peer
// re-fetches hub-info on the next discovery tick in that case.
type HubInfo struct {
	DeviceID string `json:"deviceId"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	NodeKey  string `json:"nodeKey,omitempty"`
	Version  string `json:"version"`
}

// JoinResponse is the response shape of POST/GET /api/v1/peers/join-request.
type JoinResponse struct {
	State string   `json:"state"`
	Hub   *HubInfo `json:"hub,omitempty"`
}

// DiscoveryConfig parameterises NewDiscovery.
type DiscoveryConfig struct {
	HubURLOverride string
	DefaultHubPort int
	PeerPublicURL  string
}

// Discovery is the long-running auto-pairing coordinator.
type Discovery struct {
	cfg      DiscoveryConfig
	identity *Identity
	store    *store.Store
	logger   *slog.Logger
	client   *http.Client
}

// NewDiscovery wires a Discovery.
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

// Run blocks until ctx is cancelled or the join is approved AND
// the Hub row in peer_registry carries a non-empty node_key. The
// extra "Hub NodeKey landed" condition closes a critical race
// flagged in the Codex re-review:
//
//   - peer hits Hub immediately after Hub boot.
//   - Hub's tsnet hasn't finished its login handshake yet → hub-info
//     returns NodeKey="" .
//   - peer stamps an empty node_key into its local peer_registry.
//   - Owner approves; discovery exits.
//   - Later, Hub dials this peer (Subscriber WS, blob push). The
//     peer's tsnet middleware resolves Hub's NodeKey via WhoIs and
//     looks it up in peer_registry — finds the row with an EMPTY
//     node_key. Mismatch → caller stays Guest → 403 → ghosted peer.
//
// Fix: discovery does not exit until peer_registry carries the
// Hub's real NodeKey. We accept the latest Hub spec returned in
// JoinResponse on every poll and rewrite the row.
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
		latestHub, approved := d.joinUntilApproved(ctx, hubURL)
		if !approved {
			// joinUntilApproved exited because of ctx cancellation
			// OR a sustained Reject cycle. Outer sleep before the
			// next attempt so a permanently-rejecting Hub isn't
			// hammered tightly.
			if ctx.Err() != nil {
				return
			}
			d.sleep(ctx, JoinHeartbeat)
			continue
		}
		// Re-stamp the Hub row using the freshest Hub spec the
		// approved response carries. Exit ONLY after a successful
		// upsert with a non-empty NodeKey; on any failure (empty
		// NodeKey OR DB error on the upsert) sleep and retry so
		// the local Hub row eventually carries the NodeKey other
		// peers' tsnet middleware needs to resolve Hub-inbound
		// requests.
		if latestHub != nil && latestHub.NodeKey != "" {
			if err := d.upsertHubIntoRegistry(ctx, latestHub, hubURL); err != nil {
				d.logger.Warn("peer discovery: refresh Hub row after approval failed; retrying",
					"err", err)
				d.sleep(ctx, JoinHeartbeat)
				continue
			}
			d.logger.Info("peer discovery: paired with Hub", "hub", hubURL)
			return
		}
		// Approved but Hub NodeKey still missing. Re-fetch and
		// loop. Common during the brief window between Hub boot
		// and tsnet login.
		d.logger.Info("peer discovery: approved but Hub NodeKey not yet observed; waiting for tsnet to finish login",
			"hub", hubURL)
		d.sleep(ctx, JoinHeartbeat)
	}
}

// joinUntilApproved posts /join-request once, then polls GET
// /join-request/{id} every JoinHeartbeat until state=="approved".
// Returns true on approval, false on ctx cancellation.
//
// 404 on the poll means the pending row vanished (Owner Reject).
// We re-POST once to land back in pending, but cap consecutive
// re-POSTs at rejectRePostCap so a permanently-rejecting Hub
// doesn't get hammered with one join per minute forever. After
// the cap the function returns false and the outer Run loop
// backs off via its own JoinHeartbeat sleep.
const rejectRePostCap = 3

func (d *Discovery) joinUntilApproved(ctx context.Context, hubURL string) (*HubInfo, bool) {
	resp, err := d.postJoinRequest(ctx, hubURL)
	if err != nil {
		d.logger.Warn("peer discovery: initial join-request failed; will retry on next tick",
			"hub", hubURL, "err", err)
	} else if resp.State == "approved" {
		return resp.Hub, true
	} else if resp.State != "pending" {
		d.logger.Warn("peer discovery: unexpected initial join state",
			"state", resp.State)
	}
	rejectRePosts := 0
	for {
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(JoinHeartbeat):
		}
		poll, hit404, err := d.pollJoinRequest(ctx, hubURL)
		if err != nil {
			d.logger.Warn("peer discovery: join-request poll failed; retrying",
				"hub", hubURL, "err", err)
			continue
		}
		if hit404 {
			rejectRePosts++
			if rejectRePosts > rejectRePostCap {
				d.logger.Warn("peer discovery: pending row repeatedly rejected; backing off (Owner action required)",
					"hub", hubURL, "device_id", d.identity.DeviceID,
					"rePostAttempts", rejectRePosts)
				return nil, false
			}
			d.logger.Info("peer discovery: pending row missing; re-POSTing join-request",
				"hub", hubURL, "attempt", rejectRePosts)
			if _, err := d.postJoinRequest(ctx, hubURL); err != nil {
				d.logger.Warn("peer discovery: re-POST failed", "err", err)
			}
			continue
		}
		switch poll.State {
		case "approved":
			return poll.Hub, true
		case "pending":
			rejectRePosts = 0
			d.logger.Debug("peer discovery: still pending; awaiting Owner approval",
				"hub", hubURL, "device_id", d.identity.DeviceID)
		default:
			d.logger.Warn("peer discovery: unexpected poll state",
				"state", poll.State)
		}
	}
}

// resolveHubURL returns the Hub base URL.
func (d *Discovery) resolveHubURL(ctx context.Context) (string, error) {
	if v := strings.TrimSpace(d.cfg.HubURLOverride); v != "" {
		return d.canonicalHubURL(v)
	}
	if v := strings.TrimSpace(os.Getenv("KOJO_HUB_URL")); v != "" {
		return d.canonicalHubURL(v)
	}
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
	return &info, nil
}

// upsertHubIntoRegistry writes the Hub row into peer_registry.
// NodeKey is left empty here — the Hub's NodeKey is learned later
// when this peer first receives an inbound Hub request (tsnet
// middleware writes it back through TouchPeer).
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
	_, err := d.store.RegisterPeerMetadata(ctx, &store.PeerRecord{
		DeviceID: hub.DeviceID,
		Name:     hub.Name,
		URL:      rowURL,
		NodeKey:  hub.NodeKey,
	})
	return err
}

// postJoinRequest sends our identity to Hub and returns the parsed
// response. No Authorization — the Hub reads our identity from
// tsnet WhoIs on its side.
func (d *Discovery) postJoinRequest(ctx context.Context, hubURL string) (*JoinResponse, error) {
	body, err := json.Marshal(map[string]string{
		"deviceId": d.identity.DeviceID,
		"name":     d.identity.Name,
		"url":      d.cfg.PeerPublicURL,
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
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var jr JoinResponse
	if err := json.Unmarshal(respBody, &jr); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &jr, nil
}

// pollJoinRequest polls GET /join-request/{deviceId}. Returns:
//   - (jr, false, nil) on a 2xx — caller acts on jr.State.
//   - (nil, true, nil)  on a 404 — pending row is gone (Owner
//     Reject or never persisted). The reconnect loop decides
//     whether to re-POST.
//   - (nil, false, err) for any other failure.
func (d *Discovery) pollJoinRequest(ctx context.Context, hubURL string) (*JoinResponse, bool, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		hubURL+"/api/v1/peers/join-request/"+d.identity.DeviceID, nil)
	if err != nil {
		return nil, false, fmt.Errorf("build: %w", err)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if resp.StatusCode == http.StatusNotFound {
		return nil, true, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, false, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var jr JoinResponse
	if err := json.Unmarshal(respBody, &jr); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	return &jr, false, nil
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
// MagicDNSSuffix out of the response.
func readTailnetName(parent context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
	if err != nil {
		return "", fmt.Errorf("tailscale status: %w", err)
	}
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
