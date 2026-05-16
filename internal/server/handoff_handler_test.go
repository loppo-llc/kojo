//go:build heavy_test

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// handoffFixture builds a server with one agent + a fake target
// peer_registry row so the handoff orchestration can find both.
func handoffFixture(t *testing.T, targetPeerID string) (*Server, string) {
	t.Helper()
	srv, mgr := newTestServer(t)
	a, err := mgr.Create(agent.AgentConfig{
		Name: "handoff-agent", Tool: "claude", Model: "sonnet",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := srv.agents.Store().UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID:  targetPeerID,
		Name:      "target",
		PublicKey: "dGVzdA==",
		Status:    store.PeerStatusOnline,
	}); err != nil {
		t.Fatalf("seed target peer: %v", err)
	}
	return srv, a.ID
}

func TestHandoffBegin_OwnerOnly(t *testing.T) {
	srv, agentID := handoffFixture(t, "target-peer-id")
	body := `{"target_peer_id":"target-peer-id"}`
	r := mkReq(http.MethodPost, "/api/v1/agents/"+agentID+"/handoff/begin", body,
		auth.Principal{Role: auth.RoleAgent, AgentID: agentID})
	r.SetPathValue("id", agentID)
	w := httptest.NewRecorder()
	srv.handleAgentHandoffBegin(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-owner: status = %d, want 403", w.Code)
	}
}

func TestHandoffBegin_HappyPath(t *testing.T) {
	srv, agentID := handoffFixture(t, "target-peer-id")
	body := `{"target_peer_id":"target-peer-id"}`
	r := mkReq(http.MethodPost, "/api/v1/agents/"+agentID+"/handoff/begin", body,
		auth.Principal{Role: auth.RoleOwner})
	r.SetPathValue("id", agentID)
	w := httptest.NewRecorder()
	srv.handleAgentHandoffBegin(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("begin: status = %d body=%s", w.Code, w.Body.String())
	}
	var resp handoffResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Op != "begin" {
		t.Errorf("Op = %q", resp.Op)
	}
}

func TestHandoffBegin_RefusesUnknownTarget(t *testing.T) {
	srv, agentID := handoffFixture(t, "target-peer-id")
	body := `{"target_peer_id":"who-is-this"}`
	r := mkReq(http.MethodPost, "/api/v1/agents/"+agentID+"/handoff/begin", body,
		auth.Principal{Role: auth.RoleOwner})
	r.SetPathValue("id", agentID)
	w := httptest.NewRecorder()
	srv.handleAgentHandoffBegin(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown target: status = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "peer_registry") {
		t.Errorf("error should mention peer_registry: %s", w.Body.String())
	}
}

func TestHandoffComplete_TransfersLockAndSwitchesHome(t *testing.T) {
	srv, agentID := handoffFixture(t, "target-peer-id")
	// Pre-acquire a lock so complete can transfer it.
	lock, err := srv.agents.Store().AcquireAgentLock(context.Background(),
		agentID, "peer-a", store.NowMillis(), 60_000)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Pre-seed a blob row that the handoff should switch.
	blobURI := "kojo://global/agents/" + agentID + "/avatar.png"
	if _, err := srv.agents.Store().InsertOrReplaceBlobRef(context.Background(), &store.BlobRefRecord{
		URI:      blobURI,
		Scope:    "global",
		HomePeer: "peer-a",
		Size:     5,
		SHA256:   "abc",
	}, store.BlobRefInsertOptions{}); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	body := `{"target_peer_id":"target-peer-id"}`
	r := mkReq(http.MethodPost, "/api/v1/agents/"+agentID+"/handoff/complete", body,
		auth.Principal{Role: auth.RoleOwner})
	r.SetPathValue("id", agentID)
	w := httptest.NewRecorder()
	srv.handleAgentHandoffComplete(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("complete: status = %d body=%s", w.Code, w.Body.String())
	}
	var resp handoffResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.LockTransferred {
		t.Errorf("LockTransferred = false; want true")
	}
	if resp.LockHolderPeer != "target-peer-id" {
		t.Errorf("LockHolderPeer = %q, want target-peer-id", resp.LockHolderPeer)
	}
	if resp.LockFencing <= lock.FencingToken {
		t.Errorf("token did not advance: %d → %d", lock.FencingToken, resp.LockFencing)
	}
	// Blob row must have flipped home_peer.
	ref, _ := srv.agents.Store().GetBlobRef(context.Background(), blobURI)
	if ref.HomePeer != "target-peer-id" {
		t.Errorf("home_peer = %q, want target-peer-id", ref.HomePeer)
	}
	if ref.HandoffPending {
		t.Errorf("handoff_pending = true after complete")
	}
}

func TestHandoffAbort_ClearsPendingOnly(t *testing.T) {
	srv, agentID := handoffFixture(t, "target-peer-id")
	blobURI := "kojo://global/agents/" + agentID + "/avatar.png"
	if _, err := srv.agents.Store().InsertOrReplaceBlobRef(context.Background(), &store.BlobRefRecord{
		URI:      blobURI,
		Scope:    "global",
		HomePeer: "peer-a",
		Size:     5,
		SHA256:   "abc",
	}, store.BlobRefInsertOptions{}); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	_ = srv.agents.Store().SetBlobRefHandoffPending(context.Background(), blobURI, true)

	r := mkReq(http.MethodPost, "/api/v1/agents/"+agentID+"/handoff/abort", "",
		auth.Principal{Role: auth.RoleOwner})
	r.SetPathValue("id", agentID)
	w := httptest.NewRecorder()
	srv.handleAgentHandoffAbort(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("abort: status = %d body=%s", w.Code, w.Body.String())
	}
	ref, _ := srv.agents.Store().GetBlobRef(context.Background(), blobURI)
	if ref.HandoffPending {
		t.Errorf("handoff_pending must clear after abort")
	}
	if ref.HomePeer != "peer-a" {
		t.Errorf("home_peer changed on abort: %q", ref.HomePeer)
	}
}

// Use fmt in a printf-style format string for a compile-side
// reference so the import isn't pruned by goimports.
var _ = fmt.Sprintf
