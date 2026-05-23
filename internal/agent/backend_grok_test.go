package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// silentLogger discards all logs in tests.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// collectGrokEvents runs parseGrokStream against the given lines and
// returns all ChatEvents that were emitted plus the final result.
func collectGrokEvents(t *testing.T, lines ...string) ([]ChatEvent, *grokStreamResult) {
	t.Helper()
	r := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var events []ChatEvent
	send := func(e ChatEvent) bool {
		events = append(events, e)
		return true
	}
	res := parseGrokStream(r, silentLogger(), send)
	return events, res
}

func TestParseGrokStream_TextOnly(t *testing.T) {
	events, res := collectGrokEvents(t,
		`{"type":"text","data":"Hi"}`,
		`{"type":"text","data":" there"}`,
		`{"type":"end","stopReason":"EndTurn","sessionId":"019e5527-7b78-70f3-b488-2f005dbcc2fe","requestId":"r1"}`,
	)
	if res.cancelled {
		t.Fatal("unexpected cancel")
	}
	if res.text != "Hi there" {
		t.Errorf("text = %q, want %q", res.text, "Hi there")
	}
	if res.thinking != "" {
		t.Errorf("thinking = %q, want empty", res.thinking)
	}
	if res.sessionID != "019e5527-7b78-70f3-b488-2f005dbcc2fe" {
		t.Errorf("sessionID = %q, want 019e5527-7b78-70f3-b488-2f005dbcc2fe", res.sessionID)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	for _, e := range events {
		if e.Type != "text" {
			t.Errorf("event type = %q, want text", e.Type)
		}
	}
}

func TestParseGrokStream_ThoughtAndText(t *testing.T) {
	events, res := collectGrokEvents(t,
		`{"type":"thought","data":"reasoning"}`,
		`{"type":"text","data":"answer"}`,
		`{"type":"end","stopReason":"EndTurn","sessionId":"s","requestId":"r"}`,
	)
	if res.thinking != "reasoning" {
		t.Errorf("thinking = %q", res.thinking)
	}
	if res.text != "answer" {
		t.Errorf("text = %q", res.text)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].Type != "thinking" || events[0].Delta != "reasoning" {
		t.Errorf("event[0] = %+v, want thinking/reasoning", events[0])
	}
	if events[1].Type != "text" || events[1].Delta != "answer" {
		t.Errorf("event[1] = %+v, want text/answer", events[1])
	}
}

func TestParseGrokStream_SkipsMalformedAndUnknown(t *testing.T) {
	_, res := collectGrokEvents(t,
		``,
		`not-json`,
		`{"type":"phase_changed","phase":"streaming"}`, // unknown type ignored
		`{"type":"text","data":"ok"}`,
		`{"type":"end","sessionId":"s"}`,
	)
	if res.text != "ok" {
		t.Errorf("text = %q, want ok", res.text)
	}
	if res.sessionID != "s" {
		t.Errorf("sessionID = %q", res.sessionID)
	}
}

func TestParseGrokStream_CancelStopsEarly(t *testing.T) {
	r := strings.NewReader(strings.Join([]string{
		`{"type":"text","data":"a"}`,
		`{"type":"text","data":"b"}`,
		`{"type":"text","data":"c"}`,
		`{"type":"end","sessionId":"s"}`,
	}, "\n") + "\n")
	var count int
	send := func(e ChatEvent) bool {
		count++
		return count < 2 // refuse the second event onward
	}
	res := parseGrokStream(r, silentLogger(), send)
	if !res.cancelled {
		t.Fatal("expected cancelled=true")
	}
	if count != 2 {
		t.Errorf("send called %d times, want 2", count)
	}
	// Only the accepted delta should be in res.text — parseGrokStream
	// snapshots the buffer at the cancel point.
	if res.text != "ab" {
		t.Errorf("text = %q, want %q (cancel snapshot includes the rejected delta)", res.text, "ab")
	}
}

func TestGrokEscapePath(t *testing.T) {
	// Verified empirically against grok 0.1.216: each input is the
	// cwd grok ran in, each output is the directory name it wrote
	// under ~/.grok/sessions/.
	cases := []struct {
		in, want string
	}{
		{
			"/private/tmp/grok-test",
			"%2Fprivate%2Ftmp%2Fgrok-test",
		},
		{
			"/private/tmp/grok-test-special+ε=ω@&dir",
			"%2Fprivate%2Ftmp%2Fgrok-test-special%2B%CE%B5%3D%CF%89%40%26dir",
		},
		{
			"/Users/loppo",
			"%2FUsers%2Floppo",
		},
		{
			"/path-with_unreserved.chars~ok",
			"%2Fpath-with_unreserved.chars~ok",
		},
	}
	for _, tc := range cases {
		got := grokEscapePath(tc.in)
		if got != tc.want {
			t.Errorf("grokEscapePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGrokSessionDir_HonorsGROK_HOME(t *testing.T) {
	t.Setenv("GROK_HOME", "/custom/grok-root")
	got := grokSessionDir("/Users/loppo")
	want := filepath.Join("/custom/grok-root", "sessions", "%2FUsers%2Floppo")
	if got != want {
		t.Errorf("grokSessionDir(GROK_HOME) = %q, want %q", got, want)
	}
}

func TestGrokSessionDir_FallsBackToHome(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home, _ := os.UserHomeDir()
	if home == "" {
		t.Skip("no HOME")
	}
	got := grokSessionDir(home)
	want := filepath.Join(home, ".grok", "sessions", grokEscapePath(home))
	if got != want {
		t.Errorf("grokSessionDir(home) = %q, want %q", got, want)
	}
}

func TestReadWriteGrokSessionID(t *testing.T) {
	tmp := t.TempDir()
	const validID = "019e5527-7b78-70f3-b488-2f005dbcc2fe"

	if got := readGrokSessionID(tmp); got != "" {
		t.Errorf("readGrokSessionID(empty) = %q, want \"\"", got)
	}
	writeGrokSessionID(tmp, validID, silentLogger())
	if got := readGrokSessionID(tmp); got != validID {
		t.Errorf("readGrokSessionID after write = %q, want %q", got, validID)
	}
	// Removing the file restores "" (clearGrokSession behaviour).
	os.Remove(grokSessionIDFile(tmp))
	if got := readGrokSessionID(tmp); got != "" {
		t.Errorf("readGrokSessionID after remove = %q, want \"\"", got)
	}
}

func TestWriteGrokSessionID_RejectsNonUUID(t *testing.T) {
	tmp := t.TempDir()
	for _, bad := range []string{
		"",
		"abc-123",
		"--cwd=/etc",
		"../../etc/passwd",
		"019e5527-7b78-70f3-b488-2f005dbcc2fe\n--bad", // newline + extra arg
		"019E5527-7B78-70F3-B488-2F005DBCC2FE",        // uppercase
	} {
		writeGrokSessionID(tmp, bad, silentLogger())
		if got := readGrokSessionID(tmp); got != "" {
			t.Errorf("write+read round-trip of %q leaked %q (should reject)", bad, got)
		}
	}
}

func TestReadGrokSessionID_DropsPoisonedFile(t *testing.T) {
	tmp := t.TempDir()
	// Write a poisoned file directly, bypassing the writer guard.
	if err := os.MkdirAll(filepath.Dir(grokSessionIDFile(tmp)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grokSessionIDFile(tmp), []byte("--cwd=/etc/passwd"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readGrokSessionID(tmp); got != "" {
		t.Errorf("readGrokSessionID(poisoned) = %q, want \"\"", got)
	}
	// And the poisoned file should be removed so it can't keep
	// triggering the rejection on every turn.
	if _, err := os.Stat(grokSessionIDFile(tmp)); !os.IsNotExist(err) {
		t.Errorf("poisoned session_id file not removed (err=%v)", err)
	}
}

func TestIsStaleSessionError(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// observed in grok 0.1.x stdout on --resume <missing>
		{"Couldn't create session: Session does not exist", true},
		{"Error: No session found for current directory", true},
		{"NO SESSION FOUND", true}, // case-insensitive
		{"", false},
		{"some other error", false},
	}
	for _, tc := range cases {
		if got := isStaleSessionError(tc.in); got != tc.want {
			t.Errorf("isStaleSessionError(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestRemoveGrokSessionDir_RejectsTraversal(t *testing.T) {
	t.Setenv("GROK_HOME", t.TempDir())
	cwd := "/Users/test"
	dir := grokSessionDir(cwd)
	if dir == "" {
		t.Fatal("grokSessionDir empty")
	}
	// Pre-create a sibling that traversal would attack.
	parent := filepath.Dir(dir)
	sibling := filepath.Join(parent, "other-victim")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	// Attempt removal with hostile sessionID; the validator should
	// refuse before any path-join happens.
	removeGrokSessionDir(cwd, "../other-victim")
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("sibling removed despite traversal attempt: %v", err)
	}
	// And a non-UUID is a no-op too.
	removeGrokSessionDir(cwd, "not-a-uuid")
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("sibling removed by non-UUID input: %v", err)
	}
}

func TestHasGrokSession_FalseWhenMissing(t *testing.T) {
	// A path that almost certainly has no grok session.
	if hasGrokSession("/tmp/this-does-not-exist-kojo-test-" + randomSuffix()) {
		t.Error("hasGrokSession returned true for nonexistent cwd")
	}
}

func randomSuffix() string {
	// Cheap unique-enough suffix; collisions would only weaken the
	// test, not break it.
	return filepath.Base(os.TempDir()) + "-x"
}
