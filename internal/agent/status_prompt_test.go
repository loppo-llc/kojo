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

// TestBuildSystemPrompt_StatusMissing: with no status.json on disk the
// prompt must still carry the "# Your Status" section, show the default
// template, and nudge the agent to materialise the file.
func TestBuildSystemPrompt_StatusMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a := &Agent{ID: "ag_test_status_missing"}
	prompt := buildSystemPrompt(a, newQuietLogger(), "", nil, false)

	for _, want := range []string{
		"# Your Status",
		"The file does not exist yet",
		`"sleepiness": "awake"`, // default template body
		filepath.Join(agentDir(a.ID), "status.json"),
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(prompt, "Last updated:") {
		t.Error("missing file must not carry a Last updated line")
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
	if strings.Contains(prompt, "The file does not exist yet") {
		t.Error("present file must not show the missing-file nudge")
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
