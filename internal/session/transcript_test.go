package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTranscript_ProjectDirConversion(t *testing.T) {
	// Test the workDir → projectDir path conversion
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

func TestTranscript_ScanUsage(t *testing.T) {
	// Create a temp JSONL file
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

func TestTranscript_IncrementalRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	// Write initial content
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

	// Check offset was updated
	tm.mu.Lock()
	offset := tm.lastOffset
	tm.mu.Unlock()
	if offset == 0 {
		t.Error("expected non-zero offset after first scan")
	}

	// Append more content
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"assistant","message":{"usage":{"input_tokens":3000,"output_tokens":1000}}}
`)
	f.Close()

	// Scan from offset — should only see new entry
	tokens = tm.scanUsage(path, offset)
	if tokens != 4000 {
		t.Errorf("incremental scan: expected 4000 tokens, got %d", tokens)
	}
}

// pathToProjectDir converts a workDir to Claude's project dir format.
// This is the same logic used in TranscriptMonitor.findTranscriptPath.
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
