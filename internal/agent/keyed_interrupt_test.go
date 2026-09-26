package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// A stopped keyed turn is interrupted, not killed: the CLI answers the
// interrupt with error_during_execution, the turn ends with its partial text
// and the cancel marker, and the session lingers for its background task
// (claude 2.1.280: interrupt stops the turn and its foreground tool only).
func TestKeyedInterruptEndsTurnButKeepsProcess(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	b := newKeyedTestBackend()
	var mu sync.Mutex
	var abandoned []string
	b.onKeyedTasksAbandoned = func(_, _ string, pending int, reason string, _ KeyedSessionSurface) {
		mu.Lock()
		abandoned = append(abandoned, reason)
		mu.Unlock()
	}
	k := newKeyedTestSession(t, b, "test-agent:slack:C1:1.0")
	rec := &recordingWriteCloser{}
	k.s.stdinW = &claudeStdinWriter{w: rec}
	ctx, cancel := context.WithCancel(context.Background())
	sink, err := k.s.startTurn(ctx, &Agent{ID: "test-agent"}, "go", false, false)
	if err != nil {
		t.Fatal(err)
	}
	writeLine(t, k, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"bg1"}]}`)
	writeLine(t, k, `{"type":"system","subtype":"task_started","task_id":"bg1","is_backgrounded":true}`)
	writeLine(t, k, `{"type":"system","subtype":"task_started","task_id":"fg1","is_backgrounded":false}`)
	writeLine(t, k, `{"type":"assistant","message":{"content":[{"type":"text","text":"working on it"}]}}`)
	waitFor(t, "text streamed", func() bool {
		k.s.mu.Lock()
		defer k.s.mu.Unlock()
		return k.s.pendingTasks == 1
	})
	cancel()
	waitFor(t, "interrupt sent", func() bool { return strings.Contains(rec.String(), `"subtype":"interrupt"`) })
	writeLine(t, k, `{"type":"system","subtype":"task_notification","task_id":"fg1","status":"stopped"}`)
	writeLine(t, k, `{"type":"result","subtype":"error_during_execution","is_error":true}`)
	d := doneOf(t, collect(t, sink, 3*time.Second))
	if d.ErrorMessage != ErrMsgCancelled {
		t.Fatalf("done error = %q, want the cancel marker", d.ErrorMessage)
	}
	if d.Message == nil || !strings.Contains(d.Message.Content, "working on it") {
		t.Fatalf("partial text lost: %+v", d.Message)
	}
	if d.BackgroundTasksPending != 1 {
		t.Fatalf("pending = %d, want 1 (the background task survives the stop)", d.BackgroundTasksPending)
	}
	select {
	case <-k.killed:
		t.Fatal("a turn stop killed the keyed process")
	case <-time.After(150 * time.Millisecond):
	}
	if n := b.lingeringCount("test-agent", ""); n != 1 {
		t.Fatalf("lingering = %d, want 1", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(abandoned) != 0 {
		t.Fatalf("answered interrupt reported abandoned tasks: %v", abandoned)
	}
}

// A stopped keyed turn with no content is not "recovered" from the session
// JSONL (that would resurface the previous turn's reply as this one's).
func TestKeyedInterruptWithoutTextIsNotRecovered(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	b := newKeyedTestBackend()
	k := newKeyedTestSession(t, b, "test-agent:slack:C1:2.0")
	k.s.sessionID = "does-not-matter"
	ctx, cancel := context.WithCancel(context.Background())
	sink, err := k.s.startTurn(ctx, &Agent{ID: "test-agent"}, "go", false, false)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	writeLine(t, k, `{"type":"result","subtype":"error_during_execution","is_error":true}`)
	d := doneOf(t, collect(t, sink, 3*time.Second))
	if d.ErrorMessage != ErrMsgCancelled || (d.Message != nil && d.Message.Content != "") {
		t.Fatalf("done = %q / %+v", d.ErrorMessage, d.Message)
	}
	// Nothing pending: the session closes right after the result. Wait for
	// it so that close (reading keyedCloseGrace) cannot outlive the test.
	waitKilled(t, k, 3*time.Second)
}

// An interrupt the CLI never answers escalates to a kill; that takes the
// background tasks with it, which is reported (unlike an answered stop).
func TestKeyedInterruptUnansweredKillsAndReportsAbandoned(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	old := claudeInterruptEscalation
	claudeInterruptEscalation = 50 * time.Millisecond
	t.Cleanup(func() { claudeInterruptEscalation = old })
	b := newKeyedTestBackend()
	got := make(chan string, 2)
	b.onKeyedTasksAbandoned = func(_, _ string, pending int, reason string, _ KeyedSessionSurface) {
		if pending == 1 {
			got <- reason
		}
	}
	k := newKeyedTestSession(t, b, "test-agent:slack:C1:3.0")
	ctx, cancel := context.WithCancel(context.Background())
	sink, err := k.s.startTurn(ctx, &Agent{ID: "test-agent"}, "go", false, false)
	if err != nil {
		t.Fatal(err)
	}
	writeLine(t, k, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"bg1"}]}`)
	waitFor(t, "task recorded", func() bool {
		k.s.mu.Lock()
		defer k.s.mu.Unlock()
		return k.s.pendingTasks == 1
	})
	cancel()
	waitKilled(t, k, 3*time.Second)
	collect(t, sink, 3*time.Second)
	select {
	case r := <-got:
		if r != keyedInterruptKillReason {
			t.Fatalf("reason = %q", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("killed-on-escalation tasks were not reported abandoned")
	}
}

// task_started's arrival is the task's start time, even when the task only
// enters a background_tasks_changed snapshot later (a foreground task that
// was backgrounded mid-run).
func TestTaskStartedSetsStartedAt(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	b := newKeyedTestBackend()
	k := newKeyedTestSession(t, b, "groupdm:gd_started")
	if _, err := k.s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "go", false, false); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	writeLine(t, k, `{"type":"system","subtype":"task_started","task_id":"t1","is_backgrounded":false}`)
	waitFor(t, "task_started recorded", func() bool {
		k.s.mu.Lock()
		defer k.s.mu.Unlock()
		_, ok := k.s.taskLife["t1"]
		return ok
	})
	time.Sleep(80 * time.Millisecond)
	snapAt := time.Now()
	writeLine(t, k, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"t1"}]}`)
	waitFor(t, "snapshot recorded", func() bool {
		k.s.mu.Lock()
		defer k.s.mu.Unlock()
		return len(k.s.tasks) == 1
	})
	k.s.mu.Lock()
	at := k.s.taskSeen["t1"]
	bg := k.s.taskLife["t1"].backgrounded
	k.s.mu.Unlock()
	if at.Before(before) || !at.Before(snapAt) {
		t.Fatalf("startedAt = %v, want the task_started arrival (in [%v, %v))", at, before, snapAt)
	}
	if !bg {
		t.Fatal("a task listed in the background snapshot is not marked backgrounded")
	}
	// The notification ends its lifecycle record.
	writeLine(t, k, `{"type":"system","subtype":"task_notification","task_id":"t1","status":"completed"}`)
	waitFor(t, "lifecycle entry dropped", func() bool {
		k.s.mu.Lock()
		defer k.s.mu.Unlock()
		_, ok := k.s.taskLife["t1"]
		return !ok
	})
}

// After a stop the queued-steer reaper interrupts the CLI's auto-turns for
// steer lines still queued on stdin — but a background task's notification
// turn (task_notification completed while idle) is spared: a turn stop must
// not cost the task's result. A task that was itself stopped starts no
// notification turn, so the reaper stays armed for the next auto-turn.
func TestQueuedSteerReaperSparesNotificationTurn(t *testing.T) {
	for _, tc := range []struct {
		status string
		spare  bool
	}{{"completed", true}, {"stopped", false}} {
		t.Run(tc.status, func(t *testing.T) {
			setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
			b := newKeyedTestBackend()
			bg := make(chan (<-chan ChatEvent), 1)
			b.onKeyedBackgroundTurn = func(_, _ string, events <-chan ChatEvent, _ AnswerFunc, _ func(), _ SteerFunc, _ KeyedSessionSurface) {
				bg <- events
			}
			k := newKeyedTestSession(t, b, "test-agent:slack:C1:4.0")
			rec := &recordingWriteCloser{}
			k.s.stdinW = &claudeStdinWriter{w: rec}
			sink, err := k.s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "go", false, false)
			if err != nil {
				t.Fatal(err)
			}
			writeLine(t, k, `{"type":"system","subtype":"task_started","task_id":"bg1","is_backgrounded":true}`)
			writeLine(t, k, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"bg1"}]}`)
			writeLine(t, k, `{"type":"result","subtype":"success","result":"started"}`)
			collect(t, sink, 3*time.Second)
			// As armed by a stopped turn that had steer lines queued.
			k.s.mu.Lock()
			k.s.killQueuedSteerTurns = 1
			k.s.killQueuedSteerUntil = time.Now().Add(5 * time.Second)
			k.s.mu.Unlock()
			writeLine(t, k, `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`)
			writeLine(t, k, `{"type":"system","subtype":"task_notification","task_id":"bg1","status":"`+tc.status+`"}`)
			writeLine(t, k, `{"type":"assistant","message":{"content":[{"type":"text","text":"NOTIFIED"}]}}`)
			var events <-chan ChatEvent
			select {
			case events = <-bg:
			case <-time.After(3 * time.Second):
				t.Fatal("unsolicited turn not opened")
			}
			// The reaper's interrupt is async and identity-pinned to the
			// running turn, so observe it before the turn's result lands.
			hasInterrupt := func() bool { return strings.Contains(rec.String(), `"subtype":"interrupt"`) }
			if tc.spare {
				time.Sleep(50 * time.Millisecond)
			} else {
				deadline := time.Now().Add(3 * time.Second)
				for !hasInterrupt() && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
			}
			interrupted := hasInterrupt()
			writeLine(t, k, `{"type":"result","subtype":"success","result":"NOTIFIED","origin":{"kind":"task-notification"}}`)
			collect(t, events, 3*time.Second)
			k.s.mu.Lock()
			armed := k.s.killQueuedSteerTurns
			k.s.mu.Unlock()
			if tc.spare && (interrupted || armed != 1) {
				t.Fatalf("notification turn reaped (interrupt=%v, armed=%d)", interrupted, armed)
			}
			if !tc.spare && (!interrupted || armed != 0) {
				t.Fatalf("auto-turn after a stopped task not reaped (interrupt=%v, armed=%d)", interrupted, armed)
			}
		})
	}
}

// A keyed turn arriving while a non-steerable turn still runs on the thread's
// process (e.g. a stopped turn winding down after its interrupt) queues behind
// it instead of failing with ErrAgentBusy, then runs on the same process.
func TestKeyedTurnQueuesBehindRunningTurn(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	_, b := newBgSessionsTestManager(t)
	key := "test-agent:slack:C1:7.0"
	k := newKeyedTestSession(t, b, key)
	first, err := k.s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "first", false, false)
	if err != nil {
		t.Fatal(err)
	}
	type res struct {
		ch  <-chan ChatEvent
		err error
	}
	got := make(chan res, 1)
	go func() {
		ch, handled, err := b.chatViaKeyedSession(context.Background(), &Agent{ID: "test-agent", Tool: "claude"}, "second", "", ChatOptions{SessionKey: key, LingerBackgroundTasks: true})
		if !handled && err == nil {
			err = errors.New("not handled")
		}
		got <- res{ch, err}
	}()
	select {
	case r := <-got:
		t.Fatalf("second turn did not wait for the running one: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	// A pending background task keeps the process lingering past this turn.
	writeLine(t, k, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"bg1"}]}`)
	writeLine(t, k, `{"type":"result","subtype":"success","result":"one"}`)
	if d := doneOf(t, collect(t, first, 3*time.Second)); d.Message == nil || d.Message.Content != "one" {
		t.Fatalf("first done = %+v", d)
	}
	var r res
	select {
	case r = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("queued turn never started")
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	writeLine(t, k, `{"type":"result","subtype":"success","result":"two"}`)
	if d := doneOf(t, collect(t, r.ch, 3*time.Second)); d.Message == nil || d.Message.Content != "two" {
		t.Fatalf("second done = %+v", d)
	}
	b.sessMu.Lock()
	cur := b.sessions[keyedPoolKey("test-agent", key)]
	b.sessMu.Unlock()
	if cur != k.s {
		t.Fatal("queued turn did not reuse the thread's process")
	}
}

// A late abort of a finished turn never interrupts its successor: the turn
// identity is checked under the stdin mutex the successor's first line takes.
func TestInterruptIfCurrentSkipsSuccessorTurn(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	b := newKeyedTestBackend()
	k := newKeyedTestSession(t, b, "test-agent:slack:C1:8.0")
	rec := &recordingWriteCloser{}
	k.s.stdinW = &claudeStdinWriter{w: rec}
	first, err := k.s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "one", false, false)
	if err != nil {
		t.Fatal(err)
	}
	k.s.mu.Lock()
	firstDone := k.s.turnDone
	k.s.mu.Unlock()
	writeLine(t, k, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"bg1"}]}`)
	writeLine(t, k, `{"type":"result","subtype":"success","result":"one"}`)
	collect(t, first, 3*time.Second)
	if _, err := k.s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "two", false, false); err != nil {
		t.Fatal(err)
	}
	k.s.interruptIfCurrent(firstDone)
	if strings.Contains(rec.String(), `"subtype":"interrupt"`) {
		t.Fatal("a stale abort interrupted the successor turn")
	}
	k.s.mu.Lock()
	cur := k.s.turnDone
	k.s.mu.Unlock()
	k.s.interruptIfCurrent(cur)
	if !strings.Contains(rec.String(), `"subtype":"interrupt"`) {
		t.Fatal("the running turn was not interrupted")
	}
}
