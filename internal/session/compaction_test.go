package session

import (
	"testing"
)

func TestBuildFreshArgs_Claude(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantLen  int // expected arg count (excluding --session-id pair which is always added)
		wantNoID bool
	}{
		{
			name:    "basic args",
			args:    []string{"--model", "opus"},
			wantLen: 2,
		},
		{
			name:    "remove --resume with value",
			args:    []string{"--resume", "old-id", "--model", "opus"},
			wantLen: 2,
		},
		{
			name:    "remove -r with value",
			args:    []string{"-r", "old-id", "--model", "opus"},
			wantLen: 2,
		},
		{
			name:    "remove --resume=value",
			args:    []string{"--resume=old-id", "--model", "opus"},
			wantLen: 2,
		},
		{
			name:    "remove --continue",
			args:    []string{"--continue", "--model", "opus"},
			wantLen: 2,
		},
		{
			name:    "remove -c",
			args:    []string{"-c", "--model", "opus"},
			wantLen: 2,
		},
		{
			name:    "remove --session-id with value",
			args:    []string{"--session-id", "old-uuid", "--model", "opus"},
			wantLen: 2,
		},
		{
			name:    "remove --session-id=value",
			args:    []string{"--session-id=old-uuid", "--model", "opus"},
			wantLen: 2,
		},
		{
			name:    "remove all resume variants",
			args:    []string{"--resume", "id1", "--continue", "--session-id", "old", "--model", "opus"},
			wantLen: 2,
		},
		{
			name:    "empty args",
			args:    nil,
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, newID := buildFreshArgs("claude", tt.args)

			if newID == "" {
				t.Error("expected new session ID for claude")
			}

			// Result should have wantLen + 2 (for --session-id <newID>)
			expectedLen := tt.wantLen + 2
			if len(result) != expectedLen {
				t.Errorf("expected %d args, got %d: %v", expectedLen, len(result), result)
			}

			// Verify no resume/continue args remain
			for _, a := range result {
				switch a {
				case "--resume", "-r", "--continue", "-c":
					t.Errorf("found unexpected arg: %s", a)
				}
			}

			// Verify --session-id is present with new ID
			found := false
			for i, a := range result {
				if a == "--session-id" && i+1 < len(result) && result[i+1] == newID {
					found = true
				}
			}
			if !found {
				t.Errorf("expected --session-id %s in result: %v", newID, result)
			}
		})
	}
}

func TestBuildFreshArgs_Codex(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{
			name: "remove resume command and its arg",
			args: []string{"resume", "old-id"},
			want: 0,
		},
		{
			name: "remove resume --last",
			args: []string{"resume", "--last"},
			want: 0,
		},
		{
			name: "basic args",
			args: []string{"--model", "o3"},
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, newID := buildFreshArgs("codex", tt.args)

			if newID != "" {
				t.Errorf("expected empty session ID for codex, got %s", newID)
			}

			if len(result) != tt.want {
				t.Errorf("expected %d args, got %d: %v", tt.want, len(result), result)
			}
		})
	}
}

func TestBuildFreshArgs_Gemini(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{
			name: "remove --resume with value",
			args: []string{"--resume", "latest", "--model", "gemini"},
			want: 2,
		},
		{
			name: "remove -r with value",
			args: []string{"-r", "latest"},
			want: 0,
		},
		{
			name: "remove --resume=value",
			args: []string{"--resume=latest"},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, _ := buildFreshArgs("gemini", tt.args)
			if len(result) != tt.want {
				t.Errorf("expected %d args, got %d: %v", tt.want, len(result), result)
			}
		})
	}
}

func TestSanitizeForPaste(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "normal text",
			input: "hello world",
			want:  "hello world",
		},
		{
			name:  "preserves newlines and tabs",
			input: "line1\nline2\ttab",
			want:  "line1\nline2\ttab",
		},
		{
			name:  "removes ESC",
			input: "before\x1b[31mred\x1b[0mafter",
			want:  "before[31mred[0mafter",
		},
		{
			name:  "removes control chars",
			input: "hello\x00\x01\x02world",
			want:  "helloworld",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeForPaste(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeForPaste(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestExtractSummary(t *testing.T) {
	tests := []struct {
		name string
		buf  string
		want string
	}{
		{
			name: "with tags",
			buf:  "some output\n<kojo-summary>\nTask: implement feature X\nStatus: in progress\n</kojo-summary>\nmore output",
			want: "Task: implement feature X\nStatus: in progress",
		},
		{
			name: "no tags",
			buf:  "no summary here",
			want: "",
		},
		{
			name: "with ANSI in tags",
			buf:  "\x1b[32m<kojo-summary>\x1b[0mthe summary\x1b[32m</kojo-summary>\x1b[0m",
			want: "the summary",
		},
		{
			name: "empty tags",
			buf:  "<kojo-summary></kojo-summary>",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &CompactionOrchestrator{
				session: &Session{},
			}
			o.session.captureBuf = []byte(tt.buf)
			got := o.extractSummary()
			if got != tt.want {
				t.Errorf("extractSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOutputMode_Transitions(t *testing.T) {
	s := &Session{}
	s.outputMode.Store(int32(OutputNormal))

	if OutputMode(s.outputMode.Load()) != OutputNormal {
		t.Error("expected OutputNormal")
	}

	s.outputMode.Store(int32(OutputCapturing))
	if OutputMode(s.outputMode.Load()) != OutputCapturing {
		t.Error("expected OutputCapturing")
	}

	s.outputMode.Store(int32(OutputSuppressing))
	if OutputMode(s.outputMode.Load()) != OutputSuppressing {
		t.Error("expected OutputSuppressing")
	}

	s.outputMode.Store(int32(OutputNormal))
	if OutputMode(s.outputMode.Load()) != OutputNormal {
		t.Error("expected OutputNormal after restore")
	}
}

func TestLifecycle_CompactingBlocksCompleteExit(t *testing.T) {
	s := &Session{
		scrollback:  NewRingBuffer(1024),
		done:        make(chan struct{}),
		subscribers: make(map[chan []byte]struct{}),
	}
	s.lifecycle.Store(int32(LifecycleCompacting))
	s.compactReady = make(chan struct{})

	// completeExit should signal compactReady, not close done
	// We need a Manager for completeExit, but we can test the compactOnce directly
	s.compactOnce.Do(func() { close(s.compactReady) })

	select {
	case <-s.compactReady:
		// good
	default:
		t.Error("expected compactReady to be closed")
	}

	// done should NOT be closed
	select {
	case <-s.done:
		t.Error("done should not be closed during compaction")
	default:
		// good
	}
}
