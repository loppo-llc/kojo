package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeLine(t *testing.T, k *keyedTestSession, line string) {
	t.Helper()
	if _, err := io.WriteString(k.pw, line+"\n"); err != nil {
		t.Fatalf("write %s: %v", line, err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fillLingerSlots occupies n linger slots of test-agent with placeholder
// sessions (not pooled, never run).
func fillLingerSlots(b *ClaudeBackend, n int) {
	b.lingerMu.Lock()
	defer b.lingerMu.Unlock()
	if b.lingerSlots == nil {
		b.lingerSlots = make(map[string]map[*claudeSession]struct{})
	}
	set := b.lingerSlots["test-agent"]
	if set == nil {
		set = make(map[*claudeSession]struct{})
		b.lingerSlots["test-agent"] = set
	}
	for i := 0; i < n; i++ {
		set[&claudeSession{agentID: "test-agent", sessionKey: fmt.Sprintf("filler-%d", i)}] = struct{}{}
	}
}

// syncSession waits out any in-progress s.mu critical section of k.
func syncSession(k *keyedTestSession) {
	k.s.mu.Lock()
	k.s.mu.Unlock() //nolint:staticcheck // barrier
}

func initOneShotMaps(m *Manager) {
	m.oneShotCancels = make(map[string]map[int64]context.CancelFunc)
	m.oneShotSessions = make(map[string]map[int64]string)
	m.oneShotOrigins = make(map[string]map[int64]string)
	m.oneShotHandoffCaps = make(map[string]map[int64]string)
	m.oneShotArmed = make(map[string]map[int64]time.Time)
	m.oneShotDone = make(map[string]map[int64]chan struct{})
	m.oneShotSteers = make(map[string]SteerFunc)
}

// Only sessions lingering with pending tasks hold a slot: a session with no
// tasks, a session whose tasks drained, and a closing session are not counted.
func TestLingerSlotCountsOnlyLingeringWithPending(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, time.Hour)
	b := newKeyedTestBackend()
	k1 := newKeyedTestSession(t, b, "test-agent:slack:C1:1.0")
	k2 := newKeyedTestSession(t, b, "test-agent:slack:C1:2.0")
	k3 := newKeyedTestSession(t, b, "groupdm:gd_1")
	_ = k2

	writeLine(t, k1, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a"}]}`)
	writeLine(t, k3, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"b"},{"task_id":"c"}]}`)
	waitFor(t, "two lingering sessions", func() bool { return b.lingeringCount("test-agent", "") == 2 })
	// The slot is taken before the timers are armed under the same s.mu hold;
	// sync on it so no readLoop still reads the timer globals at cleanup.
	syncSession(k1)
	syncSession(k3)
	if n := b.lingeringCount("test-agent", "groupdm:gd_1"); n != 1 {
		t.Fatalf("lingering excluding self = %d, want 1", n)
	}
	if n := b.lingeringCount("other-agent", ""); n != 0 {
		t.Fatalf("other agent lingering = %d", n)
	}

	// k1's tasks drain: the slot is released although the process lingers
	// in its grace window.
	writeLine(t, k1, `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`)
	waitFor(t, "k1 slot released", func() bool { return b.lingeringCount("test-agent", "") == 1 })
	syncSession(k1)

	// k3 closing: no longer counted even while still pooled (its pool entry
	// stays until EOF so a same-key respawn waits instead of colliding).
	k3.s.mu.Lock()
	k3.s.setClosingLocked()
	k3.s.mu.Unlock()
	if n := b.lingeringCount("test-agent", ""); n != 0 {
		t.Fatalf("closing session still counted: %d", n)
	}
	if !b.HasKeyedSession("test-agent", "groupdm:gd_1") {
		t.Fatal("closing session must stay pooled until EOF")
	}
}

// A full cap never blocks a turn; a turn that ends with tasks pending while
// the cap is full is closed with the cap reason, reported as abandoned.
func TestLingerCapFullClosesAtTurnEndWithNotice(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	b := newKeyedTestBackend()
	type abandonedCall struct {
		key     string
		pending int
		reason  string
	}
	abandonedCh := make(chan abandonedCall, 4)
	b.onKeyedTasksAbandoned = func(agentID, key string, pending int, reason string, _ KeyedSessionSurface) {
		abandonedCh <- abandonedCall{key, pending, reason}
	}
	fillLingerSlots(b, maxLingeringSessionsPerAgent)
	k := newKeyedTestSession(t, b, "groupdm:gd_full")
	sink, err := k.s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "run in bg", false, false)
	if err != nil {
		t.Fatalf("turn blocked by full cap: %v", err)
	}
	writeLine(t, k, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"t1"}]}`)
	writeLine(t, k, `{"type":"assistant","message":{"content":[{"type":"text","text":"started"}]}}`)
	writeLine(t, k, `{"type":"result","subtype":"success","result":"started"}`)
	d := doneOf(t, collect(t, sink, 3*time.Second))
	if d.BackgroundTasksPending != 0 {
		t.Fatalf("pending advertised despite full cap: %d", d.BackgroundTasksPending)
	}
	waitKilled(t, k, 3*time.Second)
	select {
	case got := <-abandonedCh:
		if got.key != "groupdm:gd_full" || got.pending != 1 || got.reason != lingerCapReason() {
			t.Fatalf("abandoned = %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no abandoned notice for the cap-full close")
	}
	if n := b.lingeringCount("test-agent", ""); n != maxLingeringSessionsPerAgent {
		t.Fatalf("lingering = %d, want %d fillers", n, maxLingeringSessionsPerAgent)
	}
}

// With a free slot the same turn lingers and holds exactly one slot.
func TestLingerCapHasRoomLingers(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	b := newKeyedTestBackend()
	fillLingerSlots(b, maxLingeringSessionsPerAgent-1)
	k := newKeyedTestSession(t, b, "groupdm:gd_room")
	sink, err := k.s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "run in bg", false, false)
	if err != nil {
		t.Fatal(err)
	}
	writeLine(t, k, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"t1"}]}`)
	writeLine(t, k, `{"type":"result","subtype":"success","result":"started"}`)
	d := doneOf(t, collect(t, sink, 3*time.Second))
	if d.BackgroundTasksPending != 1 {
		t.Fatalf("pending = %d, want 1", d.BackgroundTasksPending)
	}
	if n := b.lingeringCount("test-agent", ""); n != maxLingeringSessionsPerAgent {
		t.Fatalf("lingering = %d, want %d", n, maxLingeringSessionsPerAgent)
	}
	select {
	case <-k.killed:
		t.Fatal("session with a free slot was closed")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestKeyedTurnNote(t *testing.T) {
	m, _ := newBgSessionsTestManager(t)
	b := newKeyedTestBackend()
	if got, _ := m.keyedTurnNote("test-agent", "groupdm:gd_1", b); got != "" {
		t.Fatalf("note with nothing lingering = %q", got)
	}
	m.handleKeyedTasksAbandoned("test-agent", "groupdm:gd_1", 2, lingerCapReason(), nil)
	fillLingerSlots(b, 3)
	got, oneTime := m.keyedTurnNote("test-agent", "groupdm:gd_1", b)
	for _, want := range []string{"バックグラウンドタスク2件は、完了前に終了", lingerCapReason(), "スレッドが3件", "/api/v1/agents/test-agent/background-sessions"} {
		if !strings.Contains(got, want) {
			t.Errorf("note %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "待機上限50件に到達") {
		t.Errorf("cap warning shown below the cap: %q", got)
	}
	// Only peeked until the turn started: a failed start keeps it.
	if again, _ := m.keyedTurnNote("test-agent", "groupdm:gd_1", b); !strings.Contains(again, "完了前に終了") {
		t.Errorf("uncommitted note lost: %q", again)
	}
	// One-time: committed after a started turn, it is gone.
	m.commitKeyedNote("test-agent", "groupdm:gd_1", oneTime)
	if again, _ := m.keyedTurnNote("test-agent", "groupdm:gd_1", b); strings.Contains(again, "完了前に終了") {
		t.Errorf("abandoned note repeated: %q", again)
	}
	fillLingerSlots(b, maxLingeringSessionsPerAgent)
	if full, _ := m.keyedTurnNote("test-agent", "groupdm:gd_1", b); !strings.Contains(full, "待機上限50件に到達") {
		t.Errorf("cap-full warning missing: %q", full)
	}
	// An explicit stop leaves no note.
	m.handleKeyedTasksAbandoned("test-agent", "groupdm:gd_2", 1, KeyedStopRequestedReason, nil)
	if note := m.peekKeyedNote("test-agent", "groupdm:gd_2"); note != "" {
		t.Errorf("stop request left a note: %q", note)
	}
}

func newBgSessionsTestManager(t *testing.T) (*Manager, *ClaudeBackend) {
	t.Helper()
	m := newTestManager(t)
	m.mu.Lock()
	m.agents["test-agent"] = &Agent{ID: "test-agent", Name: "T", Tool: "claude"}
	m.mu.Unlock()
	b := newKeyedTestBackend()
	m.backends["claude"] = b
	return m, b
}

func TestListAndStopBackgroundSessions(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	m, b := newBgSessionsTestManager(t)
	var mu sync.Mutex
	var abandoned []string
	b.onKeyedTasksAbandoned = func(agentID, key string, pending int, reason string, _ KeyedSessionSurface) {
		mu.Lock()
		abandoned = append(abandoned, fmt.Sprintf("%s|%d|%s", key, pending, reason))
		mu.Unlock()
	}
	if _, err := m.ListBackgroundSessions("nope"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("unknown agent err = %v", err)
	}
	idle := newKeyedTestSession(t, b, "test-agent:slack:C1:1.0")
	_ = newKeyedTestSession(t, b, "groupdm:gd_quiet") // no tasks: not listed
	writeLine(t, idle, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"t1","task_type":"local_bash","description":"sleep 600"}]}`)
	waitFor(t, "slot", func() bool { return b.lingeringCount("test-agent", "") == 1 })
	syncSession(idle)

	snap, err := m.ListBackgroundSessions("test-agent")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Cap != maxLingeringSessionsPerAgent || snap.Lingering != 1 || len(snap.Sessions) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	s := snap.Sessions[0]
	if s.SessionKey != "test-agent:slack:C1:1.0" || s.Surface != "slack" || s.ThreadID != "C1:1.0" ||
		s.State != "lingering" || s.PendingCount != 1 || s.LingeringSince == "" {
		t.Fatalf("session = %+v", s)
	}
	if len(s.Tasks) != 1 || s.Tasks[0].ID != "t1" || s.Tasks[0].Type != "local_bash" || s.Tasks[0].Description != "sleep 600" || s.Tasks[0].StartedAt == "" {
		t.Fatalf("tasks = %+v", s.Tasks)
	}

	if err := m.StopBackgroundTask("test-agent", "test-agent:slack:C1:1.0", "missing"); !errors.Is(err, ErrBackgroundTaskNotFound) {
		t.Fatalf("stop unknown task err = %v", err)
	}
	if err := m.StopBackgroundSession("test-agent", "groupdm:gd_quiet"); !errors.Is(err, ErrBackgroundSessionNotFound) {
		t.Fatalf("stop session without tasks err = %v", err)
	}
	if err := m.StopBackgroundSession("test-agent", "groupdm:none"); !errors.Is(err, ErrBackgroundSessionNotFound) {
		t.Fatalf("stop unknown session err = %v", err)
	}

	// Stopping the idle lingering session closes it; the abandoned notice
	// carries the stop reason.
	if err := m.StopBackgroundSession("test-agent", "test-agent:slack:C1:1.0"); err != nil {
		t.Fatal(err)
	}
	waitKilled(t, idle, 3*time.Second)
	waitFor(t, "stop notice", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(abandoned) == 1
	})
	mu.Lock()
	got := abandoned[0]
	mu.Unlock()
	if got != "test-agent:slack:C1:1.0|1|"+KeyedStopRequestedReason {
		t.Fatalf("abandoned = %q", got)
	}
	snap, _ = m.ListBackgroundSessions("test-agent")
	if len(snap.Sessions) != 0 || snap.Lingering != 0 {
		t.Fatalf("after stop snapshot = %+v", snap)
	}
}

// recordingWriteCloser captures stdin lines written to a test session.
type recordingWriteCloser struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (w *recordingWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
func (w *recordingWriteCloser) Close() error { return nil }
func (w *recordingWriteCloser) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// A session in the middle of a turn (possibly the turn calling the API) is not
// killed: each task gets a stop_task control request.
func TestStopBackgroundSessionInTurnSendsStopTask(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	m, b := newBgSessionsTestManager(t)
	k := newKeyedTestSession(t, b, "groupdm:gd_turn")
	rec := &recordingWriteCloser{}
	k.s.stdinW = &claudeStdinWriter{w: rec}
	if _, err := k.s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "go", false, false); err != nil {
		t.Fatal(err)
	}
	writeLine(t, k, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"x1"},{"task_id":"x2"}]}`)
	waitFor(t, "tasks recorded", func() bool {
		k.s.mu.Lock()
		defer k.s.mu.Unlock()
		return len(k.s.tasks) == 2
	})
	snap, _ := m.ListBackgroundSessions("test-agent")
	if len(snap.Sessions) != 1 || snap.Sessions[0].State != "turn" || snap.Sessions[0].Surface != "webui_thread" || snap.Sessions[0].ThreadID != "gd_turn" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if err := m.StopBackgroundTask("test-agent", "groupdm:gd_turn", "x1"); err != nil {
		t.Fatal(err)
	}
	if err := m.StopBackgroundSession("test-agent", "groupdm:gd_turn"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-k.killed:
		t.Fatal("in-turn session was killed by stop")
	case <-time.After(50 * time.Millisecond):
	}
	out := rec.String()
	// x1 is already stopping: the session stop only adds x2.
	if strings.Count(out, `"subtype":"stop_task"`) != 2 || strings.Count(out, `"task_id":"x1"`) != 1 || !strings.Contains(out, `"task_id":"x2"`) {
		t.Fatalf("stdin = %s", out)
	}
}

// --- WebUI thread delivery ---

func setupBgThread(t *testing.T) (*GroupDMManager, *Manager, string) {
	t.Helper()
	gdm, mgr := setupGroupDMTest(t)
	mgr.SetGroupDMManager(gdm)
	initOneShotMaps(mgr)
	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}
	return gdm, mgr, g.ID
}

func agentMessages(t *testing.T, gdm *GroupDMManager, groupID string) []*GroupMessage {
	t.Helper()
	msgs, _, _, err := gdm.Messages(groupID, 50, "")
	if err != nil {
		t.Fatal(err)
	}
	var out []*GroupMessage
	for _, msg := range msgs {
		if msg.AgentID == "ag_alice" {
			out = append(out, msg)
		}
	}
	return out
}

// A background notification turn of a WebUI thread session is routed to the
// GroupDMManager and posted into the same room.
func TestWebUIThreadBackgroundTurnPostsIntoRoom(t *testing.T) {
	gdm, mgr, groupID := setupBgThread(t)
	key := "groupdm:" + groupID
	if !mgr.hasKeyedBackgroundHandler("ag_alice", key) {
		t.Fatal("WebUI thread keys must have a keyed background handler")
	}
	events := make(chan ChatEvent, 4)
	events <- ChatEvent{Type: "text", Delta: "bg finished"}
	events <- ChatEvent{Type: "done", Message: &Message{Role: "assistant", Content: "bg finished"}}
	close(events)
	mgr.handleKeyedBackgroundTurn("ag_alice", key, events, nil, nil, nil, nil)
	msg := waitForMessage(t, gdm, groupID, "bg finished")
	if msg.AgentID != "ag_alice" {
		t.Fatalf("author = %q", msg.AgentID)
	}

	// A notification turn that leaves tasks running gets the pending note.
	events = make(chan ChatEvent, 4)
	events <- ChatEvent{Type: "text", Delta: "one done"}
	events <- ChatEvent{Type: "done", BackgroundTasksPending: 1, Message: &Message{Role: "assistant", Content: "one done"}}
	close(events)
	mgr.handleKeyedBackgroundTurn("ag_alice", key, events, nil, nil, nil, nil)
	waitForMessage(t, gdm, groupID, "one done\n\n"+threadBackgroundPendingNote(1))
}

func TestWebUIThreadBackgroundTurnIgnoresForeignRoom(t *testing.T) {
	gdm, _, groupID := setupBgThread(t)
	aborted := make(chan struct{}, 1)
	events := make(chan ChatEvent, 2)
	events <- ChatEvent{Type: "done", Message: &Message{Role: "assistant", Content: "wrong agent"}}
	close(events)
	// ag_bob is not the thread's member: nothing is posted, the turn aborted.
	gdm.HandleKeyedBackgroundTurn("ag_bob", "groupdm:"+groupID, events, func() { aborted <- struct{}{} })
	select {
	case <-aborted:
	case <-time.After(time.Second):
		t.Fatal("foreign background turn not aborted")
	}
	if msgs := agentMessages(t, gdm, groupID); len(msgs) != 0 {
		t.Fatalf("posted %d messages", len(msgs))
	}
}

// A normal thread turn runs with LingerBackgroundTasks and appends the pending
// note when the session keeps lingering; a message steered into a running
// background turn posts nothing itself.
func TestThreadTurnBackgroundPendingNoteAndSteer(t *testing.T) {
	gdm, _, groupID := setupBgThread(t)
	var mu sync.Mutex
	var opts []OneShotOpts
	done := ChatEvent{Type: "done", BackgroundTasksPending: 2}
	text := "started two jobs"
	gdm.oneShot = func(ctx context.Context, agentID, userMessage string, o OneShotOpts) (<-chan ChatEvent, error) {
		mu.Lock()
		opts = append(opts, o)
		d, txt := done, text
		mu.Unlock()
		ch := make(chan ChatEvent, 2)
		if txt != "" {
			ch <- ChatEvent{Type: "text", Delta: txt}
		}
		ch <- d
		close(ch)
		return ch, nil
	}
	if _, err := gdm.PostUserMessage(context.Background(), groupID, "run both", nil, true); err != nil {
		t.Fatal(err)
	}
	waitForMessage(t, gdm, groupID, "started two jobs\n\n"+threadBackgroundPendingNote(2))
	mu.Lock()
	if len(opts) != 1 || !opts[0].LingerBackgroundTasks || opts[0].SessionKey != "groupdm:"+groupID {
		t.Fatalf("opts = %+v", opts)
	}
	done = ChatEvent{Type: "done", SteeredIntoBackground: true}
	text = ""
	mu.Unlock()

	if _, err := gdm.PostUserMessage(context.Background(), groupID, "and also this", nil, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second turn", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(opts) == 2
	})
	time.Sleep(150 * time.Millisecond)
	if msgs := agentMessages(t, gdm, groupID); len(msgs) != 1 {
		t.Fatalf("steered turn posted a reply: %d agent messages", len(msgs))
	}
}

func TestWebUIThreadAbandonedNotice(t *testing.T) {
	gdm, mgr, groupID := setupBgThread(t)
	mgr.handleKeyedTasksAbandoned("ag_alice", "groupdm:"+groupID, 2, lingerCapReason(), nil)
	waitForMessage(t, gdm, groupID, threadBackgroundAbandonedNote(2, lingerCapReason()))
	if note := mgr.peekKeyedNote("ag_alice", "groupdm:"+groupID); !strings.Contains(note, "2件") {
		t.Fatalf("agent note = %q", note)
	}
	mgr.handleKeyedTasksAbandoned("ag_alice", "groupdm:"+groupID, 1, KeyedStopRequestedReason, nil)
	waitForMessage(t, gdm, groupID, threadBackgroundAbandonedNote(1, KeyedStopRequestedReason))
}

// A lifecycle cancel (reset / delete / shutdown cancelling the tracked
// one-shot) discards the background turn instead of posting its partial
// output, and the one-shot stays tracked until the handler is done.
func TestWebUIThreadBackgroundTurnLifecycleCancelPostsNothing(t *testing.T) {
	gdm, mgr, groupID := setupBgThread(t)
	key := "groupdm:" + groupID
	events := make(chan ChatEvent, 4)
	aborted := make(chan struct{}, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		mgr.handleKeyedBackgroundTurn("ag_alice", key, events, nil, func() {
			select {
			case aborted <- struct{}{}:
			default:
			}
		}, nil, nil)
	}()
	events <- ChatEvent{Type: "text", Delta: "partial"}
	waitFor(t, "background turn tracked", func() bool { return oneShotCount(mgr, "ag_alice") > 0 })
	mgr.cancelOneShots("ag_alice")
	select {
	case <-aborted:
	case <-time.After(3 * time.Second):
		t.Fatal("lifecycle cancel did not abort the CLI turn")
	}
	// The CLI answers the interrupt with a terminal event.
	events <- ChatEvent{Type: "done", Message: &Message{Role: "assistant", Content: "partial"}}
	close(events)
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("background turn did not finish")
	}
	if oneShotCount(mgr, "ag_alice") > 0 {
		t.Fatal("one-shot still tracked after the handler returned")
	}
	if msgs := agentMessages(t, gdm, groupID); len(msgs) != 0 {
		t.Fatalf("lifecycle-cancelled background turn posted %d messages: %q", len(msgs), msgs[0].Content)
	}
}

func oneShotCount(m *Manager, agentID string) int {
	m.oneShotCancelsMu.Lock()
	defer m.oneShotCancelsMu.Unlock()
	return len(m.oneShotCancels[agentID])
}

// SteerOneShot falls back to a WebUI thread's background-turn steer when no
// one-shot turn is running, and when the one-shot entry is stale (its gate
// already closed: ErrAgentNotBusy, nothing written).
func TestSteerOneShotFallsBackToKeyedBackgroundTurn(t *testing.T) {
	m := newTestManager(t)
	initOneShotMaps(m)
	key := "groupdm:gd_steer"
	var mu sync.Mutex
	var got []string
	unregister := m.registerKeyedBgSteer(key, "", func(text string) error {
		mu.Lock()
		got = append(got, text)
		mu.Unlock()
		return nil
	})
	if err := m.SteerOneShot(key, "first"); err != nil {
		t.Fatalf("steer without one-shot: %v", err)
	}
	m.oneShotSteersMu.Lock()
	m.oneShotSteers[key] = func(string) error { return ErrAgentNotBusy }
	m.oneShotSteersMu.Unlock()
	if err := m.SteerOneShot(key, "second"); err != nil {
		t.Fatalf("steer with stale one-shot entry: %v", err)
	}
	unregister()
	if err := m.SteerOneShot(key, "third"); !errors.Is(err, ErrAgentNotBusy) {
		t.Fatalf("steer after unregister err = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, ",") != "first,second" {
		t.Fatalf("background steer got %v", got)
	}
}

// A lifecycle cancel landing while the finished turn is being finalized (the
// attachment scan, or auto-titling) still discards the reply and its staged
// files: deterministic via threadFinalizeHook.
func TestThreadTurnLifecycleCancelDuringFinalize(t *testing.T) {
	for _, stage := range []string{"scanned", "titled"} {
		t.Run(stage, func(t *testing.T) {
			gdm, _, groupID := setupBgThread(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hits := 0
			threadFinalizeHook = func(s string) {
				if s == stage {
					hits++
					cancel()
				}
			}
			t.Cleanup(func() { threadFinalizeHook = nil })
			stageDir := threadAttachmentStageDir("ag_alice", groupID)
			if err := os.MkdirAll(stageDir, 0o755); err != nil {
				t.Fatal(err)
			}
			events := make(chan ChatEvent, 2)
			events <- ChatEvent{Type: "text", Delta: "late reply"}
			events <- ChatEvent{Type: "done", Message: &Message{Role: "assistant", Content: "late reply"}}
			close(events)
			gdm.consumeThreadTurn(ctx, events, threadTurnOutput{
				agentID: "ag_alice", groupID: groupID, replyMessageID: "m_finalize",
				attachmentStageDir: stageDir, firstUserMessage: "hello there",
			})
			if hits != 1 {
				t.Fatalf("finalize hook %q hit %d times", stage, hits)
			}
			if msgs := agentMessages(t, gdm, groupID); len(msgs) != 0 {
				t.Fatalf("cancel during %s posted %q", stage, msgs[0].Content)
			}
			if _, err := os.Stat(stageDir); !os.IsNotExist(err) {
				t.Fatalf("stage dir survived the discarded turn: %v", err)
			}
		})
	}
	// Control: without a cancel the same turn posts.
	gdm, _, groupID := setupBgThread(t)
	events := make(chan ChatEvent, 1)
	events <- ChatEvent{Type: "done", Message: &Message{Role: "assistant", Content: "posted"}}
	close(events)
	gdm.consumeThreadTurn(context.Background(), events, threadTurnOutput{agentID: "ag_alice", groupID: groupID, replyMessageID: "m_ok"})
	if msgs := agentMessages(t, gdm, groupID); len(msgs) != 1 || msgs[0].Content != "posted" {
		t.Fatalf("control turn not posted: %v", msgs)
	}
}

// A stopped WebUI thread turn ends only itself: its reply says the thread's
// background tasks keep running (instead of dropping the note).
func TestThreadStoppedTurnSaysBackgroundContinues(t *testing.T) {
	gdm, _, groupID := setupBgThread(t)
	gdm.threadCancelMu.Lock()
	if gdm.threadStopped == nil {
		gdm.threadStopped = make(map[string]bool)
	}
	gdm.threadStopped[groupID] = true
	gdm.threadCancelMu.Unlock()
	events := make(chan ChatEvent, 2)
	events <- ChatEvent{Type: "text", Delta: "partial"}
	events <- ChatEvent{Type: "done", ErrorMessage: ErrMsgCancelled, BackgroundTasksPending: 2}
	close(events)
	gdm.consumeThreadTurn(context.Background(), events, threadTurnOutput{agentID: "ag_alice", groupID: groupID, replyMessageID: "m_stopped"})
	msgs := agentMessages(t, gdm, groupID)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Content, threadStoppedBackgroundNote(2)) {
		var got []string
		for _, m := range msgs {
			got = append(got, m.Content)
		}
		t.Fatalf("stopped reply = %q", got)
	}
}
