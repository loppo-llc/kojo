package agent

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestAgentIDToUUID(t *testing.T) {
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

	t.Run("valid UUID format", func(t *testing.T) {
		id := agentIDToUUID("ag_8cf247118ad856e8")
		if !uuidRe.MatchString(id) {
			t.Errorf("not a valid UUID format: %s", id)
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		a := agentIDToUUID("ag_8cf247118ad856e8")
		b := agentIDToUUID("ag_8cf247118ad856e8")
		if a != b {
			t.Errorf("not deterministic: %s != %s", a, b)
		}
	})

	t.Run("different IDs produce different UUIDs", func(t *testing.T) {
		a := agentIDToUUID("ag_1111111111111111")
		b := agentIDToUUID("ag_2222222222222222")
		if a == b {
			t.Errorf("collision: %s == %s", a, b)
		}
	})

	t.Run("UUID v3 version and variant bits", func(t *testing.T) {
		id := agentIDToUUID("ag_8cf247118ad856e8")
		// Version 3: third group starts with '3'
		if id[14] != '3' {
			t.Errorf("version nibble not 3: got %c in %s", id[14], id)
		}
		// Variant RFC4122: fourth group starts with 8, 9, a, or b
		c := id[19]
		if c != '8' && c != '9' && c != 'a' && c != 'b' {
			t.Errorf("variant nibble not 8/9/a/b: got %c in %s", c, id)
		}
	})
}

func TestHasExistingSession(t *testing.T) {
	t.Run("returns false for empty directory", func(t *testing.T) {
		dir := t.TempDir()
		if hasExistingSession(dir) {
			t.Error("expected false for empty dir")
		}
	})

	t.Run("returns false for nonexistent directory", func(t *testing.T) {
		if hasExistingSession("/nonexistent/path/12345") {
			t.Error("expected false for nonexistent dir")
		}
	})

	t.Run("returns true when session JSONL exists", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		// Use a path with dots and underscores to verify encoding
		agentPath := filepath.Join(home, ".config", "kojo", "agents", "ag_test123")
		os.MkdirAll(agentPath, 0o755)

		absPath, _ := filepath.Abs(agentPath)
		// Path encoding: filepath.Separator, ".", "_" all become "-"
		encodedPath := strings.NewReplacer(string(filepath.Separator), "-", ".", "-", "_", "-").Replace(absPath)

		projectDir := filepath.Join(home, ".claude", "projects", encodedPath)
		os.MkdirAll(projectDir, 0o755)
		os.WriteFile(filepath.Join(projectDir, "test-session.jsonl"), []byte("{}"), 0o644)

		if !hasExistingSession(agentPath) {
			t.Error("expected true when session JSONL exists")
		}
	})

	t.Run("respects CLAUDE_CONFIG_DIR", func(t *testing.T) {
		home := t.TempDir()
		configDir := filepath.Join(home, "custom-claude")
		t.Setenv("HOME", home)
		t.Setenv("CLAUDE_CONFIG_DIR", configDir)

		agentPath := filepath.Join(home, "agents", "test")
		os.MkdirAll(agentPath, 0o755)

		absPath, _ := filepath.Abs(agentPath)
		encodedPath := strings.NewReplacer(string(filepath.Separator), "-", ".", "-", "_", "-").Replace(absPath)

		// Create session in custom config dir, NOT in ~/.claude
		projectDir := filepath.Join(configDir, "projects", encodedPath)
		os.MkdirAll(projectDir, 0o755)
		os.WriteFile(filepath.Join(projectDir, "session.jsonl"), []byte("{}"), 0o644)

		if !hasExistingSession(agentPath) {
			t.Error("expected true when CLAUDE_CONFIG_DIR is set and session exists there")
		}
	})
}

// writeSessionJSONL is a test helper that writes session entries as JSONL.
func writeSessionJSONL(t *testing.T, path string, entries []map[string]any) {
	t.Helper()
	var lines []string
	for _, e := range entries {
		data, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(data))
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// setupRecoverTest sets up a temp environment for recoverFromSession tests.
// Returns the agent ID and a function to write session JSONL.
func setupRecoverTest(t *testing.T) (string, func([]map[string]any)) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	agentID := "ag_test_recover"
	aDir := filepath.Join(home, ".config", "kojo", "agents", agentID)
	os.MkdirAll(aDir, 0o755)

	// Override agentsDir by setting HOME; agentDir uses agentsDir internally.
	absDir, _ := filepath.Abs(aDir)
	encoded := strings.NewReplacer(
		string(filepath.Separator), "-",
		".", "-",
		"_", "-",
	).Replace(absDir)
	projectDir := filepath.Join(home, ".claude", "projects", encoded)

	sessionFile := filepath.Join(projectDir, "session.jsonl")
	writer := func(entries []map[string]any) {
		writeSessionJSONL(t, sessionFile, entries)
	}

	return agentID, writer
}

func TestRecoverFromSession(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	t.Run("returns empty for nonexistent agent", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		got := recoverFromSession("ag_nonexistent", "", logger)
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("recovers text after user message", func(t *testing.T) {
		agentID, write := setupRecoverTest(t)
		write([]map[string]any{
			{"type": "user", "message": map[string]any{
				"role":    "user",
				"content": "hello",
			}},
			{"type": "assistant", "message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "Hi there!"},
				},
			}},
		})

		got := recoverFromSession(agentID, "", logger)
		if got != "Hi there!" {
			t.Errorf("expected %q, got %q", "Hi there!", got)
		}
	})

	t.Run("skips tool_result user entries", func(t *testing.T) {
		agentID, write := setupRecoverTest(t)
		write([]map[string]any{
			// Real user message
			{"type": "user", "message": map[string]any{
				"role":    "user",
				"content": "do something",
			}},
			// Assistant with text + tool_use
			{"type": "assistant", "message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "Sure, let me "},
				},
			}},
			{"type": "assistant", "message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "name": "Edit"},
				},
			}},
			// Tool result (user type but should NOT reset)
			{"type": "user", "message": map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
				},
			}},
			// Assistant continuation after tool
			{"type": "assistant", "message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "Done!"},
				},
			}},
		})

		got := recoverFromSession(agentID, "", logger)
		if got != "Sure, let me Done!" {
			t.Errorf("expected %q, got %q", "Sure, let me Done!", got)
		}
	})

	t.Run("uses last real user message", func(t *testing.T) {
		agentID, write := setupRecoverTest(t)
		write([]map[string]any{
			{"type": "user", "message": map[string]any{
				"role": "user", "content": "first",
			}},
			{"type": "assistant", "message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "old response"},
				},
			}},
			{"type": "user", "message": map[string]any{
				"role": "user", "content": "second",
			}},
			{"type": "assistant", "message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "new response"},
				},
			}},
		})

		got := recoverFromSession(agentID, "", logger)
		if got != "new response" {
			t.Errorf("expected %q, got %q", "new response", got)
		}
	})

	t.Run("returns empty when no assistant text after user", func(t *testing.T) {
		agentID, write := setupRecoverTest(t)
		write([]map[string]any{
			{"type": "user", "message": map[string]any{
				"role": "user", "content": "hello",
			}},
			// Only tool_use, no text
			{"type": "assistant", "message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "name": "Read"},
				},
			}},
		})

		got := recoverFromSession(agentID, "", logger)
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("ignores thinking blocks", func(t *testing.T) {
		agentID, write := setupRecoverTest(t)
		write([]map[string]any{
			{"type": "user", "message": map[string]any{
				"role": "user", "content": "think about it",
			}},
			{"type": "assistant", "message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "thinking", "thinking": "let me think..."},
				},
			}},
			{"type": "assistant", "message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "Here is my answer"},
				},
			}},
		})

		got := recoverFromSession(agentID, "", logger)
		if got != "Here is my answer" {
			t.Errorf("expected %q, got %q", "Here is my answer", got)
		}
	})

	t.Run("concatenates multi-turn text", func(t *testing.T) {
		agentID, write := setupRecoverTest(t)
		write([]map[string]any{
			{"type": "user", "message": map[string]any{
				"role": "user", "content": "go",
			}},
			{"type": "assistant", "message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "Part 1. "},
				},
			}},
			{"type": "assistant", "message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "name": "Edit"},
				},
			}},
			{"type": "user", "message": map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
				},
			}},
			{"type": "assistant", "message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "Part 2."},
				},
			}},
		})

		got := recoverFromSession(agentID, "", logger)
		if got != "Part 1. Part 2." {
			t.Errorf("expected %q, got %q", "Part 1. Part 2.", got)
		}
	})
}

func TestFindSessionFile(t *testing.T) {
	t.Run("prefers exact sessionID match", func(t *testing.T) {
		dir := t.TempDir()
		// Create two session files
		os.WriteFile(filepath.Join(dir, "old-session.jsonl"), []byte("{}"), 0o644)
		target := filepath.Join(dir, "target-id.jsonl")
		os.WriteFile(target, []byte("{}"), 0o644)

		got := findSessionFile(dir, "target-id")
		if got != target {
			t.Errorf("expected %q, got %q", target, got)
		}
	})

	t.Run("returns empty when sessionID not found", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "old.jsonl"), []byte("{}"), 0o644)
		os.WriteFile(filepath.Join(dir, "newer.jsonl"), []byte("{}"), 0o644)

		got := findSessionFile(dir, "nonexistent")
		if got != "" {
			t.Errorf("expected empty for nonexistent sessionID, got %q", got)
		}
	})

	t.Run("falls back to newest when sessionID empty", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "old.jsonl"), []byte("{}"), 0o644)
		os.WriteFile(filepath.Join(dir, "newer.jsonl"), []byte("{}"), 0o644)

		got := findSessionFile(dir, "")
		if got == "" {
			t.Error("expected a file, got empty")
		}
	})

	t.Run("returns empty for nonexistent dir", func(t *testing.T) {
		got := findSessionFile("/nonexistent/dir", "")
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
}

func TestRecoverFromSession_scannerError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create a session file with a line exceeding 2MB scanner buffer.
	// recoverFromSession should return "" instead of partial data.
	agentID, write := setupRecoverTest(t)
	_ = write

	home := os.Getenv("HOME")
	aDir := filepath.Join(home, ".config", "kojo", "agents", agentID)
	absDir, _ := filepath.Abs(aDir)
	encoded := strings.NewReplacer(
		string(filepath.Separator), "-",
		".", "-",
		"_", "-",
	).Replace(absDir)
	projectDir := filepath.Join(home, ".claude", "projects", encoded)
	sessionFile := filepath.Join(projectDir, "session.jsonl")

	// Write a valid user message, then a huge line that exceeds buffer
	userLine := `{"type":"user","message":{"role":"user","content":"hello"}}`
	assistantLine := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"old text"}]}}`
	hugeLine := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("x", 3*1024*1024) + `"}]}}`

	os.MkdirAll(filepath.Dir(sessionFile), 0o755)
	os.WriteFile(sessionFile, []byte(userLine+"\n"+assistantLine+"\n"+hugeLine+"\n"), 0o644)

	got := recoverFromSession(agentID, "", logger)
	if got != "" {
		t.Errorf("expected empty on scanner error, got %d chars", len(got))
	}
}
