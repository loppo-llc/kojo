package peer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/loppo-llc/kojo/internal/selfupdate"
)

func TestDecideAutoUpdate(t *testing.T) {
	base := autoUpdateGateInput{
		AutoUpdate:  true,
		HasRestart:  true,
		SelfVersion: "v1.0.0",
		HubVersion:  "v1.1.0",
		HubGOOS:     runtime.GOOS,
		HubGOARCH:   runtime.GOARCH,
		HubSHA:      "abc",
		PeerGOOS:    runtime.GOOS,
		PeerGOARCH:  runtime.GOARCH,
		HubHTTPS:    true,
	}
	cases := []struct {
		name string
		mod  func(*autoUpdateGateInput)
		want autoUpdateSkipReason
	}{
		{"proceed", nil, autoUpdateProceed},
		{"disabled", func(in *autoUpdateGateInput) { in.AutoUpdate = false }, autoUpdateSkipDisabled},
		{"no restart", func(in *autoUpdateGateInput) { in.HasRestart = false }, autoUpdateSkipNoRestart},
		{"empty hub version", func(in *autoUpdateGateInput) { in.HubVersion = "" }, autoUpdateSkipEmptyVersion},
		{"not newer", func(in *autoUpdateGateInput) { in.HubVersion = "v0.9.0" }, autoUpdateSkipNotNewer},
		{"dev self never updates", func(in *autoUpdateGateInput) { in.SelfVersion = "dev" }, autoUpdateSkipNotNewer},
		{"dev hub never updates", func(in *autoUpdateGateInput) { in.HubVersion = "dev" }, autoUpdateSkipNotNewer},
		{"already attempted", func(in *autoUpdateGateInput) { in.AlreadyTried = true }, autoUpdateSkipAttempted},
		{"insecure hub (not https)", func(in *autoUpdateGateInput) { in.HubHTTPS = false }, autoUpdateSkipInsecureHub},
		{"platform mismatch", func(in *autoUpdateGateInput) { in.HubGOOS = "plan9" }, autoUpdateSkipPlatform},
		{"missing sha", func(in *autoUpdateGateInput) { in.HubSHA = "" }, autoUpdateSkipNoSHA},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := base
			if c.mod != nil {
				c.mod(&in)
			}
			if got := decideAutoUpdate(in); got != c.want {
				t.Fatalf("decideAutoUpdate = %q, want %q", got, c.want)
			}
		})
	}
}

func TestMaybeAutoUpdate_DownloadSwapAndOncePerVersion(t *testing.T) {
	// Fake "new" binary the Hub serves. TLS server so the https gate passes
	// (plain httptest is http:// and is refused by the MITM check).
	payload := []byte("newer-hub-binary-bytes")
	sum := sha256.Sum256(payload)
	wantSHA := hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/peers/binary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Kojo-Binary-SHA256", wantSHA)
		_, _ = w.Write(payload)
	})
	ts := httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)

	// Point SwapExecutable at a throwaway file so the real test binary
	// is not overwritten.
	target := filepath.Join(t.TempDir(), "peer-exe")
	if err := os.WriteFile(target, []byte("old-peer"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfupdate.SetExecPathForTest(t, target)

	// downloadHubBinary builds its own HTTP client; install the test
	// TLS cert into the default transport so GET succeeds.
	prevTransport := http.DefaultTransport
	http.DefaultTransport = ts.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = prevTransport })

	var restarts atomic.Int32
	d := &Discovery{
		cfg: DiscoveryConfig{
			SelfVersion: "v1.0.0",
			AutoUpdate:  true,
			RequestRestart: func() bool {
				restarts.Add(1)
				return true
			},
		},
		logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		attemptedHubVersions: make(map[string]bool),
	}

	hub := &HubInfo{
		Version:      "v1.2.0",
		GOOS:         runtime.GOOS,
		GOARCH:       runtime.GOARCH,
		BinarySha256: wantSHA,
	}
	d.maybeAutoUpdate(context.Background(), ts.URL, hub)

	if restarts.Load() != 1 {
		t.Fatalf("RequestRestart calls = %d, want 1", restarts.Load())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("swapped binary = %q, want %q", got, payload)
	}

	// Second call with the same hub version must not download/swap again.
	d.maybeAutoUpdate(context.Background(), ts.URL, hub)
	if restarts.Load() != 1 {
		t.Fatalf("second call restarted again: %d", restarts.Load())
	}
}

func TestMaybeAutoUpdate_SkipsNonHTTPS(t *testing.T) {
	var restarts atomic.Int32
	d := &Discovery{
		cfg: DiscoveryConfig{
			SelfVersion:    "v1.0.0",
			AutoUpdate:     true,
			RequestRestart: func() bool { restarts.Add(1); return true },
		},
		logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		attemptedHubVersions: make(map[string]bool),
	}
	d.maybeAutoUpdate(context.Background(), "http://hub.example:8080", &HubInfo{
		Version:      "v9.9.9",
		GOOS:         runtime.GOOS,
		GOARCH:       runtime.GOARCH,
		BinarySha256: "deadbeef",
	})
	if restarts.Load() != 0 {
		t.Fatalf("restart fired over http hub")
	}
	if !d.attemptedHubVersions["v9.9.9"] {
		t.Fatalf("insecure hub path must mark version attempted (log once)")
	}
}

func TestMaybeAutoUpdate_SkipsWhenDisabled(t *testing.T) {
	var restarts atomic.Int32
	d := &Discovery{
		cfg: DiscoveryConfig{
			SelfVersion:    "v1.0.0",
			AutoUpdate:     false,
			RequestRestart: func() bool { restarts.Add(1); return true },
		},
		logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		attemptedHubVersions: make(map[string]bool),
	}
	d.maybeAutoUpdate(context.Background(), "http://127.0.0.1:9", &HubInfo{
		Version:      "v9.9.9",
		GOOS:         runtime.GOOS,
		GOARCH:       runtime.GOARCH,
		BinarySha256: "deadbeef",
	})
	if restarts.Load() != 0 {
		t.Fatalf("restart fired when AutoUpdate=false")
	}
	if d.attemptedHubVersions["v9.9.9"] {
		t.Fatalf("disabled path should not mark version attempted")
	}
}
