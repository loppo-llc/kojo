package agent

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

type fakeKeyedSurface struct {
	mu        sync.Mutex
	refs      int
	origin    string
	turns     chan []ChatEvent
	ctxs      chan context.Context
	abandoned chan int
	onTurn    func()
	reason    string
}

func newFakeKeyedSurface(origin string) *fakeKeyedSurface {
	return &fakeKeyedSurface{origin: origin, turns: make(chan []ChatEvent, 4), ctxs: make(chan context.Context, 4), abandoned: make(chan int, 4)}
}

func (f *fakeKeyedSurface) HandleKeyedSessionTurn(ctx context.Context, _, _ string, events <-chan ChatEvent, _ func()) {
	f.ctxs <- ctx
	if f.onTurn != nil {
		f.onTurn()
	}
	var evs []ChatEvent
	for e := range events {
		evs = append(evs, e)
	}
	f.turns <- evs
}

func (f *fakeKeyedSurface) KeyedBackgroundTasksAbandoned(_, _ string, pending int, reason string) {
	f.mu.Lock()
	f.reason = reason
	f.mu.Unlock()
	f.abandoned <- pending
}

func (f *fakeKeyedSurface) lastReason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reason
}
func (f *fakeKeyedSurface) OriginPeerID() string { return f.origin }
func (f *fakeKeyedSurface) Retain()              { f.mu.Lock(); f.refs++; f.mu.Unlock() }
func (f *fakeKeyedSurface) Release()             { f.mu.Lock(); f.refs--; f.mu.Unlock() }
func (f *fakeKeyedSurface) refCount() int        { f.mu.Lock(); defer f.mu.Unlock(); return f.refs }

func TestKeyedSurfaceTakesPrecedenceForBackgroundTurn(t *testing.T) {
	m := newTestManager(t)
	initOneShotMaps(m)
	h := &recordingKeyedHandler{got: make(chan []ChatEvent, 1)}
	defer m.RegisterKeyedBackgroundHandler("ag1", h)()
	sf := newFakeKeyedSurface("hub")
	var origins []string
	sf.onTurn = func() {
		m.oneShotCancelsMu.Lock()
		for _, o := range m.oneShotOrigins["ag1"] {
			origins = append(origins, o)
		}
		m.oneShotCancelsMu.Unlock()
	}
	events := make(chan ChatEvent, 2)
	events <- ChatEvent{Type: "done", Message: &Message{Role: "assistant", Content: "bg"}, BackgroundTasksPending: 1}
	close(events)
	m.handleKeyedBackgroundTurn("ag1", "ag1:slack:C1:1.0", events, nil, nil, nil, sf)
	evs := <-sf.turns
	if d := doneOf(t, evs); d.BackgroundTasksPending != 1 {
		t.Fatalf("done = %#v", d)
	}
	if ctx := <-sf.ctxs; ctx == nil {
		t.Fatal("surface got no lifecycle ctx")
	}
	if len(origins) != 1 || origins[0] != "hub" {
		t.Fatalf("tracked origins = %v, want [hub]", origins)
	}
	select {
	case <-h.got:
		t.Fatal("agent-wide handler received a surface-bound turn")
	default:
	}
}

func TestKeyedSurfaceReceivesAbandonedNotice(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.agents["ag1"] = &Agent{ID: "ag1", Name: "A", Tool: "claude"}
	m.mu.Unlock()
	h := &recordingKeyedHandler{got: make(chan []ChatEvent, 1)}
	defer m.RegisterKeyedBackgroundHandler("ag1", h)()
	sf := newFakeKeyedSurface("hub")
	m.handleKeyedTasksAbandoned("ag1", "ag1:slack:C1:1.0", 3, "x", sf)
	if got := <-sf.abandoned; got != 3 {
		t.Fatalf("pending = %d", got)
	}
	if note := m.peekKeyedNote("ag1", "ag1:slack:C1:1.0"); note == "" {
		t.Fatal("abandoned note not recorded for the next turn")
	}
}

func TestKeyedSessionSurfaceLifetimeAndExitNotice(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	b := newKeyedTestBackend()
	release := make(chan struct{})
	var gotSurface KeyedSessionSurface
	b.onKeyedTasksAbandoned = func(_, _ string, _ int, _ string, surface KeyedSessionSurface) {
		gotSurface = surface
		<-release
	}
	k := newKeyedTestSession(t, b, "test-agent:slack:C1:1.0")
	first, second := newFakeKeyedSurface("hub"), newFakeKeyedSurface("hub")
	k.s.bindSurface(first)
	k.s.bindSurface(second)
	if first.refCount() != 0 || second.refCount() != 1 {
		t.Fatalf("refs first=%d second=%d, want 0/1", first.refCount(), second.refCount())
	}
	io.WriteString(k.pw, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"t1"}]}`+"\n")
	time.Sleep(50 * time.Millisecond)
	b.CloseSession("test-agent")
	waitKilled(t, k, 5*time.Second)
	k.s.awaitDead(3 * time.Second)
	if b.waitKeyedExitNotices("test-agent", 100*time.Millisecond) {
		t.Fatal("exit notice wait returned before the abandoned notice finished")
	}
	if second.refCount() != 1 {
		t.Fatal("surface released before its abandoned notice was delivered")
	}
	close(release)
	if !b.waitKeyedExitNotices("test-agent", 3*time.Second) {
		t.Fatal("exit notice never finished")
	}
	if gotSurface != second || second.refCount() != 0 {
		t.Fatalf("surface=%v refs=%d, want bound surface released", gotSurface, second.refCount())
	}
	// Binding after exit must not leak a reference.
	late := newFakeKeyedSurface("hub")
	k.s.bindSurface(late)
	if late.refCount() != 0 {
		t.Fatal("surface bound to a dead session kept a reference")
	}
}
