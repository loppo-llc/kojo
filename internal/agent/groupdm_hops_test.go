package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// dropAgentFromRuntime removes an agent from the in-memory manager map so
// Manager.Chat fails fast with ErrAgentNotFound (a non-transient error)
// without touching the filesystem — the full Chat prepare path spawns
// async writes that race the test TempDir cleanup.
func dropAgentFromRuntime(mgr *Manager, id string) {
	mgr.mu.Lock()
	delete(mgr.agents, id)
	mgr.mu.Unlock()
}

// markGuidesSynced suppresses the guide-file sync that prepareChat would
// otherwise run when a test path reaches Manager.Chat — the async write
// races the test TempDir cleanup.
func markGuidesSynced(t *testing.T) {
	t.Helper()
	guidesSyncMu.Lock()
	old := guidesSyncedAt
	guidesSyncedAt = time.Now().Add(time.Hour)
	guidesSyncMu.Unlock()
	t.Cleanup(func() {
		guidesSyncMu.Lock()
		guidesSyncedAt = old
		guidesSyncMu.Unlock()
	})
}

func TestParseGroupMentions(t *testing.T) {
	members := []string{"ag_alice", "ag_bob"}
	names := map[string]string{"ag_alice": "Alice Smith", "ag_bob": "Bob"}

	tests := []struct {
		content string
		want    []string
	}{
		{"no mentions here", nil},
		{"hey @Bob check this", []string{"ag_bob"}},
		{"hey @bob lowercase", []string{"ag_bob"}},
		{"@Alice first-word match", []string{"ag_alice"}},
		{"@ag_alice by id", []string{"ag_alice"}},
		{"@user please review", []string{"user"}},
		{"@User folded", []string{"user"}},
		{"@Bob and @Bob dedup", []string{"ag_bob"}},
		{"@Bob then @Alice order", []string{"ag_bob", "ag_alice"}},
		{"email a@b.example is not @nobody", nil},
		{"contact foo@user.example please", nil}, // email, not a @user mention
		{"ping x@Bob too", nil},                  // no boundary before @
		{"(@Bob) punctuation boundary ok", []string{"ag_bob"}},
	}
	for _, tt := range tests {
		got := parseGroupMentions(tt.content, members, names)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseGroupMentions(%q) = %v, want %v", tt.content, got, tt.want)
		}
	}
}

func TestEffectiveAndClampMaxHops(t *testing.T) {
	if got := effectiveMaxHops(0); got != defaultMaxHops {
		t.Errorf("effectiveMaxHops(0) = %d, want %d", got, defaultMaxHops)
	}
	if got := effectiveMaxHops(2); got != 2 {
		t.Errorf("effectiveMaxHops(2) = %d", got)
	}
	if got := effectiveMaxHops(maxMaxHops + 5); got != maxMaxHops {
		t.Errorf("effectiveMaxHops(big) = %d, want %d", got, maxMaxHops)
	}
	if got := clampMaxHops(-1); got != 0 {
		t.Errorf("clampMaxHops(-1) = %d, want 0", got)
	}
	if got := clampMaxHops(maxMaxHops + 1); got != maxMaxHops {
		t.Errorf("clampMaxHops(over) = %d, want %d", got, maxMaxHops)
	}
}

func TestGroupDMManager_PostMessage_HopAndMentionsPersisted(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, err := gdm.Create("Team", []string{"ag_alice", "ag_bob"}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}

	// Fresh turn: hop 0.
	msg, err := gdm.PostMessage(context.Background(), g.ID, "ag_alice", "hi @Bob", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Hop != 0 {
		t.Errorf("fresh-turn hop = %d, want 0", msg.Hop)
	}
	if !reflect.DeepEqual(msg.Mentions, []string{"ag_bob"}) {
		t.Errorf("mentions = %v, want [ag_bob]", msg.Mentions)
	}

	// Notification-triggered turn: hop = trigger + 1.
	gdm.notifyMu.Lock()
	gdm.triggerHop["ag_bob"] = triggerHopEntry{hop: 2}
	gdm.notifyMu.Unlock()
	msg2, err := gdm.PostMessage(context.Background(), g.ID, "ag_bob", "reply", msg.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if msg2.Hop != 3 {
		t.Errorf("triggered-turn hop = %d, want 3", msg2.Hop)
	}

	// Round-trip through the store.
	msgs, _, _, err := gdm.Messages(g.ID, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages", len(msgs))
	}
	if msgs[0].Hop != 0 || !reflect.DeepEqual(msgs[0].Mentions, []string{"ag_bob"}) {
		t.Errorf("persisted msg0 hop/mentions = %d/%v", msgs[0].Hop, msgs[0].Mentions)
	}
	if msgs[1].Hop != 3 {
		t.Errorf("persisted msg1 hop = %d, want 3", msgs[1].Hop)
	}
}

func TestGroupDMManager_HopLimitSuppressesAgentFanout(t *testing.T) {
	gdm, mgr := setupGroupDMTest(t)
	markGuidesSynced(t)
	g, err := gdm.Create("Team", []string{"ag_alice", "ag_bob"}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Fail Chat fast and side-effect-free for the recipient.
	dropAgentFromRuntime(mgr, "ag_bob")
	one := 1
	if _, _, err := gdm.UpdateSettings(context.Background(), g.ID, GroupDMSettingsPatch{MaxHops: &one}); err != nil {
		t.Fatal(err)
	}

	// Alice posts from a notification-triggered turn (trigger hop 0), so
	// the message lands at hop 1 == maxHops → agent fan-out suppressed.
	gdm.notifyMu.Lock()
	gdm.triggerHop["ag_alice"] = triggerHopEntry{hop: 0}
	gdm.notifyMu.Unlock()
	msg, err := gdm.PostMessage(context.Background(), g.ID, "ag_alice", "capped", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Hop != 1 {
		t.Fatalf("hop = %d, want 1", msg.Hop)
	}
	time.Sleep(200 * time.Millisecond)
	gdm.notifyMu.Lock()
	n := len(gdm.notify)
	gdm.notifyMu.Unlock()
	if n != 0 {
		t.Errorf("expected no notify state after hop-capped post, got %d entries", n)
	}
	// The message is still in the transcript for humans.
	msgs, _, _, err := gdm.Messages(g.ID, 10, "")
	if err != nil || len(msgs) != 1 {
		t.Fatalf("transcript after suppressed fan-out: %d msgs, err=%v", len(msgs), err)
	}

	// Control: a fresh-turn post (hop 0 < maxHops 1) does fan out.
	gdm.notifyMu.Lock()
	delete(gdm.triggerHop, "ag_alice")
	gdm.notifyMu.Unlock()
	if _, err := gdm.PostMessage(context.Background(), g.ID, "ag_alice", "fresh", msg.ID, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		gdm.notifyMu.Lock()
		n = len(gdm.notify)
		gdm.notifyMu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n == 0 {
		t.Error("expected notify state after under-limit post")
	}
}

func TestGroupDMManager_MentionPiercesCooldown(t *testing.T) {
	gdm, mgr := setupGroupDMTest(t)
	markGuidesSynced(t)
	g, err := gdm.Create("Team", []string{"ag_alice", "ag_bob"}, 3600, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Fail Chat fast and side-effect-free for the recipient.
	dropAgentFromRuntime(mgr, "ag_alice")
	key := g.ID + ":ag_alice"

	// Seed a lastSent inside the cooldown window.
	gdm.notifyMu.Lock()
	gdm.notify[key] = &notifyState{lastSent: time.Now()}
	gdm.notifyMu.Unlock()

	msg := newGroupMessage("ag_bob", "Bob", "hello", nil)

	// Not mentioned: buffered behind a deferred timer, no delivery attempt.
	gdm.notifyAgent("ag_alice", g.ID, g.Name, msg, false, false)
	gdm.notifyMu.Lock()
	ns := gdm.notify[key]
	if ns.timer == nil || len(ns.pendingMsgs) != 1 || ns.attempts != 0 {
		t.Errorf("unmentioned: timer=%v pending=%d attempts=%d, want deferred",
			ns.timer != nil, len(ns.pendingMsgs), ns.attempts)
	}
	gdm.notifyMu.Unlock()

	// Mentioned: cooldown pierced — delivery is attempted immediately
	// (Chat fails in the fixture, so the non-transient retry path bumps
	// attempts, which proves the attempt happened).
	gdm.notifyAgent("ag_alice", g.ID, g.Name, msg, false, true)
	gdm.notifyMu.Lock()
	ns = gdm.notify[key]
	if ns.attempts == 0 {
		t.Error("mentioned: expected an immediate delivery attempt (attempts > 0)")
	}
	gdm.notifyMu.Unlock()
}

func TestGroupDMManager_NonTransientFailureDeadLetters(t *testing.T) {
	gdm, mgr := setupGroupDMTest(t)
	markGuidesSynced(t)
	g, err := gdm.Create("Team", []string{"ag_alice", "ag_bob"}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Fail Chat fast and side-effect-free for the recipient.
	dropAgentFromRuntime(mgr, "ag_alice")
	key := g.ID + ":ag_alice"
	pending := []pendingMsg{{sender: "Bob", content: "hi", timestamp: time.Now().Format(time.RFC3339)}}

	for i := 1; i <= maxNotifyAttempts; i++ {
		gdm.notifyMu.Lock()
		ns := gdm.notify[key]
		if ns == nil {
			ns = &notifyState{agentID: "ag_alice", groupID: g.ID, groupName: g.Name}
			gdm.notify[key] = ns
		}
		if ns.timer != nil {
			ns.timer.Stop()
			ns.timer = nil
		}
		// Drive the retry manually: drain the requeued buffer so the
		// dead-letter branch does not immediately re-fire it as a new batch.
		ns.pendingMsgs = nil
		ns.inFlight = true
		gen := ns.gen
		gdm.notifyMu.Unlock()
		// Chat fails non-transiently in the fixture (no backend).
		gdm.deliverNotification(key, gen, "ag_alice", g.ID, g.Name, pending)
	}

	db := getGlobalStore()
	if db == nil {
		t.Fatal("no global store in fixture")
	}
	dls, err := db.ListGroupDMDeadLetters(context.Background(), g.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(dls) != 1 {
		t.Fatalf("dead letters = %d, want 1", len(dls))
	}
	if dls[0].AgentID != "ag_alice" || dls[0].Attempts != maxNotifyAttempts || dls[0].Reason == "" {
		t.Errorf("dead letter = %+v", dls[0])
	}
	// Attempts counter reset for the next batch.
	gdm.notifyMu.Lock()
	if ns := gdm.notify[key]; ns != nil && ns.attempts != 0 {
		t.Errorf("attempts after dead-letter = %d, want 0", ns.attempts)
	}
	gdm.notifyMu.Unlock()
}

func TestGroupDMManager_MuteAtDeliveryDropsBatch(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, err := gdm.Create("Team", []string{"ag_alice", "ag_bob"}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.SetMemberNotifyMode(g.ID, "ag_alice", NotifyMuted, 0, ""); err != nil {
		t.Fatal(err)
	}
	key := g.ID + ":ag_alice"
	gdm.notifyMu.Lock()
	ns := &notifyState{agentID: "ag_alice", groupID: g.ID, groupName: g.Name, inFlight: true}
	gdm.notify[key] = ns
	gdm.notifyMu.Unlock()

	gdm.deliverNotification(key, 0, "ag_alice", g.ID, g.Name,
		[]pendingMsg{{sender: "Bob", content: "queued before mute"}})

	gdm.notifyMu.Lock()
	if ns.inFlight || ns.attempts != 0 || len(ns.pendingMsgs) != 0 {
		t.Errorf("mute-at-delivery: inFlight=%v attempts=%d pending=%d, want dropped",
			ns.inFlight, ns.attempts, len(ns.pendingMsgs))
	}
	gdm.notifyMu.Unlock()
}

func TestGroupDMManager_FindOrCreateDM(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	// Human↔agent DM: single member.
	dm, created, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}
	if !created || dm.Kind != GroupDMKindDM || len(dm.Members) != 1 {
		t.Fatalf("dm = kind %q members %d created %v", dm.Kind, len(dm.Members), created)
	}

	// Idempotent find.
	again, created2, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}
	if created2 || again.ID != dm.ID {
		t.Errorf("find = %s created %v, want existing %s", again.ID, created2, dm.ID)
	}

	// Agent↔agent DM is a distinct room; member order does not matter.
	pair, created3, err := gdm.FindOrCreateDM([]string{"ag_bob", "ag_alice"})
	if err != nil {
		t.Fatal(err)
	}
	if !created3 || pair.ID == dm.ID || len(pair.Members) != 2 {
		t.Fatalf("pair dm = %s members %d created %v", pair.ID, len(pair.Members), created3)
	}
	pair2, created4, err := gdm.FindOrCreateDM([]string{"ag_alice", "ag_bob"})
	if err != nil {
		t.Fatal(err)
	}
	if created4 || pair2.ID != pair.ID {
		t.Errorf("reordered find = %s created %v, want %s", pair2.ID, created4, pair.ID)
	}

	// 3 members refused.
	if _, _, err := gdm.FindOrCreateDM([]string{"ag_alice", "ag_bob", "ag_charlie"}); err == nil {
		t.Error("expected error for 3-member dm")
	}
}

func TestGroupDMManager_UpdateSettingsMaxHops(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, err := gdm.Create("Team", []string{"ag_alice", "ag_bob"}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if g.MaxHops != 0 {
		t.Fatalf("default MaxHops = %d, want 0 (use default)", g.MaxHops)
	}
	v := 7
	updated, _, err := gdm.UpdateSettings(context.Background(), g.ID, GroupDMSettingsPatch{MaxHops: &v})
	if err != nil {
		t.Fatal(err)
	}
	if updated.MaxHops != 7 {
		t.Errorf("MaxHops = %d, want 7", updated.MaxHops)
	}
	over := maxMaxHops + 100
	updated, _, err = gdm.UpdateSettings(context.Background(), g.ID, GroupDMSettingsPatch{MaxHops: &over})
	if err != nil {
		t.Fatal(err)
	}
	if updated.MaxHops != maxMaxHops {
		t.Errorf("clamped MaxHops = %d, want %d", updated.MaxHops, maxMaxHops)
	}
}

func TestGroupDMManager_UnreadInfo(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, err := gdm.Create("Team", []string{"ag_alice", "ag_bob"}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	m1, err := gdm.PostMessage(context.Background(), g.ID, "ag_alice", "one", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostMessage(context.Background(), g.ID, "ag_bob", "two @user", m1.ID, false); err != nil {
		t.Fatal(err)
	}
	count, mentionsUser, _, err := gdm.UnreadInfo(g.ID, m1.ID, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || !mentionsUser {
		t.Errorf("unread after m1 = (%d, %v), want (1, true)", count, mentionsUser)
	}
	count, mentionsUser, _, err = gdm.UnreadInfo(g.ID, "", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || !mentionsUser {
		t.Errorf("unread from start = (%d, %v), want (2, true)", count, mentionsUser)
	}
}

// The partial UNIQUE index on dm_member_key rejects a duplicate DM room
// for the same member set even when the in-process find is bypassed.
func TestGroupDMManager_DuplicateDMInsertRejectedByIndex(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	if _, err := gdm.create("", []string{"ag_alice"}, 0, GroupDMStyleEfficient, defaultGroupDMVenue, GroupDMKindDM, false); err != nil {
		t.Fatal(err)
	}
	_, err := gdm.create("", []string{"ag_alice"}, 0, GroupDMStyleEfficient, defaultGroupDMVenue, GroupDMKindDM, false)
	if err == nil || !strings.Contains(err.Error(), "dm_member_key") {
		t.Fatalf("duplicate dm create err = %v, want dm_member_key constraint", err)
	}
	// FindOrCreateDM recovers from the same collision by adoption.
	dm, created, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil || created {
		t.Fatalf("FindOrCreateDM after duplicate = (%v, created=%v)", err, created)
	}
	if dm.Kind != GroupDMKindDM {
		t.Errorf("kind = %q", dm.Kind)
	}
}

// A writer that advances the DB head without going through this manager's
// cache (another daemon on the same database) is caught by the
// transactional ExpectedLatestSeq check inside the append.
func TestGroupDMManager_PostMessage_DBLevelCASCatchesForeignWriter(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, err := gdm.Create("Team", []string{"ag_alice", "ag_bob"}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	m1, err := gdm.PostMessage(context.Background(), g.ID, "ag_alice", "one", "", false)
	if err != nil {
		t.Fatal(err)
	}
	// Foreign write: append directly to the store, bypassing the cache.
	foreign := newGroupMessage("ag_bob", "Bob", "sneaky", nil)
	if err := appendGroupMessage(g.ID, foreign, 0, false); err != nil {
		t.Fatal(err)
	}
	// The cache still says m1 is the head, so the in-memory CAS passes —
	// the DB transaction must reject the append.
	_, err = gdm.PostMessage(context.Background(), g.ID, "ag_alice", "reply", m1.ID, false)
	var stale *StaleExpectedIDError
	if !errors.As(err, &stale) {
		t.Fatalf("err = %v, want StaleExpectedIDError", err)
	}
	if len(stale.NewMessages) == 0 || stale.Latest != foreign.ID {
		t.Errorf("conflict = latest %q diff %d, want latest %q", stale.Latest, len(stale.NewMessages), foreign.ID)
	}
}

// enforceSeq with expectedSeq 0 expresses "the room must be empty": once a
// message exists, the transactional check rejects the append.
func TestAppendGroupMessage_EnforceEmptyRoom(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, err := gdm.Create("Team", []string{"ag_alice", "ag_bob"}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	first := newGroupMessage("ag_alice", "Alice", "first", nil)
	if err := appendGroupMessage(g.ID, first, 0, true); err != nil {
		t.Fatalf("append into empty room with enforce: %v", err)
	}
	second := newGroupMessage("ag_bob", "Bob", "also first", nil)
	if err := appendGroupMessage(g.ID, second, 0, true); err == nil || !errors.Is(err, store.ErrStaleHead) {
		t.Fatalf("second expect-empty append err = %v, want ErrStaleHead", err)
	}
	// Non-enforced zero seq keeps legacy skip semantics.
	if err := appendGroupMessage(g.ID, second, 0, false); err != nil {
		t.Fatalf("legacy append: %v", err)
	}
}

// A cursor that matches the in-memory cache but no longer resolves to a
// live DB row (foreign transcript clear) must hard-409, not silently
// disable CAS.
func TestGroupDMManager_PostMessage_UnresolvableCursorIsStale(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, err := gdm.Create("Team", []string{"ag_alice", "ag_bob"}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	m1, err := gdm.PostMessage(context.Background(), g.ID, "ag_alice", "one", "", false)
	if err != nil {
		t.Fatal(err)
	}
	// Foreign clear: wipe the transcript in the DB while leaving the
	// manager's cache pointing at m1.
	db := getGlobalStore()
	if db == nil {
		t.Fatal("no global store")
	}
	if _, err := db.SoftDeleteGroupDMMessages(context.Background(), g.ID); err != nil {
		t.Fatal(err)
	}
	_, err = gdm.PostMessage(context.Background(), g.ID, "ag_bob", "reply", m1.ID, false)
	var stale *StaleExpectedIDError
	if !errors.As(err, &stale) {
		t.Fatalf("err = %v, want StaleExpectedIDError", err)
	}
}
