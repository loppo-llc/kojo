package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSyncGuides_WritesAllGuideFiles verifies the embedded guide docs
// land on disk under <configdir>/guide/ and that a re-sync after local
// tampering restores the embedded content (upgrade / drift heal).
func TestSyncGuides_WritesAllGuideFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", "")

	SyncGuides(testLogger())

	want := []string{"groupdm.md", "todos.md", "credentials.md", "memory-conventions.md", "attachments.md", "background-sessions.md"}
	for _, name := range want {
		p := filepath.Join(GuideDir(), name)
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("guide %s not synced: %v", name, err)
		}
		if len(body) == 0 {
			t.Errorf("guide %s is empty", name)
		}
	}

	// Tamper, then re-sync: content must be restored.
	p := filepath.Join(GuideDir(), "todos.md")
	if err := os.WriteFile(p, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	SyncGuides(testLogger())
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if string(body) == "tampered" {
		t.Errorf("re-sync did not overwrite tampered guide")
	}
	if !strings.Contains(string(body), "Persistent Todo API") {
		t.Errorf("restored todos.md missing expected content")
	}
}

// TestBuildSystemPrompt_GuidesIndex covers the compact "## kojo Guides"
// section: pointers replace the inlined API docs, and the credentials
// line only appears when the agent actually has stored credentials.
func TestBuildSystemPrompt_GuidesIndex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	a := &Agent{ID: "ag_guides"}
	apiBase := "http://127.0.0.1:8080"

	t.Run("without credentials", func(t *testing.T) {
		prompt := buildSystemPrompt(a, testLogger(), apiBase, nil, false)
		if strings.Contains(prompt, "credentials.md") {
			t.Errorf("credentials pointer shown without stored credentials")
		}
		for _, want := range []string{
			"## kojo Guides",
			filepath.Join(GuideDir(), "groupdm.md"),
			filepath.Join(GuideDir(), "todos.md"),
			filepath.Join(GuideDir(), "attachments.md"),
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("prompt missing guide fragment %q", want)
			}
		}
		// The verbose inlined API docs must be gone.
		for _, stale := range []string{
			"Create group: `curl",
			"Create todo: `curl",
			"## Persistent Todo API",
			"import json, os, urllib.request",
		} {
			if strings.Contains(prompt, stale) {
				t.Errorf("prompt still inlines verbose doc fragment %q", stale)
			}
		}
	})

	t.Run("with credentials", func(t *testing.T) {
		prompt := buildSystemPrompt(a, testLogger(), apiBase, nil, true)
		if !strings.Contains(prompt, filepath.Join(GuideDir(), "credentials.md")) {
			t.Errorf("credentials pointer missing despite stored credentials")
		}
		if !strings.Contains(prompt, "NEVER display passwords or TOTP secrets") {
			t.Errorf("short credential warning missing")
		}
	})
}

// TestBuildSystemPrompt_DisabledInjections verifies the per-agent
// injection checklist: disabled sections are skipped in the system
// prompt while everything else stays.
func TestBuildSystemPrompt_DisabledInjections(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	apiBase := "http://127.0.0.1:8080"
	id := "ag_disable"
	if err := os.MkdirAll(agentDir(id), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Seed user.md, MEMORY.md, status.json so their sections would render.
	if err := os.WriteFile(filepath.Join(agentDir(id), "user.md"), []byte("USER_CANARY"), 0o644); err != nil {
		t.Fatalf("user.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir(id), "MEMORY.md"), []byte("MEMORY_CANARY"), 0o644); err != nil {
		t.Fatalf("MEMORY.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir(id), "status.json"), []byte(`{"mood":"STATUS_CANARY"}`), 0o644); err != nil {
		t.Fatalf("status.json: %v", err)
	}

	enabled := &Agent{ID: id}
	base := buildSystemPrompt(enabled, testLogger(), apiBase, nil, true)
	for _, want := range []string{"USER_CANARY", "MEMORY_CANARY", "STATUS_CANARY", "## Group DM", "todos.md", "credentials.md", "attachments.md"} {
		if !strings.Contains(base, want) {
			t.Fatalf("baseline prompt missing %q", want)
		}
	}

	disabled := &Agent{ID: id, DisabledInjections: []string{
		InjectionUserContext, InjectionMemoryMD, InjectionStatus,
		InjectionGroupDM, InjectionTodoAPI, InjectionCredentials,
		InjectionAttachments,
	}}
	prompt := buildSystemPrompt(disabled, testLogger(), apiBase, nil, true)
	for _, gone := range []string{"USER_CANARY", "MEMORY_CANARY", "STATUS_CANARY", "## Group DM", "todos.md", "credentials.md", "attachments.md"} {
		if strings.Contains(prompt, gone) {
			t.Errorf("disabled section still present: %q", gone)
		}
	}
	// memory_md disabled falls back to the Read instruction.
	if !strings.Contains(prompt, "Read "+filepath.Join(agentDir(id), "MEMORY.md")) {
		t.Errorf("memory_md disabled should keep the Read fallback")
	}
	// Mandatory memory-write rule must survive regardless of toggles.
	if !strings.Contains(prompt, "Memory Write — MANDATORY") {
		t.Errorf("mandatory memory-write rule missing")
	}
}

// TestBuildVolatileContext_DisabledInjections verifies the volatile
// per-turn block honors diary_notes / memory_search toggles.
func TestBuildVolatileContext_DisabledInjections(t *testing.T) {
	m := newTestManager(t)
	id := "ag_vol_disable"
	if err := os.MkdirAll(filepath.Join(agentDir(id), "memory"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir(id), "memory", recentSummaryFile), []byte("DIARY_CANARY"), 0o644); err != nil {
		t.Fatalf("recent.md: %v", err)
	}
	m.agents[id] = &Agent{ID: id, DisabledInjections: []string{InjectionDiaryNotes, InjectionMemorySearch}}

	out := m.BuildVolatileContext(t.Context(), id, "SEARCH_CANARY")
	if strings.Contains(out, "DIARY_CANARY") {
		t.Errorf("diary_notes disabled but diary summary injected: %q", out)
	}
	if strings.Contains(out, "SEARCH_CANARY") {
		t.Errorf("memory_search disabled but query context injected: %q", out)
	}
	if !strings.Contains(out, "now: ") {
		t.Errorf("volatile context lost the wall-clock line")
	}

	// Sanity: with no toggles both blocks appear.
	m.agents[id] = &Agent{ID: id}
	out = m.BuildVolatileContext(t.Context(), id, "SEARCH_CANARY")
	if !strings.Contains(out, "DIARY_CANARY") || !strings.Contains(out, "SEARCH_CANARY") {
		t.Errorf("enabled sections missing from volatile context: %q", out)
	}
}

// TestValidateDisabledInjections rejects unknown keys.
func TestValidateDisabledInjections(t *testing.T) {
	if err := ValidateDisabledInjections([]string{InjectionStatus, InjectionGroupDM}); err != nil {
		t.Errorf("valid keys rejected: %v", err)
	}
	if err := ValidateDisabledInjections([]string{"bogus_key"}); err == nil {
		t.Errorf("unknown key accepted")
	}
}

// TestBuildSystemPrompt_BackgroundSessionsGuide: the background-sessions
// pointer is claude-only (only ClaudeBackend lingers keyed sessions).
func TestBuildSystemPrompt_BackgroundSessionsGuide(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	apiBase := "http://127.0.0.1:8080"
	want := filepath.Join(GuideDir(), "background-sessions.md")

	claude := &Agent{ID: "ag_bg", Tool: ToolClaude}
	if p := buildSystemPrompt(claude, testLogger(), apiBase, nil, false); !strings.Contains(p, want) {
		t.Errorf("claude prompt missing background-sessions guide pointer")
	}
	codex := &Agent{ID: "ag_bg", Tool: ToolCodex}
	if p := buildSystemPrompt(codex, testLogger(), apiBase, nil, false); strings.Contains(p, want) {
		t.Errorf("codex prompt must not point at background-sessions guide")
	}
	if p := buildSystemPrompt(claude, testLogger(), "", nil, false); strings.Contains(p, want) {
		t.Errorf("prompt without API base must not point at background-sessions guide")
	}
}
