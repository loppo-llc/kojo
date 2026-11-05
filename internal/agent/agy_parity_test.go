package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func writeAgyRulesForTest(dir string, files map[string]string) error {
	agentDir := filepath.Dir(filepath.Dir(dir))
	managed := make(map[string]string)
	for name, content := range files {
		managed[filepath.Join(dir, name)] = content
	}
	release, err := holdAgyWorkspace(context.Background(), agentDir, managed)
	if err == nil {
		release()
	}
	return err
}

func TestChunkAgySystemPrompt(t *testing.T) {
	line := strings.Repeat("あ", 30) + "\n" // 91 bytes
	prompt := strings.Repeat(line, 1000) + strings.Repeat("い", 5000) + "\nend"
	chunks := chunkAgySystemPrompt(prompt, 1000)
	if strings.Join(chunks, "") != prompt {
		t.Fatal("chunks do not reassemble to the original prompt")
	}
	for i, c := range chunks {
		if len(c) > 1000 {
			t.Fatalf("chunk %d is %d bytes, over the limit", i, len(c))
		}
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d splits a UTF-8 sequence", i)
		}
	}
	if got := chunkAgySystemPrompt("", 1000); got != nil {
		t.Fatalf("empty prompt: got %d chunks", len(got))
	}
}

func TestRenderAgyRulesRespectsCap(t *testing.T) {
	prompt := strings.Repeat("日本語のシステムプロンプト。\n", 6000)
	files := renderAgyRules(prompt)
	if len(files) < 2 {
		t.Fatalf("expected several rule files, got %d", len(files))
	}
	var body strings.Builder
	for i := 1; i <= len(files); i++ {
		name := fmt.Sprintf("%s%02d.md", agyRulesFilePrefix, i)
		c, ok := files[name]
		if !ok {
			t.Fatalf("missing %s", name)
		}
		if len(c) > agyRuleChunkBytes {
			t.Fatalf("%s is %d bytes", name, len(c))
		}
		if !strings.HasPrefix(c, agyRulesFrontmatter) {
			t.Fatalf("%s lacks the always_on frontmatter", name)
		}
		rest := strings.TrimPrefix(c, agyRulesFrontmatter)
		nl := strings.IndexByte(rest, '\n')
		body.WriteString(rest[nl+1:])
	}
	if body.String() != prompt {
		t.Fatal("rule bodies do not reassemble to the prompt")
	}
}

func TestRemoveAgyConversation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".gemini", "antigravity-cli")
	id := "12927f4d-240b-4ec7-9639-c75a39e1b1f0"
	other := "15414599-1403-4310-b19b-67e8da1e88b5"
	for _, p := range []string{
		"conversations/" + id + ".db", "conversations/" + id + ".db-wal", "annotations/" + id + ".pbtxt",
		"presence/" + id + ".lock", "brain/" + id + "/notes.md", "conversations/" + other + ".db",
	} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !agyConversationExists(id) {
		t.Fatal("conversation should exist")
	}
	n, err := removeAgyConversation(id)
	if err != nil || n != 5 {
		t.Fatalf("removed %d, err %v", n, err)
	}
	if agyConversationExists(id) || !agyConversationExists(other) {
		t.Fatal("removed the wrong conversation")
	}
	if n, _ := removeAgyConversation("../conversations"); n != 0 {
		t.Fatal("non-UUID id must be ignored")
	}
}

func TestAgyRefusesSymlinkedWorkspaceDirs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	victim := t.TempDir()
	if err := os.WriteFile(filepath.Join(victim, "kojo-system-01.md"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Symlinked rules dir: no write, no stale-file removal through it.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, agyRulesDir(dir)); err != nil {
		t.Fatal(err)
	}
	if err := writeAgyRulesForTest(agyRulesDir(dir), renderAgyRules("hi")); err == nil {
		t.Fatal("symlinked rules dir accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(victim, "kojo-system-01.md")); string(b) != "keep" {
		t.Fatal("write went through the symlink")
	}

	// Symlinked state dir: refs and ledger are not written through it.
	dir2 := t.TempDir()
	if err := os.Symlink(victim, agyStateDir(dir2)); err != nil {
		t.Fatal(err)
	}
	id := "12927f4d-240b-4ec7-9639-c75a39e1b1f0"
	writeAgyConversationID(dir2, "", id, logger)
	recordAgyConversation(dir2, id, logger)
	for _, name := range []string{"conversation_id", "conversations"} {
		if _, err := os.Stat(filepath.Join(victim, name)); !os.IsNotExist(err) {
			t.Fatalf("%s written through the symlink", name)
		}
	}
}

func TestAgyRefusesSymlinkedRefFiles(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(agyStateDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{agySessionIDFile(dir), agyKnownFile(dir)} {
		if err := os.Symlink(victim, p); err != nil {
			t.Fatal(err)
		}
	}
	id := "12927f4d-240b-4ec7-9639-c75a39e1b1f0"
	writeAgyConversationID(dir, "", id, logger)
	recordAgyConversation(dir, id, logger)
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("symlinked ref file was written through: %q", b)
	}
}
