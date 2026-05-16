package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

// taskFixture seeds a Manager with a minimal agent record (in-memory
// map + DB row) so every public Task helper passes its requireLiveAgent
// guard. Mirrors memoryIOFixture / personaFixture but tailored to
// avoid the persona/memory side effects those carry.
func taskFixture(t *testing.T, agentIDs ...string) *Manager {
	t.Helper()
	mgr := newTestManager(t)
	for _, id := range agentIDs {
		a := &Agent{ID: id, Name: id, Tool: "claude"}
		mgr.mu.Lock()
		mgr.agents[id] = a
		mgr.mu.Unlock()
		if err := mgr.store.Upsert(a); err != nil {
			t.Fatalf("seed agent %s: %v", id, err)
		}
	}
	return mgr
}

// TestManagerTask_CRUD covers the full lifecycle through the cutover
// API: create returns a v0-shaped Task with ETag, list returns it,
// update with stale etag is rejected, update with the live etag flips
// status from "open" to "done", and delete removes it. The test pins
// the v0↔v1 status translation in both directions.
func TestManagerTask_CRUD(t *testing.T) {
	m := taskFixture(t, "ag_task")
	ctx := context.Background()

	// Create.
	created, err := m.CreateTask(ctx, "ag_task", TaskCreateParams{Title: "  ship release  "})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if created.Title != "ship release" {
		t.Errorf("title = %q (whitespace not trimmed)", created.Title)
	}
	if created.Status != "open" {
		t.Errorf("status = %q, want open (v0 vocabulary)", created.Status)
	}
	if created.ETag == "" {
		t.Errorf("create did not stamp etag: %+v", created)
	}

	// List.
	list, err := m.ListTasks(ctx, "ag_task")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Errorf("list = %+v", list)
	}

	// Update with stale etag fails.
	doneStr := "done"
	if _, err := m.UpdateTask(ctx, "ag_task", created.ID, "etag-bogus", TaskUpdateParams{Status: &doneStr}); !errors.Is(err, ErrTaskETagMismatch) {
		t.Errorf("stale etag: got %v, want ErrTaskETagMismatch", err)
	}

	// Update with live etag, flip to done.
	updated, err := m.UpdateTask(ctx, "ag_task", created.ID, created.ETag, TaskUpdateParams{Status: &doneStr})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Status != "done" {
		t.Errorf("status = %q, want done", updated.Status)
	}
	if updated.ETag == created.ETag {
		t.Errorf("etag should change after update")
	}

	// Update with bogus v0 status surfaces ErrInvalidTaskStatus.
	bogus := "snoozed"
	if _, err := m.UpdateTask(ctx, "ag_task", created.ID, updated.ETag, TaskUpdateParams{Status: &bogus}); !errors.Is(err, ErrInvalidTaskStatus) {
		t.Errorf("invalid status: got %v, want ErrInvalidTaskStatus", err)
	}

	// Empty title rejected.
	empty := ""
	if _, err := m.UpdateTask(ctx, "ag_task", created.ID, updated.ETag, TaskUpdateParams{Title: &empty}); err == nil {
		t.Fatal("expected empty-title rejection")
	}

	// Delete with live etag.
	if err := m.DeleteTask(ctx, "ag_task", created.ID, updated.ETag); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// List now empty.
	list2, err := m.ListTasks(ctx, "ag_task")
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(list2) != 0 {
		t.Errorf("list after delete: %+v", list2)
	}
	// GetTask on deleted task → ErrTaskNotFound.
	if _, err := m.GetTask(ctx, "ag_task", created.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("get after delete: %v", err)
	}
}

// TestManagerTask_CrossAgentProbeProtection asserts that a task id
// belonging to agent A is invisible to agent B's API surface. A
// mismatched owner surfaces as ErrTaskNotFound (not "wrong agent")
// so the handler can't be used to probe the existence of tasks under
// other agents.
func TestManagerTask_CrossAgentProbeProtection(t *testing.T) {
	m := taskFixture(t, "ag_alice", "ag_bob")
	ctx := context.Background()

	tk, err := m.CreateTask(ctx, "ag_alice", TaskCreateParams{Title: "alice's task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Bob asking for Alice's task: must return ErrTaskNotFound.
	if _, err := m.GetTask(ctx, "ag_bob", tk.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("cross-agent get: got %v, want ErrTaskNotFound", err)
	}
	doneStr := "done"
	if _, err := m.UpdateTask(ctx, "ag_bob", tk.ID, "", TaskUpdateParams{Status: &doneStr}); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("cross-agent update: got %v, want ErrTaskNotFound", err)
	}
	if err := m.DeleteTask(ctx, "ag_bob", tk.ID, ""); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("cross-agent delete: got %v, want ErrTaskNotFound", err)
	}
}

// TestManagerTask_ActiveTasksSummary verifies the prompt-injection
// summary includes only "open" tasks and escapes backticks in titles
// so a hostile title can't break out of the markdown code fence.
func TestManagerTask_ActiveTasksSummary(t *testing.T) {
	m := taskFixture(t, "ag_task")
	ctx := context.Background()

	if _, err := m.CreateTask(ctx, "ag_task", TaskCreateParams{Title: "first ` open"}); err != nil {
		t.Fatalf("create open1: %v", err)
	}
	if _, err := m.CreateTask(ctx, "ag_task", TaskCreateParams{Title: "second open"}); err != nil {
		t.Fatalf("create open2: %v", err)
	}
	doneTask, err := m.CreateTask(ctx, "ag_task", TaskCreateParams{Title: "completed"})
	if err != nil {
		t.Fatalf("create done: %v", err)
	}
	doneStr := "done"
	if _, err := m.UpdateTask(ctx, "ag_task", doneTask.ID, doneTask.ETag, TaskUpdateParams{Status: &doneStr}); err != nil {
		t.Fatalf("flip to done: %v", err)
	}

	out := m.ActiveTasksSummary(ctx, "ag_task")
	if !strings.Contains(out, "first ' open") {
		t.Errorf("backtick not escaped in title: %q", out)
	}
	if !strings.Contains(out, "second open") {
		t.Errorf("second open missing: %q", out)
	}
	if strings.Contains(out, "completed") {
		t.Errorf("done task leaked into active summary: %q", out)
	}
	if !strings.Contains(out, "## Active Todos") {
		t.Errorf("missing header: %q", out)
	}
}

// TestManagerTask_StatusFoldThrough confirms that v1-only statuses
// hand-edited into the DB are folded back to the v0 vocabulary on
// the API surface. The Web UI / MCP only knows "open" / "done";
// surfacing "in_progress" / "cancelled" / unknown values would crash
// a client that's not coded for them. The fold policy:
//
//	pending      → open
//	in_progress  → open
//	done         → done
//	cancelled    → done
func TestManagerTask_StatusFoldThrough(t *testing.T) {
	m := taskFixture(t, "ag_fold")
	ctx := context.Background()

	cases := []struct {
		v1Status string
		wantV0   string
	}{
		{"pending", "open"},
		{"in_progress", "open"},
		{"done", "done"},
		{"cancelled", "done"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.v1Status, func(t *testing.T) {
			// Insert directly via the store so the v1-only statuses
			// reach the DB without going through the v0 translator
			// (which only knows "open" / "done"). This simulates a
			// hand-edited DB row or a future caller that uses the
			// extended vocabulary.
			r, err := m.Store().CreateAgentTask(ctx, &store.AgentTaskRecord{
				ID: generateTaskID(), AgentID: "ag_fold", Title: "x", Status: c.v1Status,
			}, store.AgentTaskInsertOptions{})
			if err != nil {
				t.Fatalf("seed v1 status %q: %v", c.v1Status, err)
			}
			got, err := m.GetTask(ctx, "ag_fold", r.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.Status != c.wantV0 {
				t.Errorf("v1=%q → API=%q, want %q", c.v1Status, got.Status, c.wantV0)
			}
		})
	}
}

// TestManagerTask_UnknownAgent surfaces ErrAgentNotFound from every
// public Task helper so the HTTP handler can return a clean 404.
func TestManagerTask_UnknownAgent(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()

	if _, err := m.ListTasks(ctx, "missing"); !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("list: got %v, want ErrAgentNotFound", err)
	}
	if _, err := m.CreateTask(ctx, "missing", TaskCreateParams{Title: "x"}); !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("create: got %v, want ErrAgentNotFound", err)
	}
	if _, err := m.GetTask(ctx, "missing", "task_x"); !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("get: got %v, want ErrAgentNotFound", err)
	}
	doneStr := "done"
	if _, err := m.UpdateTask(ctx, "missing", "task_x", "", TaskUpdateParams{Status: &doneStr}); !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("update: got %v, want ErrAgentNotFound", err)
	}
	if err := m.DeleteTask(ctx, "missing", "task_x", ""); !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("delete: got %v, want ErrAgentNotFound", err)
	}
}
