package peer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/selfupdate"
)

// peerAutoUpdateMaxBytes caps a Hub binary download at 1 GiB so a
// misbehaving Hub cannot fill the peer disk.
const peerAutoUpdateMaxBytes = 1 << 30

// peerAutoUpdateDownloadTimeout bounds one binary pull. The discovery
// client's own Timeout is 10s (fine for hub-info JSON) and would kill
// a multi-hundred-MB download, so maybeAutoUpdate uses a dedicated
// NoKeepAliveHTTPClient with this deadline instead.
const peerAutoUpdateDownloadTimeout = 10 * time.Minute

// autoUpdateGateInput is the pure decision input for peer auto-update
// (version / platform / attempt / sha presence / hub scheme). Network
// and swap live outside this helper so unit tests need no HTTP.
type autoUpdateGateInput struct {
	AutoUpdate   bool
	HasRestart   bool
	SelfVersion  string
	HubVersion   string
	HubGOOS      string
	HubGOARCH    string
	HubSHA       string
	PeerGOOS     string
	PeerGOARCH   string
	AlreadyTried bool
	// HubHTTPS is true only when the hub URL scheme is "https".
	// TLS cert validation is the only server-authentication the client
	// has; WhoIs authenticates clients, not servers. Over http:// both
	// pins (hub-info binarySha256 and the response header) travel the
	// same plaintext channel, so a MITM can substitute both.
	HubHTTPS bool
}

// autoUpdateSkipReason is why maybeAutoUpdate should not download.
// Empty string means the download path may proceed.
type autoUpdateSkipReason string

const (
	autoUpdateProceed          autoUpdateSkipReason = ""
	autoUpdateSkipDisabled     autoUpdateSkipReason = "disabled"
	autoUpdateSkipNoRestart    autoUpdateSkipReason = "no_restart"
	autoUpdateSkipEmptyVersion autoUpdateSkipReason = "empty_hub_version"
	autoUpdateSkipNotNewer     autoUpdateSkipReason = "not_newer"
	autoUpdateSkipAttempted    autoUpdateSkipReason = "already_attempted"
	autoUpdateSkipInsecureHub  autoUpdateSkipReason = "insecure_hub"
	autoUpdateSkipPlatform     autoUpdateSkipReason = "platform_mismatch"
	autoUpdateSkipNoSHA        autoUpdateSkipReason = "missing_sha"
)

// decideAutoUpdate evaluates the pure gates for peer auto-update.
// IsNewer is false when either version is unparseable (dev builds),
// so those never reach download.
func decideAutoUpdate(in autoUpdateGateInput) autoUpdateSkipReason {
	if !in.AutoUpdate {
		return autoUpdateSkipDisabled
	}
	if !in.HasRestart {
		return autoUpdateSkipNoRestart
	}
	if strings.TrimSpace(in.HubVersion) == "" {
		return autoUpdateSkipEmptyVersion
	}
	if !selfupdate.IsNewer(in.HubVersion, in.SelfVersion) {
		return autoUpdateSkipNotNewer
	}
	if in.AlreadyTried {
		return autoUpdateSkipAttempted
	}
	// TLS is the only server auth the peer has (WhoIs is client-side).
	// Refuse auto-update over non-https so a MITM cannot rewrite both
	// digest pins on a plaintext hub channel.
	if !in.HubHTTPS {
		return autoUpdateSkipInsecureHub
	}
	if in.HubGOOS != in.PeerGOOS || in.HubGOARCH != in.PeerGOARCH {
		return autoUpdateSkipPlatform
	}
	if strings.TrimSpace(in.HubSHA) == "" {
		return autoUpdateSkipNoSHA
	}
	return autoUpdateProceed
}

// maybeAutoUpdate downloads the Hub binary when hub is strictly newer
// and platforms match, verifies dual SHA-256 pins, swaps the local
// executable, and requests a graceful restart.
//
// Security posture: the download is dialed to a Hub this peer already
// paired with over the Tailscale tailnet (tsnet WhoIs identity; no
// Authorization header). Integrity is double-pinned — the digest from
// hub-info (binarySha256) AND the X-Kojo-Binary-SHA256 response header
// must both equal the SHA-256 of the downloaded bytes. A mismatch or
// any I/O failure marks the hub version as attempted so a crash or
// bad response cannot tight-loop every refresh tick.
//
// Concurrency: refreshLoop is the only caller and runs on a single
// goroutine, so attemptedHubVersions needs no mutex.
func (d *Discovery) maybeAutoUpdate(ctx context.Context, hubURL string, hub *HubInfo) {
	if hub == nil {
		return
	}
	if d.attemptedHubVersions == nil {
		d.attemptedHubVersions = make(map[string]bool)
	}
	// TLS cert validation is the only server-authentication the client
	// has; WhoIs authenticates clients, not servers. Over http:// both
	// pins travel the same plaintext channel, so a MITM can substitute
	// both — refuse auto-update unless the hub URL is https.
	hubHTTPS := false
	if u, err := url.Parse(hubURL); err == nil {
		hubHTTPS = u.Scheme == "https"
	}
	reason := decideAutoUpdate(autoUpdateGateInput{
		AutoUpdate:   d.cfg.AutoUpdate,
		HasRestart:   d.cfg.RequestRestart != nil,
		SelfVersion:  d.cfg.SelfVersion,
		HubVersion:   hub.Version,
		HubGOOS:      hub.GOOS,
		HubGOARCH:    hub.GOARCH,
		HubSHA:       hub.BinarySha256,
		PeerGOOS:     runtime.GOOS,
		PeerGOARCH:   runtime.GOARCH,
		AlreadyTried: d.attemptedHubVersions[hub.Version],
		HubHTTPS:     hubHTTPS,
	})
	switch reason {
	case autoUpdateProceed:
		// fall through
	case autoUpdateSkipDisabled, autoUpdateSkipNoRestart,
		autoUpdateSkipEmptyVersion, autoUpdateSkipNotNewer,
		autoUpdateSkipAttempted:
		d.logger.Debug("peer auto-update: skip",
			"reason", string(reason),
			"hub_version", hub.Version,
			"self_version", d.cfg.SelfVersion)
		return
	case autoUpdateSkipInsecureHub:
		// Mark attempted so we log Info once per hub version.
		d.attemptedHubVersions[hub.Version] = true
		d.logger.Info("peer auto-update disabled: hub URL is not https; update manually with 'kojo update'",
			"hub_version", hub.Version,
			"hub_url", hubURL)
		return
	case autoUpdateSkipPlatform:
		// Mark attempted so we log Info once per hub version.
		d.attemptedHubVersions[hub.Version] = true
		d.logger.Info("hub is newer but platform differs; update this peer manually with 'kojo update'",
			"hub_version", hub.Version,
			"self_version", d.cfg.SelfVersion,
			"hub_goos", hub.GOOS, "hub_goarch", hub.GOARCH,
			"self_goos", runtime.GOOS, "self_goarch", runtime.GOARCH)
		return
	case autoUpdateSkipNoSHA:
		d.attemptedHubVersions[hub.Version] = true
		d.logger.Warn("peer auto-update: hub is newer but binarySha256 is empty; skipping",
			"hub_version", hub.Version)
		return
	default:
		d.logger.Debug("peer auto-update: skip", "reason", string(reason))
		return
	}

	// Mark before download so a crash mid-transfer does not retry the
	// same hub version until this peer process restarts.
	d.attemptedHubVersions[hub.Version] = true

	tmpPath, err := d.downloadHubBinary(ctx, hubURL, hub.BinarySha256)
	if err != nil {
		d.logger.Warn("peer auto-update: download failed",
			"hub_version", hub.Version, "err", err)
		return
	}
	// SwapExecutable leaves tmpPath in place on success (it copies);
	// always clean up the download artifact.
	defer os.Remove(tmpPath)

	if err := selfupdate.SwapExecutable(tmpPath); err != nil {
		d.logger.Warn("peer auto-update: swap failed",
			"hub_version", hub.Version, "err", err)
		return
	}
	d.logger.Info("peer auto-update: swapped binary, restarting",
		"from", d.cfg.SelfVersion, "to", hub.Version)
	if d.cfg.RequestRestart == nil {
		// Defensive: decideAutoUpdate already required HasRestart.
		return
	}
	if ok := d.cfg.RequestRestart(); !ok {
		d.logger.Warn("peer auto-update: restart refused (shutdown already in flight)",
			"hub_version", hub.Version)
	}
}

// downloadHubBinary GETs {hubURL}/api/v1/peers/binary, streams into a
// temp file beside this executable while hashing, and verifies the
// body digest equals both expectedSHA (hub-info) and the response
// X-Kojo-Binary-SHA256 header.
func (d *Discovery) downloadHubBinary(ctx context.Context, hubURL, expectedSHA string) (string, error) {
	expectedSHA = strings.ToLower(strings.TrimSpace(expectedSHA))
	if expectedSHA == "" {
		return "", fmt.Errorf("empty expected sha256")
	}

	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".kojo-peer-update-*")
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		tmp.Close()
		if cleanup {
			os.Remove(tmpPath)
		}
	}()

	reqCtx, cancel := context.WithTimeout(ctx, peerAutoUpdateDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		strings.TrimRight(hubURL, "/")+"/api/v1/peers/binary", nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}

	// Dedicated client: discovery's default client has a 10s Timeout
	// that would abort a large binary transfer.
	client := NoKeepAliveHTTPClient(peerAutoUpdateDownloadTimeout)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET binary: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("GET binary: status %d", resp.StatusCode)
	}
	headerSHA := strings.ToLower(strings.TrimSpace(resp.Header.Get("X-Kojo-Binary-SHA256")))
	if headerSHA == "" {
		return "", fmt.Errorf("missing X-Kojo-Binary-SHA256 header")
	}
	if headerSHA != expectedSHA {
		return "", fmt.Errorf("header sha256 %s != hub-info sha256 %s", headerSHA, expectedSHA)
	}

	hasher := sha256.New()
	limited := io.LimitReader(resp.Body, peerAutoUpdateMaxBytes+1)
	n, err := io.Copy(io.MultiWriter(tmp, hasher), limited)
	if err != nil {
		return "", fmt.Errorf("stream body: %w", err)
	}
	if n > peerAutoUpdateMaxBytes {
		return "", fmt.Errorf("binary exceeds %d byte cap", peerAutoUpdateMaxBytes)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temp: %w", err)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if got != expectedSHA {
		return "", fmt.Errorf("body sha256 %s != expected %s", got, expectedSHA)
	}
	if got != headerSHA {
		return "", fmt.Errorf("body sha256 %s != header %s", got, headerSHA)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return "", fmt.Errorf("chmod temp: %w", err)
	}
	cleanup = false
	return tmpPath, nil
}
