package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/chathistory"
)

// A line just over the cap, terminated by a newline, followed by normal
// lines. Strict mode must stop with ErrLineTooLarge; skip mode must drop it
// and keep scanning.
func oversizedThenNormal() string {
	big := strings.Repeat("x", chathistory.MaxJSONLLineBytes+1)
	return "first\n" + big + "\nsecond\nthird\n"
}

func TestJSONLScanner_StrictStopsOnOversized(t *testing.T) {
	s := newCodexLineScanner(strings.NewReader(oversizedThenNormal()))
	if !s.Scan() || s.Text() != "first" {
		t.Fatalf("expected first line, got %q (scan ok=%v)", s.Text(), s.Err())
	}
	if s.Scan() {
		t.Fatalf("strict mode should stop on oversized line, got %q", s.Text())
	}
	if !errors.Is(s.Err(), chathistory.ErrLineTooLarge) {
		t.Fatalf("expected ErrLineTooLarge, got %v", s.Err())
	}
}

func TestJSONLScanner_SkipModeDropsOversizedAndContinues(t *testing.T) {
	s := newSkippingLineScanner(strings.NewReader(oversizedThenNormal()))
	var got []string
	for s.Scan() {
		got = append(got, s.Text())
	}
	if s.Err() != nil {
		t.Fatalf("unexpected error: %v", s.Err())
	}
	want := []string{"first", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %v, want %v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d: got %q want %q", i, got[i], want[i])
		}
	}
	if s.Skipped() != 1 {
		t.Fatalf("Skipped() = %d, want 1", s.Skipped())
	}
}

func TestJSONLScanner_SkipModeMultipleOversized(t *testing.T) {
	big := strings.Repeat("y", chathistory.MaxJSONLLineBytes+100)
	in := big + "\nok1\n" + big + "\n" + big + "\nok2\n"
	s := newSkippingLineScanner(strings.NewReader(in))
	var got []string
	for s.Scan() {
		got = append(got, s.Text())
	}
	if len(got) != 2 || got[0] != "ok1" || got[1] != "ok2" {
		t.Fatalf("got %v, want [ok1 ok2]", got)
	}
	if s.Skipped() != 3 {
		t.Fatalf("Skipped() = %d, want 3", s.Skipped())
	}
}

func TestJSONLScanner_SkipModeOversizedFinalUnterminatedLine(t *testing.T) {
	big := strings.Repeat("z", chathistory.MaxJSONLLineBytes+1)
	s := newSkippingLineScanner(strings.NewReader("keep\n" + big)) // no trailing \n
	if !s.Scan() || s.Text() != "keep" {
		t.Fatalf("expected keep, got %q", s.Text())
	}
	if s.Scan() {
		t.Fatalf("expected EOF after dropped oversized tail, got %q", s.Text())
	}
	if s.Err() != nil {
		t.Fatalf("EOF should not surface as error, got %v", s.Err())
	}
	if s.Skipped() != 1 {
		t.Fatalf("Skipped() = %d, want 1", s.Skipped())
	}
}

// Large-but-under-cap lines (the actual 7/9 incident shape: a ~2MB base64
// tool_result that killed the old 1MB bufio.Scanner cap) must be returned
// intact in both modes.
func TestJSONLScanner_LargeLineUnderCapIsReturned(t *testing.T) {
	big := strings.Repeat("a", 2*1024*1024) // 2MB < 10MB cap
	for name, s := range map[string]*jsonlLineScanner{
		"strict": newCodexLineScanner(strings.NewReader(big + "\nnext\n")),
		"skip":   newSkippingLineScanner(strings.NewReader(big + "\nnext\n")),
	} {
		if !s.Scan() || len(s.Text()) != len(big) {
			t.Fatalf("%s: expected %d-byte line, got %d (err=%v)", name, len(big), len(s.Text()), s.Err())
		}
		if !s.Scan() || s.Text() != "next" {
			t.Fatalf("%s: expected next line, got %q", name, s.Text())
		}
	}
}
