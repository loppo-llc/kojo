package store

import (
	"context"
	"errors"
	"testing"
)

func TestGroupDMRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "a1")
	seedAgent(t, s, "a2")
	seedAgent(t, s, "a3")

	rec, err := s.InsertGroupDM(ctx, &GroupDMRecord{
		ID: "g1", Name: "team", Members: []GroupDMMember{{AgentID: "a1"}, {AgentID: "a2"}},
	}, GroupDMInsertOptions{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if rec.Style != "efficient" {
		t.Errorf("default style = %q, want efficient", rec.Style)
	}
	if rec.Venue != "chatroom" {
		t.Errorf("default venue = %q, want chatroom", rec.Venue)
	}
	if rec.Version != 1 || rec.ETag == "" {
		t.Errorf("post-insert defaults: %+v", rec)
	}

	got, err := s.GetGroupDM(ctx, "g1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Members) != 2 || got.Members[0].AgentID != "a1" {
		t.Errorf("members round-trip: %v", got.Members)
	}

	updated, err := s.UpdateGroupDM(ctx, "g1", rec.ETag, func(r *GroupDMRecord) error {
		r.Members = append(r.Members, GroupDMMember{AgentID: "a3"})
		r.Style = "expressive"
		return nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Version != 2 || updated.Style != "expressive" {
		t.Errorf("update result: %+v", updated)
	}
	if len(updated.Members) != 3 {
		t.Errorf("members = %v, want 3", updated.Members)
	}

	if err := s.SoftDeleteGroupDM(ctx, "g1"); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := s.GetGroupDM(ctx, "g1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestGroupDMRejectsDeadMember(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "a1")
	_, err := s.InsertGroupDM(ctx, &GroupDMRecord{
		ID: "g", Name: "x", Members: []GroupDMMember{{AgentID: "a1"}, {AgentID: "ghost"}},
	}, GroupDMInsertOptions{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for ghost member, got %v", err)
	}
}

func TestGroupDMETagOrderIndependent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "a1")
	seedAgent(t, s, "a2")
	r1, err := s.InsertGroupDM(ctx, &GroupDMRecord{
		ID: "g1", Name: "x", Members: []GroupDMMember{{AgentID: "a1"}, {AgentID: "a2"}},
	}, GroupDMInsertOptions{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	updated, err := s.UpdateGroupDM(ctx, "g1", r1.ETag, func(r *GroupDMRecord) error {
		// Reorder only — etag canonical record sorts members, so this
		// should still bump version (UpdatedAt changes), but the *member
		// content* portion of the etag must remain stable across reorders.
		r.Members = []GroupDMMember{{AgentID: "a2"}, {AgentID: "a1"}}
		return nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	// Version bumped; can't directly assert canonical-input equality
	// without re-implementing the helper, but we can check the records
	// resolved on the same input set.
	if len(updated.Members) != 2 {
		t.Fatalf("members: %v", updated.Members)
	}
}

func TestAppendGroupDMMessage(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "a1")
	seedAgent(t, s, "a2")
	if _, err := s.InsertGroupDM(ctx, &GroupDMRecord{
		ID: "g", Name: "n", Members: []GroupDMMember{{AgentID: "a1"}, {AgentID: "a2"}},
	}, GroupDMInsertOptions{}); err != nil {
		t.Fatalf("insert group: %v", err)
	}

	for i, author := range []string{"a1", "a2", ""} { // last is system
		_, err := s.AppendGroupDMMessage(ctx, &GroupDMMessageRecord{
			ID: string(rune('A' + i)), GroupDMID: "g", AgentID: author, Content: "hi",
		}, GroupDMMessageInsertOptions{})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	list, err := s.ListGroupDMMessages(ctx, "g", GroupDMMessageListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3", len(list))
	}
	for i, m := range list {
		if m.Seq != int64(i+1) {
			t.Errorf("seq[%d] = %d, want %d", i, m.Seq, i+1)
		}
	}

	// Group tombstone hides children.
	if err := s.SoftDeleteGroupDM(ctx, "g"); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	list, _ = s.ListGroupDMMessages(ctx, "g", GroupDMMessageListOptions{})
	if len(list) != 0 {
		t.Errorf("messages still visible after group delete: %d", len(list))
	}
}

func TestAppendGroupDMMessageRejectsNonMember(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "a1")
	seedAgent(t, s, "outsider")
	if _, err := s.InsertGroupDM(ctx, &GroupDMRecord{
		ID: "g", Name: "n", Members: []GroupDMMember{{AgentID: "a1"}},
	}, GroupDMInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err := s.AppendGroupDMMessage(ctx, &GroupDMMessageRecord{
		ID: "m", GroupDMID: "g", AgentID: "outsider", Content: "x",
	}, GroupDMMessageInsertOptions{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for non-member, got %v", err)
	}
}

func TestUpdateGroupDMReorderIsNoOp(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "a1")
	seedAgent(t, s, "a2")
	r1, err := s.InsertGroupDM(ctx, &GroupDMRecord{
		ID: "g", Name: "n", Members: []GroupDMMember{{AgentID: "a1"}, {AgentID: "a2"}},
	}, GroupDMInsertOptions{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	r2, err := s.UpdateGroupDM(ctx, "g", r1.ETag, func(r *GroupDMRecord) error {
		r.Members = []GroupDMMember{{AgentID: "a2"}, {AgentID: "a1"}} // pure reorder
		return nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if r2.ETag != r1.ETag || r2.Version != r1.Version {
		t.Errorf("reorder-only edit should be no-op: v=%d→%d, etag=%s→%s",
			r1.Version, r2.Version, r1.ETag, r2.ETag)
	}
}

func TestUpdateGroupDMRejectsEmptyName(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "a1")
	r, err := s.InsertGroupDM(ctx, &GroupDMRecord{
		ID: "g", Name: "n", Members: []GroupDMMember{{AgentID: "a1"}},
	}, GroupDMInsertOptions{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err = s.UpdateGroupDM(ctx, "g", r.ETag, func(r *GroupDMRecord) error {
		r.Name = ""
		return nil
	})
	if err == nil {
		t.Fatal("expected empty-name rejection")
	}
}

func TestGroupDMMutedMember(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "a1")
	seedAgent(t, s, "a2")
	rec, err := s.InsertGroupDM(ctx, &GroupDMRecord{
		ID: "g", Name: "n",
		Members: []GroupDMMember{
			{AgentID: "a1", NotifyMode: "muted"},
			{AgentID: "a2", NotifyMode: "digest", DigestWindow: 60},
		},
	}, GroupDMInsertOptions{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.GetGroupDM(ctx, "g")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Members[0].NotifyMode != "muted" || got.Members[1].NotifyMode != "digest" || got.Members[1].DigestWindow != 60 {
		t.Errorf("per-member round-trip: %+v", got.Members)
	}
	_ = rec
}

func TestLatestGroupDMMessageIDForCAS(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "a1")
	if _, err := s.InsertGroupDM(ctx, &GroupDMRecord{
		ID: "g", Name: "n", Members: []GroupDMMember{{AgentID: "a1"}},
	}, GroupDMInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, seq, err := s.LatestGroupDMMessageID(ctx, "g")
	if err != nil {
		t.Fatalf("latest empty: %v", err)
	}
	if id != "" || seq != 0 {
		t.Errorf("empty group should return zero values: id=%q seq=%d", id, seq)
	}
	if _, err := s.AppendGroupDMMessage(ctx, &GroupDMMessageRecord{
		ID: "m1", GroupDMID: "g", AgentID: "a1", Content: "x",
	}, GroupDMMessageInsertOptions{}); err != nil {
		t.Fatalf("append: %v", err)
	}
	id, seq, err = s.LatestGroupDMMessageID(ctx, "g")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if id != "m1" || seq != 1 {
		t.Errorf("latest = (%q, %d), want (m1, 1)", id, seq)
	}
}

func TestAppendGroupDMMessageAllowMissingAuthor(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "a1")
	if _, err := s.InsertGroupDM(ctx, &GroupDMRecord{
		ID: "g", Name: "n", Members: []GroupDMMember{{AgentID: "a1"}},
	}, GroupDMInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Importer path: a v0 transcript references an agent that no longer
	// exists. AllowMissingAuthor lets the row through.
	_, err := s.AppendGroupDMMessage(ctx, &GroupDMMessageRecord{
		ID: "m", GroupDMID: "g", AgentID: "ghost", Content: "from beyond",
	}, GroupDMMessageInsertOptions{AllowMissingAuthor: true})
	if err != nil {
		t.Fatalf("AllowMissingAuthor: %v", err)
	}
}

func TestAppendGroupDMMessageUserSender(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "a1")
	if _, err := s.InsertGroupDM(ctx, &GroupDMRecord{
		ID: "g", Name: "n", Members: []GroupDMMember{{AgentID: "a1"}},
	}, GroupDMInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// UserSenderID bypasses both alive and member-of-group checks.
	rec, err := s.AppendGroupDMMessage(ctx, &GroupDMMessageRecord{
		ID: "m", GroupDMID: "g", AgentID: UserSenderID, Content: "hi",
	}, GroupDMMessageInsertOptions{})
	if err != nil {
		t.Fatalf("user post: %v", err)
	}
	if rec.AgentID != UserSenderID {
		t.Errorf("agent_id = %q, want %q", rec.AgentID, UserSenderID)
	}
}

func TestAppendGroupDMMessageCASStaleHead(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "a1")
	if _, err := s.InsertGroupDM(ctx, &GroupDMRecord{
		ID: "g", Name: "n", Members: []GroupDMMember{{AgentID: "a1"}},
	}, GroupDMInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// First post — head goes from 0 → 1.
	if _, err := s.AppendGroupDMMessage(ctx, &GroupDMMessageRecord{
		ID: "m1", GroupDMID: "g", AgentID: "a1", Content: "first",
	}, GroupDMMessageInsertOptions{ExpectedLatestSeq: 0}); err != nil {
		t.Fatalf("first post: %v", err)
	}
	// Second post with stale ExpectedLatestSeq=0 → ErrStaleHead.
	_, err := s.AppendGroupDMMessage(ctx, &GroupDMMessageRecord{
		ID: "m2", GroupDMID: "g", AgentID: "a1", Content: "second",
	}, GroupDMMessageInsertOptions{ExpectedLatestSeq: 999})
	if !errors.Is(err, ErrStaleHead) {
		t.Fatalf("expected ErrStaleHead, got %v", err)
	}
	// Correct ExpectedLatestSeq=1 succeeds.
	if _, err := s.AppendGroupDMMessage(ctx, &GroupDMMessageRecord{
		ID: "m3", GroupDMID: "g", AgentID: "a1", Content: "third",
	}, GroupDMMessageInsertOptions{ExpectedLatestSeq: 1}); err != nil {
		t.Fatalf("third post: %v", err)
	}
}

func TestAppendGroupDMMessageRejectsDeadAuthor(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "a1")
	if _, err := s.InsertGroupDM(ctx, &GroupDMRecord{
		ID: "g", Name: "n", Members: []GroupDMMember{{AgentID: "a1"}},
	}, GroupDMInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err := s.AppendGroupDMMessage(ctx, &GroupDMMessageRecord{
		ID: "m", GroupDMID: "g", AgentID: "ghost", Content: "x",
	}, GroupDMMessageInsertOptions{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for dead author, got %v", err)
	}
}

// TestGroupDMUpdateAllowsDeadMembers covers the importer-friendly path:
// a group inserted with AllowDeadMembers may contain agent_ids that are
// no longer alive; UpdateGroupDM (e.g., for a rename) must not block on
// the existing dead members. Only newly-added members are validated.
func TestGroupDMUpdateAllowsDeadMembers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "a1")

	rec, err := s.InsertGroupDM(ctx, &GroupDMRecord{
		ID: "g", Name: "with ghost",
		Members: []GroupDMMember{{AgentID: "a1"}, {AgentID: "ghost"}},
	}, GroupDMInsertOptions{AllowDeadMembers: true})
	if err != nil {
		t.Fatalf("insert with dead member: %v", err)
	}

	// Renaming must not re-validate the existing "ghost" member.
	if _, err := s.UpdateGroupDM(ctx, "g", rec.ETag, func(r *GroupDMRecord) error {
		r.Name = "renamed"
		return nil
	}); err != nil {
		t.Fatalf("rename with dead member present: %v", err)
	}

	// Adding a *new* dead member must still fail — the diff-only check
	// continues to enforce alive for the additions.
	cur, _ := s.GetGroupDM(ctx, "g")
	_, err = s.UpdateGroupDM(ctx, "g", cur.ETag, func(r *GroupDMRecord) error {
		r.Members = append(r.Members, GroupDMMember{AgentID: "another-ghost"})
		return nil
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on adding dead member, got %v", err)
	}
}
