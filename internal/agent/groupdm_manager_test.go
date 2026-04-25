package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClampCooldown(t *testing.T) {
	tests := []struct {
		input int
		want  int
	}{
		{0, 0},
		{-1, 0},
		{50, 50},
		{3600, 3600},
		{3601, 3600},
		{100000, 3600},
	}
	for _, tt := range tests {
		got := clampCooldown(tt.input)
		if got != tt.want {
			t.Errorf("clampCooldown(%d) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

// setupGroupDMTest creates a GroupDMManager with a minimal Manager for testing.
// The Manager has two fake agents that can be resolved by GroupDMManager.
func setupGroupDMTest(t *testing.T) (*GroupDMManager, *Manager) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("APPDATA", "")

	mgr := newTestManager(t)

	// Create two agents directly in the manager's map
	mgr.mu.Lock()
	mgr.agents["ag_alice"] = &Agent{ID: "ag_alice", Name: "Alice", Tool: "claude"}
	mgr.agents["ag_bob"] = &Agent{ID: "ag_bob", Name: "Bob", Tool: "claude"}
	mgr.agents["ag_charlie"] = &Agent{ID: "ag_charlie", Name: "Charlie", Tool: "claude"}
	mgr.mu.Unlock()

	gdm := NewGroupDMManager(mgr, mgr.logger)
	return gdm, mgr
}

// newTestManager creates a minimal Manager for group DM tests.
// Does NOT start cron or notify poller.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("APPDATA", "")

	logger := testLogger()
	m := &Manager{
		agents:     make(map[string]*Agent),
		backends:   make(map[string]ChatBackend),
		store:      &store{dir: tmp + "/agents", logger: logger},
		logger:     logger,
		busy:       make(map[string]busyEntry),
		resetting:  make(map[string]bool),
		profileGen: make(map[string]bool),
		memIndexes: make(map[string]*MemoryIndex),
	}
	return m
}

func TestGroupDMManager_Create(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, err := gdm.Create("Test Group", []string{"ag_alice", "ag_bob"}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != "Test Group" {
		t.Errorf("name = %q, want %q", g.Name, "Test Group")
	}
	if len(g.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(g.Members))
	}
	if g.ID == "" {
		t.Error("expected non-empty group ID")
	}
}

func TestGroupDMManager_CreateDefaultName(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, err := gdm.Create("", []string{"ag_alice", "ag_bob"}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// defaultGroupName joins member names in input order
	if g.Name != "Alice, Bob" {
		t.Errorf("default name = %q, want %q", g.Name, "Alice, Bob")
	}
}

func TestGroupDMManager_CreateTooFewMembers(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	_, err := gdm.Create("Solo", []string{"ag_alice"}, 0, "", "")
	if !errors.Is(err, ErrGroupTooFew) {
		t.Errorf("expected ErrGroupTooFew, got %v", err)
	}
}

func TestGroupDMManager_CreateNonexistentAgent(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	_, err := gdm.Create("Bad", []string{"ag_alice", "ag_nonexistent"}, 0, "", "")
	if !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("expected ErrAgentNotFound, got %v", err)
	}
}

func TestGroupDMManager_CreateDeduplicatesMembers(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	_, err := gdm.Create("Dup", []string{"ag_alice", "ag_alice"}, 0, "", "")
	if !errors.Is(err, ErrGroupTooFew) {
		t.Errorf("expected ErrGroupTooFew after dedup, got %v", err)
	}
}

func TestGroupDMManager_GetAndList(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	created, _ := gdm.Create("Group1", []string{"ag_alice", "ag_bob"}, 0, "", "")

	got, ok := gdm.Get(created.ID)
	if !ok {
		t.Fatal("expected group to be found")
	}
	if got.Name != "Group1" {
		t.Errorf("name = %q, want %q", got.Name, "Group1")
	}

	list := gdm.List()
	if len(list) != 1 {
		t.Errorf("expected 1 group, got %d", len(list))
	}
}

func TestGroupDMManager_GetNotFound(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	_, ok := gdm.Get("gd_nonexistent")
	if ok {
		t.Error("expected not found")
	}
}

func TestGroupDMManager_Delete(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("ToDelete", []string{"ag_alice", "ag_bob"}, 0, "", "")

	if err := gdm.Delete(g.ID, false); err != nil {
		t.Fatal(err)
	}

	_, ok := gdm.Get(g.ID)
	if ok {
		t.Error("group should be deleted")
	}
}

func TestGroupDMManager_DeleteNotFound(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	err := gdm.Delete("gd_nonexistent", false)
	if !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("expected ErrGroupNotFound, got %v", err)
	}
}

func TestGroupDMManager_SetCooldown(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("CD", []string{"ag_alice", "ag_bob"}, 0, "", "")

	updated, err := gdm.SetCooldown(g.ID, 120)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Cooldown != 120 {
		t.Errorf("cooldown = %d, want 120", updated.Cooldown)
	}
}

func TestGroupDMManager_SetCooldownClamped(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("CD", []string{"ag_alice", "ag_bob"}, 0, "", "")

	updated, err := gdm.SetCooldown(g.ID, 9999)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Cooldown != maxCooldown {
		t.Errorf("cooldown = %d, want %d", updated.Cooldown, maxCooldown)
	}
}

func TestGroupDMManager_Rename(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("Old Name", []string{"ag_alice", "ag_bob"}, 0, "", "")

	renamed, err := gdm.Rename(g.ID, "New Name", "ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "New Name" {
		t.Errorf("name = %q, want %q", renamed.Name, "New Name")
	}
}

func TestGroupDMManager_RenameNotMember(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0, "", "")

	_, err := gdm.Rename(g.ID, "New", "ag_charlie")
	if !errors.Is(err, ErrGroupNotMember) {
		t.Errorf("expected ErrGroupNotMember, got %v", err)
	}
}

func TestGroupDMManager_RenameNotFound(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	_, err := gdm.Rename("gd_nonexistent", "New", "ag_alice")
	if !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("expected ErrGroupNotFound, got %v", err)
	}
}

func TestGroupDMManager_AddMember(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0, "", "")

	updated, err := gdm.AddMember(g.ID, "ag_charlie", "ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Members) != 3 {
		t.Errorf("expected 3 members, got %d", len(updated.Members))
	}
}

func TestGroupDMManager_AddMemberDuplicate(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0, "", "")

	_, err := gdm.AddMember(g.ID, "ag_alice", "ag_bob")
	if !errors.Is(err, ErrGroupAlreadyMember) {
		t.Errorf("expected ErrGroupAlreadyMember, got %v", err)
	}
}

func TestGroupDMManager_AddMemberNotMember(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0, "", "")

	_, err := gdm.AddMember(g.ID, "ag_charlie", "ag_charlie")
	if !errors.Is(err, ErrGroupNotMember) {
		t.Errorf("expected ErrGroupNotMember, got %v", err)
	}
}

func TestGroupDMManager_LeaveGroup(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob", "ag_charlie"}, 0, "", "")

	if err := gdm.LeaveGroup(g.ID, "ag_charlie"); err != nil {
		t.Fatal(err)
	}

	got, ok := gdm.Get(g.ID)
	if !ok {
		t.Fatal("group should still exist")
	}
	if len(got.Members) != 2 {
		t.Errorf("expected 2 members after leave, got %d", len(got.Members))
	}
}

func TestGroupDMManager_LeaveGroupNotMember(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0, "", "")

	err := gdm.LeaveGroup(g.ID, "ag_charlie")
	if !errors.Is(err, ErrGroupNotMember) {
		t.Errorf("expected ErrGroupNotMember, got %v", err)
	}
}

func TestGroupDMManager_LeaveGroupDissolvesWithTwoMembers(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0, "", "")

	if err := gdm.LeaveGroup(g.ID, "ag_bob"); err != nil {
		t.Fatal(err)
	}

	_, ok := gdm.Get(g.ID)
	if ok {
		t.Error("group should be dissolved when < 2 members")
	}
}

func TestGroupDMManager_GroupsForAgent(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	gdm.Create("G1", []string{"ag_alice", "ag_bob"}, 0, "", "")
	gdm.Create("G2", []string{"ag_bob", "ag_charlie"}, 0, "", "")

	aliceGroups := gdm.GroupsForAgent("ag_alice")
	if len(aliceGroups) != 1 {
		t.Errorf("expected 1 group for alice, got %d", len(aliceGroups))
	}

	bobGroups := gdm.GroupsForAgent("ag_bob")
	if len(bobGroups) != 2 {
		t.Errorf("expected 2 groups for bob, got %d", len(bobGroups))
	}
}

func TestGroupDMManager_RemoveAgent(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	gdm.Create("G1", []string{"ag_alice", "ag_bob", "ag_charlie"}, 0, "", "")
	gdm.Create("G2", []string{"ag_alice", "ag_bob"}, 0, "", "")

	gdm.RemoveAgent("ag_alice")

	// G1 should still exist (bob + charlie)
	g1List := gdm.List()
	found := false
	for _, g := range g1List {
		for _, m := range g.Members {
			if m.AgentID == "ag_alice" {
				t.Error("alice should be removed from all groups")
			}
		}
		if len(g.Members) >= 2 {
			found = true
		}
	}
	if !found {
		t.Error("expected at least one group to survive with 2+ members")
	}
}

func TestGroupDMManager_CopyIsolation(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0, "", "")

	// Mutating the returned copy should not affect the internal state
	g.Name = "Mutated"
	g.Members = nil

	got, _ := gdm.Get(g.ID)
	if got.Name == "Mutated" {
		t.Error("internal state should not be affected by copy mutation")
	}
	if len(got.Members) != 2 {
		t.Error("internal members should not be affected by copy mutation")
	}
}

func TestGroupDMManager_CreateWithStyle(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, err := gdm.Create("Styled", []string{"ag_alice", "ag_bob"}, 0, "expressive", "")
	if err != nil {
		t.Fatal(err)
	}
	if g.Style != GroupDMStyleExpressive {
		t.Errorf("style = %q, want %q", g.Style, GroupDMStyleExpressive)
	}
}

func TestGroupDMManager_CreateDefaultStyle(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, err := gdm.Create("Default", []string{"ag_alice", "ag_bob"}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if g.Style != GroupDMStyleEfficient {
		t.Errorf("style = %q, want %q", g.Style, GroupDMStyleEfficient)
	}
}

func TestGroupDMManager_CreateInvalidStyle(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	_, err := gdm.Create("Bad", []string{"ag_alice", "ag_bob"}, 0, "invalid", "")
	if err == nil {
		t.Error("expected error for invalid style")
	}
}

func TestGroupDMManager_SetStyle(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0, "", "")

	updated, err := gdm.SetStyle(g.ID, "expressive", "ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Style != GroupDMStyleExpressive {
		t.Errorf("style = %q, want %q", updated.Style, GroupDMStyleExpressive)
	}

	// Verify persistence
	got, _ := gdm.Get(g.ID)
	if got.Style != GroupDMStyleExpressive {
		t.Errorf("persisted style = %q, want %q", got.Style, GroupDMStyleExpressive)
	}
}

func TestGroupDMManager_SetStyleInvalid(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0, "", "")

	_, err := gdm.SetStyle(g.ID, "bogus", "ag_alice")
	if err == nil {
		t.Error("expected error for invalid style")
	}
}

func TestGroupDMManager_SetStyleNotMember(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0, "", "")

	_, err := gdm.SetStyle(g.ID, "expressive", "ag_charlie")
	if !errors.Is(err, ErrGroupNotMember) {
		t.Errorf("expected ErrGroupNotMember, got %v", err)
	}
}

func TestGroupDMManager_SetStyleNoCallerSkipsCheck(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0, "", "")

	// Empty callerAgentID should skip membership check (admin/UI call)
	updated, err := gdm.SetStyle(g.ID, "expressive", "")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Style != GroupDMStyleExpressive {
		t.Errorf("style = %q, want %q", updated.Style, GroupDMStyleExpressive)
	}
}

func TestGroupDMManager_SetMemberNotifyMode(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("G", []string{"ag_alice", "ag_bob"}, 0, "", "")

	updated, err := gdm.SetMemberNotifyMode(g.ID, "ag_alice", NotifyDigest, 600)
	if err != nil {
		t.Fatal(err)
	}
	var alice *GroupMember
	for i := range updated.Members {
		if updated.Members[i].AgentID == "ag_alice" {
			alice = &updated.Members[i]
		}
	}
	if alice == nil {
		t.Fatal("alice missing from updated group")
	}
	if alice.NotifyMode != NotifyDigest {
		t.Errorf("notifyMode = %q, want %q", alice.NotifyMode, NotifyDigest)
	}
	if alice.DigestWindow != 600 {
		t.Errorf("digestWindow = %d, want 600", alice.DigestWindow)
	}

	// Verify persistence via memberNotifySettings.
	mode, window := gdm.memberNotifySettings(g.ID, "ag_alice")
	if mode != NotifyDigest || window != 600 {
		t.Errorf("memberNotifySettings = (%q, %d), want (digest, 600)", mode, window)
	}
}

func TestGroupDMManager_SetMemberNotifyModeInvalid(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("G", []string{"ag_alice", "ag_bob"}, 0, "", "")

	if _, err := gdm.SetMemberNotifyMode(g.ID, "ag_alice", "bogus", 0); err == nil {
		t.Error("expected error for invalid mode")
	}
}

func TestGroupDMManager_SetMemberNotifyModeNotMember(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("G", []string{"ag_alice", "ag_bob"}, 0, "", "")

	_, err := gdm.SetMemberNotifyMode(g.ID, "ag_charlie", NotifyMuted, 0)
	if !errors.Is(err, ErrGroupNotMember) {
		t.Errorf("expected ErrGroupNotMember, got %v", err)
	}
}

func TestGroupDMManager_SetMemberNotifyModeNotFound(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	_, err := gdm.SetMemberNotifyMode("gd_nope", "ag_alice", NotifyMuted, 0)
	if !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("expected ErrGroupNotFound, got %v", err)
	}
}

func TestGroupDMManager_SetMemberNotifyModeDigestWindowClamped(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("G", []string{"ag_alice", "ag_bob"}, 0, "", "")

	// Oversized window is clamped to maxDigestWindow; negative goes to 0.
	updated, err := gdm.SetMemberNotifyMode(g.ID, "ag_alice", NotifyDigest, 99999)
	if err != nil {
		t.Fatal(err)
	}
	for _, mem := range updated.Members {
		if mem.AgentID == "ag_alice" && mem.DigestWindow != maxDigestWindow {
			t.Errorf("clamped digestWindow = %d, want %d", mem.DigestWindow, maxDigestWindow)
		}
	}

	updated, err = gdm.SetMemberNotifyMode(g.ID, "ag_alice", NotifyDigest, -10)
	if err != nil {
		t.Fatal(err)
	}
	for _, mem := range updated.Members {
		if mem.AgentID == "ag_alice" && mem.DigestWindow != 0 {
			t.Errorf("clamped digestWindow = %d, want 0", mem.DigestWindow)
		}
	}
}

func TestGroupDMManager_MemberNotifySettingsDefault(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("G", []string{"ag_alice", "ag_bob"}, 0, "", "")

	mode, window := gdm.memberNotifySettings(g.ID, "ag_alice")
	if mode != NotifyRealtime {
		t.Errorf("default mode = %q, want realtime", mode)
	}
	if window != 0 {
		t.Errorf("default window = %d, want 0", window)
	}

	// Unknown agent -> realtime default, not an error.
	mode, _ = gdm.memberNotifySettings(g.ID, "ag_unknown")
	if mode != NotifyRealtime {
		t.Errorf("unknown agent mode = %q, want realtime", mode)
	}
}

func TestGroupDMManager_MutedMemberDropsNotification(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("G", []string{"ag_alice", "ag_bob"}, 0, "", "")

	if _, err := gdm.SetMemberNotifyMode(g.ID, "ag_alice", NotifyMuted, 0); err != nil {
		t.Fatal(err)
	}

	msg := newGroupMessage("ag_bob", "Bob", "ping")
	// notifyAgent must return without touching notify state for a muted member.
	gdm.notifyAgent("ag_alice", g.ID, g.Name, msg, false)

	gdm.notifyMu.Lock()
	_, exists := gdm.notify[g.ID+":ag_alice"]
	gdm.notifyMu.Unlock()
	if exists {
		t.Error("muted member should have no notify state entry")
	}
}

func TestGroupDMManager_EffectiveCooldown(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("G", []string{"ag_alice", "ag_bob"}, 120, "", "")

	// Realtime respects group cooldown.
	if got := gdm.effectiveCooldown(g.ID, NotifyRealtime, 0); got != 120*time.Second {
		t.Errorf("realtime cooldown = %v, want 120s", got)
	}

	// Digest takes the larger of group cooldown and window.
	if got := gdm.effectiveCooldown(g.ID, NotifyDigest, 60); got != 120*time.Second {
		t.Errorf("digest window<cooldown = %v, want 120s", got)
	}
	if got := gdm.effectiveCooldown(g.ID, NotifyDigest, 600); got != 600*time.Second {
		t.Errorf("digest window>cooldown = %v, want 600s", got)
	}

	// Digest with zero window falls back to defaultDigestWindow.
	g2, _ := gdm.Create("G2", []string{"ag_alice", "ag_bob"}, 0, "", "")
	want := time.Duration(defaultDigestWindow) * time.Second
	if got := gdm.effectiveCooldown(g2.ID, NotifyDigest, 0); got != want {
		t.Errorf("digest zero-window = %v, want %v", got, want)
	}
}

func TestGroupDMManager_RenderNotificationInlinesContent(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("G", []string{"ag_alice", "ag_bob"}, 0, "", "")
	gdm.SetAPIBase("https://example/ts.net")

	pending := []pendingMsg{
		{sender: "Bob", content: "hi there", timestamp: "2026-04-25T00:00:00Z"},
		{sender: "User", content: "ping", timestamp: "2026-04-25T00:00:01Z", senderIsUser: true},
	}
	out := gdm.renderNotification("ag_alice", g.ID, g.Name, pending)
	if !strings.Contains(out, "2 new message(s) from User (human operator)") {
		t.Errorf("missing batch count + latest-sender suffix: %s", out)
	}
	if !strings.Contains(out, "Bob: hi there") {
		t.Errorf("missing bob content: %s", out)
	}
	if !strings.Contains(out, "User (human operator): ping") {
		t.Errorf("missing user operator label: %s", out)
	}
	if !strings.Contains(out, "Reply: curl -sk") {
		t.Errorf("missing reply curl (https should use -sk): %s", out)
	}
	// Full-transcript pointer appears only when truncation/omission kicked in.
	if strings.Contains(out, "Full transcript:") {
		t.Error("unexpected full-transcript pointer for small inline batch")
	}
}

func TestGroupDMManager_RenderNotificationHeaderHasLatestSender(t *testing.T) {
	// The "from <latest_sender>" suffix lives in the trusted header so the
	// Web UI can extract the latest sender without parsing the untrusted
	// message block. Verify both agent senders and human-user senders.
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("G", []string{"ag_alice", "ag_bob"}, 0, "", "")

	pending := []pendingMsg{
		{sender: "Old", content: "first", timestamp: "t"},
		{sender: "Bob", content: "second", timestamp: "t"},
	}
	out := gdm.renderNotification("ag_alice", g.ID, g.Name, pending)
	if !strings.Contains(out, "from Bob.") {
		t.Errorf("expected 'from Bob.' suffix from newest pending entry: %s", out)
	}
	if strings.Contains(out, "from Old") {
		t.Errorf("header should reference newest sender, not oldest: %s", out)
	}

	// Human-user message gets the explicit operator tag in the header too.
	pendingUser := []pendingMsg{{sender: "User", content: "ping", timestamp: "t", senderIsUser: true}}
	out = gdm.renderNotification("ag_alice", g.ID, g.Name, pendingUser)
	if !strings.Contains(out, "from User (human operator).") {
		t.Errorf("expected '(human operator)' tag in header: %s", out)
	}
}

func TestGroupDMManager_RenderNotificationLargeMessageInlinedFully(t *testing.T) {
	// A multi-KB message that fits inside notifyMaxBatchBytes must be
	// inlined verbatim — the per-message char cap was removed because
	// truncating defeats the inlining win (agent has to curl anyway).
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("G", []string{"ag_alice", "ag_bob"}, 0, "", "")
	gdm.SetAPIBase("http://localhost:8080")

	const size = 3000 // > old 500-cap, < notifyMaxSingleContent and < batch budget
	long := strings.Repeat("x", size)
	pending := []pendingMsg{{sender: "Bob", content: long, timestamp: "t"}}
	out := gdm.renderNotification("ag_alice", g.ID, g.Name, pending)
	if !strings.Contains(out, long) {
		t.Error("expected full content inlined without truncation")
	}
	if strings.Contains(out, "…[truncated]") {
		t.Error("unexpected truncation marker for in-budget content")
	}
	if strings.Contains(out, "Full transcript:") {
		t.Error("unexpected transcript pointer for fully-inlined content")
	}
	if !strings.Contains(out, "Reply: curl -s ") {
		t.Errorf("http base should use -s not -sk: %s", out)
	}
}

func TestGroupDMManager_RenderNotificationDropsOldWhenBatchTooBig(t *testing.T) {
	// Selection is newest-first under the byte budget. When the queued
	// total exceeds notifyMaxBatchBytes, the oldest messages are dropped
	// (whole-message) rather than each being truncated mid-content.
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("G", []string{"ag_alice", "ag_bob"}, 0, "", "")

	// Each message ~2KB; 12 of them (~24KB) overflow the 16KB budget.
	chunk := strings.Repeat("y", 2000)
	pending := make([]pendingMsg, 12)
	for i := range pending {
		pending[i] = pendingMsg{
			sender:    "Bob",
			content:   fmt.Sprintf("%d-%s", i, chunk),
			timestamp: "t",
		}
	}
	out := gdm.renderNotification("ag_alice", g.ID, g.Name, pending)

	// Newest message must be present in full.
	newestPrefix := fmt.Sprintf("%d-", len(pending)-1)
	if !strings.Contains(out, newestPrefix) {
		t.Errorf("newest message %q missing: %s", newestPrefix, out[:200])
	}
	// At least the oldest must be omitted.
	if !strings.Contains(out, "earlier message(s) omitted") {
		t.Error("expected omission marker when batch overflowed budget")
	}
	if !strings.Contains(out, "Full transcript:") {
		t.Error("expected transcript pointer when older messages dropped")
	}
	// No mid-message truncation — kept messages render untruncated.
	if strings.Contains(out, "…[truncated]") {
		t.Error("unexpected truncation marker — drop-old policy should not clip")
	}
}

func TestGroupDMManager_RenderNotificationSingleHugeMessageClipped(t *testing.T) {
	// Pathological case: a single message bigger than the entire batch
	// budget. We still inline it (clipped to notifyMaxSingleContent) so
	// the agent has *something* to react to without a curl round-trip.
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("G", []string{"ag_alice", "ag_bob"}, 0, "", "")

	huge := strings.Repeat("z", notifyMaxBatchBytes+5000)
	pending := []pendingMsg{{sender: "Bob", content: huge, timestamp: "t"}}
	out := gdm.renderNotification("ag_alice", g.ID, g.Name, pending)
	if !strings.Contains(out, "…[truncated]") {
		t.Error("expected truncation marker for single huge message")
	}
	if !strings.Contains(out, "Full transcript:") {
		t.Error("expected transcript pointer when single message was clipped")
	}
	// Should clip to notifyMaxSingleContent, not pass full content.
	if strings.Count(out, "z") > notifyMaxSingleContent+10 {
		t.Errorf("clipped content too long: %d 'z' chars", strings.Count(out, "z"))
	}
}

// TestGroupDMManager_RenderedSizeRespectsBudget feeds the renderer a
// worst-case load (max batch count, each message close to the line budget,
// API base + group/agent IDs at realistic length, transcript footer
// included) and asserts the resulting system-prompt string stays within
// notifyMaxBatchBytes. If this fails it means notifyHeaderFooterReserve is
// too small for the current header/footer text and needs to grow.
func TestGroupDMManager_RenderedSizeRespectsBudget(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("Long Realistic Group Name For Headroom", []string{"ag_alice", "ag_bob"}, 0, "", "")
	gdm.SetAPIBase("https://kojo.tail451b50.ts.net:8080")

	// Build a queue that selectBatch is likely to fill: lots of medium-sized
	// messages so the line budget is the binding cap, plus enough total
	// volume to force the "Full transcript" footer.
	chunk := strings.Repeat("a", 800)
	pending := make([]pendingMsg, 30) // > notifyMaxBatch so selectBatch drops oldest
	for i := range pending {
		pending[i] = pendingMsg{
			sender:       "BobWithAReasonablyLongName",
			content:      fmt.Sprintf("%d-%s", i, chunk),
			timestamp:    "2026-04-25T00:00:00Z",
			senderIsUser: i%5 == 0, // sprinkle "(human operator)" labels
		}
	}
	out := gdm.renderNotification("ag_alice", g.ID, g.Name, pending)
	if len(out) > notifyMaxBatchBytes {
		t.Fatalf("rendered notification = %d bytes, exceeds notifyMaxBatchBytes=%d. "+
			"Bump notifyHeaderFooterReserve.", len(out), notifyMaxBatchBytes)
	}
	// Sanity: the test should actually be exercising the cap, not just
	// rendering a tiny payload that trivially fits.
	if len(out) < notifyMaxBatchBytes/2 {
		t.Errorf("rendered output too small (%d bytes) — test does not exercise budget", len(out))
	}
}

func TestGroupDMManager_RenderNotificationBatchLimit(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("G", []string{"ag_alice", "ag_bob"}, 0, "", "")

	many := make([]pendingMsg, notifyMaxBatch+5)
	for i := range many {
		many[i] = pendingMsg{sender: "Bob", content: "x", timestamp: "t"}
	}
	out := gdm.renderNotification("ag_alice", g.ID, g.Name, many)
	if !strings.Contains(out, "5 earlier message(s) omitted") {
		t.Errorf("expected omission marker: %s", out)
	}
	if !strings.Contains(out, "Full transcript:") {
		t.Error("expected full-transcript pointer when batch overflowed")
	}
}

func TestCapPending(t *testing.T) {
	// Below the cap: returned slice is the input unchanged.
	small := []pendingMsg{{sender: "a"}, {sender: "b"}}
	if got := capPending(small); len(got) != 2 || got[0].sender != "a" || got[1].sender != "b" {
		t.Errorf("small slice mangled: %+v", got)
	}

	// Empty is a no-op.
	if got := capPending(nil); got != nil {
		t.Errorf("nil input should stay nil, got %+v", got)
	}

	// Above the cap: drops the oldest, keeps the newest notifyMaxPending.
	over := make([]pendingMsg, notifyMaxPending+30)
	for i := range over {
		over[i] = pendingMsg{sender: fmt.Sprintf("s-%d", i)}
	}
	got := capPending(over)
	if len(got) != notifyMaxPending {
		t.Fatalf("cap len = %d, want %d", len(got), notifyMaxPending)
	}
	// Oldest kept is index 30 of the input.
	if got[0].sender != fmt.Sprintf("s-%d", 30) {
		t.Errorf("oldest kept = %q, want s-30", got[0].sender)
	}
	if got[len(got)-1].sender != fmt.Sprintf("s-%d", notifyMaxPending+30-1) {
		t.Errorf("newest kept = %q", got[len(got)-1].sender)
	}

	// Capped result must not alias the input's backing array — otherwise
	// a later append to the returned slice could stomp on the original.
	// We verify by mutating the returned slice and checking the input.
	first := over[30]
	got[0] = pendingMsg{sender: "mutated"}
	if over[30].sender != first.sender {
		t.Error("capPending should return a copy that does not alias input storage")
	}
}

func TestGroupDMManager_PendingBufferCap(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("G", []string{"ag_alice", "ag_bob"}, 0, "", "")

	// Put the (group, agent) key into a state that defers all further
	// notifyAgent calls without actually invoking the agent backend: mark
	// the slot as inFlight so notifyAgent unconditionally buffers. This
	// isolates the cap behavior from the Chat() plumbing.
	key := g.ID + ":ag_alice"
	gdm.notifyMu.Lock()
	gdm.notify[key] = &notifyState{inFlight: true}
	gdm.notifyMu.Unlock()

	// Push twice the cap; expect the oldest half to be dropped.
	for i := 0; i < notifyMaxPending*2; i++ {
		msg := newGroupMessage("ag_bob", "Bob", fmt.Sprintf("msg-%d", i))
		gdm.notifyAgent("ag_alice", g.ID, g.Name, msg, false)
	}

	gdm.notifyMu.Lock()
	ns := gdm.notify[key]
	got := len(ns.pendingMsgs)
	first := ns.pendingMsgs[0].content
	last := ns.pendingMsgs[len(ns.pendingMsgs)-1].content
	gdm.notifyMu.Unlock()

	if got != notifyMaxPending {
		t.Errorf("pending len = %d, want %d", got, notifyMaxPending)
	}
	// Oldest kept should be msg-notifyMaxPending, newest should be the last.
	wantFirst := fmt.Sprintf("msg-%d", notifyMaxPending)
	if first != wantFirst {
		t.Errorf("oldest kept = %q, want %q", first, wantFirst)
	}
	wantLast := fmt.Sprintf("msg-%d", notifyMaxPending*2-1)
	if last != wantLast {
		t.Errorf("newest kept = %q, want %q", last, wantLast)
	}
}

func TestGroupDMManager_CreateDefaultVenue(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, err := gdm.Create("V", []string{"ag_alice", "ag_bob"}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if g.Venue != GroupDMVenueChatroom {
		t.Errorf("default venue = %q, want %q", g.Venue, GroupDMVenueChatroom)
	}
}

func TestGroupDMManager_CreateColocatedVenue(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, err := gdm.Create("V", []string{"ag_alice", "ag_bob"}, 0, "", GroupDMVenueColocated)
	if err != nil {
		t.Fatal(err)
	}
	if g.Venue != GroupDMVenueColocated {
		t.Errorf("venue = %q, want %q", g.Venue, GroupDMVenueColocated)
	}
}

func TestGroupDMManager_CreateInvalidVenue(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	_, err := gdm.Create("V", []string{"ag_alice", "ag_bob"}, 0, "", "elsewhere")
	if err == nil {
		t.Error("expected error for invalid venue")
	}
}

func TestGroupDMManager_SetVenue(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("V", []string{"ag_alice", "ag_bob"}, 0, "", "")

	updated, err := gdm.SetVenue(g.ID, GroupDMVenueColocated, "ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Venue != GroupDMVenueColocated {
		t.Errorf("venue = %q, want %q", updated.Venue, GroupDMVenueColocated)
	}
	got, _ := gdm.Get(g.ID)
	if got.Venue != GroupDMVenueColocated {
		t.Errorf("persisted venue = %q, want %q", got.Venue, GroupDMVenueColocated)
	}
}

func TestGroupDMManager_SetVenueInvalid(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("V", []string{"ag_alice", "ag_bob"}, 0, "", "")
	if _, err := gdm.SetVenue(g.ID, "elsewhere", "ag_alice"); err == nil {
		t.Error("expected error for invalid venue")
	}
}

func TestGroupDMManager_SetVenueNotMember(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("V", []string{"ag_alice", "ag_bob"}, 0, "", "")
	_, err := gdm.SetVenue(g.ID, GroupDMVenueColocated, "ag_charlie")
	if !errors.Is(err, ErrGroupNotMember) {
		t.Errorf("expected ErrGroupNotMember, got %v", err)
	}
}

func TestGroupDMManager_GroupVenueDefaultsForLegacy(t *testing.T) {
	// A group loaded from disk without a venue field (legacy on-disk JSON
	// from before this feature shipped) must read back as defaultGroupDMVenue
	// without rewriting the on-disk file.
	gdm, _ := setupGroupDMTest(t)
	g, _ := gdm.Create("V", []string{"ag_alice", "ag_bob"}, 0, "", "")
	// Force venue back to empty (simulating legacy JSON).
	gdm.mu.Lock()
	gdm.groups[g.ID].Venue = ""
	gdm.mu.Unlock()

	if v := gdm.groupVenue(g.ID); v != defaultGroupDMVenue {
		t.Errorf("groupVenue for legacy = %q, want %q", v, defaultGroupDMVenue)
	}
}

func TestGroupDMManager_RenderNotificationVenueHint(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	gdm.SetAPIBase("http://localhost:8080")

	// Default (chatroom) venue should yield the chat-room hint.
	gChat, _ := gdm.Create("Chat", []string{"ag_alice", "ag_bob"}, 0, "", "")
	pending := []pendingMsg{{sender: "Bob", content: "hi", timestamp: "t"}}
	out := gdm.renderNotification("ag_alice", gChat.ID, gChat.Name, pending)
	if !strings.Contains(out, "Venue: closed online chat room") {
		t.Errorf("missing chatroom venue hint: %s", out)
	}
	if strings.Contains(out, "Venue: same physical space") {
		t.Errorf("chatroom group must not include colocated hint: %s", out)
	}

	// Colocated venue should yield the co-presence hint.
	gCo, _ := gdm.Create("Co", []string{"ag_alice", "ag_bob"}, 0, "", GroupDMVenueColocated)
	out = gdm.renderNotification("ag_alice", gCo.ID, gCo.Name, pending)
	if !strings.Contains(out, "Venue: same physical space") {
		t.Errorf("missing colocated venue hint: %s", out)
	}
	if strings.Contains(out, "Venue: closed online chat room") {
		t.Errorf("colocated group must not include chatroom hint: %s", out)
	}
}

func TestGroupDMManager_LoadNormalizesLegacyStyle(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("APPDATA", "")

	mgr := newTestManager(t)
	mgr.mu.Lock()
	mgr.agents["ag_alice"] = &Agent{ID: "ag_alice", Name: "Alice", Tool: "claude"}
	mgr.agents["ag_bob"] = &Agent{ID: "ag_bob", Name: "Bob", Tool: "claude"}
	mgr.mu.Unlock()

	// Write a legacy groups.json without the style field.
	dir := groupdmsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `[{"id":"gd_legacy","name":"Legacy","members":[{"agentId":"ag_alice","agentName":"Alice"},{"agentId":"ag_bob","agentName":"Bob"}],"cooldown":0,"createdAt":"2025-01-01T00:00:00Z","updatedAt":"2025-01-01T00:00:00Z"}]`
	if err := os.WriteFile(filepath.Join(dir, "groups.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	gdm := NewGroupDMManager(mgr, mgr.logger)
	g, ok := gdm.Get("gd_legacy")
	if !ok {
		t.Fatal("expected legacy group to be loaded")
	}
	if g.Style != GroupDMStyleEfficient {
		t.Errorf("legacy style = %q, want %q", g.Style, GroupDMStyleEfficient)
	}
}
