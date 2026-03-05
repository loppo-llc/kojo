package agent

import (
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
