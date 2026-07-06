package agent

import (
	"context"
	"io"
	"log/slog"
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
	sink, err := s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "hi")
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
	_, pw := newTestSession(t, func(agentID string, events <-chan ChatEvent) {
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
	s, pw := newTestSession(t, func(agentID string, events <-chan ChatEvent) {
		bgCh <- events
	})
	sink, err := s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "hi")
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
	s, pw := newTestSession(t, func(agentID string, events <-chan ChatEvent) {
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
	sink, err := s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "hi")
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
	if _, err := s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "hi"); err != nil {
		t.Fatalf("startTurn 1: %v", err)
	}
	if _, err := s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "again"); err != ErrAgentBusy {
		t.Fatalf("startTurn 2 err = %v, want ErrAgentBusy", err)
	}
}

func TestSessionStrayIdleEventsDoNotOpenTurn(t *testing.T) {
	called := make(chan struct{}, 1)
	s, pw := newTestSession(t, func(agentID string, events <-chan ChatEvent) {
		called <- struct{}{}
		for range events {
		}
	})
	// Complete a solicited turn.
	sink, err := s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "hi")
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
