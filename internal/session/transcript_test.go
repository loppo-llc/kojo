package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTranscript_ProjectDirConversion(t *testing.T) {
	tests := []struct {
		workDir string
		want    string
	}{
		{"/Users/foo/bar", "-Users-foo-bar"},
		{"/home/user/project", "-home-user-project"},
		{"/", "-"},
	}

	for _, tt := range tests {
		got := pathToProjectDir(tt.workDir)
		if got != tt.want {
			t.Errorf("pathToProjectDir(%q) = %q, want %q", tt.workDir, got, tt.want)
		}
	}
}

func TestTranscript_BuildPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot get home dir")
	}

	got := buildTranscriptPath("/Users/foo/project", "abc-123")
	want := filepath.Join(home, ".claude", "projects", "-Users-foo-project", "abc-123.jsonl")
	if got != want {
		t.Errorf("buildTranscriptPath:\n got  %s\n want %s", got, want)
	}
}

func TestTranscript_BuildPathEmpty(t *testing.T) {
	got := buildTranscriptPath("/any", "")
	if got != "" {
		t.Errorf("expected empty path for empty sessionID, got %q", got)
	}
}

func TestTranscript_ScanUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	content := `{"type":"assistant","message":{"usage":{"input_tokens":1000,"output_tokens":500}}}
{"type":"assistant","message":{"usage":{"input_tokens":2000,"output_tokens":800}}}
{"type":"user"}
{"type":"assistant","message":{"usage":{"input_tokens":5000,"output_tokens":1200}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	tm := &TranscriptMonitor{}
	tokens := tm.scanUsage(path, 0)

	// Should return the maximum total tokens found (5000+1200 = 6200)
	if tokens != 6200 {
		t.Errorf("expected 6200 tokens, got %d", tokens)
	}
}

func TestTranscript_ScanUsageCacheTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	// Realistic Claude usage with cache tokens
	content := `{"type":"assistant","message":{"usage":{"input_tokens":1,"cache_creation_input_tokens":462,"cache_read_input_tokens":61917,"output_tokens":361}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	tm := &TranscriptMonitor{}
	tokens := tm.scanUsage(path, 0)

	// 1 + 462 + 61917 + 361 = 62741
	if tokens != 62741 {
		t.Errorf("expected 62741 tokens (including cache), got %d", tokens)
	}
}

func TestTranscript_ScanUsageTopLevelUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	content := `{"type":"result","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":200,"cache_read_input_tokens":3000}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	tm := &TranscriptMonitor{}
	tokens := tm.scanUsage(path, 0)

	// 100 + 50 + 200 + 3000 = 3350
	if tokens != 3350 {
		t.Errorf("expected 3350 tokens, got %d", tokens)
	}
}

func TestTranscript_IncrementalRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	initial := `{"type":"assistant","message":{"usage":{"input_tokens":1000,"output_tokens":500}}}
`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	tm := &TranscriptMonitor{}
	tokens := tm.scanUsage(path, 0)
	if tokens != 1500 {
		t.Errorf("first scan: expected 1500 tokens, got %d", tokens)
	}

	tm.mu.Lock()
	offset := tm.lastOffset
	tm.mu.Unlock()
	if offset == 0 {
		t.Error("expected non-zero offset after first scan")
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"assistant","message":{"usage":{"input_tokens":3000,"output_tokens":1000}}}
`)
	f.Close()

	tokens = tm.scanUsage(path, offset)
	if tokens != 4000 {
		t.Errorf("incremental scan: expected 4000 tokens, got %d", tokens)
	}
}

func TestTranscriptUsage_Total(t *testing.T) {
	u := &transcriptUsage{
		InputTokens:              1,
		OutputTokens:             361,
		CacheCreationInputTokens: 462,
		CacheReadInputTokens:     61917,
	}
	if got := u.Total(); got != 62741 {
		t.Errorf("Total() = %d, want 62741", got)
	}
}

// pathToProjectDir converts a workDir to Claude's project dir format.
func pathToProjectDir(workDir string) string {
	return replaceSlashes(filepath.ToSlash(workDir))
}

func replaceSlashes(s string) string {
	result := make([]byte, len(s))
	for i := range s {
		if s[i] == '/' {
			result[i] = '-'
		} else {
			result[i] = s[i]
		}
	}
	return string(result)
}
