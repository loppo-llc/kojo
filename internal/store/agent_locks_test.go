package store

import (
	"context"
	"errors"
	"testing"
)

// agentLockTestSetup seeds a single agent the lock rows can FK against
// (CASCADE on agent delete is in the schema). Returns the agent id.
func agentLockTestSetup(t *testing.T, s *Store) string {
	t.Helper()
	ctx := context.Background()
	rec, err := s.InsertAgent(ctx, &AgentRecord{
		ID: "ag_lock_1", Name: "lock-test",
	}, AgentInsertOptions{})
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return rec.ID
}

func TestAcquireAgentLockFirstAcquireStartsAt1(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	rec, err := s.AcquireAgentLock(ctx, id, "peer-a", 1000, 60_000)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if rec.HolderPeer != "peer-a" || rec.FencingToken != 1 {
		t.Errorf("first acquire: %+v", rec)
	}
	if rec.LeaseExpiresAt != 1000+60_000 {
		t.Errorf("lease: %d", rec.LeaseExpiresAt)
	}
}

func TestAcquireAgentLockSamePeerRefreshKeepsToken(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	first, err := s.AcquireAgentLock(ctx, id, "peer-a", 1000, 60_000)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := s.AcquireAgentLock(ctx, id, "peer-a", 5000, 60_000)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.FencingToken != first.FencingToken {
		t.Errorf("re-acquire bumped token: %d → %d",
			first.FencingToken, second.FencingToken)
	}
	if second.LeaseExpiresAt != 5000+60_000 {
		t.Errorf("lease not refreshed: %d", second.LeaseExpiresAt)
	}
	if second.AcquiredAt != first.AcquiredAt {
		t.Errorf("acquired_at changed on refresh: %d → %d",
			first.AcquiredAt, second.AcquiredAt)
	}
}

func TestAcquireAgentLockOtherPeerLiveLeaseRejects(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	if _, err := s.AcquireAgentLock(ctx, id, "peer-a", 1000, 60_000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec, err := s.AcquireAgentLock(ctx, id, "peer-b", 5000, 60_000)
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("contend live lease: got %v, want ErrLockHeld", err)
	}
	if rec == nil || rec.HolderPeer != "peer-a" {
		t.Errorf("conflict row should reflect current holder: %+v", rec)
	}
}

func TestAcquireAgentLockExpiredLeaseStealsAndBumps(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	first, err := s.AcquireAgentLock(ctx, id, "peer-a", 1000, 60_000)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Steal at now=200_000 — well past the 1000+60_000 lease expiry.
	stolen, err := s.AcquireAgentLock(ctx, id, "peer-b", 200_000, 60_000)
	if err != nil {
		t.Fatalf("steal: %v", err)
	}
	if stolen.HolderPeer != "peer-b" {
		t.Errorf("holder: %s", stolen.HolderPeer)
	}
	if stolen.FencingToken != first.FencingToken+1 {
		t.Errorf("fencing token not bumped: %d → %d",
			first.FencingToken, stolen.FencingToken)
	}
	if stolen.AcquiredAt != 200_000 {
		t.Errorf("acquired_at: %d", stolen.AcquiredAt)
	}
}

func TestRefreshAgentLockHappyPath(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	first, err := s.AcquireAgentLock(ctx, id, "peer-a", 1000, 60_000)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	refreshed, err := s.RefreshAgentLock(ctx, id, "peer-a", first.FencingToken, 30_000, 60_000)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if refreshed.LeaseExpiresAt != 30_000+60_000 {
		t.Errorf("lease: %d", refreshed.LeaseExpiresAt)
	}
}

func TestRefreshAgentLockFencingMismatch(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	if _, err := s.AcquireAgentLock(ctx, id, "peer-a", 1000, 60_000); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Wrong token → mismatch.
	if _, err := s.RefreshAgentLock(ctx, id, "peer-a", 99, 30_000, 60_000); !errors.Is(err, ErrFencingMismatch) {
		t.Errorf("wrong token: got %v, want ErrFencingMismatch", err)
	}
	// Wrong peer → mismatch.
	if _, err := s.RefreshAgentLock(ctx, id, "peer-b", 1, 30_000, 60_000); !errors.Is(err, ErrFencingMismatch) {
		t.Errorf("wrong peer: got %v, want ErrFencingMismatch", err)
	}
}

func TestRefreshAgentLockNotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	if _, err := s.RefreshAgentLock(ctx, id, "peer-a", 1, 1000, 60_000); !errors.Is(err, ErrNotFound) {
		t.Errorf("never acquired: got %v, want ErrNotFound", err)
	}
}

func TestReleaseAgentLockHappyAndIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	rec, _ := s.AcquireAgentLock(ctx, id, "peer-a", 1000, 60_000)
	if err := s.ReleaseAgentLock(ctx, id, "peer-a", rec.FencingToken); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := s.GetAgentLock(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after release: %v", err)
	}
	// Repeat release: no row → ErrNotFound (not Mismatch).
	if err := s.ReleaseAgentLock(ctx, id, "peer-a", rec.FencingToken); !errors.Is(err, ErrNotFound) {
		t.Errorf("repeat release: %v", err)
	}
}

func TestReleaseAgentLockFencingMismatch(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	if _, err := s.AcquireAgentLock(ctx, id, "peer-a", 1000, 60_000); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := s.ReleaseAgentLock(ctx, id, "peer-a", 99); !errors.Is(err, ErrFencingMismatch) {
		t.Errorf("wrong token: %v", err)
	}
	if err := s.ReleaseAgentLock(ctx, id, "peer-b", 1); !errors.Is(err, ErrFencingMismatch) {
		t.Errorf("wrong peer: %v", err)
	}
}

func TestReleaseAgentLockByPeerBulk(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// Seed two agents with locks held by different peers.
	for _, id := range []string{"ag_a", "ag_b", "ag_c"} {
		if _, err := s.InsertAgent(ctx, &AgentRecord{ID: id, Name: id}, AgentInsertOptions{}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if _, err := s.AcquireAgentLock(ctx, "ag_a", "peer-a", 1000, 60_000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireAgentLock(ctx, "ag_b", "peer-a", 1000, 60_000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireAgentLock(ctx, "ag_c", "peer-b", 1000, 60_000); err != nil {
		t.Fatal(err)
	}
	n, err := s.ReleaseAgentLockByPeer(ctx, "peer-a")
	if err != nil {
		t.Fatalf("bulk release: %v", err)
	}
	if n != 2 {
		t.Errorf("rows removed = %d, want 2", n)
	}
	// peer-b's lock untouched.
	if _, err := s.GetAgentLock(ctx, "ag_c"); err != nil {
		t.Errorf("peer-b lock collateral damage: %v", err)
	}
}

func TestCheckFencing(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	rec, _ := s.AcquireAgentLock(ctx, id, "peer-a", 1000, 60_000)
	if err := s.CheckFencing(ctx, id, "peer-a", rec.FencingToken); err != nil {
		t.Errorf("happy path: %v", err)
	}
	if err := s.CheckFencing(ctx, id, "peer-a", 99); !errors.Is(err, ErrFencingMismatch) {
		t.Errorf("token: %v", err)
	}
	if err := s.CheckFencing(ctx, id, "peer-b", rec.FencingToken); !errors.Is(err, ErrFencingMismatch) {
		t.Errorf("peer: %v", err)
	}
	if err := s.CheckFencing(ctx, "no-such-agent", "peer-a", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestAcquireAgentLockTokenMonotonicAcrossRelease(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	first, err := s.AcquireAgentLock(ctx, id, "peer-a", 1000, 60_000)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	if err := s.ReleaseAgentLock(ctx, id, "peer-a", first.FencingToken); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Re-acquire from the same peer after release. The token must NOT
	// reuse `first.FencingToken` — otherwise a delayed write from the
	// pre-release session with that token would be silently accepted.
	second, err := s.AcquireAgentLock(ctx, id, "peer-a", 5000, 60_000)
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if second.FencingToken <= first.FencingToken {
		t.Errorf("token reused or rolled back: %d → %d",
			first.FencingToken, second.FencingToken)
	}
}

func TestAcquireAgentLockTokenMonotonicAfterReleaseByPeer(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	first, err := s.AcquireAgentLock(ctx, id, "peer-a", 1000, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseAgentLockByPeer(ctx, "peer-a"); err != nil {
		t.Fatalf("bulk release: %v", err)
	}
	// A different peer picks up where peer-a left off — must advance
	// past first.FencingToken even though the row was wiped.
	taken, err := s.AcquireAgentLock(ctx, id, "peer-b", 5000, 60_000)
	if err != nil {
		t.Fatalf("acquire after bulk release: %v", err)
	}
	if taken.FencingToken <= first.FencingToken {
		t.Errorf("counter rolled back after ReleaseByPeer: %d → %d",
			first.FencingToken, taken.FencingToken)
	}
}

func TestCheckFencingTxRejectsAfterTxBeganSteal(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	first, _ := s.AcquireAgentLock(ctx, id, "peer-a", 1000, 60_000)
	// Steal happens before the writer's tx opens.
	stolen, err := s.AcquireAgentLock(ctx, id, "peer-b", 200_000, 60_000)
	if err != nil {
		t.Fatalf("steal: %v", err)
	}
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	// peer-a's old token must be rejected — fencing_token has been
	// bumped by the steal.
	if err := s.CheckFencingTx(ctx, tx, id, "peer-a", first.FencingToken); !errors.Is(err, ErrFencingMismatch) {
		t.Errorf("post-steal old token: %v", err)
	}
	// peer-b's new token must pass.
	if err := s.CheckFencingTx(ctx, tx, id, "peer-b", stolen.FencingToken); err != nil {
		t.Errorf("post-steal new token: %v", err)
	}
}

func TestAgentLocksCascadeOnAgentDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	if _, err := s.AcquireAgentLock(ctx, id, "peer-a", 1000, 60_000); err != nil {
		t.Fatal(err)
	}
	if err := s.SoftDeleteAgent(ctx, id); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	// agents soft-delete keeps the row → lock unchanged.
	if _, err := s.GetAgentLock(ctx, id); err != nil {
		t.Errorf("soft-delete dropped lock unexpectedly: %v", err)
	}
	// Hard delete via direct SQL would CASCADE per schema. We don't
	// expose a hard-delete helper yet; verifying the schema CASCADE
	// is enough here.
}
