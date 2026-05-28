package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// docs/multi-device-storage.md §3.14 — split-brain prevention is
// structural in v1: all writes go through Hub-allocated fencing
// tokens, and the in-tx FencingPredicate guards every agent-runtime
// write path. These tests pin the structural property as exercisable
// invariants rather than as design comments alone.
//
// Test layout: each scenario simulates two peers racing on the same
// agent (a "current holder" and a "would-be steal"), then exercises
// the write helpers' FencingPredicate / IdempotencyTag against the
// post-rotation state.

// seedAgentForLock returns an agent id + the current lock token a
// peer would carry. Used by the test helpers below.
func seedAgentForLock(t *testing.T, s *Store, agentID, peer string, leaseMs int64) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := s.InsertAgent(ctx, &AgentRecord{ID: agentID, Name: "sb-test"}, AgentInsertOptions{}); err != nil {
		t.Fatalf("InsertAgent: %v", err)
	}
	lock, err := s.AcquireAgentLock(ctx, agentID, peer, NowMillis(), leaseMs)
	if err != nil {
		t.Fatalf("AcquireAgentLock: %v", err)
	}
	return lock.FencingToken
}

// TestSplitBrain_AppendMessageRejectsStaleFencingToken covers the
// canonical split-brain case: peer-A held the lock and prepared a
// transcript append; peer-B then stole the lock under an expired
// lease; peer-A's write arrives carrying its old fencing token.
// The FencingPredicate must reject — without this, peer-A could
// silently append to a transcript a different peer is now writing.
func TestSplitBrain_AppendMessageRejectsStaleFencingToken(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	agentID := "ag_sb_1"
	// peer-a gets the initial lock (short lease so peer-b's
	// later Acquire steals it).
	tokenA := seedAgentForLock(t, s, agentID, "peer-a", 1)
	// Wait for the lease to age out, then peer-b steals.
	time.Sleep(2 * time.Millisecond)
	lockB, err := s.AcquireAgentLock(ctx, agentID, "peer-b", NowMillis(), 60_000)
	if err != nil {
		t.Fatalf("steal: %v", err)
	}
	if lockB.FencingToken <= tokenA {
		t.Fatalf("steal did not advance fencing token: %d → %d", tokenA, lockB.FencingToken)
	}
	// peer-a's delayed write carries its old token. Must reject.
	_, err = s.AppendMessage(ctx, &MessageRecord{
		ID: "m_stale", AgentID: agentID, Role: "user", Content: "stale write",
	}, MessageInsertOptions{
		Fencing: &FencingPredicate{
			AgentID:      agentID,
			Peer:         "peer-a",
			FencingToken: tokenA,
		},
	})
	if !errors.Is(err, ErrFencingMismatch) {
		t.Fatalf("AppendMessage with stale token: want ErrFencingMismatch, got %v", err)
	}
	// peer-b's fresh token must succeed against the same row.
	rec, err := s.AppendMessage(ctx, &MessageRecord{
		ID: "m_fresh", AgentID: agentID, Role: "user", Content: "fresh write",
	}, MessageInsertOptions{
		Fencing: &FencingPredicate{
			AgentID:      agentID,
			Peer:         "peer-b",
			FencingToken: lockB.FencingToken,
		},
	})
	if err != nil {
		t.Fatalf("AppendMessage with fresh token: %v", err)
	}
	if rec.Content != "fresh write" {
		t.Errorf("unexpected content: %q", rec.Content)
	}
}

// TestSplitBrain_UpdateMemoryEntryRejectsCrossAgentFencing covers
// the scope-escape vector closed by the row-vs-fencing agent_id
// match in UpdateMemoryEntry. A peer with a valid lock for agent A
// MUST NOT be able to patch a memory entry that belongs to agent B
// just because it threaded body.id pointing at B's row.
func TestSplitBrain_UpdateMemoryEntryRejectsCrossAgentFencing(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// Two agents; a memory entry on each.
	if _, err := s.InsertAgent(ctx, &AgentRecord{ID: "ag_a", Name: "A"}, AgentInsertOptions{}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if _, err := s.InsertAgent(ctx, &AgentRecord{ID: "ag_b", Name: "B"}, AgentInsertOptions{}); err != nil {
		t.Fatalf("seed B: %v", err)
	}
	bEntry, err := s.InsertMemoryEntry(ctx, &MemoryEntryRecord{
		ID: "me_b_1", AgentID: "ag_b", Kind: "daily", Name: "n", Body: "b",
	}, MemoryEntryInsertOptions{})
	if err != nil {
		t.Fatalf("seed entry B: %v", err)
	}
	// peer-a holds the lock for agent A. Agent rows already
	// inserted above; just acquire the lock directly.
	lockA, err := s.AcquireAgentLock(ctx, "ag_a", "peer-a", NowMillis(), 60_000)
	if err != nil {
		t.Fatalf("AcquireAgentLock ag_a: %v", err)
	}
	tokenA := lockA.FencingToken
	// Peer-a tries to patch agent B's entry while threading
	// FencingPredicate against agent A's lock. Must refuse.
	newBody := "tampered"
	_, err = s.UpdateMemoryEntry(ctx, bEntry.ID, bEntry.ETag, MemoryEntryPatch{
		Body: &newBody,
		Fencing: &FencingPredicate{
			AgentID:      "ag_a",
			Peer:         "peer-a",
			FencingToken: tokenA,
		},
	})
	if !errors.Is(err, ErrFencingMismatch) {
		t.Fatalf("UpdateMemoryEntry cross-agent: want ErrFencingMismatch, got %v", err)
	}
	// Verify the row was NOT tampered.
	got, err := s.GetMemoryEntry(ctx, bEntry.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.Body != "b" {
		t.Errorf("body changed despite refusal: %q", got.Body)
	}
}

// TestSplitBrain_LedgerShortCircuitSurvivesLockRotation pins the
// post-Round-3 invariant: an exact-replay (matching fingerprint)
// op_id MUST succeed even if the original lock was released and a
// new peer holds the agent. Otherwise a peer that retries after a
// brief partition would always 409 once any lock rotation
// happens — the design's "peer-side responsibility to retry"
// would become unactionable.
func TestSplitBrain_LedgerShortCircuitSurvivesLockRotation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	agentID := "ag_sb_3"
	tokenA := seedAgentForLock(t, s, agentID, "peer-a", 60_000)
	// peer-a writes successfully with idempotency tag.
	tag := &IdempotencyTag{OpID: "op-survive-1", Fingerprint: "fp-x"}
	first, err := s.AppendMessage(ctx, &MessageRecord{
		ID: "op-survive-1", AgentID: agentID, Role: "user", Content: "live",
	}, MessageInsertOptions{
		Fencing: &FencingPredicate{
			AgentID:      agentID,
			Peer:         "peer-a",
			FencingToken: tokenA,
		},
		Idempotency: tag,
	})
	if err != nil {
		t.Fatalf("first AppendMessage: %v", err)
	}
	// peer-b steals the lock (advancing the token).
	if _, err := s.ReleaseAgentLockByPeer(ctx, "peer-a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	lockB, err := s.AcquireAgentLock(ctx, agentID, "peer-b", NowMillis(), 60_000)
	if err != nil {
		t.Fatalf("steal: %v", err)
	}
	if lockB.FencingToken <= tokenA {
		t.Fatalf("token did not advance: %d → %d", tokenA, lockB.FencingToken)
	}
	// peer-a now retries the SAME op_id against the rotated lock.
	// The ledger short-circuit (probe BEFORE fencing) must return
	// the prior etag without surfacing fencing as the error.
	replay, err := s.AppendMessage(ctx, &MessageRecord{
		ID: "op-survive-1", AgentID: agentID, Role: "user", Content: "live",
	}, MessageInsertOptions{
		Fencing: &FencingPredicate{
			AgentID:      agentID,
			Peer:         "peer-a",
			FencingToken: tokenA, // STALE — pre-steal token
		},
		Idempotency: tag,
	})
	if err != nil {
		t.Fatalf("replay with stale token + ledger hit: %v", err)
	}
	if replay.ETag != first.ETag {
		t.Errorf("replay etag drift: %q != %q", replay.ETag, first.ETag)
	}
}

// TestSplitBrain_LedgerRefusesOpIDReuseWithDifferentFingerprint
// closes the scenario where the SAME op_id is re-sent with a
// different (table, op, body). A naive implementation that only
// checked op_id presence in the ledger would silently return the
// prior etag for the unrelated write — masking a peer-side bug.
func TestSplitBrain_LedgerRefusesOpIDReuseWithDifferentFingerprint(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	agentID := "ag_sb_4"
	token := seedAgentForLock(t, s, agentID, "peer-a", 60_000)
	tag1 := &IdempotencyTag{OpID: "op-dup-1", Fingerprint: "fp-original"}
	if _, err := s.AppendMessage(ctx, &MessageRecord{
		ID: "op-dup-1", AgentID: agentID, Role: "user", Content: "original",
	}, MessageInsertOptions{
		Fencing:     &FencingPredicate{AgentID: agentID, Peer: "peer-a", FencingToken: token},
		Idempotency: tag1,
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Same op_id, different fingerprint → must refuse.
	tag2 := &IdempotencyTag{OpID: "op-dup-1", Fingerprint: "fp-DIFFERENT"}
	_, err := s.AppendMessage(ctx, &MessageRecord{
		ID: "op-dup-1", AgentID: agentID, Role: "user", Content: "tampered",
	}, MessageInsertOptions{
		Fencing:     &FencingPredicate{AgentID: agentID, Peer: "peer-a", FencingToken: token},
		Idempotency: tag2,
	})
	if !errors.Is(err, ErrOplogOpIDReused) {
		t.Fatalf("reuse: want ErrOplogOpIDReused, got %v", err)
	}
	// Legitimate replay: same op_id, same fp, same agent → success
	// (returns the prior etag).
	if _, err := s.AppendMessage(ctx, &MessageRecord{
		ID: "op-dup-1", AgentID: agentID, Role: "user", Content: "original",
	}, MessageInsertOptions{
		Fencing:     &FencingPredicate{AgentID: agentID, Peer: "peer-a", FencingToken: token},
		Idempotency: &IdempotencyTag{OpID: "op-dup-1", Fingerprint: "fp-original"},
	}); err != nil {
		t.Fatalf("legitimate replay (same fp, same agent): %v", err)
	}
	// Cross-agent ledger collision: same op_id under a DIFFERENT
	// agent_id must also surface as ErrOplogOpIDReused. The
	// ledger probe is scoped to (op_id, agent_id, fingerprint);
	// reusing op_id for an unrelated agent is a peer-side bug.
	otherID := "ag_other_sb_4"
	tokenOther := seedAgentForLock(t, s, otherID, "peer-c", 60_000)
	_, err = s.AppendMessage(ctx, &MessageRecord{
		ID: "op-dup-1", AgentID: otherID, Role: "user", Content: "other-agent",
	}, MessageInsertOptions{
		Fencing:     &FencingPredicate{AgentID: otherID, Peer: "peer-c", FencingToken: tokenOther},
		Idempotency: &IdempotencyTag{OpID: "op-dup-1", Fingerprint: "fp-original"},
	})
	if !errors.Is(err, ErrOplogOpIDReused) {
		t.Fatalf("cross-agent op_id reuse: want ErrOplogOpIDReused, got %v", err)
	}
	// Verify only the original landed.
	msgs, err := s.ListMessages(ctx, agentID, MessageListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "original" {
		t.Errorf("unexpected messages: %+v", msgs)
	}
}

// TestSplitBrain_FencingPredicateRejectsCrossAgentClaim covers the
// store-side guard against an op-log handler bug: if a malformed
// FencingPredicate names a different agent_id than the write
// target, the store must refuse rather than blindly calling
// CheckFencingTx with the predicate's agent_id (which would
// "succeed" against the predicate's agent even though we're
// writing a different agent's row).
func TestSplitBrain_FencingPredicateRejectsCrossAgentClaim(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.InsertAgent(ctx, &AgentRecord{ID: "ag_x", Name: "X"}, AgentInsertOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tokenY := seedAgentForLock(t, s, "ag_y", "peer-a", 60_000)
	// Write target: ag_x. Predicate names ag_y. Refuse.
	_, err := s.AppendMessage(ctx, &MessageRecord{
		ID: "m_xfer", AgentID: "ag_x", Role: "user", Content: "smuggled",
	}, MessageInsertOptions{
		Fencing: &FencingPredicate{
			AgentID:      "ag_y", // mismatched with rec.AgentID
			Peer:         "peer-a",
			FencingToken: tokenY,
		},
	})
	if err == nil {
		t.Fatal("AppendMessage cross-agent fencing claim: want error")
	}
	if !strings.Contains(err.Error(), "agent_id") {
		t.Errorf("error should mention agent_id mismatch: %v", err)
	}
}
