package agent

import "testing"

func TestReplyTagFilter_Basic(t *testing.T) {
	f := &ReplyTagFilter{}
	got := f.Feed("考え中...<reply>こんにちは！</reply>")
	if got != "こんにちは！" {
		t.Errorf("expected 'こんにちは！', got %q", got)
	}
	// After </reply>, further text should be discarded
	got = f.Feed("more text")
	if got != "" {
		t.Errorf("expected empty after </reply>, got %q", got)
	}
}

func TestReplyTagFilter_SplitAcrossDeltas(t *testing.T) {
	f := &ReplyTagFilter{}

	// Open tag split across deltas
	got := f.Feed("thinking...<rep")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
	got = f.Feed("ly>Hello")
	if got != "Hello" {
		t.Errorf("expected 'Hello', got %q", got)
	}
	got = f.Feed(" world</reply>")
	if got != " world" {
		t.Errorf("expected ' world', got %q", got)
	}
}

func TestReplyTagFilter_CloseTagSplit(t *testing.T) {
	f := &ReplyTagFilter{}

	f.Feed("<reply>")
	got := f.Feed("Hello</rep")
	if got != "Hello" {
		t.Errorf("expected 'Hello', got %q", got)
	}
	got = f.Feed("ly>after")
	if got != "" {
		t.Errorf("expected empty after close tag, got %q", got)
	}
}

func TestReplyTagFilter_NoTags_GracefulDegradation(t *testing.T) {
	f := &ReplyTagFilter{}
	f.Feed("Hello ")
	f.Feed("world")
	got := f.Flush()
	if got != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", got)
	}
}

func TestReplyTagFilter_NoCloseTag(t *testing.T) {
	f := &ReplyTagFilter{}
	got := f.Feed("thinking...<reply>Hello world")
	if got != "Hello world" {
		t.Errorf("Feed: expected 'Hello world', got %q", got)
	}
	// Flush should return empty since Feed already emitted everything
	got = f.Flush()
	if got != "" {
		t.Errorf("Flush: expected empty, got %q", got)
	}
}

func TestReplyTagFilter_NoCloseTag_BufferedPartial(t *testing.T) {
	f := &ReplyTagFilter{}
	// Text ending with partial close tag stays buffered
	got := f.Feed("<reply>Hello</re")
	if got != "Hello" {
		t.Errorf("Feed: expected 'Hello', got %q", got)
	}
	// Flush should return the buffered partial
	got = f.Flush()
	if got != "</re" {
		t.Errorf("Flush: expected '</re', got %q", got)
	}
}

func TestReplyTagFilter_EmptyReply(t *testing.T) {
	f := &ReplyTagFilter{}
	got := f.Feed("<reply></reply>")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestReplyTagFilter_HTMLInsideReply(t *testing.T) {
	f := &ReplyTagFilter{}
	got := f.Feed("<reply>Use <b>bold</b> text</reply>")
	if got != "Use <b>bold</b> text" {
		t.Errorf("expected 'Use <b>bold</b> text', got %q", got)
	}
}

func TestReplyTagFilter_TextAfterCloseDiscarded(t *testing.T) {
	f := &ReplyTagFilter{}
	got := f.Feed("<reply>answer</reply>internal notes")
	if got != "answer" {
		t.Errorf("expected 'answer', got %q", got)
	}
	got = f.Flush()
	if got != "" {
		t.Errorf("expected empty from Flush, got %q", got)
	}
}

func TestReplyTagFilter_StreamingDeltas(t *testing.T) {
	f := &ReplyTagFilter{}
	var all string

	// Simulate streaming character by character for the reply part
	deltas := []string{"<reply>", "H", "e", "l", "l", "o", "</reply>"}
	for _, d := range deltas {
		all += f.Feed(d)
	}
	if all != "Hello" {
		t.Errorf("expected 'Hello', got %q", all)
	}
}

func TestReplyTagFilter_FlushAfterDone(t *testing.T) {
	f := &ReplyTagFilter{}
	f.Feed("<reply>done</reply>")
	got := f.Flush()
	if got != "" {
		t.Errorf("expected empty from Flush after done, got %q", got)
	}
}

func TestPartialSuffix(t *testing.T) {
	tests := []struct {
		s    string
		tag  string
		want int
	}{
		{"hello<", "<reply>", 1},
		{"hello<r", "<reply>", 2},
		{"hello<rep", "<reply>", 4},
		{"hello<reply", "<reply>", 6},
		{"hello", "<reply>", 0},
		{"<reply>", "<reply>", 0}, // full match is not a partial suffix
		{"text</", "</reply>", 2},
		{"text</repl", "</reply>", 6},
	}
	for _, tt := range tests {
		got := partialSuffix(tt.s, tt.tag)
		if got != tt.want {
			t.Errorf("partialSuffix(%q, %q) = %d, want %d", tt.s, tt.tag, got, tt.want)
		}
	}
}
