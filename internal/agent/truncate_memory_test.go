package agent

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustParseRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tt
}

func writeMessages(t *testing.T, dir string, msgs []*Message) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, messagesFile)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			t.Fatal(err)
		}
	}
}

func readMessages(t *testing.T, dir string) []*Message {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, messagesFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []*Message
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m Message
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		out = append(out, &m)
	}
	return out
}

func TestTruncateMessagesJSONL(t *testing.T) {
	dir := t.TempDir()

	msgs := []*Message{
		{ID: "m_a", Role: "user", Content: "before", Timestamp: "2026-05-09T10:00:00+09:00"},
		{ID: "m_b", Role: "assistant", Content: "before-reply", Timestamp: "2026-05-09T10:00:30+09:00"},
		{ID: "m_c", Role: "user", Content: "boundary", Timestamp: "2026-05-09T12:00:00+09:00"},
		{ID: "m_d", Role: "user", Content: "after", Timestamp: "2026-05-09T13:00:00+09:00"},
	}
	writeMessages(t, dir, msgs)

	since := mustParseRFC3339(t, "2026-05-09T12:00:00+09:00")
	removed, err := truncateMessagesJSONL(dir, since, "")
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed=%d, want 2", removed)
	}
	got := readMessages(t, dir)
	if len(got) != 2 {
		t.Fatalf("kept %d messages, want 2", len(got))
	}
	if got[0].ID != "m_a" || got[1].ID != "m_b" {
		t.Errorf("kept IDs = %s,%s, want m_a,m_b", got[0].ID, got[1].ID)
	}
}

func TestTruncateMessagesJSONLMissingFile(t *testing.T) {
	dir := t.TempDir()
	since := mustParseRFC3339(t, "2026-05-09T12:00:00+09:00")
	removed, err := truncateMessagesJSONL(dir, since, "")
	if err != nil {
		t.Fatalf("missing file should be no-op, got %v", err)
	}
	if removed != 0 {
		t.Errorf("removed=%d, want 0", removed)
	}
}

func TestTruncateMessagesJSONLPreservesMalformed(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Mix of valid and malformed lines.
	content := `{"id":"m_a","role":"user","content":"keep","timestamp":"2026-05-09T10:00:00+09:00"}
not-json-garbage
{"id":"m_b","role":"user","content":"drop","timestamp":"2026-05-09T13:00:00+09:00"}
{"id":"m_c","role":"user","content":"no-timestamp"}
`
	if err := os.WriteFile(filepath.Join(dir, messagesFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	since := mustParseRFC3339(t, "2026-05-09T12:00:00+09:00")
	removed, err := truncateMessagesJSONL(dir, since, "")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed=%d, want 1 (only the parseable post-T entry)", removed)
	}
	data, _ := os.ReadFile(filepath.Join(dir, messagesFile))
	if !strings.Contains(string(data), "not-json-garbage") {
		t.Error("malformed line was dropped; should be preserved")
	}
	if !strings.Contains(string(data), `"id":"m_c"`) {
		t.Error("entry without timestamp was dropped; should be preserved")
	}
	if strings.Contains(string(data), `"id":"m_b"`) {
		t.Error("post-T entry m_b was kept; should be dropped")
	}
}

// jsonl helper to build Claude session lines.
func claudeLine(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTruncateClaudeSessionFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")

	// Build a session that ends mid-turn:
	//   user (real) @ 10:00      ← keep
	//   assistant     @ 10:00:30 ← keep (response to first user)
	//   user (real) @ 11:00      ← keep (boundary not after T)
	//   assistant   @ 11:00:30   ← drop (after T)
	//   user (tool_result) @ 11:00:31 ← drop (after T + synthetic, tool-result trim would catch it anyway)
	// After timestamp filter we still need trailing trim because the
	// 11:00 user assistant turn is now incomplete? Actually 11:00 user is
	// real and complete on its own as the end of the file — this is fine.
	lines := []string{
		claudeLine(t, map[string]any{
			"type":      "user",
			"timestamp": "2026-05-09T01:00:00.000Z",
			"message": map[string]any{
				"role":    "user",
				"content": "hello",
			},
		}),
		claudeLine(t, map[string]any{
			"type":      "assistant",
			"timestamp": "2026-05-09T01:00:30.000Z",
			"message": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "hi"}},
			},
		}),
		claudeLine(t, map[string]any{
			"type":      "user",
			"timestamp": "2026-05-09T02:00:00.000Z",
			"message": map[string]any{
				"role":    "user",
				"content": "boundary",
			},
		}),
		claudeLine(t, map[string]any{
			"type":      "assistant",
			"timestamp": "2026-05-09T03:30:00.000Z",
			"message": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "after"}},
			},
		}),
		claudeLine(t, map[string]any{
			"type":      "user",
			"timestamp": "2026-05-09T03:30:01.000Z",
			"message": map[string]any{
				"role":    "user",
				"content": []map[string]any{{"type": "tool_result", "content": "x"}},
			},
		}),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// since = 2026-05-09T03:00:00Z. UTC threshold strictly above 02:00:00.
	since := mustParseRFC3339(t, "2026-05-09T03:00:00Z")
	removed, deleted, err := truncateClaudeSessionFile(path, since)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if deleted {
		t.Fatal("file should not be deleted; some entries remain")
	}
	if removed < 2 {
		t.Errorf("removed=%d, want >=2", removed)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "after") {
		t.Error("post-T assistant text was kept")
	}
	if !strings.Contains(string(data), `"content":"boundary"`) {
		t.Error("pre-T boundary user message was dropped; should be kept")
	}

	// Last kept entry must be a real user message so --resume is valid.
	lastLine := ""
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			lastLine = l
		}
	}
	var entry struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal([]byte(lastLine), &entry); err != nil {
		t.Fatalf("last line not JSON: %v", err)
	}
	if entry.Type != "user" || !isRealUserEntry(entry.Message) {
		t.Errorf("last entry must be a real user message; type=%s", entry.Type)
	}
}

func TestTruncateClaudeSessionFileNoopOnHealthySession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")

	// A complete user → assistant turn with no tool calls. Both entries
	// predate the threshold. Expectation: byte-identical no-op. The
	// integrity trim must NOT nibble at a healthy assistant tail.
	lines := []string{
		claudeLine(t, map[string]any{
			"type":      "user",
			"timestamp": "2026-05-09T01:00:00.000Z",
			"message": map[string]any{
				"role":    "user",
				"content": "hi",
			},
		}),
		claudeLine(t, map[string]any{
			"type":      "assistant",
			"timestamp": "2026-05-09T01:00:30.000Z",
			"message": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "trailing"}},
			},
		}),
	}
	original := []byte(strings.Join(lines, "\n") + "\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	since := mustParseRFC3339(t, "2030-01-01T00:00:00Z")
	removed, deleted, err := truncateClaudeSessionFile(path, since)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("file should not be deleted on no-op truncation")
	}
	if removed != 0 {
		t.Errorf("removed=%d, want 0 (no-op truncation must leave a healthy session alone)", removed)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(original) {
		t.Errorf("file content changed; want byte-identical\nbefore: %q\nafter:  %q", original, got)
	}
}

func TestTruncateClaudeSessionFileTrimsOrphanToolResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")

	// Session with a tool-using assistant turn that gets cut by the
	// timestamp filter, leaving an orphan tool_result behind. Integrity
	// trim must drop it.
	lines := []string{
		claudeLine(t, map[string]any{
			"type":      "user",
			"timestamp": "2026-05-09T01:00:00.000Z",
			"message":   map[string]any{"role": "user", "content": "use a tool"},
		}),
		claudeLine(t, map[string]any{
			"type":      "assistant",
			"timestamp": "2026-05-09T03:30:00.000Z", // dropped by timestamp
			"message": map[string]any{
				"content": []map[string]any{
					{"type": "tool_use", "id": "toolu_abc", "name": "X", "input": map[string]any{}},
				},
			},
		}),
		claudeLine(t, map[string]any{
			"type":      "user",
			"timestamp": "2026-05-09T01:30:00.000Z", // pre-T but synthetic
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "toolu_abc", "content": "ok"},
				},
			},
		}),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	since := mustParseRFC3339(t, "2026-05-09T03:00:00Z")
	removed, deleted, err := truncateClaudeSessionFile(path, since)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("file should not be deleted; first user turn remains")
	}
	if removed < 2 {
		t.Errorf("removed=%d, want >=2 (assistant by timestamp + orphan tool_result by integrity trim)", removed)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "tool_result") {
		t.Error("orphan tool_result should have been trimmed")
	}
}

func TestTruncateClaudeSessionFileDeletesWhenAllAfter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	lines := []string{
		claudeLine(t, map[string]any{
			"type":      "user",
			"timestamp": "2026-05-09T05:00:00.000Z",
			"message":   map[string]any{"role": "user", "content": "after"},
		}),
		claudeLine(t, map[string]any{
			"type":      "assistant",
			"timestamp": "2026-05-09T05:00:30.000Z",
			"message":   map[string]any{"content": []map[string]any{{"type": "text", "text": "after"}}},
		}),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	since := mustParseRFC3339(t, "2026-05-09T01:00:00Z")
	_, deleted, err := truncateClaudeSessionFile(path, since)
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Error("file should be deleted when all entries are after T")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still exists after delete: err=%v", err)
	}
}

func TestTrimDiaryFileEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "2026-05-09.md")
	body := `## 2026-05-09

- 09:00 — morning standup
- 11:30 — fixed bug
  with a continuation line
- 12:00 — lunch decision
- 14:00 — afternoon work
  more detail
- 15:30 — wrap up
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// since = 12:00 → drop 12:00, 14:00 (incl. continuation), 15:30
	removed, err := trimDiaryFileEntries(path, 12*60+0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Errorf("removed=%d, want 3", removed)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	for _, banned := range []string{"lunch decision", "afternoon work", "more detail", "wrap up"} {
		if strings.Contains(got, banned) {
			t.Errorf("output still contains %q:\n%s", banned, got)
		}
	}
	for _, must := range []string{"morning standup", "fixed bug", "continuation line"} {
		if !strings.Contains(got, must) {
			t.Errorf("output missing %q:\n%s", must, got)
		}
	}
}

func TestTruncateDiary(t *testing.T) {
	root := t.TempDir()
	memDir := filepath.Join(root, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{
		"2026-05-08.md": "## 2026-05-08\n\n- 09:00 — yesterday\n",
		"2026-05-09.md": "## 2026-05-09\n\n- 10:00 — early\n- 14:00 — late\n",
		"2026-05-10.md": "## 2026-05-10\n\n- 09:00 — tomorrow\n",
		"recent.md":     "rolling summary, not a diary file\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(memDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 2026-05-09 12:00 JST.
	since := time.Date(2026, 5, 9, 12, 0, 0, 0, jst)
	entries, removedFiles, err := truncateDiary(root, since, slog.Default())
	if err != nil {
		t.Fatalf("truncateDiary: %v", err)
	}
	if removedFiles != 1 {
		t.Errorf("removedFiles=%d, want 1 (the 05-10 file)", removedFiles)
	}
	if entries != 1 {
		t.Errorf("entries=%d, want 1 (the 14:00 entry)", entries)
	}

	// Yesterday untouched.
	if data, _ := os.ReadFile(filepath.Join(memDir, "2026-05-08.md")); !strings.Contains(string(data), "yesterday") {
		t.Error("2026-05-08 should be untouched")
	}
	// Today trimmed.
	if data, _ := os.ReadFile(filepath.Join(memDir, "2026-05-09.md")); strings.Contains(string(data), "late") {
		t.Error("2026-05-09 should have 14:00 entry removed")
	}
	// Tomorrow gone.
	if _, err := os.Stat(filepath.Join(memDir, "2026-05-10.md")); !os.IsNotExist(err) {
		t.Error("2026-05-10 should have been deleted")
	}
	// Non-diary file untouched.
	if data, _ := os.ReadFile(filepath.Join(memDir, "recent.md")); !strings.Contains(string(data), "rolling summary") {
		t.Error("recent.md should be untouched")
	}
}

func TestTruncateDiaryMissingDir(t *testing.T) {
	root := t.TempDir()
	since := time.Date(2026, 5, 9, 12, 0, 0, 0, jst)
	entries, removedFiles, err := truncateDiary(root, since, slog.Default())
	if err != nil {
		t.Fatalf("missing memory dir should be no-op, got %v", err)
	}
	if entries != 0 || removedFiles != 0 {
		t.Errorf("entries=%d files=%d, want 0/0", entries, removedFiles)
	}
}

func TestTrimDiaryFileEntriesDropsPreCompactSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "2026-05-09.md")
	body := `## 2026-05-09

- 09:00 — earlier bullet

## Pre-compaction summary (10:30)

Some summary body
- still inside the section
- more body

## Pre-compaction summary (14:00)

Later summary that must be dropped.

## 2026-05-09 (continued)

- 14:30 — also dropped
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// since = 14:00 → drop the 14:00 section AND the 14:30 bullet, keep
	// the 10:30 section and the 09:00 bullet.
	removed, err := trimDiaryFileEntries(path, 14*60+0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed=%d, want 2 (one section + one bullet)", removed)
	}
	got, _ := os.ReadFile(path)
	out := string(got)
	if !strings.Contains(out, "earlier bullet") {
		t.Errorf("09:00 bullet was dropped:\n%s", out)
	}
	if !strings.Contains(out, "Pre-compaction summary (10:30)") {
		t.Errorf("10:30 pre-compact section was dropped:\n%s", out)
	}
	if !strings.Contains(out, "still inside the section") {
		t.Errorf("10:30 section body was dropped:\n%s", out)
	}
	if strings.Contains(out, "Pre-compaction summary (14:00)") {
		t.Errorf("14:00 pre-compact section header survived:\n%s", out)
	}
	if strings.Contains(out, "Later summary") {
		t.Errorf("14:00 pre-compact section body survived:\n%s", out)
	}
	if strings.Contains(out, "14:30") {
		t.Errorf("14:30 bullet survived:\n%s", out)
	}
}

func TestTruncateMessagesJSONLByMessageID(t *testing.T) {
	dir := t.TempDir()
	// Two messages share the same RFC3339-second timestamp. fromMsgID
	// must distinguish them by position, not by timestamp.
	msgs := []*Message{
		{ID: "m_a", Role: "user", Content: "first", Timestamp: "2026-05-09T12:00:00+09:00"},
		{ID: "m_b", Role: "user", Content: "tied-with-m_a", Timestamp: "2026-05-09T12:00:00+09:00"},
		{ID: "m_c", Role: "user", Content: "later", Timestamp: "2026-05-09T13:00:00+09:00"},
	}
	writeMessages(t, dir, msgs)

	// Cut from m_b (inclusive). m_a stays even though it shares a
	// timestamp with m_b. Pass a deliberately mismatched `since` to
	// prove the function uses ID, not timestamp, in this branch.
	removed, err := truncateMessagesJSONL(dir, mustParseRFC3339(t, "2099-01-01T00:00:00Z"), "m_b")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed=%d, want 2 (m_b + m_c)", removed)
	}
	got := readMessages(t, dir)
	if len(got) != 1 || got[0].ID != "m_a" {
		t.Errorf("kept = %v, want [m_a]", got)
	}
}

func TestTruncateMessagesJSONLFromMessageNotFound(t *testing.T) {
	dir := t.TempDir()
	msgs := []*Message{
		{ID: "m_a", Role: "user", Content: "x", Timestamp: "2026-05-09T12:00:00+09:00"},
	}
	writeMessages(t, dir, msgs)
	// Unknown ID — must be a no-op (zero records dropped, file unchanged).
	removed, err := truncateMessagesJSONL(dir, time.Time{}, "m_zzz")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("removed=%d, want 0 for unknown ID", removed)
	}
	got := readMessages(t, dir)
	if len(got) != 1 {
		t.Errorf("got %d messages, want 1 (file unchanged)", len(got))
	}
}

func TestTruncateClaudeSessionFileTrimsTrailingToolResult(t *testing.T) {
	// Scenario: an assistant text-only turn at T+ε is dropped by the
	// timestamp filter, leaving a session whose tail is now a synthetic
	// user (tool_result) that was emitted *before* T. Even though that
	// tool_result references a still-live tool_use, the integrity trim
	// drops it so --resume lands on the previous real user.
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	lines := []string{
		claudeLine(t, map[string]any{
			"type":      "user",
			"timestamp": "2026-05-09T01:00:00.000Z",
			"message":   map[string]any{"role": "user", "content": "do a tool"},
		}),
		claudeLine(t, map[string]any{
			"type":      "assistant",
			"timestamp": "2026-05-09T01:00:30.000Z",
			"message": map[string]any{
				"content": []map[string]any{
					{"type": "tool_use", "id": "toolu_X", "name": "Y", "input": map[string]any{}},
				},
			},
		}),
		claudeLine(t, map[string]any{
			"type":      "user",
			"timestamp": "2026-05-09T01:01:00.000Z",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "toolu_X", "content": "ok"},
				},
			},
		}),
		claudeLine(t, map[string]any{
			"type":      "assistant",
			"timestamp": "2026-05-09T05:00:00.000Z", // dropped
			"message": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "done"}},
			},
		}),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	since := mustParseRFC3339(t, "2026-05-09T03:00:00Z")
	_, deleted, err := truncateClaudeSessionFile(path, since)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("file should not be deleted")
	}
	got, _ := os.ReadFile(path)
	// Tail must be the real user "do a tool" — the tool turn (assistant
	// + synthetic-user tool_result) is trimmed back, plus the post-T
	// final assistant.
	tail := ""
	for _, l := range strings.Split(strings.TrimRight(string(got), "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			tail = l
		}
	}
	var entry struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal([]byte(tail), &entry); err != nil {
		t.Fatalf("tail line not JSON: %v\nfile:\n%s", err, got)
	}
	if entry.Type != "user" || !isRealUserEntry(entry.Message) {
		t.Errorf("tail must be real user; got type=%s line=%s", entry.Type, tail)
	}
}

func TestTrimDiaryFileEntriesIgnoresHashInsideFencedCode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "2026-05-09.md")
	body := "## 2026-05-09\n\n" +
		"- 09:00 — early\n" +
		"\n" +
		"## Pre-compaction summary (15:00)\n" +
		"\n" +
		"Body before fence.\n" +
		"```markdown\n" +
		"## This Hash Is Inside A Fence — must not terminate the section drop\n" +
		"some inner content\n" +
		"```\n" +
		"Body after fence — still inside the dropped section.\n" +
		"\n" +
		"## 2026-05-09 (continued)\n" +
		"\n" +
		"- 16:00 — also dropped\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// since = 14:00 → drop the 15:00 section + the 16:00 bullet, keep 09:00.
	if _, err := trimDiaryFileEntries(path, 14*60+0); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	out := string(got)
	// All section content must be gone, including text after the
	// fence and the fenced inner-heading line itself.
	for _, banned := range []string{
		"Pre-compaction summary (15:00)",
		"Body before fence",
		"This Hash Is Inside A Fence",
		"Body after fence",
		"16:00",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("output still contains %q (fenced `## ` should not have terminated section drop):\n%s", banned, out)
		}
	}
	if !strings.Contains(out, "early") {
		t.Errorf("09:00 bullet was dropped:\n%s", out)
	}
}

func TestTrimDiaryFileEntriesPreservesPseudoHeadingInKeptFence(t *testing.T) {
	// A kept (pre-T) section contains a fenced code sample that itself
	// includes lines that *look like* a pre-compaction header and a
	// `- HH:MM` bullet. The fence guard must prevent us from interpreting
	// those samples as live entries and dropping them.
	dir := t.TempDir()
	path := filepath.Join(dir, "2026-05-09.md")
	body := "## 2026-05-09\n\n" +
		"## Pre-compaction summary (10:00)\n" +
		"\n" +
		"Here is a markdown sample we want kept verbatim:\n" +
		"```markdown\n" +
		"## Pre-compaction summary (15:00)\n" +
		"- 15:30 — sample bullet inside fence\n" +
		"```\n" +
		"\n" +
		"- 11:00 — outside-fence bullet, also kept\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// since = 14:00 — both the fenced "15:00" header and the fenced
	// "15:30" bullet would be deleted if the regex matched them, but
	// they're inside a fence inside a kept (10:00 < 14:00) section so
	// nothing should change.
	removed, err := trimDiaryFileEntries(path, 14*60+0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("removed=%d, want 0 (everything is pre-T or fenced)", removed)
	}
	got, _ := os.ReadFile(path)
	out := string(got)
	for _, must := range []string{
		"Pre-compaction summary (10:00)",
		"Pre-compaction summary (15:00)",
		"15:30 — sample bullet inside fence",
		"11:00",
	} {
		if !strings.Contains(out, must) {
			t.Errorf("output missing %q (fenced or pre-T content was dropped):\n%s", must, out)
		}
	}
}

func TestDiaryFenceRegexAcceptsIndentedFence(t *testing.T) {
	cases := []struct {
		line string
		ok   bool
	}{
		{"```", true},
		{"```python", true},
		{"~~~", true},
		{" ```", true},      // 1-space indent
		{"   ```", true},    // 3-space indent (spec max)
		{"\t```", true},     // tab
		{"    ```", false},  // 4 spaces — would be a code block in CommonMark, not a fence
		{"  ~~~bash", true}, // 2-space indent
		{"x```", false},     // mid-line fence text
	}
	for _, c := range cases {
		got := diaryFenceLine.MatchString(c.line)
		if got != c.ok {
			t.Errorf("fence(%q) = %v, want %v", c.line, got, c.ok)
		}
	}
}

// Sanity check: the regex behind diary entry parsing accepts the documented
// `- HH:MM — ...` form used by buildSystemPrompt's "Memory Write" directive.
func TestDiaryEntryRegex(t *testing.T) {
	cases := []struct {
		line string
		ok   bool
	}{
		{"- 09:00 — entry", true},
		{"  - 09:00 — indented", true},
		{"- 9:00 — short", false}, // not 2-digit
		{"- 09:00:30 — has seconds", true},
		{"-09:00 — no space", false},
		{"## 2026-05-09", false},
	}
	for _, c := range cases {
		got := diaryEntryHHMM.MatchString(c.line)
		if got != c.ok {
			t.Errorf("regex(%q) = %v, want %v", c.line, got, c.ok)
		}
	}
}

