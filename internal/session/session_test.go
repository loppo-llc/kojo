package session

import (
	"strings"
	"testing"
)

func newTestSession(yolo bool) *Session {
	return &Session{
		YoloMode:    yolo,
		subscribers: make(map[chan []byte]struct{}),
		done:        make(chan struct{}),
	}
}

func TestCheckYolo_BasicMatch(t *testing.T) {
	s := newTestSession(true)
	prompt := "Do you want to proceed? ❯ 1. Yes"
	approval, _ := s.CheckYolo([]byte(prompt))
	if approval == nil {
		t.Fatal("expected match for basic prompt")
	}
}

func TestCheckYolo_LongPromptWithANSI(t *testing.T) {
	s := newTestSession(true)

	// Simulate a long command display with heavy ANSI codes that would
	// push the prompt out of a 512-byte buffer but fits in 4096.
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("\x1b[1;32m")  // SGR bold green
		b.WriteString("\x1b[?25l")   // DEC hide cursor
		b.WriteString("some output line\r\n")
		b.WriteString("\x1b[0m")     // SGR reset
		b.WriteString("\x1b[?25h")   // DEC show cursor
	}
	b.WriteString("Do you want to proceed? ❯ 1. Yes")
	data := []byte(b.String())

	if len(data) < 512 {
		t.Fatalf("test data too short (%d bytes), expected >512", len(data))
	}

	approval, _ := s.CheckYolo(data)
	if approval == nil {
		t.Fatal("expected match for long prompt with ANSI codes")
	}
}

func TestCheckYolo_ChunkSplit(t *testing.T) {
	s := newTestSession(true)

	// Feed prompt question and options in separate chunks to simulate
	// readLoop delivering data across multiple reads.
	var preamble strings.Builder
	for i := 0; i < 50; i++ {
		preamble.WriteString("\x1b[1;32m")
		preamble.WriteString("output line content\r\n")
		preamble.WriteString("\x1b[0m")
	}
	preamble.WriteString("Do you want to proceed?")
	chunk1 := []byte(preamble.String())

	if len(chunk1) < 512 {
		t.Fatalf("chunk1 too short (%d bytes), need >512 to test buffer", len(chunk1))
	}

	// First chunk: no match yet (missing "1. Yes")
	approval, _ := s.CheckYolo(chunk1)
	if approval != nil {
		t.Fatal("should not match without options")
	}

	// Second chunk: options arrive — with 512 buffer the "Do you" part
	// would have been truncated, but 4096 retains it.
	chunk2 := []byte("\x1b[1m\r\n  ❯ \x1b[32m1. Yes\x1b[0m\r\n    2. No\r\n")
	approval, _ = s.CheckYolo(chunk2)
	if approval == nil {
		t.Fatal("expected match after second chunk with 4096 buffer")
	}
}

func TestCheckYolo_BufferTruncation(t *testing.T) {
	s := newTestSession(true)

	// Fill the buffer with data exceeding yoloTailSize to verify
	// that only the trailing yoloTailSize bytes are kept.
	filler := make([]byte, yoloTailSize+100)
	for i := range filler {
		filler[i] = 'x'
	}
	s.CheckYolo(filler)

	// Now send the prompt — should match since the filler is just 'x's
	// and the prompt fits in the retained tail.
	prompt := []byte("Do you want to proceed? ❯ 1. Yes")
	approval, _ := s.CheckYolo(prompt)
	if approval == nil {
		t.Fatal("expected match after buffer truncation")
	}
}

func TestCheckYolo_NoRematch(t *testing.T) {
	s := newTestSession(true)

	prompt := "Do you want to proceed? ❯ 1. Yes"
	approval, _ := s.CheckYolo([]byte(prompt))
	if approval == nil {
		t.Fatal("expected first match")
	}

	// After a match, yoloTail is cleared. Sending non-prompt data
	// should not produce another match.
	approval, _ = s.CheckYolo([]byte("some follow-up output"))
	if approval != nil {
		t.Fatal("expected no re-match after tail was cleared")
	}
}

func TestCheckYolo_NoMatch(t *testing.T) {
	s := newTestSession(true)
	approval, _ := s.CheckYolo([]byte("some random output without any prompt"))
	if approval != nil {
		t.Fatal("expected no match for non-prompt output")
	}
}

func TestCheckYolo_Disabled(t *testing.T) {
	s := newTestSession(false)
	prompt := "Do you want to proceed? ❯ 1. Yes"
	approval, _ := s.CheckYolo([]byte(prompt))
	if approval != nil {
		t.Fatal("expected no match when yolo mode is disabled")
	}
}

func TestAnsiRe_StripsDECPrivateMode(t *testing.T) {
	input := "\x1b[?25hvisible\x1b[?25l"
	clean := ansiRe.ReplaceAll([]byte(input), []byte(""))
	if string(clean) != "visible" {
		t.Fatalf("expected 'visible', got %q", string(clean))
	}
}

func TestAnsiRe_StripsTildeTerminated(t *testing.T) {
	// Function key sequences like F5 (\x1b[15~) should be stripped.
	input := "\x1b[15~visible\x1b[2~"
	clean := ansiRe.ReplaceAll([]byte(input), []byte(""))
	if string(clean) != "visible" {
		t.Fatalf("expected 'visible', got %q", string(clean))
	}
}
