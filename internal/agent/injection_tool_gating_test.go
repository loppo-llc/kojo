package agent

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// toolOnlyInjectionKeysFromWeb parses TOOL_ONLY_INJECTION_KEYS out of the
// web client. The list drives which injection toggles the settings UI
// greys out for custom-bare, so it has to stay in step with the hasTools
// gates in buildSystemPrompt — this test is that guard.
func toolOnlyInjectionKeysFromWeb(t *testing.T) []string {
	t.Helper()
	// repoRoot: internal/agent -> ../..
	path := filepath.Join("..", "..", "web", "src", "lib", "agentApi.ts")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block := regexp.MustCompile(`(?s)TOOL_ONLY_INJECTION_KEYS[^=]*=\s*\[(.*?)\]`).FindSubmatch(src)
	if block == nil {
		t.Fatalf("TOOL_ONLY_INJECTION_KEYS not found in %s", path)
	}
	// The body must be a flat list of quoted literals and nothing else —
	// a comment or a nested expression would make the naive scan below
	// silently drop or invent entries, and this test's whole job is to
	// fail loudly when the two sides drift.
	body := strings.TrimSpace(string(block[1]))
	var keys []string
	for _, item := range strings.Split(body, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		m := regexp.MustCompile(`^"([a-z_]+)"$`).FindStringSubmatch(item)
		if m == nil {
			t.Fatalf("TOOL_ONLY_INJECTION_KEYS entry %q in %s is not a plain quoted key; this test parses the literal and cannot follow expressions", item, path)
		}
		if !ValidInjectionKey(m[1]) {
			t.Fatalf("TOOL_ONLY_INJECTION_KEYS lists unknown injection key %q", m[1])
		}
		keys = append(keys, m[1])
	}
	if len(keys) == 0 {
		t.Fatalf("TOOL_ONLY_INJECTION_KEYS parsed empty from %s", path)
	}
	sort.Strings(keys)
	return keys
}

// TestInjectionToolGating_MatchesWebToolOnlyList verifies that exactly the
// sections the web UI disables for custom-bare are the ones a tool-less
// prompt drops: each listed key must change a tool-capable prompt and
// leave a custom-bare prompt byte-identical.
//
// todo_api is deliberately NOT in the list even though its system-prompt
// pointer is tool-gated: BuildVolatileContext still injects the active
// task summary for custom-bare, so the toggle keeps having an effect. The
// other volatile-only sections are excluded for the same reason.
func TestInjectionToolGating_MatchesWebToolOnlyList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	apiBase := "http://127.0.0.1:8080"
	id := "ag_toolgate"
	if err := os.MkdirAll(agentDir(id), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Seed the files behind the content-bearing sections; without them
	// those toggles would look inert for reasons unrelated to tooling.
	for name, body := range map[string]string{
		"user.md":     "USER_CANARY",
		"MEMORY.md":   "MEMORY_CANARY",
		"status.json": `{"mood":"STATUS_CANARY"}`,
	} {
		if err := os.WriteFile(filepath.Join(agentDir(id), name), []byte(body), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	build := func(tool string, disabled ...string) string {
		a := &Agent{ID: id, Tool: tool, DisabledInjections: disabled}
		return buildSystemPrompt(a, testLogger(), apiBase, nil, true)
	}

	var inertForBare []string
	for _, key := range sortedInjectionKeysForTest() {
		if build(ToolCustomBare) == build(ToolCustomBare, key) {
			inertForBare = append(inertForBare, key)
		}
	}
	sort.Strings(inertForBare)

	want := toolOnlyInjectionKeysFromWeb(t)
	for _, key := range want {
		if build(ToolClaude) == build(ToolClaude, key) {
			t.Errorf("%q is listed as tool-only but does not affect a tool-capable prompt either", key)
		}
	}

	// Sections inert for bare but excluded from the web list are allowed
	// only when they still act elsewhere (todo_api via volatile context).
	// Volatile-context-only sections never touch the system prompt, so
	// they read as inert here for every backend and stay toggleable.
	allowedExtra := map[string]bool{
		InjectionTodoAPI:            true,
		InjectionDiaryNotes:         true,
		InjectionMemorySearch:       true,
		InjectionRecentConversation: true,
		InjectionPersonaAnchor:      true,
		// Wake-turn wording only; never part of the system prompt.
		InjectionModelSwitchInfo: true,
	}
	var unexpected []string
	for _, key := range inertForBare {
		if allowedExtra[key] {
			continue
		}
		unexpected = append(unexpected, key)
	}
	if strings.Join(unexpected, ",") != strings.Join(want, ",") {
		t.Errorf("tool-gated injections drifted: prompt says %v, web TOOL_ONLY_INJECTION_KEYS says %v", unexpected, want)
	}
}

// sortedInjectionKeysForTest returns the injection allowlist in a stable
// order (validInjectionKeys is a map).
func sortedInjectionKeysForTest() []string {
	keys := make([]string, 0, len(validInjectionKeys))
	for k := range validInjectionKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestInjectionTodoAPI_StillReachesBare pins the exclusion above: the
// todo_api pointer is tool-gated in the system prompt, but the active
// task summary rides the volatile context, which every backend gets. So
// the toggle keeps working for custom-bare and the UI must not grey it out.
func TestInjectionTodoAPI_StillReachesBare(t *testing.T) {
	id := "ag_bare_todo"
	m := taskFixture(t, id)
	ctx := context.Background()

	m.mu.Lock()
	m.agents[id].Tool = ToolCustomBare
	m.mu.Unlock()

	if _, err := m.CreateTask(ctx, id, TaskCreateParams{Title: "TODO_CANARY"}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	if got := m.BuildVolatileContext(ctx, id, ""); !strings.Contains(got, "TODO_CANARY") {
		t.Fatalf("todo summary missing from custom-bare volatile context: %q", got)
	}

	m.mu.Lock()
	m.agents[id].DisabledInjections = []string{InjectionTodoAPI}
	m.mu.Unlock()

	if got := m.BuildVolatileContext(ctx, id, ""); strings.Contains(got, "TODO_CANARY") {
		t.Fatalf("disabled todo_api still injected for custom-bare: %q", got)
	}
}
