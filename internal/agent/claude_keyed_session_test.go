package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func TestBackgroundTaskCountParsing(t *testing.T) {
	cases := []struct {
		line   string
		n      int
		wantOK bool
	}{
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a","task_type":"local_bash","description":"sleep"},{"task_id":"b"}]}`, 2, true},
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`, 0, true},
		{`{"type":"system","subtype":"background_tasks_changed"}`, 0, true},
		{`{"type":"system","subtype":"init","tasks":[{"task_id":"a"}]}`, 0, false},
		{`{"type":"result","subtype":"success"}`, 0, false},
	}
	for _, c := range cases {
		var e claudeStreamEvent
		if err := json.Unmarshal([]byte(c.line), &e); err != nil {
			t.Fatalf("unmarshal %s: %v", c.line, err)
		}
		n, ok := backgroundTaskCount(e)
		if n != c.n || ok != c.wantOK {
			t.Fatalf("%s: got (%d,%v) want (%d,%v)", c.line, n, ok, c.n, c.wantOK)
		}
	}
}

type keyedTestSession struct {
	s      *claudeSession
	pw     *io.PipeWriter
	killed chan struct{}
}

// newKeyedTestSession builds a keyed claudeSession with no real process. Its
// procCancel closes stdout, emulating the CLI exiting.
func newKeyedTestSession(t *testing.T, b *ClaudeBackend, key string) *keyedTestSession {
	t.Helper()
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	killed := make(chan struct{})
	var once sync.Once
	s := &claudeSession{
		b:            b,
		agentID:      "test-agent",
		logger:       b.logger,
		fingerprint:  "fp",
		stdinW:       &claudeStdinWriter{w: nopWriteCloser{io.Discard}},
		state:        sessIdle,
		lastActivity: time.Now(),
		keyed:        true,
		sessionKey:   key,
		poolKey:      keyedPoolKey("test-agent", key),
		procCtx:      ctx,
		procCancel: func() {
			once.Do(func() {
				cancel()
				close(killed)
				pw.Close()
			})
		},
	}
	b.sessMu.Lock()
	b.sessions[s.poolKey] = s
	b.sessMu.Unlock()
	go s.readLoop(pr)
	t.Cleanup(func() { s.procCancel() })
	return &keyedTestSession{s: s, pw: pw, killed: killed}
}

func newKeyedTestBackend() *ClaudeBackend {
	return &ClaudeBackend{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), sessions: map[string]*claudeSession{}}
}

func setKeyedTimers(t *testing.T, grace, max, closeGrace time.Duration) {
	t.Helper()
	og, om, oc := keyedLingerGrace, keyedLingerMax, keyedCloseGrace
	keyedLingerGrace, keyedLingerMax, keyedCloseGrace = grace, max, closeGrace
	t.Cleanup(func() { keyedLingerGrace, keyedLingerMax, keyedCloseGrace = og, om, oc })
}

func waitKilled(t *testing.T, k *keyedTestSession, within time.Duration) {
	t.Helper()
	select {
	case <-k.killed:
	case <-time.After(within):
		t.Fatal("keyed session was not closed")
	}
}

func doneOf(t *testing.T, evs []ChatEvent) ChatEvent {
	t.Helper()
	for _, e := range evs {
		if e.Type == "done" {
			return e
		}
	}
	t.Fatalf("no done event in %+v", evs)
	return ChatEvent{}
}

func TestKeyedSessionClosesAtResultWithoutPendingTasks(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	b := newKeyedTestBackend()
	k := newKeyedTestSession(t, b, "test-agent:slack:C1:1.0")
	sink, err := k.s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "hi", false, false)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(k.pw, `{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`+"\n")
	io.WriteString(k.pw, `{"type":"result","subtype":"success","result":"ok"}`+"\n")
	d := doneOf(t, collect(t, sink, 3*time.Second))
	if d.BackgroundTasksPending != 0 {
		t.Fatalf("pending = %d", d.BackgroundTasksPending)
	}
	waitKilled(t, k, 3*time.Second)
}

func TestKeyedSessionLingersWhilePendingThenCloses(t *testing.T) {
	setKeyedTimers(t, 50*time.Millisecond, time.Hour, 10*time.Millisecond)
	b := newKeyedTestBackend()
	bgCh := make(chan (<-chan ChatEvent), 1)
	b.onBackgroundTurn = func(string, <-chan ChatEvent, AnswerFunc, func()) {
		t.Error("keyed unsolicited turn reached the main background handler")
	}
	b.onKeyedBackgroundTurn = func(agentID, key string, events <-chan ChatEvent, _ AnswerFunc, _ func(), _ SteerFunc, _ KeyedSessionSurface) {
		if key != "test-agent:slack:C1:1.0" {
			t.Errorf("key = %q", key)
		}
		bgCh <- events
	}
	k := newKeyedTestSession(t, b, "test-agent:slack:C1:1.0")
	sink, err := k.s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "run in bg", false, false)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(k.pw, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"t1"}]}`+"\n")
	io.WriteString(k.pw, `{"type":"assistant","message":{"content":[{"type":"text","text":"started"}]}}`+"\n")
	io.WriteString(k.pw, `{"type":"result","subtype":"success","result":"started"}`+"\n")
	d := doneOf(t, collect(t, sink, 3*time.Second))
	if d.BackgroundTasksPending != 1 {
		t.Fatalf("pending = %d, want 1", d.BackgroundTasksPending)
	}
	select {
	case <-k.killed:
		t.Fatal("session closed despite pending background task")
	case <-time.After(150 * time.Millisecond):
	}
	// Task completes: tasks=[] then the notification turn.
	io.WriteString(k.pw, `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`+"\n")
	io.WriteString(k.pw, `{"type":"system","subtype":"init"}`+"\n")
	io.WriteString(k.pw, `{"type":"assistant","message":{"content":[{"type":"text","text":"bg finished"}]}}`+"\n")
	io.WriteString(k.pw, `{"type":"result","subtype":"success","result":"bg finished","origin":{"kind":"task-notification"}}`+"\n")
	var events <-chan ChatEvent
	select {
	case events = <-bgCh:
	case <-time.After(3 * time.Second):
		t.Fatal("keyed background handler not invoked")
	}
	bd := doneOf(t, collect(t, events, 3*time.Second))
	if bd.Message == nil || bd.Message.Content != "bg finished" {
		t.Fatalf("background done = %+v", bd.Message)
	}
	// Nothing pending anymore: the grace elapses and the process closes.
	waitKilled(t, k, 3*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for {
		b.sessMu.Lock()
		n := len(b.sessions)
		b.sessMu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool still holds %d sessions", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestKeyedSessionClosedByAgentLifecycle(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	b := newKeyedTestBackend()
	var mu sync.Mutex
	var abandoned []int
	b.onKeyedTasksAbandoned = func(agentID, key string, pending int, reason string, _ KeyedSessionSurface) {
		mu.Lock()
		abandoned = append(abandoned, pending)
		mu.Unlock()
	}
	k1 := newKeyedTestSession(t, b, "test-agent:slack:C1:1.0")
	k2 := newKeyedTestSession(t, b, "test-agent:slack:C1:2.0")
	io.WriteString(k1.pw, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"t1"},{"task_id":"t2"}]}`+"\n")
	time.Sleep(50 * time.Millisecond)
	if !b.HasLiveSession("test-agent") {
		t.Fatal("HasLiveSession should see keyed sessions")
	}
	b.CloseSession("test-agent")
	waitKilled(t, k1, 5*time.Second)
	waitKilled(t, k2, 5*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		got := append([]int(nil), abandoned...)
		mu.Unlock()
		if len(got) == 1 && got[0] == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("abandoned notices = %v, want [2]", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if b.HasKeyedSession("test-agent", "test-agent:slack:C1:1.0") {
		t.Fatal("keyed session still pooled")
	}
}

type recordingKeyedHandler struct {
	got chan []ChatEvent
}

func (h *recordingKeyedHandler) HandleKeyedBackgroundTurn(agentID, key string, events <-chan ChatEvent, _ func()) {
	var evs []ChatEvent
	for e := range events {
		evs = append(evs, e)
	}
	h.got <- evs
}

func (h *recordingKeyedHandler) KeyedBackgroundTasksAbandoned(string, string, int, string) {}

func TestManagerRoutesKeyedBackgroundTurnToHandler(t *testing.T) {
	m := newTestManager(t)
	m.oneShotCancels = make(map[string]map[int64]context.CancelFunc)
	m.oneShotSessions = make(map[string]map[int64]string)
	m.oneShotOrigins = make(map[string]map[int64]string)
	m.oneShotHandoffCaps = make(map[string]map[int64]string)
	m.oneShotArmed = make(map[string]map[int64]time.Time)
	m.oneShotDone = make(map[string]map[int64]chan struct{})
	m.oneShotSteers = make(map[string]SteerFunc)
	h := &recordingKeyedHandler{got: make(chan []ChatEvent, 1)}
	unregister := m.RegisterKeyedBackgroundHandler("ag1", h)
	defer unregister()
	if !m.hasKeyedBackgroundHandler("ag1", "ag1:slack:C1:1.0") {
		t.Fatal("handler not registered")
	}
	events := make(chan ChatEvent, 4)
	events <- ChatEvent{Type: "text", Delta: "bg result"}
	events <- ChatEvent{Type: "done", Message: &Message{Role: "assistant", Content: "bg result"}}
	close(events)
	m.handleKeyedBackgroundTurn("ag1", "ag1:slack:C1:1.0", events, nil, nil, nil, nil)
	evs := <-h.got
	d := doneOf(t, evs)
	if d.Message == nil || d.Message.Content != "bg result" {
		t.Fatalf("done = %+v", d.Message)
	}
	m.busyMu.Lock()
	_, busy := m.busy["ag1"]
	m.busyMu.Unlock()
	if busy {
		t.Fatal("keyed background turn must not take the main busy slot")
	}
	unregister()
	if m.hasKeyedBackgroundHandler("ag1", "ag1:slack:C1:1.0") {
		t.Fatal("unregister did not remove handler")
	}
}
