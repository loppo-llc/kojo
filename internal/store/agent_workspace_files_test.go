package store

import (
	"context"
	"errors"
	"testing"
)

func TestAgentWorkspaceFileUpsertRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	r1, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, "first", "", AgentWorkspaceFileInsertOptions{})
	if err != nil {
		t.Fatalf("upsert v1: %v", err)
	}
	if r1.Version != 1 || r1.ETag == "" || r1.BodySHA256 == "" {
		t.Errorf("post-insert defaults: %+v", r1)
	}
	if r1.Kind != WorkspaceFileKindUser {
		t.Errorf("kind = %q, want user", r1.Kind)
	}

	got, err := s.GetAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Body != "first" {
		t.Errorf("body = %q, want first", got.Body)
	}
	if got.ETag != r1.ETag {
		t.Errorf("etag drift on read-back: %q vs %q", got.ETag, r1.ETag)
	}

	// Update with matching etag.
	r2, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, "second", r1.ETag, AgentWorkspaceFileInsertOptions{})
	if err != nil {
		t.Fatalf("upsert v2: %v", err)
	}
	if r2.Version != 2 {
		t.Errorf("v2 version = %d, want 2", r2.Version)
	}
	if r2.CreatedAt != r1.CreatedAt {
		t.Errorf("created_at must not change on update")
	}
	if r2.Seq != r1.Seq {
		t.Errorf("seq must not change on update")
	}
	if r2.ETag == r1.ETag {
		t.Errorf("etag should change between versions")
	}
}

func TestAgentWorkspaceFileIfMatchConcurrency(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	r1, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindCheckin, "v1", "", AgentWorkspaceFileInsertOptions{})
	if err != nil {
		t.Fatalf("upsert v1: %v", err)
	}

	// Stale etag.
	if _, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindCheckin, "stale", "0-deadbeef", AgentWorkspaceFileInsertOptions{}); !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("expected ErrETagMismatch on stale etag, got %v", err)
	}

	// Blind overwrite refused.
	if _, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindCheckin, "blind", "", AgentWorkspaceFileInsertOptions{}); !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("expected ErrETagMismatch on blind overwrite, got %v", err)
	}

	// AllowOverwrite (importer path).
	r2, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindCheckin, "imported", "", AgentWorkspaceFileInsertOptions{AllowOverwrite: true})
	if err != nil {
		t.Fatalf("AllowOverwrite: %v", err)
	}
	if r2.Body != "imported" || r2.Version != 2 {
		t.Errorf("AllowOverwrite result: %+v", r2)
	}
	_ = r1
}

func TestAgentWorkspaceFileTombstoneRevive(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	r1, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, "alive", "", AgentWorkspaceFileInsertOptions{})
	if err != nil {
		t.Fatalf("upsert v1: %v", err)
	}

	if err := s.SoftDeleteAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, ""); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := s.GetAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound after tombstone, got %v", err)
	}

	// Revive with a fresh insert: PK collides with the tombstone, so
	// the upsert path must DELETE-then-INSERT. After revival the row
	// is brand new: version=1, seq freshly allocated.
	r3, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, "reborn", "", AgentWorkspaceFileInsertOptions{})
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	if r3.Version != 1 {
		t.Errorf("revived version = %d, want 1", r3.Version)
	}
	if r3.Body != "reborn" {
		t.Errorf("revived body = %q, want reborn", r3.Body)
	}
	if r3.Seq == r1.Seq {
		t.Errorf("revive should allocate a fresh seq, got reused %d", r3.Seq)
	}
	if r3.DeletedAt != nil {
		t.Errorf("revived row should not be tombstoned, deleted_at=%v", r3.DeletedAt)
	}
}

func TestAgentWorkspaceFileListUpdatedAtSince(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	t0 := NowMillis()
	if _, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, "user-body", "", AgentWorkspaceFileInsertOptions{UpdatedAt: t0}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	t1 := t0 + 1000
	checkin, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindCheckin, "checkin-body", "", AgentWorkspaceFileInsertOptions{UpdatedAt: t1})
	if err != nil {
		t.Fatalf("upsert checkin: %v", err)
	}

	// Default list returns both, ordered by seq ASC.
	all, err := s.ListAgentWorkspaceFiles(ctx, "ag", WorkspaceFileListOptions{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("list all len = %d, want 2", len(all))
	}
	if all[0].Seq > all[1].Seq {
		t.Errorf("default order should be seq ASC, got %d > %d", all[0].Seq, all[1].Seq)
	}

	// Kind filter.
	onlyUser, err := s.ListAgentWorkspaceFiles(ctx, "ag", WorkspaceFileListOptions{Kind: WorkspaceFileKindUser})
	if err != nil {
		t.Fatalf("list kind=user: %v", err)
	}
	if len(onlyUser) != 1 || onlyUser[0].Kind != WorkspaceFileKindUser {
		t.Errorf("kind filter result: %+v", onlyUser)
	}

	// Delta: anything updated at or after t1 → just checkin.
	delta, err := s.ListAgentWorkspaceFiles(ctx, "ag", WorkspaceFileListOptions{UpdatedAtSince: t1})
	if err != nil {
		t.Fatalf("list delta: %v", err)
	}
	if len(delta) != 1 || delta[0].Kind != WorkspaceFileKindCheckin {
		t.Errorf("delta result: %+v", delta)
	}
	if delta[0].UpdatedAt != checkin.UpdatedAt {
		t.Errorf("delta updated_at = %d, want %d", delta[0].UpdatedAt, checkin.UpdatedAt)
	}

	// Tombstone the user row, then verify IncludeDeleted behavior.
	if err := s.SoftDeleteAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, ""); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	live, err := s.ListAgentWorkspaceFiles(ctx, "ag", WorkspaceFileListOptions{})
	if err != nil {
		t.Fatalf("list live: %v", err)
	}
	if len(live) != 1 || live[0].Kind != WorkspaceFileKindCheckin {
		t.Errorf("live list after tombstone: %+v", live)
	}
	withDead, err := s.ListAgentWorkspaceFiles(ctx, "ag", WorkspaceFileListOptions{IncludeDeleted: true})
	if err != nil {
		t.Fatalf("list incl deleted: %v", err)
	}
	if len(withDead) != 2 {
		t.Errorf("IncludeDeleted list len = %d, want 2", len(withDead))
	}
}

func TestAgentWorkspaceFileKindValidation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	if _, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKind("bogus"), "x", "", AgentWorkspaceFileInsertOptions{}); err == nil {
		t.Errorf("expected error for invalid kind, got nil")
	}
	if _, err := s.GetAgentWorkspaceFile(ctx, "ag", WorkspaceFileKind("bogus")); err == nil {
		t.Errorf("expected error for invalid kind on Get, got nil")
	}
	if err := s.SoftDeleteAgentWorkspaceFile(ctx, "ag", WorkspaceFileKind("bogus"), ""); err == nil {
		t.Errorf("expected error for invalid kind on SoftDelete, got nil")
	}
	if _, err := s.ListAgentWorkspaceFiles(ctx, "ag", WorkspaceFileListOptions{Kind: WorkspaceFileKind("bogus")}); err == nil {
		t.Errorf("expected error for invalid kind on List, got nil")
	}

	if !IsValidWorkspaceFileKind(WorkspaceFileKindUser) || !IsValidWorkspaceFileKind(WorkspaceFileKindCheckin) {
		t.Errorf("user/checkin kinds should be valid")
	}
	if IsValidWorkspaceFileKind(WorkspaceFileKind("")) {
		t.Errorf("empty kind should be invalid")
	}
}

func TestAgentWorkspaceFileSoftDeleteIdempotency(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	r1, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, "body", "", AgentWorkspaceFileInsertOptions{})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// First delete succeeds.
	if err := s.SoftDeleteAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, ""); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// Repeat delete is idempotent for the unconditional caller.
	if err := s.SoftDeleteAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, ""); err != nil {
		t.Errorf("repeat delete should be idempotent, got %v", err)
	}
	// Missing kind for the agent is also idempotent unconditionally.
	if err := s.SoftDeleteAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindCheckin, ""); err != nil {
		t.Errorf("delete on missing kind should be idempotent, got %v", err)
	}
	// Conditional delete on a tombstoned row → ErrETagMismatch.
	if err := s.SoftDeleteAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, r1.ETag); !errors.Is(err, ErrETagMismatch) {
		t.Errorf("conditional delete on tombstoned row: got %v, want ErrETagMismatch", err)
	}
	// Conditional delete on a missing row → ErrETagMismatch.
	if err := s.SoftDeleteAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindCheckin, "0-cafebabe"); !errors.Is(err, ErrETagMismatch) {
		t.Errorf("conditional delete on missing row: got %v, want ErrETagMismatch", err)
	}

	// Revive then delete with wrong etag → ErrETagMismatch, live row unchanged.
	r2, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, "again", "", AgentWorkspaceFileInsertOptions{})
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	if err := s.SoftDeleteAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, "0-deadbeef"); !errors.Is(err, ErrETagMismatch) {
		t.Errorf("delete with wrong etag: got %v, want ErrETagMismatch", err)
	}
	got, err := s.GetAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser)
	if err != nil {
		t.Fatalf("get after refused delete: %v", err)
	}
	if got.ETag != r2.ETag {
		t.Errorf("row mutated despite refused delete: %q vs %q", got.ETag, r2.ETag)
	}
	// Correct etag → succeeds.
	if err := s.SoftDeleteAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, r2.ETag); err != nil {
		t.Errorf("conditional delete with correct etag: %v", err)
	}

	// Sanity: tombstoning recomputes etag (deleted_at participates in the canonical input).
	// Pull via IncludeDeleted to inspect.
	dead, err := s.ListAgentWorkspaceFiles(ctx, "ag", WorkspaceFileListOptions{Kind: WorkspaceFileKindUser, IncludeDeleted: true})
	if err != nil {
		t.Fatalf("list dead: %v", err)
	}
	if len(dead) != 1 {
		t.Fatalf("dead row count = %d, want 1", len(dead))
	}
	if dead[0].ETag == r2.ETag {
		t.Errorf("etag should differ after tombstone")
	}
	if dead[0].DeletedAt == nil {
		t.Errorf("deleted_at should be set on tombstoned row")
	}
}

func TestAgentWorkspaceFileIdempotencyReplay(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	tag := &IdempotencyTag{OpID: "op-1", Fingerprint: "fp-1"}
	r1, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, "first", "", AgentWorkspaceFileInsertOptions{Idempotency: tag})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Replay with the same op_id short-circuits to the prior result.
	// The body / etag argument that "would have been written" if we
	// hadn't already committed must not matter; even AllowOverwrite is
	// irrelevant on the replay path.
	r2, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, "different", "", AgentWorkspaceFileInsertOptions{Idempotency: tag, AllowOverwrite: true})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if r2.ETag != r1.ETag {
		t.Errorf("replay should return prior etag: got %q, want %q", r2.ETag, r1.ETag)
	}
	if r2.Body != "first" {
		t.Errorf("replay body = %q, want first (the replay must not apply the new body)", r2.Body)
	}
}

func TestAgentWorkspaceFileETagCanonicalInputs(t *testing.T) {
	// Pins that the canonical etag input genuinely incorporates Kind
	// and UpdatedAt: changing either while holding the other inputs
	// fixed must shift the etag. Body / DeletedAt coverage is implicit
	// in the other tests (revive / tombstone).
	mk := func(kind WorkspaceFileKind, body string, updatedAt int64, deleted *int64) *AgentWorkspaceFileRecord {
		return &AgentWorkspaceFileRecord{
			AgentID:    "ag",
			Kind:       kind,
			Body:       body,
			BodySHA256: SHA256Hex([]byte(body)),
			Version:    1,
			UpdatedAt:  updatedAt,
			DeletedAt:  deleted,
		}
	}
	user := mk(WorkspaceFileKindUser, "same", 1_000, nil)
	checkin := mk(WorkspaceFileKindCheckin, "same", 1_000, nil)
	later := mk(WorkspaceFileKindUser, "same", 2_000, nil)
	tomb := mk(WorkspaceFileKindUser, "same", 1_000, func() *int64 { v := int64(1_500); return &v }())
	otherBody := mk(WorkspaceFileKindUser, "different", 1_000, nil)
	otherAgent := mk(WorkspaceFileKindUser, "same", 1_000, nil)
	otherAgent.AgentID = "ag2"

	for _, r := range []*AgentWorkspaceFileRecord{user, checkin, later, tomb, otherBody, otherAgent} {
		e, err := computeAgentWorkspaceFileETag(r)
		if err != nil {
			t.Fatalf("compute etag: %v", err)
		}
		r.ETag = e
	}
	if user.ETag == checkin.ETag {
		t.Errorf("Kind must shift etag: user=%q checkin=%q", user.ETag, checkin.ETag)
	}
	if user.ETag == later.ETag {
		t.Errorf("UpdatedAt must shift etag: t1=%q t2=%q", user.ETag, later.ETag)
	}
	if user.ETag == tomb.ETag {
		t.Errorf("DeletedAt must shift etag: live=%q tomb=%q", user.ETag, tomb.ETag)
	}
	if user.ETag == otherBody.ETag {
		t.Errorf("BodySHA256 must shift etag: a=%q b=%q", user.ETag, otherBody.ETag)
	}
	if user.ETag == otherAgent.ETag {
		t.Errorf("AgentID must shift etag: ag1=%q ag2=%q", user.ETag, otherAgent.ETag)
	}
}

func TestAgentWorkspaceFileDeltaIncludesTombstones(t *testing.T) {
	// §3.7 device-switch delta MUST surface tombstones — otherwise a
	// deletion on device A is invisible to device B. The List path
	// honors IncludeDeleted together with UpdatedAtSince; this test
	// pins that contract.
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	r1, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, "alive", "", AgentWorkspaceFileInsertOptions{UpdatedAt: 1_000})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.SoftDeleteAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, ""); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	// Without IncludeDeleted, delta loses the tombstone.
	noTomb, err := s.ListAgentWorkspaceFiles(ctx, "ag", WorkspaceFileListOptions{UpdatedAtSince: 1_000})
	if err != nil {
		t.Fatalf("delta sans deleted: %v", err)
	}
	if len(noTomb) != 0 {
		t.Errorf("delta should be empty without IncludeDeleted, got %+v", noTomb)
	}

	// With IncludeDeleted the tombstone is visible.
	withTomb, err := s.ListAgentWorkspaceFiles(ctx, "ag", WorkspaceFileListOptions{UpdatedAtSince: 1_000, IncludeDeleted: true})
	if err != nil {
		t.Fatalf("delta with deleted: %v", err)
	}
	if len(withTomb) != 1 || withTomb[0].DeletedAt == nil {
		t.Errorf("delta with IncludeDeleted: %+v", withTomb)
	}
	if withTomb[0].ETag == r1.ETag {
		t.Errorf("tombstone etag should differ from live etag")
	}
}

func TestAgentWorkspaceFileDeltaInclusiveSameMillisecond(t *testing.T) {
	// last_seen-resume semantics MUST be inclusive: a peer who saw
	// updated_at=T must be able to refetch T and dedupe on
	// (agent_id, kind). Bumping by +1ms would lose any concurrent
	// same-ms write. Pin that two rows written at the same ms both
	// surface when UpdatedAtSince == that ms.
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	if _, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, "u", "", AgentWorkspaceFileInsertOptions{UpdatedAt: 5_000}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	if _, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindCheckin, "c", "", AgentWorkspaceFileInsertOptions{UpdatedAt: 5_000}); err != nil {
		t.Fatalf("upsert checkin: %v", err)
	}

	got, err := s.ListAgentWorkspaceFiles(ctx, "ag", WorkspaceFileListOptions{UpdatedAtSince: 5_000})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("inclusive resume should return both same-ms rows, got %d", len(got))
	}
	// Ordering must be stable: updated_at ASC, kind ASC.
	if got[0].Kind != WorkspaceFileKindCheckin || got[1].Kind != WorkspaceFileKindUser {
		t.Errorf("tiebreak should be kind ASC, got %s,%s", got[0].Kind, got[1].Kind)
	}
}

func TestAgentWorkspaceFileIdempotencyReplaySurvivesAgentDelete(t *testing.T) {
	// getAgentWorkspaceFileTx intentionally skips the agents JOIN so
	// the idempotency re-read can succeed even when the parent agent
	// was tombstoned after the original write committed. Mirrors the
	// "ledger short-circuit survives lock rotation" invariant for the
	// agent-lifecycle dimension.
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	tag := &IdempotencyTag{OpID: "op-replay-after-delete", Fingerprint: "fp-x"}
	r1, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, "body", "", AgentWorkspaceFileInsertOptions{Idempotency: tag})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Agent gets soft-deleted by some other path.
	if err := s.SoftDeleteAgent(ctx, "ag"); err != nil {
		t.Fatalf("soft delete agent: %v", err)
	}

	// Replay with the same tag. The agents JOIN in Get would refuse;
	// the tx-scoped re-read used by the idempotency probe must not.
	r2, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, "body", "", AgentWorkspaceFileInsertOptions{Idempotency: tag})
	if err != nil {
		t.Fatalf("replay after agent delete: %v", err)
	}
	if r2.ETag != r1.ETag {
		t.Errorf("replay etag drift: %q != %q", r2.ETag, r1.ETag)
	}
}

func TestAgentWorkspaceFileRejectsTombstonedAgent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")
	if err := s.SoftDeleteAgent(ctx, "ag"); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	if _, err := s.UpsertAgentWorkspaceFile(ctx, "ag", WorkspaceFileKindUser, "x", "", AgentWorkspaceFileInsertOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on tombstoned agent, got %v", err)
	}
}
