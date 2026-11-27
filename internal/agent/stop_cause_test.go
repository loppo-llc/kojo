package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func cancelledWith(cause error) context.Context {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	return ctx
}

func TestStopCauseOfClassification(t *testing.T) {
	expired, cancelExpired := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelExpired()
	expiredCause, cancelExpiredCause := context.WithTimeoutCause(context.Background(), time.Nanosecond, KojoStop("制限時間 5 分超過"))
	defer cancelExpiredCause()
	<-expired.Done()
	<-expiredCause.Done()
	child, cancelChild := context.WithCancel(cancelledWith(UserStop()))
	defer cancelChild()

	for _, tc := range []struct {
		name   string
		ctx    context.Context
		ok     bool
		byUser bool
		reason string
	}{
		{"user", cancelledWith(UserStop()), true, true, ""},
		{"kojo reason", cancelledWith(KojoStop(stopReasonReset)), true, false, stopReasonReset},
		{"kojo without reason", cancelledWith(KojoStop("")), true, false, stopReasonInternal},
		{"wrapped cause", cancelledWith(errors.Join(errors.New("x"), KojoStop(stopReasonFork))), true, false, stopReasonFork},
		{"agent requested", cancelledWith(AgentRequestedStop()), false, false, ""},
		{"noted by caller", cancelledWith(ErrStopNotedByCaller), false, false, ""},
		{"plain cancel defaults to kojo", cancelledWith(nil), true, false, stopReasonInternal},
		{"untyped cause defaults to kojo", cancelledWith(errors.New("boom")), true, false, stopReasonInternal},
		{"deadline is a kojo timeout", expired, true, false, stopReasonTimeout},
		{"deadline with cause", expiredCause, true, false, "制限時間 5 分超過"},
		{"child inherits parent cause", child, true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := stopCauseOf(tc.ctx)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (cause %v)", ok, tc.ok, c)
			}
			if !ok {
				return
			}
			if c.ByUser != tc.byUser || (!tc.byUser && c.Reason != tc.reason) {
				t.Fatalf("cause = %+v, want byUser=%v reason=%q", c, tc.byUser, tc.reason)
			}
		})
	}
}

func TestTurnStopCause(t *testing.T) {
	alive := context.Background()
	if _, ok := turnStopCause(alive, turnEnd{terminal: true}); ok {
		t.Fatal("a normal done is not a stop")
	}
	if _, ok := turnStopCause(alive, turnEnd{terminal: true, errMsg: "API error"}); ok {
		t.Fatal("a backend error is not a stop")
	}
	if _, ok := turnStopCause(alive, turnEnd{}); ok {
		t.Fatal("a stream ending on its own is not a stop")
	}
	if c, ok := turnStopCause(alive, turnEnd{terminal: true, errMsg: ErrMsgCancelled}); !ok || c.ByUser || c.Reason != stopReasonRemote {
		t.Fatalf("cancelled terminal with a live context = %+v, %v", c, ok)
	}
	if c, ok := turnStopCause(alive, turnEnd{terminal: true, errMsg: "codex turn interrupted"}); !ok || c.ByUser || c.Reason != stopReasonCodex {
		t.Fatalf("codex interrupt = %+v, %v", c, ok)
	}
	if c, ok := turnStopCause(cancelledWith(UserStop()), turnEnd{}); !ok || !c.ByUser {
		t.Fatalf("user-cancelled turn = %+v, %v", c, ok)
	}
	// A terminal that arrived while the context was alive wins over a later
	// cancel (the turn had already finished).
	if _, ok := turnStopCause(cancelledWith(UserStop()), turnEnd{terminal: true}); ok {
		t.Fatal("a turn that finished before the cancel is not a stop")
	}
}

func TestStopNoteText(t *testing.T) {
	if got := stopNoteText(&StopCause{ByUser: true}); got != "" {
		t.Fatalf("user stop must leave no note, got %q", got)
	}
	want := "[kojo] 直前のターンはユーザーではなく kojo が停止しました（理由: エージェントのリセット）。会話ログに \"interrupted by user\" 等とあってもユーザーの操作ではありません。"
	if got := stopNoteText(&StopCause{Reason: stopReasonReset}); got != want {
		t.Fatalf("kojo note = %q, want %q", got, want)
	}
}

func TestCronRunContextTimeoutCause(t *testing.T) {
	if c, ok := stopCauseOf(cancelledWith(cronTimeoutCause(15 * time.Minute))); !ok || c.ByUser || c.Reason != "制限時間 15 分超過" {
		t.Fatalf("cron timeout cause = %+v, %v", c, ok)
	}
	ctx, cancel, timeout := cronRunContext(1)
	defer cancel()
	if dl, ok := ctx.Deadline(); !ok || timeout != time.Minute || time.Until(dl) > time.Minute {
		t.Fatalf("cron deadline = %v, %v, %v", dl, ok, timeout)
	}
	// Cancelling before the deadline (normal end) is not a timeout stop:
	// the turn ended first, so it records a terminal.
	cancel()
	if _, ok := turnStopCause(ctx, turnEnd{terminal: true}); ok {
		t.Fatal("finished cron turn reported as stopped")
	}
}

func TestWithoutStopNotesAndCommitRewritten(t *testing.T) {
	old := stopNoteText(&StopCause{Reason: stopReasonReset})
	text := "[kojo] other note\n" + old
	if got := withoutStopNotes(text); got != "[kojo] other note" {
		t.Fatalf("withoutStopNotes = %q", got)
	}
	if got := withoutLines("a\nb\na\nc", "a\nc"); got != "b\na" {
		t.Fatalf("withoutLines = %q", got)
	}
}

// stopNoteBackend records each turn's message and context. A blocking turn
// runs until its context is cancelled; otherwise it finishes at once.
//
// waitStart makes the next Chat call wait for its context and fail before
// any stream (a backend still waiting for response headers); hold makes the
// next turn keep its stream open after done until the channel is closed.
type stopNoteBackend struct {
	msgs      chan string
	ctxs      chan context.Context
	block     chan bool
	waitStart chan bool
	hold      chan chan struct{}
}

func newStopNoteBackend() *stopNoteBackend {
	return &stopNoteBackend{msgs: make(chan string, 16), ctxs: make(chan context.Context, 16), block: make(chan bool, 16),
		waitStart: make(chan bool, 16), hold: make(chan chan struct{}, 16)}
}

func (b *stopNoteBackend) Name() string    { return "stopnote-test" }
func (b *stopNoteBackend) Available() bool { return true }
func (b *stopNoteBackend) Chat(ctx context.Context, _ *Agent, msg, _ string, _ ChatOptions) (<-chan ChatEvent, error) {
	block := false
	select {
	case block = <-b.block:
	default:
	}
	var hold chan struct{}
	select {
	case hold = <-b.hold:
	default:
	}
	b.msgs <- msg
	b.ctxs <- ctx
	select {
	case w := <-b.waitStart:
		if w {
			<-ctx.Done()
			return nil, ctx.Err()
		}
	default:
	}
	out := make(chan ChatEvent, 1)
	go func() {
		defer close(out)
		if block {
			<-ctx.Done()
			return
		}
		m := newAssistantMessage()
		m.Content = "ok"
		out <- ChatEvent{Type: "done", Message: m}
		if hold != nil {
			<-hold
		}
	}()
	return out, nil
}

func newStopNoteManager(t *testing.T, dir string) (*Manager, *stopNoteBackend, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	m, err := NewManager(slog.Default())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	b := newStopNoteBackend()
	const id = "ag_stopnote"
	a, ok := m.Get(id)
	if !ok {
		a = &Agent{ID: id, Name: "StopNote", Tool: b.Name()}
		if err := m.store.Upsert(a); err != nil {
			t.Fatalf("seed agent: %v", err)
		}
	}
	m.mu.Lock()
	m.agents[id] = &Agent{ID: id, Name: "StopNote", Tool: b.Name()}
	m.backends[b.Name()] = b
	m.mu.Unlock()
	_ = a
	return m, b, id
}

func waitIdle(t *testing.T, m *Manager, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.WaitChatIdle(ctx, id); err != nil {
		t.Fatalf("WaitChatIdle: %v", err)
	}
}

func drainEvents(ch <-chan ChatEvent) {
	for range ch {
	}
}

func mainTurn(t *testing.T, m *Manager, b *stopNoteBackend, id string, block bool) (string, context.Context, <-chan ChatEvent) {
	t.Helper()
	b.block <- block
	events, err := m.Chat(context.Background(), id, "hello", "user", nil, BusySourceUser)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	return <-b.msgs, <-b.ctxs, events
}

// The WebUI stop button: the main turn is a user stop (no note, and it clears
// an undelivered older one) and concurrent thread turns keep running. A kojo
// stop of the thread turn leaves a keyed note that is delivered once.
func TestAbortByUserNotesAndOneTimeDelivery(t *testing.T) {
	m, b, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	key := id + ":ext.test:k1"

	m.putMainStopNote(id, stopNoteText(&StopCause{Reason: stopReasonReset}))
	_, mainCtx, mainEvents := mainTurn(t, m, b, id, true)
	b.block <- true
	osEvents, err := m.ChatOneShot(context.Background(), id, "external", OneShotOpts{SessionKey: key})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	<-b.msgs
	osCtx := <-b.ctxs

	m.AbortByUser(id)
	drainEvents(mainEvents)
	if c, ok := stopCauseOf(mainCtx); !ok || !c.ByUser {
		t.Fatalf("main turn cause = %+v, %v", c, ok)
	}
	if err := osCtx.Err(); err != nil {
		t.Fatalf("main-chat stop cancelled the thread turn: %v", err)
	}
	if got := m.peekKeyedNote(id, key); got != "" {
		t.Fatalf("main-chat stop left a keyed note: %q", got)
	}

	m.cancelOneShots(id, KojoStop(stopReasonTruncate))
	drainEvents(osEvents)
	waitIdle(t, m, id)
	if c, ok := stopCauseOf(osCtx); !ok || c.ByUser || c.Reason != stopReasonTruncate {
		t.Fatalf("one-shot cause = %+v, %v", c, ok)
	}
	if got := m.PendingMainStopNote(id); got != "" {
		t.Fatalf("user stop left a main note: %q", got)
	}
	if got := m.peekKeyedNote(id, key); got != stopNoteText(&StopCause{Reason: stopReasonTruncate}) {
		t.Fatalf("keyed note = %q", got)
	}

	// Next main turn carries no note.
	msg, _, events := mainTurn(t, m, b, id, false)
	drainEvents(events)
	waitIdle(t, m, id)
	if strings.Contains(msg, stopNotePrefix) {
		t.Fatalf("note delivered twice: %q", msg)
	}

	// Next keyed turn carries its note once, too.
	osEvents, err = m.ChatOneShot(context.Background(), id, "external", OneShotOpts{SessionKey: key})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	msg = <-b.msgs
	<-b.ctxs
	drainEvents(osEvents)
	if !strings.Contains(msg, stopNoteText(&StopCause{Reason: stopReasonTruncate})) {
		t.Fatalf("keyed turn lacks the note: %q", msg)
	}
	if got := m.peekKeyedNote(id, key); got != "" {
		t.Fatalf("keyed note not consumed: %q", got)
	}
	osEvents, err = m.ChatOneShot(context.Background(), id, "external", OneShotOpts{SessionKey: key})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	msg = <-b.msgs
	<-b.ctxs
	drainEvents(osEvents)
	if strings.Contains(msg, stopNotePrefix) {
		t.Fatalf("keyed note delivered twice: %q", msg)
	}
}

// A kojo stop of a keyed turn, a caller-noted turn, an agent-requested stop
// and a caller-provided TurnNote.
func TestKeyedTurnStopNotes(t *testing.T) {
	m, b, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	key := id + ":ext.test:k2"

	run := func(opts OneShotOpts, stop func()) string {
		t.Helper()
		b.block <- stop != nil
		ev, err := m.ChatOneShot(context.Background(), id, "external", opts)
		if err != nil {
			t.Fatalf("ChatOneShot: %v", err)
		}
		msg := <-b.msgs
		<-b.ctxs
		if stop != nil {
			stop()
		}
		drainEvents(ev)
		return msg
	}

	run(OneShotOpts{SessionKey: key}, func() { m.CancelOneShotsForAgent(id, KojoStop(stopReasonSwitch)) })
	want := stopNoteText(&StopCause{Reason: stopReasonSwitch})
	if got := m.peekKeyedNote(id, key); got != want {
		t.Fatalf("keyed note = %q, want %q", got, want)
	}
	// A second stop before any delivery replaces the first.
	m.commitKeyedNote(id, key, want)
	run(OneShotOpts{SessionKey: key}, func() { m.CancelOneShotsForAgent(id, KojoStop(stopReasonSwitch)) })
	// A user stop leaves no note and clears the undelivered one.
	run(OneShotOpts{SessionKey: key}, func() { m.CancelOneShotsForAgent(id, UserStop()) })
	if got := m.peekKeyedNote(id, key); got != "" {
		t.Fatalf("user stop left a note: %q", got)
	}
	run(OneShotOpts{SessionKey: key + "x"}, nil) // unrelated key
	m.RecordKeyedStopCause(id, key, KojoStop(stopReasonReset))
	if got := m.peekKeyedNote(id, key); got != stopNoteText(&StopCause{Reason: stopReasonReset}) {
		t.Fatalf("latest stop must replace the undelivered one: %q", got)
	}
	m.commitKeyedNote(id, key, m.peekKeyedNote(id, key))

	// The Hub records relayed turns; the holder records none.
	run(OneShotOpts{SessionKey: key, StopNotedByCaller: true}, func() { m.CancelOneShotsForAgent(id, UserStop()) })
	if got := m.peekKeyedNote(id, key); got != "" {
		t.Fatalf("caller-noted turn left a note: %q", got)
	}
	// A stop the agent asked for leaves none.
	run(OneShotOpts{SessionKey: key}, func() { m.CancelOneShotsForAgent(id, AgentRequestedStop()) })
	if got := m.peekKeyedNote(id, key); got != "" {
		t.Fatalf("agent-requested stop left a note: %q", got)
	}
	// A caller's TurnNote is injected.
	msg := run(OneShotOpts{SessionKey: key, TurnNote: "[kojo] hub note"}, nil)
	if !strings.Contains(msg, "[kojo] hub note") {
		t.Fatalf("TurnNote missing: %q", msg)
	}
}

func TestResetGuardStopsWithCause(t *testing.T) {
	m, b, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	_, ctx, events := mainTurn(t, m, b, id, true)
	release, err := m.acquireResetGuard(id, KojoStop(stopReasonReset))
	if err != nil {
		t.Fatal(err)
	}
	drainEvents(events)
	release()
	waitIdle(t, m, id)
	if c, ok := stopCauseOf(ctx); !ok || c.Reason != stopReasonReset {
		t.Fatalf("cause = %+v, %v", c, ok)
	}
	if got := m.PendingMainStopNote(id); got != stopNoteText(&StopCause{Reason: stopReasonReset}) {
		t.Fatalf("note = %q", got)
	}
	m.dropMainStopNote(id)
	if got := m.PendingMainStopNote(id); got != "" {
		t.Fatalf("dropped note still pending: %q", got)
	}
}

// Shutdown is the most common kojo stop: the main-chat note must survive the
// restart and reach the first turn after it.
func TestMainStopNoteSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	m, b, id := newStopNoteManager(t, dir)
	_, ctx, events := mainTurn(t, m, b, id, true)
	m.schedulersStarted = true
	m.Shutdown()
	drainEvents(events)
	waitIdle(t, m, id)
	if c, ok := stopCauseOf(ctx); !ok || c.Reason != stopReasonShutdown {
		t.Fatalf("cause = %+v, %v", c, ok)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	m2, b2, _ := newStopNoteManager(t, dir)
	t.Cleanup(func() { _ = m2.Close() })
	want := stopNoteText(&StopCause{Reason: stopReasonShutdown})
	if got := m2.PendingMainStopNote(id); got != want {
		t.Fatalf("note after restart = %q, want %q", got, want)
	}
	msg, _, events := mainTurn(t, m2, b2, id, false)
	drainEvents(events)
	waitIdle(t, m2, id)
	if !strings.Contains(msg, want) {
		t.Fatalf("first turn after restart lacks the note: %q", msg)
	}
	if got := m2.PendingMainStopNote(id); got != "" {
		t.Fatalf("note not consumed: %q", got)
	}
}

func TestThreadStopCauses(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	var got error
	gdm.threadCancelMu.Lock()
	gdm.threadCancels["g1"] = func(c error) { got = c }
	gdm.threadCancelMu.Unlock()
	if !gdm.StopThreadTurn("g1") {
		t.Fatal("StopThreadTurn found no turn")
	}
	if c, ok := stopCauseOf(cancelledWith(got)); !ok || !c.ByUser {
		t.Fatalf("thread stop button = %+v, %v", c, ok)
	}
	gdm.cancelThreadTurn("g1")
	if c, ok := stopCauseOf(cancelledWith(got)); !ok || c.ByUser || c.Reason != keyedThreadDeletedReason {
		t.Fatalf("thread deletion = %+v, %v", c, ok)
	}
}

func TestShutdownSkipsEndedTurn(t *testing.T) {
	m, _, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	// A turn whose stream is over but still holds the slot (post-turn
	// indexing) gets no shutdown note.
	_, cancel := context.WithCancelCause(context.Background())
	m.busyMu.Lock()
	m.busy[id] = busyEntry{cancelCause: cancel}
	m.busyMu.Unlock()
	m.markMainTurnEnded(id)
	m.schedulersStarted = true
	m.Shutdown()
	if got := m.PendingMainStopNote(id); got != "" {
		t.Fatalf("ended turn got a shutdown note: %q", got)
	}
}

func TestKeyedStopNoteSkipsDeletedThread(t *testing.T) {
	m, _, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	m.RecordKeyedStopCause(id, "groupdm:g1", KojoStop(keyedThreadDeletedReason))
	if got := m.peekKeyedNote(id, "groupdm:g1"); got != "" {
		t.Fatalf("deleted thread got a note: %q", got)
	}
	m.RecordKeyedStopCause(id, "groupdm:g1", KojoStop(stopReasonReset))
	if got := m.peekKeyedNote(id, "groupdm:g1"); got != stopNoteText(&StopCause{Reason: stopReasonReset}) {
		t.Fatalf("user stop note = %q", got)
	}
}

// Shutdown after the turn's done arrived (attachment persistence, indexing
// still running) leaves no stop note.
func TestShutdownAfterDoneLeavesNoNote(t *testing.T) {
	m, b, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	release := make(chan struct{})
	b.hold <- release
	_, _, events := mainTurn(t, m, b, id, false)
	for evt := range events {
		if evt.Type == "done" {
			break
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.busyMu.Lock()
		ended := m.busy[id].turnEnded
		m.busyMu.Unlock()
		if ended {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn not marked ended at done")
		}
		time.Sleep(time.Millisecond)
	}
	m.schedulersStarted = true
	m.Shutdown()
	close(release)
	drainEvents(events)
	waitIdle(t, m, id)
	if got := m.PendingMainStopNote(id); got != "" {
		t.Fatalf("finished turn got a note: %q", got)
	}
}

// A stop while the backend is still starting (no stream yet) is noted too.
func TestStopBeforeStreamIsNoted(t *testing.T) {
	m, b, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })

	b.waitStart <- true
	chatErr := make(chan error, 1)
	go func() {
		_, err := m.Chat(context.Background(), id, "hello", "user", nil, BusySourceUser)
		chatErr <- err
	}()
	<-b.msgs
	<-b.ctxs
	m.Abort(id, KojoStop(stopReasonReset))
	if err := <-chatErr; err == nil {
		t.Fatal("Chat succeeded")
	}
	waitIdle(t, m, id)
	mainNote := stopNoteText(&StopCause{Reason: stopReasonReset})
	if got := m.PendingMainStopNote(id); got != mainNote {
		t.Fatalf("main note = %q, want %q", got, mainNote)
	}

	// Next main turn carries the note once, inside the context block.
	msg, _, events := mainTurn(t, m, b, id, false)
	drainEvents(events)
	waitIdle(t, m, id)
	if !strings.Contains(msg, mainNote) {
		t.Fatalf("next turn message lacks the note: %q", msg)
	}
	if strings.HasPrefix(msg, mainNote) && strings.Contains(msg, "<context>") {
		t.Fatalf("note must sit inside the context block: %q", msg)
	}
	if got := m.PendingMainStopNote(id); got != "" {
		t.Fatalf("note not consumed: %q", got)
	}
	msg, _, events = mainTurn(t, m, b, id, false)
	drainEvents(events)
	waitIdle(t, m, id)
	if strings.Contains(msg, stopNotePrefix) {
		t.Fatalf("note delivered twice: %q", msg)
	}

	key := id + ":ext.test:k1"
	b.waitStart <- true
	osErr := make(chan error, 1)
	go func() {
		_, err := m.ChatOneShot(context.Background(), id, "external", OneShotOpts{SessionKey: key})
		osErr <- err
	}()
	<-b.msgs
	<-b.ctxs
	m.cancelOneShots(id, KojoStop(stopReasonSwitch))
	if err := <-osErr; err == nil {
		t.Fatal("ChatOneShot succeeded")
	}
	if got, want := m.peekKeyedNote(id, key), stopNoteText(&StopCause{Reason: stopReasonSwitch}); got != want {
		t.Fatalf("keyed note = %q, want %q", got, want)
	}
}

// A goal request's message is no turn the model reads: the pending note is
// neither injected nor consumed.
func TestGoalRequestKeepsStopNote(t *testing.T) {
	m, b, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	m.mu.Lock()
	m.agents[id].Tool = ToolCodex
	m.backends[ToolCodex] = b
	m.mu.Unlock()
	key := id + ":ext.test:k1"
	m.RecordKeyedStopCause(id, key, KojoStop(stopReasonReset))
	want := stopNoteText(&StopCause{Reason: stopReasonReset})
	events, err := m.ChatOneShot(context.Background(), id, "setup", OneShotOpts{
		SessionKey: key, Goal: &GoalRequest{Action: "start", Objective: "x"}, TurnNote: want,
	})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	if msg := <-b.msgs; strings.Contains(msg, stopNotePrefix) {
		t.Fatalf("goal request carried the note: %q", msg)
	}
	<-b.ctxs
	drainEvents(events)
	if got := m.peekKeyedNote(id, key); got != want {
		t.Fatalf("note consumed by a goal request: %q", got)
	}
}

// Shutdown keeps the cause of a turn another stop already cancelled (its
// goroutine still draining): its note is not overwritten by a shutdown one.
func TestShutdownKeepsEarlierStopCause(t *testing.T) {
	m, _, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	ctx, cancel := context.WithCancelCause(context.Background())
	m.busyMu.Lock()
	m.busy[id] = busyEntry{cancelCause: cancel, ctx: ctx}
	m.busyMu.Unlock()
	m.putMainStopNote(id, stopNoteText(&StopCause{Reason: stopReasonReset}))
	cancel(UserStop())
	m.schedulersStarted = true
	m.Shutdown()
	// The user stop leaves no note (clearing the older one); Shutdown must
	// not add one.
	if got := m.PendingMainStopNote(id); got != "" {
		t.Fatalf("note = %q, want none", got)
	}
}

// A relayed turn (StopNotedByCaller) stopped on this side ends with a
// cancelled terminal for the Hub; the Hub's own cancel adds none.
func TestRelayedTurnStopEndsWithCancelledTerminal(t *testing.T) {
	m, b, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	key := id + ":ext.test:k1"
	last := func(events <-chan ChatEvent) (ChatEvent, bool) {
		var evt ChatEvent
		seen := false
		for e := range events {
			evt, seen = e, true
		}
		return evt, seen
	}

	b.block <- true
	events, err := m.ChatOneShot(context.Background(), id, "hi", OneShotOpts{SessionKey: key, StopNotedByCaller: true})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	<-b.msgs
	<-b.ctxs
	m.cancelOneShots(id, KojoStop(stopReasonFork))
	if evt, ok := last(events); !ok || evt.Type != "error" || evt.ErrorMessage != ErrMsgCancelled {
		t.Fatalf("last event = %+v (%v)", evt, ok)
	}
	if got := m.peekKeyedNote(id, key); got != "" {
		t.Fatalf("holder recorded a note: %q", got)
	}

	b.block <- true
	ctx, cancel := context.WithCancelCause(context.Background())
	events, err = m.ChatOneShot(ctx, id, "hi", OneShotOpts{SessionKey: key, StopNotedByCaller: true})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	<-b.msgs
	<-b.ctxs
	cancel(ErrStopNotedByCaller)
	if evt, ok := last(events); ok {
		t.Fatalf("caller cancel produced %+v", evt)
	}
}

// The Hub records a relayed turn's stop even when the agent runs only on
// the holder (not in the Hub's Manager); a local record needs it live.
func TestRelayedStopNoteWithoutLocalAgent(t *testing.T) {
	m, _, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	m.mu.Lock()
	delete(m.agents, id)
	m.mu.Unlock()
	key := id + ":slack:C1:1.0"
	m.recordKeyedTurnStop(id, key, cancelledWith(KojoStop(stopReasonReset)), turnEnd{})
	if got := m.peekKeyedNote(id, key); got != "" {
		t.Fatalf("local record for a missing agent: %q", got)
	}
	m.RecordKeyedStop(id, key, cancelledWith(KojoStop(stopReasonReset)), false, "")
	if got, want := m.PeekKeyedNote(id, key), stopNoteText(&StopCause{Reason: stopReasonReset}); got != want {
		t.Fatalf("relayed stop note = %q, want %q", got, want)
	}
	m.RecordKeyedStopCause(id, key, KojoStop(stopReasonSwitch))
	if got, want := m.PeekKeyedNote(id, key), stopNoteText(&StopCause{Reason: stopReasonSwitch}); got != want {
		t.Fatalf("relayed stop cause note = %q, want %q", got, want)
	}
}

// A goal control request stopped mid-way leaves the pending note alone.
func TestGoalControlStopKeepsNote(t *testing.T) {
	m, b, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	m.mu.Lock()
	m.agents[id].Tool = ToolCodex
	m.backends[ToolCodex] = b
	m.mu.Unlock()
	key := id + ":ext.test:k1"
	m.RecordKeyedStopCause(id, key, KojoStop(stopReasonReset))
	want := stopNoteText(&StopCause{Reason: stopReasonReset})

	b.block <- true
	events, err := m.ChatOneShot(context.Background(), id, "status", OneShotOpts{SessionKey: key, Goal: &GoalRequest{Action: "status"}})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	<-b.msgs
	<-b.ctxs
	m.cancelOneShots(id, KojoStop(stopReasonSwitch))
	drainEvents(events)

	b.waitStart <- true
	osErr := make(chan error, 1)
	go func() {
		_, err := m.ChatOneShot(context.Background(), id, "status", OneShotOpts{SessionKey: key, Goal: &GoalRequest{Action: "status"}})
		osErr <- err
	}()
	<-b.msgs
	<-b.ctxs
	m.cancelOneShots(id, KojoStop(stopReasonSwitch))
	<-osErr
	if got := m.peekKeyedNote(id, key); got != want {
		t.Fatalf("note after goal control stop = %q", got)
	}
}

// A relayed turn stopped here before its stream started tells the caller
// so (ErrTurnStopped); the caller's own cancel does not.
func TestRelayedTurnStopBeforeStream(t *testing.T) {
	m, b, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	key := id + ":ext.test:k1"
	start := func(ctx context.Context, stop func()) error {
		b.waitStart <- true
		errc := make(chan error, 1)
		go func() {
			_, err := m.ChatOneShot(ctx, id, "hi", OneShotOpts{SessionKey: key, StopNotedByCaller: true})
			errc <- err
		}()
		<-b.msgs
		<-b.ctxs
		stop()
		return <-errc
	}
	if err := start(context.Background(), func() { m.cancelOneShots(id, KojoStop(stopReasonFork)) }); !errors.Is(err, ErrTurnStopped) {
		t.Fatalf("holder stop err = %v", err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	if err := start(ctx, func() { cancel(ErrStopNotedByCaller) }); err == nil || errors.Is(err, ErrTurnStopped) {
		t.Fatalf("caller cancel err = %v", err)
	}
	if got := m.peekKeyedNote(id, key); got != "" {
		t.Fatalf("relayed turn noted here: %q", got)
	}
}

func TestMarkStoppedTerminals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  string
	}{
		{"alive", nil, ""},
		{"kojo stop", KojoStop(stopReasonFork), ErrMsgCancelled},
		{"user stop", UserStop(), ErrMsgCancelled},
		{"caller end", ErrStopNotedByCaller, ""},
		{"agent stop", AgentRequestedStop(), ""},
	} {
		ctx, cancel := context.WithCancelCause(context.Background())
		if tc.cause != nil {
			cancel(tc.cause)
		}
		in := make(chan ChatEvent, 2)
		in <- ChatEvent{Type: "text", Delta: "x"}
		in <- ChatEvent{Type: "done"}
		close(in)
		var got []ChatEvent
		for e := range markStoppedTerminals(ctx, in) {
			got = append(got, e)
		}
		cancel(nil)
		if len(got) != 2 || got[0].ErrorMessage != "" || got[1].ErrorMessage != tc.want {
			t.Fatalf("%s: events = %+v", tc.name, got)
		}
	}
}

// A holder background turn relayed to a remote surface and stopped here
// ends with a cancelled terminal, whether its backend sent one or none.
func TestRemoteKeyedBackgroundStopTerminal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stop     bool
		terminal *ChatEvent
		want     *ChatEvent
	}{
		{"stopped, no terminal", true, nil, &ChatEvent{Type: "error", ErrorMessage: ErrMsgCancelled}},
		{"stopped, plain done", true, &ChatEvent{Type: "done"}, &ChatEvent{Type: "done", ErrorMessage: ErrMsgCancelled}},
		{"finished", false, &ChatEvent{Type: "done"}, &ChatEvent{Type: "done"}},
		{"closed early", false, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			initOneShotMaps(m)
			key := "ag1:slack:C1:9.0"
			sf := newFakeKeyedSurface("hub-a")
			events := make(chan ChatEvent, 2)
			done := make(chan struct{})
			go func() {
				defer close(done)
				m.handleKeyedBackgroundTurn("ag1", key, events, nil, nil, nil, sf)
			}()
			waitFor(t, "background turn tracked", func() bool {
				_, ok := m.InFlightOneShotOrigin("ag1", key)
				return ok
			})
			if tc.stop {
				m.cancelOneShots("ag1", KojoStop(stopReasonFork))
			}
			if tc.terminal != nil {
				events <- *tc.terminal
			}
			close(events)
			<-done
			evs := <-sf.turns
			var last *ChatEvent
			if len(evs) > 0 {
				last = &evs[len(evs)-1]
			}
			if (last == nil) != (tc.want == nil) || (last != nil && (last.Type != tc.want.Type || last.ErrorMessage != tc.want.ErrorMessage)) {
				t.Fatalf("events = %+v, want last %+v", evs, tc.want)
			}
		})
	}
}

// A relayed turn whose backend still sends a terminal after a stop here:
// the first terminal the caller sees says it was cancelled.
func TestRelayedTurnStopMarksBackendTerminal(t *testing.T) {
	m, b, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	m.mu.Lock()
	m.backends[b.Name()] = &doneOnCancelBackend{stopNoteBackend: b}
	m.mu.Unlock()
	key := id + ":ext.test:k1"
	for i := 0; i < 20; i++ {
		events, err := m.ChatOneShot(context.Background(), id, "hi", OneShotOpts{SessionKey: key, StopNotedByCaller: true})
		if err != nil {
			t.Fatalf("ChatOneShot: %v", err)
		}
		<-b.msgs
		<-b.ctxs
		m.cancelOneShots(id, KojoStop(stopReasonFork))
		var first *ChatEvent
		for e := range events {
			if first == nil && (e.Type == "done" || e.Type == "error") {
				e := e
				first = &e
			}
		}
		if first == nil || first.ErrorMessage != ErrMsgCancelled {
			t.Fatalf("first terminal = %+v", first)
		}
	}
}

// doneOnCancelBackend runs until cancelled, then ends with a plain done.
type doneOnCancelBackend struct{ *stopNoteBackend }

func (b *doneOnCancelBackend) Chat(ctx context.Context, _ *Agent, msg, _ string, _ ChatOptions) (<-chan ChatEvent, error) {
	b.msgs <- msg
	b.ctxs <- ctx
	out := make(chan ChatEvent, 1)
	go func() {
		defer close(out)
		<-ctx.Done()
		out <- ChatEvent{Type: "done", Message: newAssistantMessage()}
	}()
	return out, nil
}

// The race behind a relayed stop: the backend's terminal and the stop are
// both ready. Whichever wins, a terminal that reaches the caller says it was
// cancelled, and sent tells ChatOneShot whether to add one.
func TestOneShotTerminalAfterStopIsMarked(t *testing.T) {
	m := newTestManager(t)
	sentSeen, droppedSeen := false, false
	for i := 0; i < 200 && !(sentSeen && droppedSeen); i++ {
		ctx := cancelledWith(KojoStop(stopReasonFork))
		in := make(chan ChatEvent, 1)
		in <- ChatEvent{Type: "done"}
		close(in)
		out := make(chan ChatEvent, 4)
		end := m.processOneShotEvents(ctx, "ag1", markStoppedTerminals(ctx, in), out, false)
		close(out)
		var got []ChatEvent
		for e := range out {
			got = append(got, e)
		}
		if end.sent != (len(got) > 0) {
			t.Fatalf("sent = %v, events = %+v", end.sent, got)
		}
		if end.sent {
			sentSeen = true
			if got[0].ErrorMessage != ErrMsgCancelled {
				t.Fatalf("forwarded terminal = %+v", got[0])
			}
		} else {
			droppedSeen = true
		}
	}
	if !sentSeen || !droppedSeen {
		t.Logf("paths covered: sent %v dropped %v", sentSeen, droppedSeen)
	}
}
