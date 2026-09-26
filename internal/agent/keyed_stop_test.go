package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// lingeringTestSession is a keyed test session idling with n background tasks
// pending, bound to sf.
func lingeringTestSession(t *testing.T, b *ClaudeBackend, key string, n int, sf KeyedSessionSurface) *keyedTestSession {
	t.Helper()
	k := newKeyedTestSession(t, b, key)
	k.s.bindSurface(sf)
	sink, err := k.s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "go", false, false)
	if err != nil {
		t.Fatal(err)
	}
	var tasks []string
	for i := 0; i < n; i++ {
		tasks = append(tasks, fmt.Sprintf(`{"task_id":"t%d"}`, i))
	}
	writeLine(t, k, `{"type":"system","subtype":"background_tasks_changed","tasks":[`+strings.Join(tasks, ",")+`]}`)
	writeLine(t, k, `{"type":"result","subtype":"success","result":"started"}`)
	if d := doneOf(t, collect(t, sink, 3*time.Second)); d.BackgroundTasksPending != n {
		t.Fatalf("pending = %d, want %d", d.BackgroundTasksPending, n)
	}
	return k
}

func recvAbandoned(t *testing.T, sf *fakeKeyedSurface) int {
	t.Helper()
	select {
	case n := <-sf.abandoned:
		return n
	case <-time.After(3 * time.Second):
		t.Fatal("no stop / abandoned notice reached the thread surface")
		return 0
	}
}

// Stopping a single task posts a notice into the thread too (it used to be
// silent), and — being agent-requested — leaves the agent no note.
func TestStopBackgroundTaskNotifiesThread(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	m, b := newBgSessionsTestManager(t)
	sf := newFakeKeyedSurface("")
	k := lingeringTestSession(t, b, "test-agent:slack:C1:1.0", 2, sf)
	if err := m.StopBackgroundTask("test-agent", "test-agent:slack:C1:1.0", "t1"); err != nil {
		t.Fatal(err)
	}
	if n := recvAbandoned(t, sf); n != 1 {
		t.Fatalf("notice pending = %d, want 1", n)
	}
	if note := m.peekKeyedNote("test-agent", "test-agent:slack:C1:1.0"); note != "" {
		t.Fatalf("agent-requested task stop left a note: %q", note)
	}
	select {
	case <-k.killed:
		t.Fatal("a single-task stop closed the session")
	case <-time.After(50 * time.Millisecond):
	}
}

// `!stop all` (StopThreadBackgroundTasks) closes the session with the user
// reason: the thread gets the stop notice and the agent a one-time note.
func TestStopThreadBackgroundTasksUserReason(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	m, b := newBgSessionsTestManager(t)
	reasons := make(chan string, 2)
	// As wired in production: the exit notice goes through the Manager.
	b.onKeyedTasksAbandoned = func(agentID, key string, pending int, reason string, surface KeyedSessionSurface) {
		reasons <- reason
		m.handleKeyedTasksAbandoned(agentID, key, pending, reason, surface)
	}
	sf := newFakeKeyedSurface("")
	key := "test-agent:slack:C1:2.0"
	k := lingeringTestSession(t, b, key, 2, sf)
	if got := m.keyedBackgroundPending("test-agent", key); got != 2 {
		t.Fatalf("keyedBackgroundPending = %d, want 2", got)
	}
	if err := m.StopThreadBackgroundTasks("test-agent", key); err != nil {
		t.Fatal(err)
	}
	// The note is stored before the stop returns (an immediate follow-up
	// turn must see it); the follow-up consumes it before the exit notice.
	note := m.peekKeyedNote("test-agent", key)
	if !strings.Contains(note, "!stop all") || !strings.Contains(note, "2件") {
		t.Fatalf("note = %q", note)
	}
	m.commitKeyedNote("test-agent", key, note)
	waitKilled(t, k, 3*time.Second)
	if n := recvAbandoned(t, sf); n != 2 {
		t.Fatalf("notice pending = %d, want 2", n)
	}
	if r := <-reasons; r != keyedUserStopNotedReason {
		t.Fatalf("reason = %q, want %q", r, keyedUserStopNotedReason)
	}
	if got := sf.lastReason(); got != KeyedUserStopReason {
		t.Fatalf("surface reason = %q, want %q", got, KeyedUserStopReason)
	}
	if note := m.peekKeyedNote("test-agent", key); note != "" {
		t.Fatalf("exit notice stored the note again: %q", note)
	}
	if got := m.keyedBackgroundPending("test-agent", key); got != 0 {
		t.Fatalf("keyedBackgroundPending after stop = %d", got)
	}
	if got := m.keyedBackgroundPending("test-agent", "test-agent:slack:C1:none"); got != 0 {
		t.Fatalf("keyedBackgroundPending for a missing key = %d", got)
	}
	if err := m.StopThreadBackgroundTasks("test-agent", "test-agent:slack:C1:none"); !errors.Is(err, ErrBackgroundSessionNotFound) {
		t.Fatalf("missing key err = %v", err)
	}
}

// A Hub-relayed `!stop all` may only stop a session reporting to that Hub.
func TestStopThreadBackgroundTasksFromOrigin(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	m, b := newBgSessionsTestManager(t)
	sf := newFakeKeyedSurface("hub-a")
	key := "groupdm:gd_origin"
	k := lingeringTestSession(t, b, key, 1, sf)
	if err := m.StopThreadBackgroundTasksFromOrigin("test-agent", key, "hub-b"); !errors.Is(err, ErrSteerOriginForbidden) {
		t.Fatalf("foreign origin err = %v", err)
	}
	select {
	case <-k.killed:
		t.Fatal("a foreign origin stopped the session")
	case <-time.After(50 * time.Millisecond):
	}
	if err := m.StopThreadBackgroundTasksFromOrigin("test-agent", key, "hub-a"); err != nil {
		t.Fatal(err)
	}
	waitKilled(t, k, 3*time.Second)
	if err := m.StopThreadBackgroundTasksFromOrigin("test-agent", key, "hub-a"); !errors.Is(err, ErrBackgroundSessionNotFound) {
		t.Fatalf("stopped session err = %v", err)
	}

	// A thread this holder serves itself (no remote origin) is no peer's to
	// stop; the unsafe local mode (empty origin) still may.
	localKey := "groupdm:gd_local"
	local := lingeringTestSession(t, b, localKey, 1, newFakeKeyedSurface(""))
	if err := m.StopThreadBackgroundTasksFromOrigin("test-agent", localKey, "hub-a"); !errors.Is(err, ErrSteerOriginForbidden) {
		t.Fatalf("peer stop of a local session err = %v", err)
	}
	if err := m.StopThreadBackgroundTasksFromOrigin("test-agent", "groupdm:gd_none", "hub-a"); !errors.Is(err, ErrBackgroundSessionNotFound) {
		t.Fatalf("missing session err = %v", err)
	}
	if err := m.StopThreadBackgroundTasksFromOrigin("test-agent", localKey, ""); err != nil {
		t.Fatal(err)
	}
	waitKilled(t, local, 3*time.Second)
}

func TestKeyedNotesPeekCommitExpireDrop(t *testing.T) {
	m, _ := newBgSessionsTestManager(t)
	m.setKeyedNote("a1", "k1", "first")
	if got := m.peekKeyedNote("a1", "k1"); got != "first" {
		t.Fatalf("peek = %q", got)
	}
	// A newer note replacing the delivered one survives the commit.
	m.setKeyedNote("a1", "k1", "second")
	m.commitKeyedNote("a1", "k1", "first")
	if got := m.peekKeyedNote("a1", "k1"); got != "second" {
		t.Fatalf("replaced note lost on commit: %q", got)
	}
	m.commitKeyedNote("a1", "k1", "second")
	if got := m.peekKeyedNote("a1", "k1"); got != "" {
		t.Fatalf("committed note still pending: %q", got)
	}
	// Expired notes are never delivered.
	m.setKeyedNote("a1", "old", "stale")
	m.keyedBgMu.Lock()
	n := m.keyedNotes[keyedNoteKey("a1", "old")]
	n.at = time.Now().Add(-keyedNoteTTL - time.Minute)
	m.keyedNotes[keyedNoteKey("a1", "old")] = n
	m.keyedBgMu.Unlock()
	if got := m.peekKeyedNote("a1", "old"); got != "" {
		t.Fatalf("expired note delivered: %q", got)
	}
	// Bounded.
	for i := 0; i < maxKeyedNotes+100; i++ {
		m.setKeyedNote("a2", fmt.Sprintf("k%d", i), "x")
	}
	m.keyedBgMu.Lock()
	size := len(m.keyedNotes)
	m.keyedBgMu.Unlock()
	if size > maxKeyedNotes {
		t.Fatalf("notes = %d, want <= %d", size, maxKeyedNotes)
	}
	// Drop: one key, or the whole agent (never another agent's).
	m.setKeyedNote("a1", "k2", "y")
	m.setKeyedNote("a1", "k3", "y")
	m.setKeyedNote("a10", "k2", "keep")
	m.dropKeyedNotes("a1", "k2")
	if m.peekKeyedNote("a1", "k2") != "" || m.peekKeyedNote("a1", "k3") == "" {
		t.Fatal("single-key drop wrong")
	}
	m.dropKeyedNotes("a1", "")
	if m.peekKeyedNote("a1", "k3") != "" {
		t.Fatal("agent drop kept a note")
	}
	if m.peekKeyedNote("a10", "k2") != "keep" {
		t.Fatal("agent drop removed another agent's note (prefix collision)")
	}
}

// No note is recorded when nobody could ever read it: the agent is gone or
// the thread room was deleted.
func TestAbandonedNoteSkippedForGoneAgentOrThread(t *testing.T) {
	m, _ := newBgSessionsTestManager(t)
	m.handleKeyedTasksAbandoned("test-agent", "groupdm:gd_del", 2, keyedThreadDeletedReason, nil)
	if note := m.peekKeyedNote("test-agent", "groupdm:gd_del"); note != "" {
		t.Fatalf("deleted thread got a note: %q", note)
	}
	m.handleKeyedTasksAbandoned("ghost", "groupdm:gd_x", 2, "idle", nil)
	if note := m.peekKeyedNote("ghost", "groupdm:gd_x"); note != "" {
		t.Fatalf("unknown agent got a note: %q", note)
	}
	m.handleKeyedTasksAbandoned("test-agent", "groupdm:gd_x", 2, "idle", nil)
	if note := m.peekKeyedNote("test-agent", "groupdm:gd_x"); note == "" {
		t.Fatal("live agent's abandoned note missing")
	}
}

// Deleting a thread room drops its pending note.
func TestThreadDeleteDropsKeyedNote(t *testing.T) {
	gdm, mgr := setupGroupDMTest(t)
	mgr.SetGroupDMManager(gdm)
	initOneShotMaps(mgr)
	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	other, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	mgr.setKeyedNote("ag_alice", "groupdm:"+g.ID, "note")
	mgr.setKeyedNote("ag_alice", "groupdm:"+other.ID, "other")
	if err := gdm.Delete(g.ID, false); err != nil {
		t.Fatal(err)
	}
	if note := mgr.peekKeyedNote("ag_alice", "groupdm:"+g.ID); note != "" {
		t.Fatalf("deleted thread's note survived: %q", note)
	}
	if mgr.peekKeyedNote("ag_alice", "groupdm:"+other.ID) != "other" {
		t.Fatal("another thread's note was dropped")
	}
}

// A Slack thread message sent while a keyed background (notification) turn
// streams is steered into it through the Slack entry point (SteerOneShotAsUser),
// locally and — fenced by the dispatching Hub — from a peer.
func TestSlackSteerReachesKeyedBackgroundTurn(t *testing.T) {
	for _, tc := range []struct {
		name, surfaceOrigin, steerOrigin string
		wantErr                          error
	}{
		{name: "local"},
		{name: "remote", surfaceOrigin: "hub-a", steerOrigin: "hub-a"},
		{name: "foreign hub", surfaceOrigin: "hub-a", steerOrigin: "hub-b", wantErr: ErrSteerOriginForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			initOneShotMaps(m)
			key := "ag1:slack:C1:9.0"
			steered := make(chan string, 1)
			steer := func(text string) error { steered <- text; return nil }
			events := make(chan ChatEvent, 2)
			var sf *fakeKeyedSurface
			var surface KeyedSessionSurface
			if tc.surfaceOrigin != "" {
				sf = newFakeKeyedSurface(tc.surfaceOrigin)
				surface = sf
			} else {
				h := &recordingKeyedHandler{got: make(chan []ChatEvent, 1)}
				defer m.RegisterKeyedBackgroundHandler("ag1", h)()
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				m.handleKeyedBackgroundTurn("ag1", key, events, nil, nil, steer, surface)
			}()
			waitFor(t, "background turn tracked", func() bool {
				_, ok := m.InFlightOneShotOrigin("ag1", key)
				return ok && m.keyedBgSteerFor(key) != nil
			})
			err := m.SteerOneShotAsUser("ag1", key, tc.steerOrigin, "follow-up", "U1")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if got := <-steered; got != "follow-up" {
				t.Fatalf("steered %q", got)
			}
			events <- ChatEvent{Type: "done", Message: &Message{Role: "assistant", Content: "bg"}}
			close(events)
			<-done
			if m.keyedBgSteerFor(key) != nil {
				t.Fatal("background steer left registered after the turn")
			}
			if err := m.SteerOneShotAsUser("ag1", key, tc.steerOrigin, "late", "U1"); err == nil {
				t.Fatal("steer after the background turn succeeded")
			}
		})
	}
}

// `!stop all` hitting a running turn: the note is stored before the call
// returns, a repeat is coalesced (no second notice), and a task the turn
// spawns after the stop's snapshot is killed at the turn's result with only
// that task reported.
func TestStopAllInTurnCoalescesAndCoversLaterTasks(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	m, b := newBgSessionsTestManager(t)
	b.onKeyedTasksAbandoned = func(agentID, key string, pending int, reason string, surface KeyedSessionSurface) {
		m.handleKeyedTasksAbandoned(agentID, key, pending, reason, surface)
	}
	b.onKeyedNoteLocked = m.storeKeyedNoteLocked
	key := "test-agent:slack:C1:9.0"
	k := newKeyedTestSession(t, b, key)
	sf := newFakeKeyedSurface("")
	k.s.bindSurface(sf)
	rec := &recordingWriteCloser{}
	k.s.stdinW = &claudeStdinWriter{w: rec}
	sink, err := k.s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "go", false, false)
	if err != nil {
		t.Fatal(err)
	}
	writeLine(t, k, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a"}]}`)
	waitFor(t, "task recorded", func() bool {
		k.s.mu.Lock()
		defer k.s.mu.Unlock()
		return len(k.s.tasks) == 1
	})
	if err := m.StopThreadBackgroundTasks("test-agent", key); err != nil {
		t.Fatal(err)
	}
	if note := m.peekKeyedNote("test-agent", key); !strings.Contains(note, "!stop all") {
		t.Fatalf("note not stored synchronously: %q", note)
	}
	if n := recvAbandoned(t, sf); n != 1 {
		t.Fatalf("stop notice pending = %d, want 1", n)
	}
	if err := m.StopThreadBackgroundTasks("test-agent", key); err != nil {
		t.Fatalf("repeat stop: %v", err)
	}
	select {
	case n := <-sf.abandoned:
		t.Fatalf("repeat stop posted a second notice (%d)", n)
	case <-time.After(50 * time.Millisecond):
	}
	if got := strings.Count(rec.String(), `"subtype":"stop_task"`); got != 1 {
		t.Fatalf("stop_task writes = %d, want 1", got)
	}
	// The interrupted turn spawned b after the stop's snapshot.
	writeLine(t, k, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a"},{"task_id":"b"}]}`)
	writeLine(t, k, `{"type":"result","subtype":"error_during_execution"}`)
	collect(t, sink, 3*time.Second)
	// The turn-end kill's note is stored before the turn's result is out
	// (a follow-up turn peeks it right away), appended to the first one.
	if note := m.peekKeyedNote("test-agent", key); strings.Count(note, "!stop all") != 2 {
		t.Fatalf("note at turn end = %q, want both stop notes", note)
	}
	waitKilled(t, k, 3*time.Second)
	if n := recvAbandoned(t, sf); n != 1 {
		t.Fatalf("turn-end notice pending = %d, want 1 (only the uncovered task)", n)
	}
	if got := sf.lastReason(); got != KeyedUserStopReason {
		t.Fatalf("turn-end notice reason = %q, want %q", got, KeyedUserStopReason)
	}
	if note := m.peekKeyedNote("test-agent", key); strings.Count(note, "!stop all") != 2 {
		t.Fatalf("exit notice stored the note again: %q", note)
	}
}

// Without `!stop all` (the agent's own DELETE from its turn) a task spawned
// later in the turn keeps running.
func TestAgentStopInTurnLetsLaterTasksLinger(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	m, b := newBgSessionsTestManager(t)
	key := "groupdm:gd_later"
	k := newKeyedTestSession(t, b, key)
	k.s.stdinW = &claudeStdinWriter{w: &recordingWriteCloser{}}
	sink, err := k.s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "go", false, false)
	if err != nil {
		t.Fatal(err)
	}
	writeLine(t, k, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a"}]}`)
	waitFor(t, "task recorded", func() bool {
		k.s.mu.Lock()
		defer k.s.mu.Unlock()
		return len(k.s.tasks) == 1
	})
	if err := m.StopBackgroundSession("test-agent", key); err != nil {
		t.Fatal(err)
	}
	writeLine(t, k, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"b"}]}`)
	writeLine(t, k, `{"type":"result","subtype":"success","result":"ok"}`)
	if d := doneOf(t, collect(t, sink, 3*time.Second)); d.BackgroundTasksPending != 1 {
		t.Fatalf("pending = %d, want 1", d.BackgroundTasksPending)
	}
	select {
	case <-k.killed:
		t.Fatal("agent-requested stop killed a task spawned afterwards")
	case <-time.After(50 * time.Millisecond):
	}
}

// A second note for a key whose note is still undelivered is added, not
// swapped in; committing the delivered part keeps the newer one.
func TestKeyedNotesCombineUntilDelivered(t *testing.T) {
	m, _ := newBgSessionsTestManager(t)
	key := "test-agent:slack:C1:11.0"
	m.setKeyedNote("test-agent", key, "A")
	delivered := m.peekKeyedNote("test-agent", key)
	m.setKeyedNote("test-agent", key, "A") // same wording, a second event
	if got := m.peekKeyedNote("test-agent", key); got != "A\nA" {
		t.Fatalf("combined note = %q", got)
	}
	m.commitKeyedNote("test-agent", key, delivered)
	if got := m.peekKeyedNote("test-agent", key); got != "A" {
		t.Fatalf("after commit = %q, want the newer note kept", got)
	}
	m.commitKeyedNote("test-agent", key, "A")
	if got := m.peekKeyedNote("test-agent", key); got != "" {
		t.Fatalf("after second commit = %q", got)
	}
	m.setKeyedNote("test-agent", key, strings.Repeat("x", maxKeyedNoteBytes))
	m.setKeyedNote("test-agent", key, "B")
	if got := m.peekKeyedNote("test-agent", key); got != "B" {
		t.Fatalf("oversized combine kept %d bytes", len(got))
	}
}

// A note stored by an exit notice still in flight at deletion time is dropped
// once that notice finishes.
func TestDeletionRedropsNotesAfterExitNotices(t *testing.T) {
	m, b := newBgSessionsTestManager(t)
	key := "groupdm:gd_late"
	b.beginKeyedExitNotice("test-agent")
	m.dropKeyedNotesAfterExitNotices("test-agent", key)
	m.setKeyedNote("test-agent", key, "late")
	if m.peekKeyedNote("test-agent", key) != "late" {
		t.Fatal("note not stored")
	}
	b.endKeyedExitNotice("test-agent")
	waitFor(t, "late note dropped", func() bool { return m.peekKeyedNote("test-agent", key) == "" })
}

// stopTaskGateWriter blocks every stop_task write until release is closed.
type stopTaskGateWriter struct {
	entered chan struct{}
	release chan struct{}
	once    bool
}

func (w *stopTaskGateWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), `"subtype":"stop_task"`) {
		if !w.once {
			w.once = true
			close(w.entered)
		}
		<-w.release
	}
	return len(p), nil
}
func (w *stopTaskGateWriter) Close() error { return nil }

// The in-turn `!stop all` note is stored under the session lock, before the
// turn can end and a follow-up turn peek the key's note: it is already
// visible while the stop_task write itself is still blocked.
func TestStopAllInTurnStoresNoteBeforeStopTaskWrite(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	m, b := newBgSessionsTestManager(t)
	key := "test-agent:slack:C1:11.0"
	k := newKeyedTestSession(t, b, key)
	sf := newFakeKeyedSurface("")
	k.s.bindSurface(sf)
	gate := &stopTaskGateWriter{entered: make(chan struct{}), release: make(chan struct{})}
	k.s.stdinW = &claudeStdinWriter{w: gate}
	sink, err := k.s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "go", false, false)
	if err != nil {
		t.Fatal(err)
	}
	writeLine(t, k, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a"}]}`)
	waitFor(t, "task recorded", func() bool {
		k.s.mu.Lock()
		defer k.s.mu.Unlock()
		return len(k.s.tasks) == 1
	})
	done := make(chan error, 1)
	go func() { done <- m.StopThreadBackgroundTasks("test-agent", key) }()
	select {
	case <-gate.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("stop_task never written")
	}
	if note := m.peekKeyedNote("test-agent", key); !strings.Contains(note, "!stop all") {
		t.Fatalf("note not stored before the stop_task write: %q", note)
	}
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	writeLine(t, k, `{"type":"result","subtype":"error_during_execution"}`)
	collect(t, sink, 3*time.Second)
}

// The inter-peer steer fallback only reaches a keyed background turn that
// the same Hub dispatched.
func TestKeyedBgSteerFromOriginChecksRegistration(t *testing.T) {
	m := newTestManager(t)
	key := "test-agent:slack:C1:12.0"
	var got []string
	unregister := m.registerKeyedBgSteer(key, "hub-a", func(text string) error {
		got = append(got, text)
		return nil
	})
	defer unregister()
	if fn := m.keyedBgSteerFromOrigin(key, "hub-b"); fn != nil {
		t.Fatal("another Hub's steer reached the background turn")
	}
	if fn := m.keyedBgSteerFromOrigin(key, ""); fn != nil {
		t.Fatal("an origin-less steer passed the fence")
	}
	if err := m.SteerOneShotFromOrigin("test-agent", key, "hub-b", "x"); err == nil {
		t.Fatal("SteerOneShotFromOrigin from another Hub succeeded")
	}
	fn := m.keyedBgSteerFromOrigin(key, "hub-a")
	if fn == nil {
		t.Fatal("the dispatching Hub's steer was refused")
	}
	if err := fn("y"); err != nil || len(got) != 1 || got[0] != "y" {
		t.Fatalf("steer = %v / %v", err, got)
	}
	if fn := m.keyedBgSteerFor(key); fn == nil {
		t.Fatal("local fallback lost the registration")
	}
}

// A single-task stop holds its own surface reference from the moment it
// excludes the task from the exit's abandoned count: an exit detaching the
// surface while the stop_task write is still in flight cannot lose the notice.
func TestStopBackgroundTaskKeepsSurfaceAcrossExit(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	m, b := newBgSessionsTestManager(t)
	sf := newFakeKeyedSurface("")
	key := "test-agent:slack:C1:13.0"
	k := lingeringTestSession(t, b, key, 1, sf)
	gate := &stopTaskGateWriter{entered: make(chan struct{}), release: make(chan struct{})}
	k.s.stdinW = &claudeStdinWriter{w: gate}
	done := make(chan error, 1)
	go func() { done <- m.StopBackgroundTask("test-agent", key, "t0") }()
	select {
	case <-gate.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("stop_task never written")
	}
	// The exit detaches (and releases) the session's surface meanwhile.
	if got := k.s.takeSurfaceAtExit(); got != nil {
		got.Release()
	}
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := recvAbandoned(t, sf); n != 1 {
		t.Fatalf("notice pending = %d, want 1", n)
	}
}
