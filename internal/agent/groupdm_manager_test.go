package agent

import (
	"errors"
	"testing"
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

	g, err := gdm.Create("Test Group", []string{"ag_alice", "ag_bob"}, 0)
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

	g, err := gdm.Create("", []string{"ag_alice", "ag_bob"}, 0)
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

	_, err := gdm.Create("Solo", []string{"ag_alice"}, 0)
	if !errors.Is(err, ErrGroupTooFew) {
		t.Errorf("expected ErrGroupTooFew, got %v", err)
	}
}

func TestGroupDMManager_CreateNonexistentAgent(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	_, err := gdm.Create("Bad", []string{"ag_alice", "ag_nonexistent"}, 0)
	if !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("expected ErrAgentNotFound, got %v", err)
	}
}

func TestGroupDMManager_CreateDeduplicatesMembers(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	_, err := gdm.Create("Dup", []string{"ag_alice", "ag_alice"}, 0)
	if !errors.Is(err, ErrGroupTooFew) {
		t.Errorf("expected ErrGroupTooFew after dedup, got %v", err)
	}
}

func TestGroupDMManager_GetAndList(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	created, _ := gdm.Create("Group1", []string{"ag_alice", "ag_bob"}, 0)

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

	g, _ := gdm.Create("ToDelete", []string{"ag_alice", "ag_bob"}, 0)

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

	g, _ := gdm.Create("CD", []string{"ag_alice", "ag_bob"}, 0)

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

	g, _ := gdm.Create("CD", []string{"ag_alice", "ag_bob"}, 0)

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

	g, _ := gdm.Create("Old Name", []string{"ag_alice", "ag_bob"}, 0)

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

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0)

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

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0)

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

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0)

	_, err := gdm.AddMember(g.ID, "ag_alice", "ag_bob")
	if !errors.Is(err, ErrGroupAlreadyMember) {
		t.Errorf("expected ErrGroupAlreadyMember, got %v", err)
	}
}

func TestGroupDMManager_AddMemberNotMember(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0)

	_, err := gdm.AddMember(g.ID, "ag_charlie", "ag_charlie")
	if !errors.Is(err, ErrGroupNotMember) {
		t.Errorf("expected ErrGroupNotMember, got %v", err)
	}
}

func TestGroupDMManager_LeaveGroup(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob", "ag_charlie"}, 0)

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

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0)

	err := gdm.LeaveGroup(g.ID, "ag_charlie")
	if !errors.Is(err, ErrGroupNotMember) {
		t.Errorf("expected ErrGroupNotMember, got %v", err)
	}
}

func TestGroupDMManager_LeaveGroupDissolvesWithTwoMembers(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0)

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

	gdm.Create("G1", []string{"ag_alice", "ag_bob"}, 0)
	gdm.Create("G2", []string{"ag_bob", "ag_charlie"}, 0)

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

	gdm.Create("G1", []string{"ag_alice", "ag_bob", "ag_charlie"}, 0)
	gdm.Create("G2", []string{"ag_alice", "ag_bob"}, 0)

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

	g, _ := gdm.Create("Group", []string{"ag_alice", "ag_bob"}, 0)

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
