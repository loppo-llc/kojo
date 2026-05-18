package chathistory

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoryFilePath(t *testing.T) {
	got := HistoryFilePath("/data/agents/ag1", "slack", "C123", "1712345.678")
	want := filepath.Join("/data/agents/ag1", "chat_history", "slack", "C123", "1712345.678.jsonl")
	if got != want {
		t.Errorf("HistoryFilePath thread = %q, want %q", got, want)
	}

	got = HistoryFilePath("/data/agents/ag1", "slack", "C123", "")
	want = filepath.Join("/data/agents/ag1", "chat_history", "slack", "C123", "_channel.jsonl")
	if got != want {
		t.Errorf("HistoryFilePath channel = %q, want %q", got, want)
	}
}

func TestAppendAndLoadHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	msgs := []HistoryMessage{
		{Platform: "slack", MessageID: "1.0", UserName: "alice", Text: "hello"},
		{Platform: "slack", MessageID: "2.0", UserName: "bob", Text: "hi"},
	}

	if err := AppendMessages(path, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}

	loaded, err := LoadHistory(path)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("LoadHistory got %d messages, want 2", len(loaded))
	}
	if loaded[0].Text != "hello" || loaded[1].Text != "hi" {
		t.Errorf("unexpected messages: %+v", loaded)
	}

	// Append more
	more := []HistoryMessage{
		{Platform: "slack", MessageID: "3.0", UserName: "carol", Text: "hey"},
	}
	if err := AppendMessages(path, more); err != nil {
		t.Fatalf("AppendMessages 2: %v", err)
	}

	loaded, err = LoadHistory(path)
	if err != nil {
		t.Fatalf("LoadHistory 2: %v", err)
	}
	if len(loaded) != 3 {
		t.Fatalf("got %d messages, want 3", len(loaded))
	}
}

func TestLastPlatformTS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	// Non-existent file
	if id := LastPlatformTS(path); id != "" {
		t.Errorf("expected empty, got %q", id)
	}

	msgs := []HistoryMessage{
		{Platform: "slack", MessageID: "1712345678.100000", Text: "first"},
		{Platform: "slack", MessageID: "1712345678.200000", Text: "second"},
		{Platform: "slack", MessageID: "1712345999.bot", Text: "bot response", IsBot: true},
	}
	if err := AppendMessages(path, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}

	// Should return the last real Slack ts, skipping ".bot" entry
	if id := LastPlatformTS(path); id != "1712345678.200000" {
		t.Errorf("LastPlatformTS = %q, want %q", id, "1712345678.200000")
	}
}

func TestLastPlatformTS_AllReal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	msgs := []HistoryMessage{
		{Platform: "slack", MessageID: "1712345678.100000", Text: "first"},
		{Platform: "slack", MessageID: "1712345678.200000", Text: "second"},
	}
	if err := AppendMessages(path, msgs); err != nil {
		t.Fatal(err)
	}

	if id := LastPlatformTS(path); id != "1712345678.200000" {
		t.Errorf("LastPlatformTS = %q, want %q", id, "1712345678.200000")
	}
}

func TestLastPlatformTS_OnlyBot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	msgs := []HistoryMessage{
		{Platform: "slack", MessageID: "1712345999.bot", Text: "bot only"},
	}
	if err := AppendMessages(path, msgs); err != nil {
		t.Fatal(err)
	}

	// No real ts → empty
	if id := LastPlatformTS(path); id != "" {
		t.Errorf("expected empty, got %q", id)
	}
}

func TestWriteMessages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	msgs1 := []HistoryMessage{
		{Platform: "slack", MessageID: "1.0", Text: "old"},
	}
	if err := AppendMessages(path, msgs1); err != nil {
		t.Fatal(err)
	}

	// Overwrite
	msgs2 := []HistoryMessage{
		{Platform: "slack", MessageID: "2.0", Text: "new"},
	}
	if err := WriteMessages(path, msgs2); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].MessageID != "2.0" {
		t.Errorf("expected overwritten data, got %+v", loaded)
	}
}

func TestLoadHistoryMissingFile(t *testing.T) {
	msgs, err := LoadHistory("/nonexistent/path.jsonl")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if msgs != nil {
		t.Errorf("expected nil msgs, got %v", msgs)
	}
}

// TestScanJSONLLines_BasicAndEOF guards the contract LoadHistory /
// LastMessage / LastPlatformTS rely on: every newline-terminated line is
// passed to onLine, the final line is delivered even without a trailing
// newline, and the scan returns nil on a clean EOF.
func TestScanJSONLLines_BasicAndEOF(t *testing.T) {
	input := "line one\nline two\nline three no newline"
	var got []string
	err := ScanJSONLLines(strings.NewReader(input), func(line []byte) {
		got = append(got, strings.TrimRight(string(line), "\n"))
	})
	if err != nil {
		t.Fatalf("ScanJSONLLines: %v", err)
	}
	if len(got) != 3 || got[0] != "line one" || got[1] != "line two" || got[2] != "line three no newline" {
		t.Errorf("unexpected lines: %#v", got)
	}
}

// TestScanJSONLLines_LongLineCrossingBufioBuffer ensures that lines longer
// than bufio's default buffer (4 KiB) are correctly reassembled across
// ReadSlice/ErrBufferFull boundaries — this is the regression boundary the
// switch from ReadBytes to a bounded scanner introduced.
func TestScanJSONLLines_LongLineCrossingBufioBuffer(t *testing.T) {
	long := strings.Repeat("a", 100_000) // > default bufio buffer
	input := "first\n" + long + "\nlast\n"
	var got []string
	err := ScanJSONLLines(strings.NewReader(input), func(line []byte) {
		got = append(got, strings.TrimRight(string(line), "\n"))
	})
	if err != nil {
		t.Fatalf("ScanJSONLLines: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(got))
	}
	if got[1] != long {
		t.Errorf("long line corrupted: len=%d want=%d", len(got[1]), len(long))
	}
	if got[0] != "first" || got[2] != "last" {
		t.Errorf("surrounding lines: %q / %q", got[0], got[2])
	}
}

// TestScanJSONLLines_OversizeLineReturnsErr is the actual security-relevant
// behavior of this helper: a single line that exceeds MaxJSONLLineBytes
// must abort with ErrLineTooLarge instead of growing the buffer
// unboundedly. We use a bytes.Reader so the test is deterministic even on
// systems where allocations would otherwise be slow.
func TestScanJSONLLines_OversizeLineReturnsErr(t *testing.T) {
	// One line larger than the cap. Use a reader that doesn't actually
	// allocate the full payload upfront (compose-on-demand) so the test
	// itself stays cheap.
	huge := bytes.Repeat([]byte("x"), MaxJSONLLineBytes+1)
	var got int
	err := ScanJSONLLines(bytes.NewReader(huge), func(line []byte) {
		got++
	})
	if !errors.Is(err, ErrLineTooLarge) {
		t.Fatalf("expected ErrLineTooLarge, got %v", err)
	}
	if got != 0 {
		t.Errorf("onLine should not be called for an oversize line, got %d", got)
	}
}

// TestScanJSONLLines_EmptyLinesIgnored ensures the helper treats lone
// "\n\n" runs without crashing and passes through empty lines that the
// caller can choose to skip (JSON unmarshal of "" is a no-op already).
func TestScanJSONLLines_EmptyLinesIgnored(t *testing.T) {
	input := "a\n\nb\n"
	var got []string
	err := ScanJSONLLines(strings.NewReader(input), func(line []byte) {
		got = append(got, strings.TrimRight(string(line), "\n"))
	})
	if err != nil {
		t.Fatalf("ScanJSONLLines: %v", err)
	}
	if len(got) != 3 || got[0] != "a" || got[1] != "" || got[2] != "b" {
		t.Errorf("unexpected lines: %#v", got)
	}
}

// TestLoadHistory_OversizeLinePropagates ensures that a corrupted history
// file with an oversize line surfaces as an error to LoadHistory's caller
// (instead of silently truncating the result or OOMing). LastMessage /
// LastPlatformTS deliberately swallow this error — see their docstrings —
// so they're not covered here.
func TestLoadHistory_OversizeLinePropagates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.jsonl")
	// One valid record followed by a line > MaxJSONLLineBytes so we can
	// confirm the error fires on the second line and the first is reachable
	// via partial-state tools if we ever change the contract.
	first := `{"platform":"slack","messageId":"1.0","userName":"alice","text":"hi"}` + "\n"
	huge := strings.Repeat("x", MaxJSONLLineBytes+1) + "\n"
	if err := os.WriteFile(path, []byte(first+huge), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadHistory(path)
	if err == nil {
		t.Fatalf("expected error on oversize line, got nil")
	}
	if !errors.Is(err, ErrLineTooLarge) {
		t.Errorf("expected ErrLineTooLarge, got %v", err)
	}
}

func TestAppendCreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c", "test.jsonl")

	msgs := []HistoryMessage{{Platform: "test", MessageID: "1"}}
	if err := AppendMessages(path, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}
