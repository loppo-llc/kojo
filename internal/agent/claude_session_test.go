package agent

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// newTestSession builds a claudeSession with no real process; stdout is fed
// via the returned pipe writer and stdin writes are discarded.
func newTestSession(t *testing.T, bg BackgroundTurnFunc) (*claudeSession, *io.PipeWriter) {
	t.Helper()
	b := &ClaudeBackend{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), sessions: map[string]*claudeSession{}, onBackgroundTurn: bg}
	pr, pw := io.Pipe()
	s := &claudeSession{
		b:            b,
		agentID:      "test-agent",
		logger:       b.logger,
		fingerprint:  "fp",
		stdinW:       &claudeStdinWriter{w: nopWriteCloser{io.Discard}},
		state:        sessIdle,
		lastActivity: time.Now(),
	}
	b.sessions["test-agent"] = s
	go s.readLoop(pr)
	return s, pw
}

func collect(t *testing.T, ch <-chan ChatEvent, timeout time.Duration) []ChatEvent {
	t.Helper()
	var evs []ChatEvent
	deadline := time.After(timeout)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return evs
			}
			evs = append(evs, e)
		case <-deadline:
			t.Fatalf("timed out collecting events; got %d so far", len(evs))
			return evs
		}
	}
}

func TestSessionSolicitedTurn(t *testing.T) {
	s, pw := newTestSession(t, nil)
	sink, err := s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "hi", false, false)
	if err != nil {
		t.Fatalf("startTurn: %v", err)
	}
	io.WriteString(pw, `{"type":"system","subtype":"init","session_id":"sess-1"}`+"\n")
	io.WriteString(pw, `{"type":"assistant","message":{"content":[{"type":"text","text":"hello world"}]},"session_id":"sess-1"}`+"\n")
	io.WriteString(pw, `{"type":"result","subtype":"success","result":"hello world","session_id":"sess-1"}`+"\n")

	evs := collect(t, sink, 3*time.Second)
	var gotDone bool
	for _, e := range evs {
		if e.Type == "done" {
			gotDone = true
			if e.Message == nil || e.Message.Content != "hello world" {
				t.Fatalf("done message wrong: %+v", e.Message)
			}
		}
	}
	if !gotDone {
		t.Fatalf("no done event; got %+v", evs)
	}
	// Back to idle and session id captured.
	s.mu.Lock()
	st, sid := s.state, s.sessionID
	s.mu.Unlock()
	if st != sessIdle {
		t.Fatalf("state = %v, want idle", st)
	}
	if sid != "sess-1" {
		t.Fatalf("sessionID = %q", sid)
	}
	pw.Close()
}

func TestSessionUnsolicitedTurn(t *testing.T) {
	bgCh := make(chan (<-chan ChatEvent), 1)
	_, pw := newTestSession(t, func(agentID string, events <-chan ChatEvent, _ AnswerFunc, _ func()) {
		bgCh <- events
	})
	// No solicited turn: feed an unsolicited notification segment.
	io.WriteString(pw, `{"type":"system","subtype":"init"}`+"\n")
	io.WriteString(pw, `{"type":"assistant","message":{"content":[{"type":"text","text":"background done"}]}}`+"\n")
	io.WriteString(pw, `{"type":"result","subtype":"success","result":"background done","origin":{"kind":"task-notification"}}`+"\n")

	var events <-chan ChatEvent
	select {
	case events = <-bgCh:
	case <-time.After(3 * time.Second):
		t.Fatal("background handler not invoked")
	}
	evs := collect(t, events, 3*time.Second)
	var gotDone bool
	for _, e := range evs {
		if e.Type == "done" && e.Message != nil && e.Message.Content == "background done" {
			gotDone = true
		}
	}
	if !gotDone {
		t.Fatalf("unsolicited done missing; got %+v", evs)
	}
	pw.Close()
}

// A task-notification result that races into an active solicited turn must be
// routed to a separate unsolicited turn: it must neither complete nor pollute
// the user turn's content.
func TestSessionNotifResultDuringSolicitedTurn(t *testing.T) {
	bgCh := make(chan (<-chan ChatEvent), 1)
	s, pw := newTestSession(t, func(agentID string, events <-chan ChatEvent, _ AnswerFunc, _ func()) {
		bgCh <- events
	})
	sink, err := s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "hi", false, false)
	if err != nil {
		t.Fatalf("startTurn: %v", err)
	}
	io.WriteString(pw, `{"type":"assistant","message":{"content":[{"type":"text","text":"user answer"}]}}`+"\n")
	// A background subagent notification result interleaves BEFORE the user
	// turn's own result. It must not complete or pollute the user turn.
	io.WriteString(pw, `{"type":"result","subtype":"success","result":"BACKGROUND-NOTIF","origin":{"kind":"task-notification"}}`+"\n")
	// The user turn's own result (no origin) completes it.
	io.WriteString(pw, `{"type":"result","subtype":"success","result":"user answer"}`+"\n")

	evs := collect(t, sink, 3*time.Second)
	var done *ChatEvent
	for i := range evs {
		if evs[i].Type == "done" {
			done = &evs[i]
		}
	}
	if done == nil || done.Message == nil {
		t.Fatalf("no solicited done; got %+v", evs)
	}
	if done.Message.Content != "user answer" {
		t.Fatalf("solicited turn polluted by notification: content=%q", done.Message.Content)
	}
	// The notification must have been surfaced on its own background turn.
	select {
	case bgEvents := <-bgCh:
		bgEvs := collect(t, bgEvents, 3*time.Second)
		var gotNotif bool
		for _, e := range bgEvs {
			if e.Type == "done" && e.Message != nil && e.Message.Content == "BACKGROUND-NOTIF" {
				gotNotif = true
			}
		}
		if !gotNotif {
			t.Fatalf("notification not surfaced on background turn; got %+v", bgEvs)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("background handler not invoked for racing notification")
	}
	pw.Close()
}

func TestSessionEmptyUnsolicitedDropped(t *testing.T) {
	bgCh := make(chan (<-chan ChatEvent), 1)
	s, pw := newTestSession(t, func(agentID string, events <-chan ChatEvent, _ AnswerFunc, _ func()) {
		bgCh <- events
	})
	// Stray content-less segment: should open+close the bg turn with no done.
	io.WriteString(pw, `{"type":"system","subtype":"init"}`+"\n")
	io.WriteString(pw, `{"type":"result","subtype":"success","origin":{"kind":"task-notification"}}`+"\n")

	select {
	case events := <-bgCh:
		evs := collect(t, events, 3*time.Second)
		for _, e := range evs {
			if e.Type == "done" {
				t.Fatalf("empty unsolicited turn should not emit done; got %+v", evs)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("background handler not invoked")
	}
	_ = s
	pw.Close()
}

func TestSessionEOFMidTurnErrors(t *testing.T) {
	s, pw := newTestSession(t, nil)
	sink, err := s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "hi", false, false)
	if err != nil {
		t.Fatalf("startTurn: %v", err)
	}
	io.WriteString(pw, `{"type":"system","subtype":"init"}`+"\n")
	// Process dies before result.
	pw.Close()

	evs := collect(t, sink, 3*time.Second)
	var gotErr bool
	for _, e := range evs {
		if e.Type == "error" {
			gotErr = true
		}
	}
	if !gotErr {
		t.Fatalf("expected error event on mid-turn EOF; got %+v", evs)
	}
	s.mu.Lock()
	dead := s.state == sessDead
	s.mu.Unlock()
	if !dead {
		t.Fatal("session should be dead after EOF")
	}
}

func TestSessionBusyRejectsSecondTurn(t *testing.T) {
	s, pw := newTestSession(t, nil)
	defer pw.Close()
	if _, err := s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "hi", false, false); err != nil {
		t.Fatalf("startTurn 1: %v", err)
	}
	if _, err := s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "again", false, false); err != ErrAgentBusy {
		t.Fatalf("startTurn 2 err = %v, want ErrAgentBusy", err)
	}
}

func TestSessionStrayIdleEventsDoNotOpenTurn(t *testing.T) {
	called := make(chan struct{}, 1)
	s, pw := newTestSession(t, func(agentID string, events <-chan ChatEvent, _ AnswerFunc, _ func()) {
		called <- struct{}{}
		for range events {
		}
	})
	// Complete a solicited turn.
	sink, err := s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "hi", false, false)
	if err != nil {
		t.Fatalf("startTurn: %v", err)
	}
	io.WriteString(pw, `{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`+"\n")
	io.WriteString(pw, `{"type":"result","subtype":"success","result":"ok"}`+"\n")
	collect(t, sink, 3*time.Second)

	// Stray idle events must NOT open an unsolicited turn.
	io.WriteString(pw, `{"type":"rate_limit_event"}`+"\n")
	io.WriteString(pw, `{"type":"system","subtype":"init"}`+"\n")
	select {
	case <-called:
		t.Fatal("background handler wrongly invoked for stray idle events")
	case <-time.After(500 * time.Millisecond):
	}
	s.mu.Lock()
	idle := s.state == sessIdle
	s.mu.Unlock()
	if !idle {
		t.Fatal("session should remain idle after stray events")
	}
	pw.Close()
}

func TestFingerprintExcludesSessionFlag(t *testing.T) {
	base := []string{"-p", "--model", "haiku", "--system-prompt", "you are x"}
	a := fingerprintArgs(append(append([]string{}, base...), "--session-id", "uuid-1"))
	b := fingerprintArgs(append(append([]string{}, base...), "--resume", "uuid-1"))
	if a != b {
		t.Fatal("fingerprint must ignore --session-id vs --resume flip")
	}
	c := fingerprintArgs(append(append([]string{}, base...), "--resume", "uuid-2"))
	if a != c {
		t.Fatal("fingerprint must ignore session id value")
	}
	d := fingerprintArgs([]string{"-p", "--model", "sonnet", "--system-prompt", "you are x", "--resume", "uuid-1"})
	if a == d {
		t.Fatal("fingerprint must change when model differs")
	}
}

// syncBuffer is a threadsafe writer capturing stdin lines for assertions.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Close() error { return nil }

// Aborting a steered turn must arm the queued-steer reaper: the CLI does not
// discard steer lines queued on stdin, so the unsolicited auto-turn it starts
// right after the aborted turn's result must be interrupted on open.
func TestSessionQueuedSteerReaperInterruptsAutoTurn(t *testing.T) {
	bgCh := make(chan (<-chan ChatEvent), 2)
	s, pw := newTestSession(t, func(agentID string, events <-chan ChatEvent, _ AnswerFunc, _ func()) {
		bgCh <- events
	})
	stdin := &syncBuffer{}
	s.stdinW = &claudeStdinWriter{w: stdin}

	ctx, cancel := context.WithCancel(context.Background())
	sink, err := s.startTurn(ctx, &Agent{ID: "test-agent"}, "hi", false, false)
	if err != nil {
		t.Fatalf("startTurn: %v", err)
	}
	go func() {
		for range sink {
		}
	}()

	// Steer the running turn, then abort it.
	if err := s.turnSteerFunc()("queued steer"); err != nil {
		t.Fatalf("steer: %v", err)
	}
	cancel()

	// The abort watcher must write an interrupt control_request.
	waitFor := func(what string, n int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Count(stdin.String(), what) >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("stdin did not receive %d× %q; stdin=%q", n, what, stdin.String())
	}
	waitFor(`"subtype":"interrupt"`, 1)

	// The CLI answers the interrupt with the aborted turn's result.
	io.WriteString(pw, `{"type":"result","subtype":"error_during_execution","session_id":"sess-1"}`+"\n")

	// Reaper must be armed with the steer count once the turn ends.
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		armed := s.killQueuedSteerTurns
		s.mu.Unlock()
		if armed == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reaper not armed; killQueuedSteerTurns=%d", armed)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The queued steer line now auto-starts an unsolicited turn — the reaper
	// must interrupt it immediately (a second interrupt on stdin).
	io.WriteString(pw, `{"type":"assistant","message":{"content":[{"type":"text","text":"auto turn"}]}}`+"\n")
	waitFor(`"subtype":"interrupt"`, 2)

	s.mu.Lock()
	remaining := s.killQueuedSteerTurns
	s.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("killQueuedSteerTurns = %d, want 0", remaining)
	}
	pw.Close()
}

// A fresh solicited turn must disarm the reaper so a user's new turn is never
// killed by a stale abort.
func TestSessionQueuedSteerReaperDisarmedBySolicitedTurn(t *testing.T) {
	s, pw := newTestSession(t, nil)
	s.mu.Lock()
	s.killQueuedSteerTurns = 2
	s.killQueuedSteerUntil = time.Now().Add(15 * time.Second)
	s.mu.Unlock()

	if _, err := s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "hi", false, false); err != nil {
		t.Fatalf("startTurn: %v", err)
	}
	s.mu.Lock()
	armed := s.killQueuedSteerTurns
	s.mu.Unlock()
	if armed != 0 {
		t.Fatalf("killQueuedSteerTurns = %d, want 0 after solicited startTurn", armed)
	}
	pw.Close()
}

// An unsolicited turn's abort func must interrupt the CLI while the turn is
// live, and must be a no-op once the turn ended (never hitting a successor).
func TestSessionUnsolicitedAbortInterruptsCLI(t *testing.T) {
	abortCh := make(chan func(), 2)
	s, pw := newTestSession(t, func(agentID string, events <-chan ChatEvent, _ AnswerFunc, abort func()) {
		abortCh <- abort
		go func() {
			for range events {
			}
		}()
	})
	stdin := &syncBuffer{}
	s.stdinW = &claudeStdinWriter{w: stdin}

	// Open an unsolicited turn.
	io.WriteString(pw, `{"type":"assistant","message":{"content":[{"type":"text","text":"bg turn"}]}}`+"\n")
	var abort func()
	select {
	case abort = <-abortCh:
	case <-time.After(3 * time.Second):
		t.Fatal("background handler not invoked")
	}
	if abort == nil {
		t.Fatal("abort func is nil for a live unsolicited turn")
	}

	// abort is async (it must never block Manager.Abort behind a wedged
	// stdin) — poll for the interrupt write.
	abort()
	interruptDeadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(stdin.String(), `"subtype":"interrupt"`) {
		if time.Now().After(interruptDeadline) {
			t.Fatalf("abort did not write an interrupt; stdin=%q", stdin.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// End the turn; a late abort must now be a no-op.
	io.WriteString(pw, `{"type":"result","subtype":"success","result":"bg turn","origin":{"kind":"task-notification"}}`+"\n")
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		idle := s.state != sessInTurn
		s.mu.Unlock()
		if idle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unsolicited turn did not end")
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := strings.Count(stdin.String(), `"subtype":"interrupt"`)
	abort()
	// Async no-op: give the goroutine time to (incorrectly) write before
	// asserting nothing changed.
	time.Sleep(200 * time.Millisecond)
	if got := strings.Count(stdin.String(), `"subtype":"interrupt"`); got != before {
		t.Fatalf("stale abort wrote an interrupt (%d -> %d)", before, got)
	}
	pw.Close()
}
