package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newQuietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestBuildSystemPrompt_StatusMissing: with no status.json on disk (or an
// empty `{}` object) the whole "# Your Status" section — header included —
// must be omitted from the prompt. There is no state yet to remind the
// agent of.
func TestBuildSystemPrompt_StatusMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a := &Agent{ID: "ag_test_status_missing"}
	prompt := buildSystemPrompt(a, newQuietLogger(), "", nil, false)

	if strings.Contains(prompt, "# Your Status") {
		t.Error("prompt must omit the status section when status.json is missing")
	}
	if strings.Contains(prompt, "Last updated:") {
		t.Error("missing file must not carry a Last updated line")
	}
}

// TestBuildSystemPrompt_StatusEmptyObject: an on-disk status.json holding
// just `{}` (the seeded default) must be treated the same as a missing
// file — the whole status section is omitted.
func TestBuildSystemPrompt_StatusEmptyObject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a := &Agent{ID: "ag_test_status_empty"}
	dir := agentDir(a.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte(DefaultStatusContent), 0o644); err != nil {
		t.Fatal(err)
	}

	prompt := buildSystemPrompt(a, newQuietLogger(), "", nil, false)
	if strings.Contains(prompt, "# Your Status") {
		t.Error("prompt must omit the status section when status.json is an empty object")
	}
}

// TestIsEmptyStatusContent covers the edge cases around what counts as
// "no state yet": whitespace-only objects, nested-but-populated objects,
// and malformed content the agent may have left half-written.
func TestIsEmptyStatusContent(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"blank", "", true},
		{"whitespace only", "   \n\t", true},
		{"compact empty object", "{}", true},
		{"spaced empty object", "{ }", true},
		{"newline-padded empty object", "{\n\n}\n", true},
		{"nested non-empty object", `{"a":{}}`, false},
		{"populated object", `{"mood":"good"}`, false},
		{"non-object JSON", `["a","b"]`, false},
		{"invalid JSON", `{"mood":`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isEmptyStatusContent(c.content); got != c.want {
				t.Errorf("isEmptyStatusContent(%q) = %v, want %v", c.content, got, c.want)
			}
		})
	}
}

// TestBuildSystemPrompt_StatusPresent: an on-disk status.json is injected
// verbatim with its mtime as the Last updated line, and the status block
// sits at the very end of the prompt (cache-prefix invariant: the most
// frequently edited injection must come last).
func TestBuildSystemPrompt_StatusPresent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a := &Agent{ID: "ag_test_status_present", Persona: "test persona"}
	dir := agentDir(a.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "{\n  \"mood\": \"irritated\",\n  \"custom_key\": \"anything\"\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	prompt := buildSystemPrompt(a, newQuietLogger(), "", nil, false)

	for _, want := range []string{
		"# Your Status",
		"Last updated:",
		`"mood": "irritated"`,
		`"custom_key": "anything"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// Status must come after the persona section so status edits leave
	// the persona-and-everything-before prefix cacheable.
	statusIdx := strings.Index(prompt, "# Your Status")
	personaIdx := strings.Index(prompt, "# Who You Are")
	if personaIdx == -1 || statusIdx < personaIdx {
		t.Errorf("status block must follow persona: statusIdx=%d personaIdx=%d", statusIdx, personaIdx)
	}
}

// TestEnsureAgentDir_SeedsStatus: new agent dirs start with the default
// status.json so the status block is live from the first turn.
func TestEnsureAgentDir_SeedsStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a := &Agent{ID: "ag_test_status_seed", Name: "seed"}
	if err := ensureAgentDir(a); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(agentDir(a.ID), "status.json"))
	if err != nil {
		t.Fatalf("status.json not seeded: %v", err)
	}
	if string(data) != DefaultStatusContent {
		t.Errorf("seeded content mismatch:\n%s", data)
	}

	// Re-running must not clobber an edited file.
	edited := `{"mood":"custom"}`
	if err := os.WriteFile(filepath.Join(agentDir(a.ID), "status.json"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureAgentDir(a); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(agentDir(a.ID), "status.json"))
	if string(data) != edited {
		t.Errorf("ensureAgentDir clobbered an existing status.json: %s", data)
	}
}

// TestEnsureAgentDir_SeedsMission: a task-first create materialises the
// transient Mission into MEMORY.md as a "## Mission" section and clears
// the field so it never reaches settings_json.
func TestEnsureAgentDir_SeedsMission(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a := &Agent{ID: "ag_test_mission_seed", Name: "seed", Mission: "  Watch the CI pipeline and report failures.  "}
	if err := ensureAgentDir(a); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(agentDir(a.ID), "MEMORY.md"))
	if err != nil {
		t.Fatalf("MEMORY.md not seeded: %v", err)
	}
	for _, want := range []string{"# seed's Memory", "## Mission", "Watch the CI pipeline and report failures."} {
		if !strings.Contains(string(data), want) {
			t.Errorf("MEMORY.md missing %q:\n%s", want, data)
		}
	}
	if a.Mission != "" {
		t.Errorf("Mission not cleared after materialisation: %q", a.Mission)
	}

	// An existing MEMORY.md must never be clobbered by a later Mission.
	edited := "# seed's Memory\n\ncustom content\n"
	if err := os.WriteFile(filepath.Join(agentDir(a.ID), "MEMORY.md"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	a.Mission = "new mission"
	if err := ensureAgentDir(a); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(agentDir(a.ID), "MEMORY.md"))
	if string(data) != edited {
		t.Errorf("ensureAgentDir clobbered an existing MEMORY.md: %s", data)
	}
	if a.Mission != "" {
		t.Errorf("Mission not cleared on the existing-file path: %q", a.Mission)
	}
}

// TestEnsureAgentDir_NoMissionKeepsDefaultSeed: without a Mission the
// seed is exactly the historical default (no stray Mission header).
func TestEnsureAgentDir_NoMissionKeepsDefaultSeed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a := &Agent{ID: "ag_test_mission_none", Name: "plain"}
	if err := ensureAgentDir(a); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(agentDir(a.ID), "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "## Mission") {
		t.Errorf("default seed must not contain a Mission section:\n%s", data)
	}
}
