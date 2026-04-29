package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
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

func TestLastSessionContextTokens(t *testing.T) {
	t.Run("returns untrusted for missing file", func(t *testing.T) {
		n, ok := lastSessionContextTokens("/nonexistent/session.jsonl")
		if ok || n != 0 {
			t.Errorf("expected (0,false) for missing file, got (%d,%v)", n, ok)
		}
	})

	t.Run("returns 0 trusted for file without usage", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "session.jsonl")
		os.WriteFile(path, []byte(`{"type":"user","message":{"content":"hi"}}`+"\n"), 0o644)
		n, ok := lastSessionContextTokens(path)
		if !ok || n != 0 {
			t.Errorf("expected (0,true) for entry without usage, got (%d,%v)", n, ok)
		}
	})

	t.Run("returns last usage sum", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "session.jsonl")
		entries := []string{
			`{"type":"assistant","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":1000,"cache_creation_input_tokens":500}}}`,
			`{"type":"user","message":{"content":"next"}}`,
			`{"type":"assistant","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":2000,"cache_creation_input_tokens":100}}}`,
		}
		os.WriteFile(path, []byte(strings.Join(entries, "\n")+"\n"), 0o644)
		want := 5 + 2000 + 100
		n, ok := lastSessionContextTokens(path)
		if !ok || n != want {
			t.Errorf("expected (%d,true), got (%d,%v)", want, n, ok)
		}
	})

	t.Run("skips malformed lines", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "session.jsonl")
		data := `{"type":"assistant","message":{"usage":{"input_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":0}}}` + "\n" +
			`not valid json` + "\n"
		os.WriteFile(path, []byte(data), 0o644)
		n, ok := lastSessionContextTokens(path)
		if !ok || n != 10 {
			t.Errorf("expected (10,true), got (%d,%v)", n, ok)
		}
	})

	t.Run("tails huge session and picks up only the last usage", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "session.jsonl")

		// Produce a session file that dwarfs sessionTailReadBytes. Every
		// line embeds a distinct "old" usage record; only the final entry
		// carries the value we expect to recover, proving the reader
		// really is bounded to the tail window.
		filler := strings.Repeat("x", 16*1024)
		var b strings.Builder
		for i := 0; i < 200; i++ {
			fmt.Fprintf(&b,
				`{"type":"assistant","message":{"usage":{"input_tokens":1,"cache_read_input_tokens":%d,"cache_creation_input_tokens":0}},"filler":"%s"}`+"\n",
				i, filler)
		}
		final := `{"type":"assistant","message":{"usage":{"input_tokens":9,"cache_read_input_tokens":11,"cache_creation_input_tokens":13}}}` + "\n"
		b.WriteString(final)

		os.WriteFile(path, []byte(b.String()), 0o644)

		n, ok := lastSessionContextTokens(path)
		if !ok || n != 9+11+13 {
			t.Errorf("expected (%d,true) from tail window, got (%d,%v)", 9+11+13, n, ok)
		}
	})

	t.Run("returns untrusted when last usage straddles tail boundary", func(t *testing.T) {
		// Simulate a session whose only usage-bearing line sits far enough
		// before EOF that our tail window starts inside it. The leading
		// partial line gets dropped (as intended), so no usage is recovered
		// from the window — we must surface this as untrusted instead of
		// returning (0, true), which would suppress a legitimate reset.
		dir := t.TempDir()
		path := filepath.Join(dir, "session.jsonl")

		// Single giant usage line (~1.5MiB) with no trailing lines.
		giant := `{"type":"assistant","message":{"usage":{"input_tokens":50000,"cache_read_input_tokens":50000,"cache_creation_input_tokens":50000}},"filler":"` +
			strings.Repeat("x", int(sessionTailReadBytes)+512*1024) + `"}`
		os.WriteFile(path, []byte(giant+"\n"), 0o644)

		n, ok := lastSessionContextTokens(path)
		if ok {
			t.Errorf("expected untrusted when last usage line straddles window, got (%d,true)", n)
		}
	})

}

func TestSessionFileUsable_ResetOverThreshold(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	aDir := filepath.Join(home, ".config", "kojo", "agents", "ag_threshold_test")
	os.MkdirAll(aDir, 0o755)

	absDir, _ := filepath.Abs(aDir)
	encoded := strings.NewReplacer(
		string(filepath.Separator), "-",
		".", "-",
		"_", "-",
	).Replace(absDir)
	projectDir := filepath.Join(home, ".claude", "projects", encoded)
	os.MkdirAll(projectDir, 0o755)

	sessionID := "deadbeef-1234-3abc-8def-000000000001"
	sessionFile := filepath.Join(projectDir, sessionID+".jsonl")

	t.Run("usable below threshold", func(t *testing.T) {
		entry := `{"type":"assistant","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":50000,"cache_creation_input_tokens":1000}}}`
		os.WriteFile(sessionFile, []byte(entry+"\n"), 0o644)
		if !sessionFileUsable(aDir, sessionID, false, "", sessionResetMinIdleDuration, nil) {
			t.Error("expected usable for small context")
		}
		if _, err := os.Stat(sessionFile); err != nil {
			t.Error("expected file to still exist")
		}
	})

	t.Run("not usable and removed when over threshold", func(t *testing.T) {
		entry := `{"type":"assistant","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":160000,"cache_creation_input_tokens":1000}}}`
		os.WriteFile(sessionFile, []byte(entry+"\n"), 0o644)
		// Age past idle window so the reset path actually fires (the
		// active-chat guard is covered in a dedicated subtest).
		old := time.Now().Add(-2 * sessionResetMinIdleDuration)
		if err := os.Chtimes(sessionFile, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		if sessionFileUsable(aDir, sessionID, false, "", sessionResetMinIdleDuration, nil) {
			t.Error("expected not usable for oversized context")
		}
		if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
			t.Error("expected file to be removed after threshold breach")
		}
	})

	t.Run("keeps session when remove fails", func(t *testing.T) {
		// Make the project directory read-only so os.Remove on the
		// session file fails. sessionFileUsable must then fall back to
		// --resume (return true) rather than report the file gone.
		entry := `{"type":"assistant","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":200000,"cache_creation_input_tokens":0}}}`
		os.WriteFile(sessionFile, []byte(entry+"\n"), 0o644)
		old := time.Now().Add(-2 * sessionResetMinIdleDuration)
		if err := os.Chtimes(sessionFile, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}

		if err := os.Chmod(projectDir, 0o555); err != nil {
			t.Fatalf("chmod project dir: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(projectDir, 0o755) })

		if !sessionFileUsable(aDir, sessionID, false, "", sessionResetMinIdleDuration, nil) {
			t.Error("expected fallback to true when remove fails")
		}
		if _, err := os.Stat(sessionFile); err != nil {
			t.Error("expected file to remain when remove fails")
		}
	})

	t.Run("keeps session when over threshold but recently active", func(t *testing.T) {
		// Short interactive turns rack up cache_read fast without writing
		// to MEMORY.md; a mid-conversation reset would drop context the
		// agent can't recover from disk. As long as the session file was
		// touched within sessionResetMinIdleDuration, we defer to
		// claude's own auto-compact instead of pulling the rug out.
		entry := `{"type":"assistant","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":160000,"cache_creation_input_tokens":1000}}}`
		os.WriteFile(sessionFile, []byte(entry+"\n"), 0o644)
		// Default write sets mtime to now — already "active".
		if !sessionFileUsable(aDir, sessionID, false, "", sessionResetMinIdleDuration, nil) {
			t.Error("expected active session to survive threshold")
		}
		if _, err := os.Stat(sessionFile); err != nil {
			t.Error("expected active session file to remain")
		}
	})

	t.Run("resets over threshold once idle window elapsed", func(t *testing.T) {
		entry := `{"type":"assistant","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":160000,"cache_creation_input_tokens":1000}}}`
		os.WriteFile(sessionFile, []byte(entry+"\n"), 0o644)
		// Age the file well past the idle window.
		old := time.Now().Add(-2 * sessionResetMinIdleDuration)
		if err := os.Chtimes(sessionFile, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		if sessionFileUsable(aDir, sessionID, false, "", sessionResetMinIdleDuration, nil) {
			t.Error("expected idle over-threshold session to reset")
		}
		if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
			t.Error("expected idle session file to be removed")
		}
	})

	t.Run("automated trigger bypasses idle guard", func(t *testing.T) {
		// Cron / groupdm / notify fires pass automatedTrigger=true to signal
		// there's no interactive conversation to protect. With a fresh
		// mtime (which would normally block the reset), the session must
		// still be wiped when the token threshold is breached.
		entry := `{"type":"assistant","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":160000,"cache_creation_input_tokens":1000}}}`
		os.WriteFile(sessionFile, []byte(entry+"\n"), 0o644)
		// mtime = now; idle guard would block this for interactive callers.
		if sessionFileUsable(aDir, sessionID, true, "", sessionResetMinIdleDuration, nil) {
			t.Error("expected automated trigger to reset over-threshold session regardless of idle")
		}
		if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
			t.Error("expected file to be removed under automated trigger")
		}
	})

	t.Run("invokes pre-reset summary hook before deleting session", func(t *testing.T) {
		// Regression guard for the "agent didn't write MEMORY.md" failure
		// mode: before we unlink the JSONL, we MUST call the summary hook
		// so recent turns make it into the agent's diary. Skipping this
		// would silently amputate context on every over-threshold reset.
		entry := `{"type":"assistant","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":160000,"cache_creation_input_tokens":1000}}}`
		os.WriteFile(sessionFile, []byte(entry+"\n"), 0o644)
		old := time.Now().Add(-2 * sessionResetMinIdleDuration)
		os.Chtimes(sessionFile, old, old)

		var calledAgent, calledTool string
		orig := preResetSummarize
		preResetSummarize = func(agentID, tool string, _ *slog.Logger) error {
			calledAgent = agentID
			calledTool = tool
			return nil
		}
		t.Cleanup(func() { preResetSummarize = orig })

		if sessionFileUsable(aDir, sessionID, false, "ag_summary_probe", sessionResetMinIdleDuration, nil) {
			t.Error("expected reset to proceed")
		}
		if calledAgent != "ag_summary_probe" {
			t.Errorf("summary hook called with agent=%q, want ag_summary_probe", calledAgent)
		}
		if calledTool != "claude" {
			t.Errorf("summary hook called with tool=%q, want claude", calledTool)
		}
		if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
			t.Error("expected session file removed after successful summary")
		}
	})

	t.Run("aborts reset when summary hook fails", func(t *testing.T) {
		// If we can't capture a summary, deleting the JSONL would discard
		// context with no breadcrumb. Safer to let the session run one
		// more turn (claude's auto-compact still bounds growth) than to
		// silently lose state.
		entry := `{"type":"assistant","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":160000,"cache_creation_input_tokens":1000}}}`
		os.WriteFile(sessionFile, []byte(entry+"\n"), 0o644)
		old := time.Now().Add(-2 * sessionResetMinIdleDuration)
		os.Chtimes(sessionFile, old, old)

		orig := preResetSummarize
		preResetSummarize = func(_, _ string, _ *slog.Logger) error {
			return fmt.Errorf("simulated summary failure")
		}
		t.Cleanup(func() { preResetSummarize = orig })

		if !sessionFileUsable(aDir, sessionID, false, "ag_summary_fail", sessionResetMinIdleDuration, nil) {
			t.Error("expected reset to be aborted when summary fails")
		}
		if _, err := os.Stat(sessionFile); err != nil {
			t.Error("expected session file to be preserved when summary fails")
		}
	})

	t.Run("keeps empty session when remove fails", func(t *testing.T) {
		// Parallel to the threshold-remove case: a zero-byte session file
		// whose delete fails must not then be advertised as "fresh" to
		// the caller, or a subsequent --session-id invocation would race
		// with the same-UUID leftover.
		os.WriteFile(sessionFile, []byte{}, 0o644)

		if err := os.Chmod(projectDir, 0o555); err != nil {
			t.Fatalf("chmod project dir: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(projectDir, 0o755) })

		if !sessionFileUsable(aDir, sessionID, false, "", sessionResetMinIdleDuration, nil) {
			t.Error("expected fallback to true on empty-file remove failure")
		}
		if _, err := os.Stat(sessionFile); err != nil {
			t.Error("expected empty file to remain when remove fails")
		}
	})

	t.Run("per-agent idleThreshold shrinks the active-chat guard", func(t *testing.T) {
		// A 1-minute override should let a session that was modified 90s ago
		// (still well inside the package default of 5m) be reset, proving
		// the threshold parameter is honored end-to-end.
		entry := `{"type":"assistant","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":160000,"cache_creation_input_tokens":1000}}}`
		os.WriteFile(sessionFile, []byte(entry+"\n"), 0o644)
		recent := time.Now().Add(-90 * time.Second)
		if err := os.Chtimes(sessionFile, recent, recent); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		// Stub the summary hook so the reset path can complete without
		// spawning a real claude subprocess.
		orig := preResetSummarize
		preResetSummarize = func(_, _ string, _ *slog.Logger) error { return nil }
		t.Cleanup(func() { preResetSummarize = orig })

		if sessionFileUsable(aDir, sessionID, false, "ag_per_agent_idle", 1*time.Minute, nil) {
			t.Error("expected reset under shorter per-agent idle window")
		}
		if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
			t.Error("expected session file removed under shorter idle window")
		}
	})

	t.Run("non-positive idleThreshold falls back to package default", func(t *testing.T) {
		// Defensive: ResumeIdleDuration() will not return <=0 in practice,
		// but a 0/negative value passed by a caller must not turn the guard
		// off entirely — that would be indistinguishable from automated
		// trigger and would let interactive sessions get reset mid-turn.
		entry := `{"type":"assistant","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":160000,"cache_creation_input_tokens":1000}}}`
		os.WriteFile(sessionFile, []byte(entry+"\n"), 0o644)
		// mtime = now → inside the 5-minute default window.
		if !sessionFileUsable(aDir, sessionID, false, "", 0, nil) {
			t.Error("expected zero idleThreshold to fall back to default and keep active session")
		}
		if _, err := os.Stat(sessionFile); err != nil {
			t.Error("expected session file to remain under default fallback")
		}
	})
}

// TestAgent_ResumeIdleDuration_Default covers the fallback contract callers
// rely on: ResumeIdleMinutes==0 (the default for legacy / unset agents) MUST
// resolve to defaultResumeIdleDuration so existing behavior is preserved.
func TestAgent_ResumeIdleDuration_Default(t *testing.T) {
	cases := []struct {
		name string
		a    *Agent
		want time.Duration
	}{
		{"nil agent", nil, defaultResumeIdleDuration},
		{"unset", &Agent{}, defaultResumeIdleDuration},
		{"explicit 5", &Agent{ResumeIdleMinutes: 5}, 5 * time.Minute},
		{"explicit 1", &Agent{ResumeIdleMinutes: 1}, 1 * time.Minute},
		{"explicit 60", &Agent{ResumeIdleMinutes: 60}, 60 * time.Minute},
		{"negative falls back", &Agent{ResumeIdleMinutes: -3}, defaultResumeIdleDuration},
		// Off-whitelist positive values must not be honored — covers the
		// store-layer-bypass case where a hand-edited agents.json carries
		// e.g. 2 (legal int, not in allowedResumeIdles).
		{"invalid positive falls back", &Agent{ResumeIdleMinutes: 2}, defaultResumeIdleDuration},
		{"invalid large falls back", &Agent{ResumeIdleMinutes: 999}, defaultResumeIdleDuration},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.ResumeIdleDuration(); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPrepareClaudeSettings_BashCaptureHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentID := "ag_bash_capture_test"
	if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	t.Run("apiBase empty installs PreToolUse only", func(t *testing.T) {
		PrepareClaudeSettings(agentID, "", nil, logger)

		claudeDir := filepath.Join(agentDir(agentID), ".claude")
		settingsPath := filepath.Join(claudeDir, "settings.local.json")
		hookPath := filepath.Join(claudeDir, "hooks", "bash-capture.sh")
		captureDir := filepath.Join(claudeDir, "captures")

		// Hook script must exist and be executable.
		st, err := os.Stat(hookPath)
		if err != nil {
			t.Fatalf("hook script not written: %v", err)
		}
		if st.Mode().Perm()&0o111 == 0 {
			t.Errorf("hook script not executable: mode=%v", st.Mode())
		}
		// Captures dir must exist.
		if st, err := os.Stat(captureDir); err != nil || !st.IsDir() {
			t.Errorf("captures dir missing: %v", err)
		}

		// Settings JSON must contain PreToolUse pointing at hook script,
		// and must NOT contain PreCompact (apiBase empty).
		raw, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("settings is not valid JSON: %v\n%s", err, raw)
		}
		hooks, _ := parsed["hooks"].(map[string]any)
		if _, ok := hooks["PreToolUse"]; !ok {
			t.Errorf("PreToolUse missing from settings: %s", raw)
		}
		if _, ok := hooks["PreCompact"]; ok {
			t.Errorf("PreCompact must not appear when apiBase empty: %s", raw)
		}
		if !strings.Contains(string(raw), hookPath) {
			t.Errorf("hook path %q not embedded in settings: %s", hookPath, raw)
		}

		// Hook script must contain capture dir path.
		hookSrc, _ := os.ReadFile(hookPath)
		if !strings.Contains(string(hookSrc), captureDir) {
			t.Errorf("capture dir not baked into hook script")
		}
		if strings.Contains(string(hookSrc), "__CAP_DIR__") {
			t.Errorf("template placeholder __CAP_DIR__ left unreplaced")
		}
	})

	t.Run("apiBase set installs both PreToolUse and PreCompact", func(t *testing.T) {
		PrepareClaudeSettings(agentID, "https://kojo.example/api", nil, logger)

		settingsPath := filepath.Join(agentDir(agentID), ".claude", "settings.local.json")
		raw, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("settings is not valid JSON: %v\n%s", err, raw)
		}
		hooks, _ := parsed["hooks"].(map[string]any)
		if _, ok := hooks["PreToolUse"]; !ok {
			t.Errorf("PreToolUse missing: %s", raw)
		}
		if _, ok := hooks["PreCompact"]; !ok {
			t.Errorf("PreCompact missing: %s", raw)
		}
	})

	t.Run("windows skips PreToolUse hook", func(t *testing.T) {
		// Fresh agent dir to avoid carryover from previous subtests.
		winAgentID := "ag_bash_capture_test_win"
		if err := os.MkdirAll(agentDir(winAgentID), 0o755); err != nil {
			t.Fatal(err)
		}
		orig := hostOS
		hostOS = "windows"
		t.Cleanup(func() { hostOS = orig })

		PrepareClaudeSettings(winAgentID, "https://kojo.example/api", nil, logger)

		claudeDir := filepath.Join(agentDir(winAgentID), ".claude")
		hookPath := filepath.Join(claudeDir, "hooks", "bash-capture.sh")
		if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
			t.Errorf("hook script must not exist on windows: stat err=%v", err)
		}

		raw, err := os.ReadFile(filepath.Join(claudeDir, "settings.local.json"))
		if err != nil {
			t.Fatal(err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("settings is not valid JSON: %v\n%s", err, raw)
		}
		hooks, _ := parsed["hooks"].(map[string]any)
		if _, ok := hooks["PreToolUse"]; ok {
			t.Errorf("PreToolUse must not appear on windows: %s", raw)
		}
		// PreCompact still installs (curl-based, also POSIX-shell but
		// matches existing kojo Windows assumption).
		if _, ok := hooks["PreCompact"]; !ok {
			t.Errorf("PreCompact missing: %s", raw)
		}
	})
}
