package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// TestApplyFinalizeTailMessage_AppendsAtMaxSeqPlusOne is the unit gate
// for the device-switch deferred-finalize tail apply. The handler
// stamps the row at (current max seq) + 1 so order matches the rest
// of the transcript without source needing to know target's seq
// arithmetic.
func TestApplyFinalizeTailMessage_AppendsAtMaxSeqPlusOne(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	ctx := context.Background()
	st := srv.agents.Store()

	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_t", Name: "t"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	// Seed two existing rows so max-seq lookup has work to do.
	if _, err := st.AppendMessage(ctx, &store.MessageRecord{
		ID: "u1", AgentID: "ag_t", Role: "user", Content: "transfer + check",
	}, store.MessageInsertOptions{}); err != nil {
		t.Fatalf("append u1: %v", err)
	}
	if _, err := st.AppendMessage(ctx, &store.MessageRecord{
		ID: "a_snap", AgentID: "ag_t", Role: "assistant", Content: "",
	}, store.MessageInsertOptions{}); err != nil {
		t.Fatalf("append a_snap: %v", err)
	}

	tail := &store.MessageRecord{
		ID:      "a_tail",
		AgentID: "ag_t",
		Role:    "assistant",
		Content: "到着したらセキュリティチェックを実施する。",
	}
	if err := srv.applyFinalizeTailMessage(ctx, "ag_t", tail); err != nil {
		t.Fatalf("applyFinalizeTailMessage: %v", err)
	}

	got, err := st.GetMessage(ctx, "a_tail")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Seq != 3 {
		t.Errorf("seq = %d; want 3 (max=2 + 1)", got.Seq)
	}
	if got.Content != tail.Content {
		t.Errorf("content = %q; want %q", got.Content, tail.Content)
	}
	if got.Role != "assistant" {
		t.Errorf("role = %q; want assistant", got.Role)
	}
}

// TestApplyFinalizeTailMessage_FirstRowSeqIsOne: with no pre-existing
// rows the tail lands at seq=1.
func TestApplyFinalizeTailMessage_FirstRowSeqIsOne(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	ctx := context.Background()
	st := srv.agents.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_e", Name: "e"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	tail := &store.MessageRecord{
		ID:      "a_only",
		AgentID: "ag_e",
		Role:    "assistant",
		Content: "tail",
	}
	if err := srv.applyFinalizeTailMessage(ctx, "ag_e", tail); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, err := st.GetMessage(ctx, "a_only")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Seq != 1 {
		t.Errorf("seq = %d; want 1", got.Seq)
	}
}

// TestApplyFinalizeTailMessage_NilIsNoop: a nil tail message must not
// error and must not allocate a row.
func TestApplyFinalizeTailMessage_NilIsNoop(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	ctx := context.Background()
	st := srv.agents.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_n", Name: "n"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := srv.applyFinalizeTailMessage(ctx, "ag_n", nil); err != nil {
		t.Errorf("nil tail returned err: %v", err)
	}
	msgs, err := st.ListMessages(ctx, "ag_n", store.MessageListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected zero messages; got %d", len(msgs))
	}
}

// TestApplyFinalizeTailMessage_RejectsMissingID: an empty id is a
// caller bug — better to fail loud than UPSERT under whatever id
// SyncAgentFromPeer fabricates.
func TestApplyFinalizeTailMessage_RejectsMissingID(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	tail := &store.MessageRecord{
		AgentID: "ag_x",
		Role:    "assistant",
		Content: "tail",
	}
	err := srv.applyFinalizeTailMessage(context.Background(), "ag_x", tail)
	if err == nil {
		t.Fatal("expected error for missing id")
	}
	if !strings.Contains(err.Error(), "id required") {
		t.Errorf("error = %q; want substring 'id required'", err.Error())
	}
}

// TestApplyFinalizeTailMessage_RejectsAgentIDMismatch: a wire payload
// where TailMessage.AgentID disagrees with the URL agent_id is a
// boundary-routing bug; the receiver MUST refuse rather than write
// under either id.
func TestApplyFinalizeTailMessage_RejectsAgentIDMismatch(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	tail := &store.MessageRecord{
		ID:      "a1",
		AgentID: "ag_wrong",
		Role:    "assistant",
		Content: "x",
	}
	err := srv.applyFinalizeTailMessage(context.Background(), "ag_right", tail)
	if err == nil {
		t.Fatal("expected error for agent_id mismatch")
	}
	if !strings.Contains(err.Error(), "agent_id mismatch") {
		t.Errorf("error = %q; want substring 'agent_id mismatch'", err.Error())
	}
}

// TestApplyFinalizeTailMessage_NoLockReturnsLockNotSelf: when target
// has no agent_locks row for this agent (ErrNotFound — pre-finalize
// race where the lock hasn't transferred yet), the helper must
// return errTailLockNotSelf so the handler 503s and the orchestrator
// retry loop picks it up.
func TestApplyFinalizeTailMessage_NoLockReturnsLockNotSelf(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	// Stamp a peerID so the fencing branch is exercised.
	srv.peerID = &peer.Identity{DeviceID: "self-peer"}
	ctx := context.Background()
	st := srv.agents.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_nolock", Name: "n"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	tail := &store.MessageRecord{
		ID:      "a_lock_race",
		AgentID: "ag_nolock",
		Role:    "assistant",
		Content: "would-be tail",
	}
	err := srv.applyFinalizeTailMessage(ctx, "ag_nolock", tail)
	if !errors.Is(err, errTailLockNotSelf) {
		t.Errorf("expected errTailLockNotSelf for missing agent_locks row; got %v", err)
	}
	// Tail must NOT have been written.
	if _, gerr := st.GetMessage(ctx, "a_lock_race"); !errors.Is(gerr, store.ErrNotFound) {
		t.Errorf("tail row was written despite lock_not_self; got err %v", gerr)
	}
}

// TestApplyFinalizeTailMessage_HolderMismatchReturnsLockNotSelf:
// the agent_locks row exists but its HolderPeer is some other peer
// (force-reclaim race or source-release straggler). The helper
// must surface errTailLockNotSelf, NOT write the tail under a
// stolen-lock window.
func TestApplyFinalizeTailMessage_HolderMismatchReturnsLockNotSelf(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	srv.peerID = &peer.Identity{DeviceID: "self-peer"}
	ctx := context.Background()
	st := srv.agents.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_stolen", Name: "s"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Acquire the lock as "other-peer" so the holder mismatch path
	// fires when self-peer tries the tail apply.
	if _, err := st.AcquireAgentLock(ctx, "ag_stolen", "other-peer", 30_000, 60_000); err != nil {
		t.Fatalf("acquire (other-peer): %v", err)
	}
	tail := &store.MessageRecord{
		ID:      "a_stolen",
		AgentID: "ag_stolen",
		Role:    "assistant",
		Content: "would-be tail",
	}
	err := srv.applyFinalizeTailMessage(ctx, "ag_stolen", tail)
	if !errors.Is(err, errTailLockNotSelf) {
		t.Errorf("expected errTailLockNotSelf for stolen lock; got %v", err)
	}
	if _, gerr := st.GetMessage(ctx, "a_stolen"); !errors.Is(gerr, store.ErrNotFound) {
		t.Errorf("tail row was written despite stolen lock; got err %v", gerr)
	}
}

// TestApplyFinalizeTailMessage_WritesUnderCorrectHolder: when target
// IS the holder, the fencing predicate matches and the tail lands
// at max-seq+1 as expected.
func TestApplyFinalizeTailMessage_WritesUnderCorrectHolder(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	srv.peerID = &peer.Identity{DeviceID: "self-peer"}
	ctx := context.Background()
	st := srv.agents.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_ok", Name: "o"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.AcquireAgentLock(ctx, "ag_ok", "self-peer", 30_000, 60_000); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	tail := &store.MessageRecord{
		ID:      "a_ok",
		AgentID: "ag_ok",
		Role:    "assistant",
		Content: "lands cleanly",
	}
	if err := srv.applyFinalizeTailMessage(ctx, "ag_ok", tail); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, err := st.GetMessage(ctx, "a_ok")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Seq != 1 {
		t.Errorf("seq = %d; want 1 (first row)", got.Seq)
	}
}

// TestApplyFinalizeTailMessage_IdempotentUnderRetry: a second apply
// with the same id (same content) succeeds without duplicating rows.
// Mirrors the orchestrator's retry semantics — finalize re-attempts
// on lock_not_self must not append the tail twice.
func TestApplyFinalizeTailMessage_IdempotentUnderRetry(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	ctx := context.Background()
	st := srv.agents.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_i", Name: "i"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	tail := &store.MessageRecord{
		ID:      "a_r",
		AgentID: "ag_i",
		Role:    "assistant",
		Content: "retry tail",
	}
	if err := srv.applyFinalizeTailMessage(ctx, "ag_i", tail); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := srv.applyFinalizeTailMessage(ctx, "ag_i", tail); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	msgs, err := st.ListMessages(ctx, "ag_i", store.MessageListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("expected 1 row after idempotent retry; got %d", len(msgs))
	}
}
