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

// oplogTestFixture builds a server + an agent with a held lock so
// each test can craft entries with a valid (peer, fencing_token)
// tuple. Returns (server, agentID, peer, fencingToken).
func oplogTestFixture(t *testing.T) (*Server, string, string, int64) {
	t.Helper()
	srv, mgr := newTestServer(t)

	a, err := mgr.Create(agent.AgentConfig{Name: "opname", Tool: "claude", Model: "sonnet"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	peer := "peer-test"
	lock, err := srv.agents.Store().AcquireAgentLock(
		context.Background(), a.ID, peer, store.NowMillis(), 5*60*1000,
	)
	if err != nil {
		t.Fatalf("AcquireAgentLock: %v", err)
	}
	return srv, a.ID, peer, lock.FencingToken
}

func postFlush(srv *Server, body string, p auth.Principal) *httptest.ResponseRecorder {
	r := mkReq(http.MethodPost, "/api/v1/oplog/flush", body, p)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleOplogFlush(w, r)
	return w
}

func TestOplogFlush_OwnerOnly(t *testing.T) {
	srv, _, peer, ft := oplogTestFixture(t)
	body := fmt.Sprintf(`{"peer_id":%q,"entries":[
		{"op_id":"op-1","agent_id":"ag_x","fencing_token":%d,"table":"agent_messages","op":"insert","body":{},"client_ts":1}
	]}`, peer, ft)
	w := postFlush(srv, body, auth.Principal{Role: auth.RoleAgent, AgentID: "ag_x"})
	if w.Code != http.StatusForbidden {
		t.Errorf("non-owner: status = %d, want 403", w.Code)
	}
}

func TestOplogFlush_BatchRejectsOnFencingMismatch(t *testing.T) {
	srv, agentID, peer, ft := oplogTestFixture(t)
	// One entry good, one entry stale fencing — whole batch must
	// reject per docs §3.13.1 step 5.1.
	body := fmt.Sprintf(`{"peer_id":%q,"entries":[
		{"op_id":"op-1","agent_id":%q,"fencing_token":%d,"table":"agent_messages","op":"insert","body":{"role":"user","content":"hello"},"client_ts":1},
		{"op_id":"op-2","agent_id":%q,"fencing_token":%d,"table":"agent_messages","op":"insert","body":{"role":"user","content":"stale"},"client_ts":2}
	]}`, peer, agentID, ft, agentID, ft+999)
	w := postFlush(srv, body, auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", w.Code, w.Body.String())
	}
	var res oplogFlushResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.Rejected {
		t.Errorf("Rejected = false, want true")
	}
	if !strings.Contains(res.RejectReason, "fencing_token") {
		t.Errorf("RejectReason should mention fencing_token: %q", res.RejectReason)
	}
	// CRITICAL: no entry must have been dispatched. The first
	// entry's good fencing would otherwise tempt the implementation
	// to partial-apply; verify by listing messages.
	msgs, err := srv.agents.Store().ListMessages(context.Background(), agentID, store.MessageListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("partial dispatch detected: %d messages persisted", len(msgs))
	}
}

func TestOplogFlush_HappyPathInsertsMessage(t *testing.T) {
	srv, agentID, peer, ft := oplogTestFixture(t)
	body := fmt.Sprintf(`{"peer_id":%q,"entries":[
		{"op_id":"op-msg-1","agent_id":%q,"fencing_token":%d,"table":"agent_messages","op":"insert","body":{"role":"user","content":"hello world"},"client_ts":12345}
	]}`, peer, agentID, ft)
	w := postFlush(srv, body, auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var res oplogFlushResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Rejected {
		t.Errorf("Rejected = true, want false")
	}
	if len(res.Results) != 1 || res.Results[0].Status != "ok" {
		t.Fatalf("results = %+v, want one ok", res.Results)
	}
	if res.Results[0].ETag == "" {
		t.Errorf("etag should be returned: %+v", res.Results[0])
	}
	// Verify the message landed with op_id as ID.
	got, err := srv.agents.Store().GetMessage(context.Background(), "op-msg-1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Content != "hello world" {
		t.Errorf("content = %q", got.Content)
	}
}

func TestOplogFlush_ReplayIsIdempotent(t *testing.T) {
	srv, agentID, peer, ft := oplogTestFixture(t)
	body := fmt.Sprintf(`{"peer_id":%q,"entries":[
		{"op_id":"op-idem-1","agent_id":%q,"fencing_token":%d,"table":"agent_messages","op":"insert","body":{"role":"user","content":"idem"},"client_ts":12345}
	]}`, peer, agentID, ft)
	// First call: ok.
	w1 := postFlush(srv, body, auth.Principal{Role: auth.RoleOwner})
	if w1.Code != http.StatusOK {
		t.Fatalf("first call: status = %d", w1.Code)
	}
	var first oplogFlushResponse
	_ = json.Unmarshal(w1.Body.Bytes(), &first)
	if len(first.Results) != 1 || first.Results[0].Status != "ok" {
		t.Fatalf("first call: %+v", first)
	}
	firstETag := first.Results[0].ETag

	// Second call with the same op_id: still ok (idempotent),
	// same etag (no double-write).
	w2 := postFlush(srv, body, auth.Principal{Role: auth.RoleOwner})
	if w2.Code != http.StatusOK {
		t.Fatalf("replay: status = %d", w2.Code)
	}
	var second oplogFlushResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &second)
	if len(second.Results) != 1 || second.Results[0].Status != "ok" {
		t.Fatalf("replay: %+v", second)
	}
	if second.Results[0].ETag != firstETag {
		t.Errorf("replay etag drifted: %q != %q", second.Results[0].ETag, firstETag)
	}
	// And only ONE message must exist.
	msgs, _ := srv.agents.Store().ListMessages(context.Background(), agentID, store.MessageListOptions{Limit: 10})
	if len(msgs) != 1 {
		t.Errorf("replay duplicated: %d messages", len(msgs))
	}
}

func TestOplogFlush_DispatchesMemoryEntryInsert(t *testing.T) {
	srv, agentID, peer, ft := oplogTestFixture(t)
	body := fmt.Sprintf(`{"peer_id":%q,"entries":[
		{"op_id":"mem-1","agent_id":%q,"fencing_token":%d,"table":"memory_entries","op":"insert","body":{"kind":"journal","name":"entry-1","body":"note"},"client_ts":1}
	]}`, peer, agentID, ft)
	w := postFlush(srv, body, auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var res oplogFlushResponse
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if len(res.Results) != 1 || res.Results[0].Status != "ok" {
		t.Errorf("results = %+v", res.Results)
	}
	got, err := srv.agents.Store().GetMemoryEntry(context.Background(), "mem-1")
	if err != nil {
		t.Fatalf("GetMemoryEntry: %v", err)
	}
	if got.Body != "note" {
		t.Errorf("body = %q", got.Body)
	}
}

func TestOplogFlush_UnsupportedTableSurfacesPerEntry(t *testing.T) {
	srv, agentID, peer, ft := oplogTestFixture(t)
	body := fmt.Sprintf(`{"peer_id":%q,"entries":[
		{"op_id":"bad-1","agent_id":%q,"fencing_token":%d,"table":"agent_messages","op":"delete","body":{},"client_ts":1}
	]}`, peer, agentID, ft)
	w := postFlush(srv, body, auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var res oplogFlushResponse
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if len(res.Results) != 1 || res.Results[0].Status != "error" {
		t.Errorf("expected per-entry error: %+v", res.Results)
	}
	if !strings.Contains(res.Results[0].Error, "unsupported") {
		t.Errorf("error should mention unsupported: %q", res.Results[0].Error)
	}
}

func TestOplogFlush_RejectsMalformedBody(t *testing.T) {
	srv, _, peer, ft := oplogTestFixture(t)
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"missing peer_id", fmt.Sprintf(`{"entries":[{"op_id":"x","agent_id":"ag_x","fencing_token":%d,"table":"agent_messages","op":"insert","body":{},"client_ts":1}]}`, ft)},
		{"missing entries", fmt.Sprintf(`{"peer_id":%q}`, peer)},
		{"empty entries", fmt.Sprintf(`{"peer_id":%q,"entries":[]}`, peer)},
		{"unknown field", fmt.Sprintf(`{"peer_id":%q,"entries":[],"extra":1}`, peer)},
		{"duplicate op_id", fmt.Sprintf(`{"peer_id":%q,"entries":[
			{"op_id":"dup","agent_id":"ag_x","fencing_token":%d,"table":"agent_messages","op":"insert","body":{},"client_ts":1},
			{"op_id":"dup","agent_id":"ag_x","fencing_token":%d,"table":"agent_messages","op":"insert","body":{},"client_ts":2}
		]}`, peer, ft, ft)},
		{"op_id with newline", fmt.Sprintf(`{"peer_id":%q,"entries":[
			{"op_id":"a\nb","agent_id":"ag_x","fencing_token":%d,"table":"agent_messages","op":"insert","body":{},"client_ts":1}
		]}`, peer, ft)},
		{"fencing_token zero", fmt.Sprintf(`{"peer_id":%q,"entries":[
			{"op_id":"z","agent_id":"ag_x","fencing_token":0,"table":"agent_messages","op":"insert","body":{},"client_ts":1}
		]}`, peer)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := postFlush(srv, c.body, auth.Principal{Role: auth.RoleOwner})
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
			}
		})
	}
}

func TestOplogFlush_BodyTooLarge(t *testing.T) {
	srv, _, _, _ := oplogTestFixture(t)
	// Build a body larger than the cap; the cheapest way is a long
	// padding field — Go's MaxBytesReader fires before
	// DisallowUnknownFields gets a chance to surface the unknown
	// field error, so the cap is what we exercise.
	pad := strings.Repeat("a", oplogFlushMaxBytes+1024)
	body := fmt.Sprintf(`{"peer_id":"p","entries":[],"pad":%q}`, pad)
	w := postFlush(srv, body, auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

func TestOplogFlush_NoEntriesPostDispatchOnFencingReject(t *testing.T) {
	// Variant of BatchRejectsOnFencingMismatch where ALL entries are
	// stale — exercises the "no agent_locks row" code path.
	srv, agentID, peer, _ := oplogTestFixture(t)
	body := fmt.Sprintf(`{"peer_id":%q,"entries":[
		{"op_id":"stale-1","agent_id":%q,"fencing_token":99999,"table":"agent_messages","op":"insert","body":{"role":"user","content":"x"},"client_ts":1}
	]}`, peer, agentID)
	w := postFlush(srv, body, auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

// Ensure handler returns 503 when wired without a store.
func TestOplogFlush_NoStoreReturnsUnavailable(t *testing.T) {
	srv := &Server{}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/oplog/flush", strings.NewReader("{}"))
	r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Role: auth.RoleOwner}))
	w := httptest.NewRecorder()
	srv.handleOplogFlush(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

