package agent

import (
	"encoding/json"
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

// claudeLine encodes a Claude session JSONL line for fixture setup.
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
	lines := []string{
		claudeLine(t, map[string]any{
			"type":      "user",
			"timestamp": "2026-05-09T01:00:00.000Z",
			"message":   map[string]any{"role": "user", "content": "use a tool"},
		}),
		claudeLine(t, map[string]any{
			"type":      "assistant",
			"timestamp": "2026-05-09T03:30:00.000Z",
			"message": map[string]any{
				"content": []map[string]any{
					{"type": "tool_use", "id": "toolu_abc", "name": "X", "input": map[string]any{}},
				},
			},
		}),
		claudeLine(t, map[string]any{
			"type":      "user",
			"timestamp": "2026-05-09T01:30:00.000Z",
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

func TestTruncateClaudeSessionFileTrimsTrailingToolResult(t *testing.T) {
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
			"timestamp": "2026-05-09T05:00:00.000Z",
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
	if _, err := trimDiaryFileEntries(path, 14*60+0); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	out := string(got)
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
		{" ```", true},
		{"   ```", true},
		{"\t```", true},
		{"    ```", false},
		{"  ~~~bash", true},
		{"x```", false},
	}
	for _, c := range cases {
		got := diaryFenceLine.MatchString(c.line)
		if got != c.ok {
			t.Errorf("fence(%q) = %v, want %v", c.line, got, c.ok)
		}
	}
}

func TestDiaryEntryRegex(t *testing.T) {
	cases := []struct {
		line string
		ok   bool
	}{
		{"- 09:00 — entry", true},
		{"  - 09:00 — indented", true},
		{"- 9:00 — short", false},
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

// TestTrimDiaryStringEntries exercises the pure-string variant the DB-only
// fallback path uses when memory_entries holds a body that hasn't been
// materialised onto disk yet.
func TestTrimDiaryStringEntries(t *testing.T) {
	body := `## 2026-05-09

- 09:00 — keep
- 12:00 — drop
- 14:00 — drop
`
	out, removed := trimDiaryStringEntries(body, 12*60)
	if removed != 2 {
		t.Errorf("removed=%d, want 2", removed)
	}
	if !strings.Contains(out, "keep") {
		t.Errorf("kept entry missing:\n%s", out)
	}
	for _, banned := range []string{"12:00", "14:00"} {
		if strings.Contains(out, banned) {
			t.Errorf("output still contains %q:\n%s", banned, out)
		}
	}
}
