package chathistory

import (
	"os"
	"path/filepath"
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
