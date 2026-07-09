package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/loppo-llc/kojo/internal/auth"
)

func TestHandlePeerBinary_ForbiddenForGuestAndAgent(t *testing.T) {
	srv := &Server{logger: slog.Default()}
	for _, p := range []auth.Principal{
		{Role: auth.RoleGuest},
		{Role: auth.RoleAgent, AgentID: "ag_x"},
	} {
		rr := httptest.NewRecorder()
		r := authedRequest(httptest.NewRequest(http.MethodGet, "/api/v1/peers/binary", nil), p)
		srv.handlePeerBinary(rr, r)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("role=%v status=%d, want 403", p.Role, rr.Code)
		}
	}
}

func TestHandlePeerBinary_ServesExecutableForPeerAndOwner(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "hub-bin")
	payload := []byte("fake-hub-binary-contents-for-peer-autoupdate")
	if err := os.WriteFile(binPath, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	wantSHA := hex.EncodeToString(sum[:])

	prev := hubBinaryPath
	hubBinaryPath = func() (string, error) { return binPath, nil }
	t.Cleanup(func() { hubBinaryPath = prev })

	srv := &Server{logger: slog.Default()}
	for _, p := range []auth.Principal{
		{Role: auth.RolePeer, PeerID: "peer-1"},
		{Role: auth.RoleOwner},
	} {
		rr := httptest.NewRecorder()
		r := authedRequest(httptest.NewRequest(http.MethodGet, "/api/v1/peers/binary", nil), p)
		srv.handlePeerBinary(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("role=%v status=%d body=%s", p.Role, rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("X-Kojo-Binary-SHA256"); got != wantSHA {
			t.Fatalf("role=%v X-Kojo-Binary-SHA256=%q want %q", p.Role, got, wantSHA)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "application/octet-stream" {
			t.Fatalf("Content-Type=%q", ct)
		}
		body, err := io.ReadAll(rr.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != string(payload) {
			t.Fatalf("body mismatch: got %q want %q", body, payload)
		}
		// Recompute from body must match header.
		bodySum := sha256.Sum256(body)
		if hex.EncodeToString(bodySum[:]) != wantSHA {
			t.Fatalf("body digest drift")
		}
	}
}

func TestEnrichHubInfoPlatform_MatchesServedBinary(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "hub-bin")
	payload := []byte("hub-info-platform-fixture")
	if err := os.WriteFile(binPath, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	wantSHA := hex.EncodeToString(sum[:])

	prev := hubBinaryPath
	hubBinaryPath = func() (string, error) { return binPath, nil }
	t.Cleanup(func() { hubBinaryPath = prev })

	srv := &Server{logger: slog.Default(), version: "v9.8.7"}
	resp := hubInfoResponse{Version: "v9.8.7"}
	srv.enrichHubInfoPlatform(&resp)
	if resp.GOOS != runtime.GOOS || resp.GOARCH != runtime.GOARCH {
		t.Fatalf("platform = %s/%s, want %s/%s", resp.GOOS, resp.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
	if resp.BinarySha256 != wantSHA {
		t.Fatalf("binarySha256 = %q, want %q", resp.BinarySha256, wantSHA)
	}

	// Served body header must agree with hub-info.
	rr := httptest.NewRecorder()
	r := authedRequest(httptest.NewRequest(http.MethodGet, "/api/v1/peers/binary", nil),
		auth.Principal{Role: auth.RolePeer, PeerID: "p"})
	srv.handlePeerBinary(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("binary status=%d", rr.Code)
	}
	if got := rr.Header().Get("X-Kojo-Binary-SHA256"); got != resp.BinarySha256 {
		t.Fatalf("header %q != hub-info %q", got, resp.BinarySha256)
	}
}

func TestOpenHubBinary_RecomputesWhenFileChanges(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "hub-bin")
	if err := os.WriteFile(binPath, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := hubBinaryPath
	hubBinaryPath = func() (string, error) { return binPath, nil }
	t.Cleanup(func() { hubBinaryPath = prev })

	srv := &Server{logger: slog.Default()}
	f1, dig1, _, err := srv.openHubBinary()
	if err != nil {
		t.Fatal(err)
	}
	f1.Close()
	sum1 := sha256.Sum256([]byte("v1"))
	if dig1 != hex.EncodeToString(sum1[:]) {
		t.Fatalf("dig1=%s", dig1)
	}

	// Same content → cache hit path still returns same digest.
	f2, dig2, _, err := srv.openHubBinary()
	if err != nil {
		t.Fatal(err)
	}
	f2.Close()
	if dig2 != dig1 {
		t.Fatalf("cached digest changed without file rewrite")
	}

	// Rewrite with different size so mtime/size key invalidates.
	if err := os.WriteFile(binPath, []byte("v2-longer"), 0o755); err != nil {
		t.Fatal(err)
	}
	f3, dig3, _, err := srv.openHubBinary()
	if err != nil {
		t.Fatal(err)
	}
	f3.Close()
	sum3 := sha256.Sum256([]byte("v2-longer"))
	if dig3 != hex.EncodeToString(sum3[:]) {
		t.Fatalf("dig3=%s want recompute", dig3)
	}
	if dig3 == dig1 {
		t.Fatalf("digest did not change after rewrite")
	}
}
