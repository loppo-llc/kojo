package slackbot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/slack-go/slack"
)

type slackCall struct{ path, thread, body string }

func newRecordingBot(t *testing.T) (*Bot, func() []slackCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []slackCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = r.ParseForm()
		mu.Lock()
		calls = append(calls, slackCall{path: r.URL.Path, thread: r.FormValue("thread_ts"), body: r.Form.Encode()})
		mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"channel":"C1","ts":"post.1","messages":[]}`)
	}))
	t.Cleanup(srv.Close)
	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bot := &Bot{
		agentID: "test-agent", agentDataDir: t.TempDir(),
		config: agent.SlackBotConfig{Enabled: true, ThreadReplies: true},
		api:    api, mgr: &mockMgr{}, logger: testLogger, botUserID: "UBOTTEST",
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
		threadLocks: make(map[string]*threadLock), activeTurns: make(map[string][]*activeTurn),
		stoppingTurns: make(map[string]*activeTurn),
		userCache:     make(map[string]string), sem: make(chan struct{}, maxConcurrentChats),
	}
	return bot, func() []slackCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]slackCall(nil), calls...)
	}
}

func postedText(calls []slackCall) string {
	var sb strings.Builder
	for _, c := range calls {
		if strings.Contains(c.path, "chat.") {
			sb.WriteString(c.body)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func TestHandleKeyedBackgroundTurnPostsIntoThread(t *testing.T) {
	bot, calls := newRecordingBot(t)
	events := make(chan agent.ChatEvent, 4)
	events <- agent.ChatEvent{Type: "text", Delta: "background job finished"}
	events <- agent.ChatEvent{Type: "done", Message: &agent.Message{Role: "assistant", Content: "background job finished"}}
	close(events)

	// A user turn holds the thread FIFO first; the synthetic admission must
	// wait for it rather than interleave.
	held := bot.reserveThread("C1", "1700.1")
	held.Wait()
	finished := make(chan struct{})
	go func() {
		bot.HandleKeyedBackgroundTurn("test-agent", slackSessionKey("test-agent", "C1", "1700.1"), events, func() {})
		close(finished)
	}()
	select {
	case <-finished:
		t.Fatal("background turn bypassed the thread FIFO")
	case <-time.After(100 * time.Millisecond):
	}
	bot.releaseThreadReservation("C1", "1700.1", held)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("background delivery did not finish")
	}
	got := calls()
	var threaded bool
	for _, c := range got {
		if strings.Contains(c.path, "chat.") && c.thread == "1700.1" {
			threaded = true
		}
		if strings.Contains(c.path, "chat.") && c.thread != "" && c.thread != "1700.1" {
			t.Fatalf("posted into wrong thread: %+v", c)
		}
	}
	if !threaded || !strings.Contains(postedText(got), "background+job+finished") {
		t.Fatalf("background reply not posted into thread: %+v", got)
	}
	if bot.hasActiveTurn("C1", "1700.1") {
		t.Fatal("synthetic active turn leaked")
	}
}

func TestHandleKeyedBackgroundTurnIgnoresForeignKey(t *testing.T) {
	bot, calls := newRecordingBot(t)
	events := make(chan agent.ChatEvent, 1)
	events <- agent.ChatEvent{Type: "done", Message: &agent.Message{Content: "x"}}
	close(events)
	cancelled := false
	bot.HandleKeyedBackgroundTurn("test-agent", "other-agent:slack:C1:1.0", events, func() { cancelled = true })
	if !cancelled || len(calls()) != 0 {
		t.Fatalf("foreign key: cancelled=%v calls=%v", cancelled, calls())
	}
}

func TestBackgroundPendingNoteAppendedToFinalReply(t *testing.T) {
	bot, calls := newRecordingBot(t)
	events := make(chan agent.ChatEvent, 4)
	events <- agent.ChatEvent{Type: "text", Delta: "started it"}
	events <- agent.ChatEvent{Type: "done", Message: &agent.Message{Role: "assistant", Content: "started it"}, BackgroundTasksPending: 2}
	close(events)
	turnCtx, turnCancel := context.WithCancel(context.Background())
	defer turnCancel()
	active := bot.registerActiveTurn("C1", "1700.2", turnCancel)
	bot.deliverAgentTurn(turnCtx, slackTurnDelivery{channel: "C1", threadTS: "1700.2", sessionKey: slackSessionKey("test-agent", "C1", "1700.2"), turnCtx: turnCtx, turnCancel: turnCancel, active: active}, events)
	want := "バックグラウンド処理 2件 実行中"
	text := postedText(calls())
	// Form bodies are URL-encoded; decode loosely by checking the helper text.
	if !strings.Contains(text, urlEncode(want)) {
		t.Fatalf("pending note missing; calls=%s", text)
	}
}

func TestBackgroundPendingNoteNotAppendedToNoReply(t *testing.T) {
	bot, calls := newRecordingBot(t)
	events := make(chan agent.ChatEvent, 2)
	events <- agent.ChatEvent{Type: "done", Message: &agent.Message{Role: "assistant", Content: agent.SlackNoReplyToken}, BackgroundTasksPending: 1}
	close(events)
	turnCtx, turnCancel := context.WithCancel(context.Background())
	defer turnCancel()
	active := bot.registerActiveTurn("C1", "1700.3", turnCancel)
	bot.deliverAgentTurn(turnCtx, slackTurnDelivery{channel: "C1", threadTS: "1700.3", sessionKey: slackSessionKey("test-agent", "C1", "1700.3"), turnCtx: turnCtx, turnCancel: turnCancel, active: active}, events)
	if strings.Contains(postedText(calls()), urlEncode("バックグラウンド処理")) {
		t.Fatal("pending note must not be posted for NO_REPLY")
	}
}

func urlEncode(s string) string { return url.QueryEscape(s) }

func TestIsStopAllCommand(t *testing.T) {
	for text, want := range map[string]bool{
		"!stop all": true, "!STOP  All": true, "!cancel all": true,
		"!stop": false, "!stop everything": false, "!stop all now": false, "stop all": false,
	} {
		if got := isStopAllCommand(text); got != want {
			t.Errorf("isStopAllCommand(%q) = %v, want %v", text, got, want)
		}
		if want && isStopCommand(text) {
			t.Errorf("%q also matched the plain !stop", text)
		}
	}
}

func TestKeyedAbandonedNoticeWording(t *testing.T) {
	if got := keyedAbandonedNotice(2, agent.KeyedUserStopReason); !strings.Contains(got, "2件 を停止しました（ユーザーの依頼）") {
		t.Fatal(got)
	}
	if got := keyedAbandonedNotice(1, agent.KeyedStopRequestedReason); !strings.Contains(got, "エージェントの依頼") {
		t.Fatal(got)
	}
	if got := keyedAbandonedNotice(3, "idle"); !strings.Contains(got, "3件 が完了前に終了しました（idle）") {
		t.Fatal(got)
	}
}

// A background (task-notification) turn has no owner: anyone in the thread may
// !stop it, while an ordinary user turn stays owner-only.
func TestBackgroundTurnAnyoneMayStop(t *testing.T) {
	bot, _ := newRecordingBot(t)
	stoppedBg := false
	bg := bot.registerBackgroundActiveTurn("C1", "1.0", func() { stoppedBg = true })
	defer bot.unregisterActiveTurn("C1", "1.0", bg)
	if _, _, denied := bot.cancelActiveTurnInternal("C1", "1.0", false, "USOMEONE", true); denied || !stoppedBg {
		t.Fatalf("background turn stop denied=%v stopped=%v", denied, stoppedBg)
	}
	if !bg.stopAllRequested() {
		t.Fatal("!stop all not recorded on the turn")
	}
	user := bot.registerActiveTurnForUser("C1", "2.0", "UOWNER", func() {})
	defer bot.unregisterActiveTurn("C1", "2.0", user)
	if _, _, denied := bot.cancelActiveTurnForCommand("C1", "2.0", "UOTHER"); !denied {
		t.Fatal("a non-owner stopped a user turn")
	}
}

// A Slack message arriving while a background turn streams is steered into it
// (whoever sends it), not queued as a separate turn.
func TestSlackMessageSteersBackgroundTurn(t *testing.T) {
	b := newTestBot(t, agent.SlackBotConfig{})
	defer b.cancel()
	m := &userSteerMgr{steerTestMgr: steerTestMgr{calls: make(chan steerCall, 1)}, users: make(chan string, 1)}
	b.mgr = m
	b.userCache["U777"] = "Bob"
	turn := b.registerBackgroundActiveTurn("C", "T", func() {})
	defer b.unregisterActiveTurn("C", "T", turn)
	b.processIncoming(context.Background(), "C", "T", "M", "also check the logs", "U777")
	select {
	case call := <-m.calls:
		if call.sessionKey != slackSessionKey(b.agentID, "C", "T") || !strings.Contains(call.content, "also check the logs") {
			t.Fatalf("steer = %+v", call)
		}
	case <-time.After(time.Second):
		t.Fatal("message was not steered into the background turn")
	}
	if u := <-m.users; u != "U777" {
		t.Fatal(u)
	}
	waitSteerQueue(t, turn)
	if m.oneShots.Load() != 0 {
		t.Fatal("steered message also queued a turn")
	}
}

// A stopped turn ends only itself: its reply (or the stop notice) says the
// thread's background tasks keep running — unless the stop was `!stop all`.
func TestStoppedTurnSaysBackgroundContinues(t *testing.T) {
	run := func(t *testing.T, thread, partial string, stopAll bool) string {
		bot, calls := newRecordingBot(t)
		events := make(chan agent.ChatEvent, 3)
		if partial != "" {
			events <- agent.ChatEvent{Type: "text", Delta: partial}
		}
		events <- agent.ChatEvent{Type: "done", ErrorMessage: agent.ErrMsgCancelled, Message: &agent.Message{Role: "assistant", Content: partial}, BackgroundTasksPending: 2}
		close(events)
		turnCtx, turnCancel := context.WithCancel(context.Background())
		defer turnCancel()
		active := bot.registerActiveTurn("C1", thread, turnCancel)
		if stopAll {
			active.markStopAll()
		}
		bot.deliverAgentTurn(turnCtx, slackTurnDelivery{channel: "C1", threadTS: thread, sessionKey: slackSessionKey("test-agent", "C1", thread), turnCtx: turnCtx, turnCancel: turnCancel, active: active}, events)
		return postedText(calls())
	}
	note := urlEncode("2件 は継続中です")
	if got := run(t, "3.0", "partial answer", false); !strings.Contains(got, note) {
		t.Fatalf("partial reply lacks the continuing note: %s", got)
	}
	if got := run(t, "3.1", "", false); !strings.Contains(got, note) {
		t.Fatalf("stop notice lacks the continuing note: %s", got)
	}
	if got := run(t, "3.2", "partial answer", true); strings.Contains(got, note) {
		t.Fatalf("!stop all reply claims the tasks continue: %s", got)
	}
}

type stopAllMgr struct {
	mockMgr
	keys chan string
	err  error
}

func (m *stopAllMgr) StopThreadBackgroundTasks(_ context.Context, agentID, sessionKey string) error {
	m.keys <- agentID + "|" + sessionKey
	return m.err
}

func TestStopAllCommandStopsThreadBackgroundTasks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		notice string
	}{
		{name: "stopped"},
		{name: "nothing", err: agent.ErrBackgroundSessionNotFound, notice: stopAllNothing},
		{name: "failed", err: fmt.Errorf("peer down"), notice: "peer down"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bot, calls := newRecordingBot(t)
			m := &stopAllMgr{keys: make(chan string, 1), err: tc.err}
			bot.mgr = m
			if !bot.handleSlackCommand(context.Background(), "C1", "5.0", "5.1", "U1", "!stop all") {
				t.Fatal("!stop all not handled")
			}
			select {
			case k := <-m.keys:
				if k != "test-agent|"+slackSessionKey("test-agent", "C1", "5.0") {
					t.Fatal(k)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("background tasks were not stopped")
			}
			if tc.notice == "" {
				time.Sleep(50 * time.Millisecond)
				if got := postedText(calls()); got != "" {
					t.Fatalf("success posted %s (the abandoned notice reports it)", got)
				}
				return
			}
			deadline := time.Now().Add(2 * time.Second)
			for !strings.Contains(postedText(calls()), urlEncode(tc.notice)) {
				if time.Now().After(deadline) {
					t.Fatalf("notice %q missing: %s", tc.notice, postedText(calls()))
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

type repeatStopAllMgr struct {
	mockMgr
	mu    sync.Mutex
	calls int
	keys  chan string
	first error // outcome of the first stop (nil: it took the tasks)
}

func (m *repeatStopAllMgr) StopThreadBackgroundTasks(_ context.Context, agentID, sessionKey string) error {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.mu.Unlock()
	m.keys <- sessionKey
	if n == 1 {
		return m.first // the first stop takes the tasks
	}
	return agent.ErrBackgroundSessionNotFound
}

func TestRepeatedStopAllDoesNotSayNothingToStop(t *testing.T) {
	t.Run("succeeded", func(t *testing.T) { testRepeatedStopAll(t, nil) })
	// A relayed stop whose response was lost may still have stopped them.
	t.Run("uncertain", func(t *testing.T) { testRepeatedStopAll(t, agent.ErrSteerDeliveryUncertain) })
}

func testRepeatedStopAll(t *testing.T, first error) {
	bot, calls := newRecordingBot(t)
	m := &repeatStopAllMgr{keys: make(chan string, 2), first: first}
	bot.mgr = m
	for i := 0; i < 2; i++ {
		if !bot.handleSlackCommand(context.Background(), "C1", "6.0", "6.1", "U1", "!stop all") {
			t.Fatal("!stop all not handled")
		}
		select {
		case <-m.keys:
		case <-time.After(2 * time.Second):
			t.Fatal("background tasks were not stopped")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := postedText(calls()); strings.Contains(got, urlEncode(stopAllNothing)) {
		t.Fatalf("repeat !stop all answered nothing-to-stop after a successful stop: %s", got)
	}
	// Another thread is unaffected.
	if !bot.handleSlackCommand(context.Background(), "C1", "7.0", "7.1", "U1", "!stop all") {
		t.Fatal("!stop all not handled")
	}
	<-m.keys
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(postedText(calls()), urlEncode(stopAllNothing)) {
		if time.Now().After(deadline) {
			t.Fatalf("other thread missing nothing-to-stop: %s", postedText(calls()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
