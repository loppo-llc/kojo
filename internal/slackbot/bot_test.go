package slackbot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/chathistory"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// newTestBot creates a Bot pointing at a mock Slack API for unit testing.
func newTestBot(t *testing.T, cfg agent.SlackBotConfig) *Bot {
	t.Helper()
	srv := mockSlackServer(t)

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	sm := socketmode.New(api)
	ctx, cancel := context.WithCancel(context.Background())

	return &Bot{
		agentID:      "test-agent",
		agentDataDir: t.TempDir(),
		config:       cfg,
		api:          api,
		sm:           sm,
		mgr:          &mockMgr{},
		logger:       testLogger,
		botUserID:    "UBOTTEST",
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
		threadLocks:  make(map[string]*threadLock),
		userCache:    make(map[string]string),
		sem:          make(chan struct{}, maxConcurrentChats),
	}
}

// --- Thread lock tests ---

func TestBotThreadLockRefCount(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	// Acquire lock for a thread.
	r1 := bot.reserveThread("C1", "1234.5678")
	r1.Wait()

	// Acquire again for the same thread — same lock, higher refcount.
	r2 := bot.reserveThread("C1", "1234.5678")
	if r1.tl != r2.tl {
		t.Fatal("expected same lock for same thread")
	}

	// Release once — should still exist (refcount > 0).
	bot.releaseThreadReservation("C1", "1234.5678", r1)
	bot.threadLocksMu.Lock()
	_, exists := bot.threadLocks["C1:1234.5678"]
	bot.threadLocksMu.Unlock()
	if !exists {
		t.Fatal("lock should still exist after one release")
	}

	// Release again — refcount hits 0, entry removed.
	r2.Wait()
	bot.releaseThreadReservation("C1", "1234.5678", r2)
	bot.threadLocksMu.Lock()
	_, exists = bot.threadLocks["C1:1234.5678"]
	bot.threadLocksMu.Unlock()
	if exists {
		t.Fatal("lock should be removed when refcount reaches zero")
	}
}

func TestBotThreadLockIsolation(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	r1 := bot.reserveThread("C1", "ts1")
	r2 := bot.reserveThread("C1", "ts2")
	r3 := bot.reserveThread("C2", "ts1")

	if r1.tl == r2.tl || r1.tl == r3.tl || r2.tl == r3.tl {
		t.Fatal("different channel/thread combos should get different locks")
	}

	bot.releaseThreadReservation("C1", "ts1", r1)
	bot.releaseThreadReservation("C1", "ts2", r2)
	bot.releaseThreadReservation("C2", "ts1", r3)
}

func TestBotThreadPairKeepsArrivalAheadOfNextTurn(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	turn, arrival := bot.reserveThreadPair("C1", "T1")
	next := bot.reserveThread("C1", "T1")
	turn.Wait()
	bot.releaseThreadReservation("C1", "T1", turn)
	arrival.Wait()

	nextReady := make(chan struct{})
	go func() {
		next.Wait()
		close(nextReady)
	}()
	select {
	case <-nextReady:
		t.Fatal("next human turn overtook the reserved arrival slot")
	case <-time.After(20 * time.Millisecond):
	}
	bot.releaseThreadReservation("C1", "T1", arrival)
	select {
	case <-nextReady:
	case <-time.After(time.Second):
		t.Fatal("next turn did not unblock after arrival release")
	}
	bot.releaseThreadReservation("C1", "T1", next)
}

func TestSlackHistoryAtTurnStartExcludesLaterUserButKeepsPriorReply(t *testing.T) {
	history := []chathistory.HistoryMessage{
		{MessageID: incompleteMessageID, Text: "diagnostic"},
		{MessageID: "1786092800.000001", Text: "prior"},
		{MessageID: "1786092826.330419", Text: "trigger"},
		{MessageID: "1786092827.000001", Text: "future"},
		{MessageID: "1786092828.000001", UserID: "B1", Text: "reply to prior", IsBot: true},
		{MessageID: "1786092829.000001", UserID: "B2", Text: "future external bot", IsBot: true},
	}
	got := slackHistoryAtTurnStart(history, "1786092826.330419", "B1")
	if len(got) != 4 || got[0].Text != "diagnostic" || got[1].Text != "prior" || got[2].Text != "trigger" || got[3].Text != "reply to prior" {
		t.Fatalf("history = %+v, want diagnostic, prior, trigger, and prior reply only", got)
	}
}

// --- Semaphore tests ---

func TestBotSemaphoreCapacity(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	if cap(bot.sem) != maxConcurrentChats {
		t.Fatalf("semaphore capacity = %d, want %d", cap(bot.sem), maxConcurrentChats)
	}

	// Fill the semaphore.
	for i := 0; i < maxConcurrentChats; i++ {
		bot.sem <- struct{}{}
	}

	// Next send should block (non-blocking test via select).
	select {
	case bot.sem <- struct{}{}:
		t.Fatal("semaphore should be full")
	default:
		// expected
	}
}

// --- User cache tests ---

func TestBotResolveUserName(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	// Mock returns display_name "Alice" for U123.
	name := bot.resolveUserName("U123")
	if name != "Alice" {
		t.Fatalf("got %q, want %q", name, "Alice")
	}

	// Should be cached now.
	bot.userCacheMu.RLock()
	cached, ok := bot.userCache["U123"]
	bot.userCacheMu.RUnlock()
	if !ok || cached != "Alice" {
		t.Fatal("expected name to be cached")
	}
}

func TestBotResolveUserNameFallbackToRealName(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	// Mock returns empty display_name for U456, falls back to real_name "Bob Real".
	name := bot.resolveUserName("U456")
	if name != "Bob Real" {
		t.Fatalf("got %q, want %q", name, "Bob Real")
	}
}

func TestBotResolveUserNameFallbackToRawID(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	// Mock returns error for unknown users, falls back to raw ID.
	name := bot.resolveUserName("UUNKNOWN")
	if name != "UUNKNOWN" {
		t.Fatalf("got %q, want %q", name, "UUNKNOWN")
	}
}

// --- shouldAutoReply tests ---

func TestBotShouldAutoReply(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{Enabled: true, ThreadReplies: true})
	defer bot.cancel()

	ch, ts := "C1", "1234.5678"

	// No history → should not auto-reply.
	if bot.shouldAutoReply(ch, ts, "hello") {
		t.Fatal("should not auto-reply without history")
	}

	// Create history where bot sent the last message.
	path := chathistory.HistoryFilePath(bot.agentDataDir, platformSlack, ch, ts)
	msgs := []chathistory.HistoryMessage{
		{Platform: platformSlack, ChannelID: ch, ThreadID: ts, MessageID: "1", UserID: "U123", Text: "hi bot", IsBot: false},
		{Platform: platformSlack, ChannelID: ch, ThreadID: ts, MessageID: "2", UserID: "UBOTTEST", Text: "hello!", IsBot: true},
	}
	if err := chathistory.AppendMessages(path, msgs); err != nil {
		t.Fatal(err)
	}

	// Bot sent last message, no other mentions → auto-reply.
	if !bot.shouldAutoReply(ch, ts, "thanks") {
		t.Fatal("should auto-reply when last message was from bot")
	}

	// Message mentions another user → should not auto-reply.
	if bot.shouldAutoReply(ch, ts, "hey <@UOTHER>") {
		t.Fatal("should not auto-reply when mentioning another user")
	}

	// Mentioning the bot itself is OK → should auto-reply.
	if !bot.shouldAutoReply(ch, ts, "hey <@UBOTTEST> thanks") {
		t.Fatal("should auto-reply when only mentioning the bot itself")
	}
}

func TestBotShouldAutoReplyEmptyDataDir(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	bot.agentDataDir = ""
	defer bot.cancel()

	if bot.shouldAutoReply("C1", "ts1", "hello") {
		t.Fatal("should not auto-reply with empty agentDataDir")
	}
}

func TestIsStopCommandExactMatchOnly(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"!stop", true},
		{" !STOP \n", true},
		{"!cancel", true},
		{"!stop please", false},
		{"please !stop", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isStopCommand(tt.text); got != tt.want {
			t.Errorf("isStopCommand(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestActiveTurnCancelIsThreadScoped(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel1()
	defer cancel2()
	turn1 := bot.registerActiveTurn("C1", "thread.1", cancel1)
	turn2 := bot.registerActiveTurn("C1", "thread.2", cancel2)
	defer bot.unregisterActiveTurn("C1", "thread.1", turn1)
	defer bot.unregisterActiveTurn("C1", "thread.2", turn2)

	if !bot.cancelActiveTurn("C1", "thread.1") {
		t.Fatal("cancelActiveTurn returned false for a registered turn")
	}
	select {
	case <-ctx1.Done():
	case <-time.After(time.Second):
		t.Fatal("thread.1 context was not cancelled")
	}
	if !turn1.stopRequested() {
		t.Fatal("thread.1 should be marked as user-stopped")
	}
	select {
	case <-ctx2.Done():
		t.Fatal("thread.2 must not be cancelled when stopping thread.1")
	default:
	}
	if turn2.stopRequested() {
		t.Fatal("thread.2 must not be marked stopped")
	}
}

func TestActiveTurnCancelTargetsFIFOHead(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel1()
	defer cancel2()
	turn1 := bot.registerActiveTurn("C1", "thread.1", cancel1)
	turn2 := bot.registerActiveTurn("C1", "thread.1", cancel2)

	if !bot.cancelActiveTurn("C1", "thread.1") {
		t.Fatal("first cancel returned false")
	}
	select {
	case <-ctx1.Done():
	case <-time.After(time.Second):
		t.Fatal("FIFO head was not cancelled")
	}
	select {
	case <-ctx2.Done():
		t.Fatal("queued turn was cancelled before the FIFO head unregistered")
	default:
	}

	bot.unregisterActiveTurn("C1", "thread.1", turn1)
	if !bot.cancelActiveTurn("C1", "thread.1") {
		t.Fatal("second cancel returned false")
	}
	select {
	case <-ctx2.Done():
	case <-time.After(time.Second):
		t.Fatal("next FIFO turn was not cancelled")
	}
	bot.unregisterActiveTurn("C1", "thread.1", turn2)
}

type steerCall struct {
	agentID, sessionKey, content string
}

type steerTestMgr struct {
	calls       chan steerCall
	turns       chan string
	release     <-chan struct{}
	err         error
	waitContext bool
	oneShots    atomic.Int32
}

func (m *steerTestMgr) ChatOneShot(_ context.Context, _, message string, _ agent.OneShotOpts) (<-chan agent.ChatEvent, error) {
	m.oneShots.Add(1)
	if m.turns != nil {
		m.turns <- message
	}
	ch := make(chan agent.ChatEvent, 1)
	ch <- agent.ChatEvent{Type: "done"}
	close(ch)
	return ch, nil
}

func (m *steerTestMgr) SteerOneShot(ctx context.Context, agentID, sessionKey, content string) error {
	select {
	case m.calls <- steerCall{agentID: agentID, sessionKey: sessionKey, content: content}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if m.release != nil {
		select {
		case <-m.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if m.waitContext {
		<-ctx.Done()
		return ctx.Err()
	}
	return m.err
}

func waitSteerQueue(t *testing.T, turn *activeTurn) {
	t.Helper()
	barrier := turn.reserveSteer(false)
	done := make(chan struct{})
	go func() {
		barrier.Wait()
		close(done)
	}()
	select {
	case <-done:
		barrier.Release()
	case <-time.After(time.Second):
		t.Fatal("steer queue did not drain")
	}
}

func TestProcessIncomingSteersActiveSlackThread(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	mgr := &steerTestMgr{calls: make(chan steerCall, 1)}
	bot.mgr = mgr
	bot.userCache["U123"] = "Alice"

	_, cancel := context.WithCancel(context.Background())
	turn := bot.registerActiveTurn("C1", "thread.1", cancel)
	defer cancel()
	defer bot.unregisterActiveTurn("C1", "thread.1", turn)

	bot.processIncoming(context.Background(), "C1", "thread.1", "100.000002", "change direction", "U123")
	select {
	case call := <-mgr.calls:
		if call.agentID != "test-agent" || call.sessionKey != slackSessionKey("test-agent", "C1", "thread.1") {
			t.Fatalf("steer route = %+v", call)
		}
		if call.content != "[Slack @Alice | channel:C1 thread:thread.1] change direction" {
			t.Fatalf("steer content = %q", call.content)
		}
	case <-time.After(time.Second):
		t.Fatal("active Slack message was not steered")
	}
	waitSteerQueue(t, turn)
	if got := mgr.oneShots.Load(); got != 0 {
		t.Fatalf("successful steer started %d follow-up turns", got)
	}
	if len(turn.steerHistory) != 1 || turn.steerHistory[0].MessageID != "100.000002" {
		t.Fatalf("steer history = %+v", turn.steerHistory)
	}
}

func TestHandleThreadMessageSteersBeforeBotReplyReachesHistory(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	mgr := &steerTestMgr{calls: make(chan steerCall, 1)}
	bot.mgr = mgr
	bot.userCache["U123"] = "Alice"

	_, cancel := context.WithCancel(context.Background())
	turn := bot.registerActiveTurn("C1", "thread.1", cancel)
	defer cancel()
	defer bot.unregisterActiveTurn("C1", "thread.1", turn)

	// The local history is deliberately empty, so shouldAutoReply alone would
	// reject this message until the first bot response had finished persisting.
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User: "U123", Channel: "C1", ChannelType: "channel",
		ThreadTimeStamp: "thread.1", TimeStamp: "100.000002", Text: "interrupt now",
	})
	select {
	case <-mgr.calls:
	case <-time.After(time.Second):
		t.Fatal("live thread reply was ignored before bot history persistence")
	}
	waitSteerQueue(t, turn)
}

func TestSlackSteersAreSerializedInMessageOrder(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	release := make(chan struct{})
	mgr := &steerTestMgr{calls: make(chan steerCall, 2), release: release}
	bot.mgr = mgr
	bot.userCache["U123"] = "Alice"

	_, cancel := context.WithCancel(context.Background())
	turn := bot.registerActiveTurn("C1", "thread.1", cancel)
	defer cancel()
	defer bot.unregisterActiveTurn("C1", "thread.1", turn)

	bot.processIncoming(context.Background(), "C1", "thread.1", "100.000002", "first", "U123")
	first := <-mgr.calls
	bot.processIncoming(context.Background(), "C1", "thread.1", "100.000003", "second", "U123")
	select {
	case call := <-mgr.calls:
		t.Fatalf("second steer overtook blocked first steer: %+v", call)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case second := <-mgr.calls:
		if !strings.HasSuffix(first.content, "] first") || !strings.HasSuffix(second.content, "] second") {
			t.Fatalf("steer order = %q, %q", first.content, second.content)
		}
	case <-time.After(time.Second):
		t.Fatal("second steer did not run after first completed")
	}
	waitSteerQueue(t, turn)
}

func TestUnsupportedSlackSteerFallsBackToFIFO(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	mgr := &steerTestMgr{calls: make(chan steerCall, 1), err: agent.ErrSteerUnsupported}
	bot.mgr = mgr
	bot.userCache["U123"] = "Alice"

	hold := bot.reserveThread("C1", "thread.1")
	hold.Wait()
	_, cancel := context.WithCancel(context.Background())
	turn := bot.registerActiveTurn("C1", "thread.1", cancel)
	bot.processIncoming(context.Background(), "C1", "thread.1", "100.000002", "follow up", "U123")
	select {
	case <-mgr.calls:
	case <-time.After(time.Second):
		t.Fatal("steer was not attempted")
	}

	deadline := time.Now().Add(time.Second)
	for {
		bot.activeTurnsMu.Lock()
		registered := len(bot.activeTurns[activeTurnKey("C1", "thread.1")])
		bot.activeTurnsMu.Unlock()
		if registered == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unsupported steer did not reserve a FIFO follow-up")
		}
		time.Sleep(time.Millisecond)
	}
	bot.finishActiveTurn("C1", "thread.1", turn)
	cancel()
	bot.releaseThreadReservation("C1", "thread.1", hold)

	deadline = time.Now().Add(time.Second)
	for mgr.oneShots.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("FIFO fallback did not start")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSlackFallbackReservesFIFOBeforeLaterAttachment(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	release := make(chan struct{})
	mgr := &steerTestMgr{
		calls: make(chan steerCall, 1), turns: make(chan string, 2),
		release: release, err: agent.ErrSteerUnsupported,
	}
	bot.mgr = mgr
	bot.userCache["U123"] = "Alice"

	hold := bot.reserveThread("C1", "thread.1")
	hold.Wait()
	_, cancel := context.WithCancel(context.Background())
	source := bot.registerActiveTurn("C1", "thread.1", cancel)
	bot.processIncoming(context.Background(), "C1", "thread.1", "100.000002", "first", "U123")
	select {
	case <-mgr.calls:
	case <-time.After(time.Second):
		t.Fatal("first steer was not attempted")
	}
	bot.processIncomingWithAttachments(context.Background(), "C1", "thread.1", "100.000003", "with file", "U123",
		[]agent.MessageAttachment{{Name: "note.txt"}})

	// The attachment shares the active-turn admission queue; it must not
	// reserve a normal FIFO slot while the earlier steer outcome is pending.
	bot.activeTurnsMu.Lock()
	beforeRelease := len(bot.activeTurns[activeTurnKey("C1", "thread.1")])
	bot.activeTurnsMu.Unlock()
	if beforeRelease != 1 {
		t.Fatalf("later attachment overtook pending steer: %d active turns", beforeRelease)
	}
	close(release)

	deadline := time.Now().Add(time.Second)
	for {
		bot.activeTurnsMu.Lock()
		registered := len(bot.activeTurns[activeTurnKey("C1", "thread.1")])
		bot.activeTurnsMu.Unlock()
		if registered == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ordered fallback turns were not registered")
		}
		time.Sleep(time.Millisecond)
	}
	bot.finishActiveTurn("C1", "thread.1", source)
	cancel()
	bot.releaseThreadReservation("C1", "thread.1", hold)

	var got []string
	for range 2 {
		select {
		case message := <-mgr.turns:
			got = append(got, message)
		case <-time.After(time.Second):
			t.Fatal("ordered fallback turn did not start")
		}
	}
	if !strings.HasSuffix(got[0], "] first") || !strings.HasSuffix(got[1], "] with file") {
		t.Fatalf("FIFO fallback order = %q", got)
	}
}

func TestActiveThreadAttachmentIsReservedBeforeSlowDownload(t *testing.T) {
	withTempUploadDir(t)
	downloadStarted := make(chan struct{})
	releaseDownload := make(chan struct{})
	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(downloadStarted)
		<-releaseDownload
		_, _ = w.Write([]byte("attachment"))
	}))
	t.Cleanup(fileServer.Close)

	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	bot.botToken = "xoxb-test"
	mgr := &steerTestMgr{calls: make(chan steerCall, 1)}
	bot.mgr = mgr
	bot.userCache["U123"] = "Alice"

	hold := bot.reserveThread("C1", "thread.1")
	hold.Wait()
	_, cancel := context.WithCancel(context.Background())
	source := bot.registerActiveTurn("C1", "thread.1", cancel)
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User: "U123", Channel: "C1", ChannelType: "channel", SubType: "file_share",
		ThreadTimeStamp: "thread.1", TimeStamp: "100.000002", Text: "read this",
		Message: &slack.Msg{Files: []slack.File{{
			ID: "F1", Name: "note.txt", Size: 10, Mimetype: "text/plain",
			URLPrivateDownload: fileServer.URL,
		}}},
	})
	select {
	case <-downloadStarted:
	case <-time.After(time.Second):
		t.Fatal("attachment download did not start")
	}

	finished := make(chan struct{})
	go func() {
		bot.finishActiveTurn("C1", "thread.1", source)
		close(finished)
	}()
	select {
	case <-finished:
		t.Fatal("terminal overtook an attachment received during the active turn")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseDownload)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("attachment admission did not release after download")
	}
	cancel()
	bot.releaseThreadReservation("C1", "thread.1", hold)
}

func TestStopCancelsActiveThreadAttachmentDownload(t *testing.T) {
	withTempUploadDir(t)
	downloadStarted := make(chan struct{})
	downloadCancelled := make(chan struct{})
	releaseDownload := make(chan struct{})
	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(downloadStarted)
		select {
		case <-r.Context().Done():
			close(downloadCancelled)
		case <-releaseDownload:
			_, _ = w.Write([]byte("attachment"))
		}
	}))
	t.Cleanup(func() {
		close(releaseDownload)
		fileServer.Close()
	})

	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	bot.botToken = "xoxb-test"
	bot.userCache["U123"] = "Alice"

	hold := bot.reserveThread("C1", "thread.1")
	hold.Wait()
	_, cancel := context.WithCancel(context.Background())
	source := bot.registerActiveTurn("C1", "thread.1", cancel)
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User: "U123", Channel: "C1", ChannelType: "channel", SubType: "file_share",
		ThreadTimeStamp: "thread.1", TimeStamp: "100.000002", Text: "read this",
		Message: &slack.Msg{Files: []slack.File{{
			ID: "F1", Name: "note.txt", Size: 10, Mimetype: "text/plain",
			URLPrivateDownload: fileServer.URL,
		}}},
	})
	select {
	case <-downloadStarted:
	case <-time.After(time.Second):
		t.Fatal("attachment download did not start")
	}

	source.requestStop()
	finished := make(chan struct{})
	go func() {
		bot.finishActiveTurn("C1", "thread.1", source)
		close(finished)
	}()
	select {
	case <-downloadCancelled:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel attachment HTTP request")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("cancelled attachment download kept terminal sealing blocked")
	}
	bot.releaseThreadReservation("C1", "thread.1", hold)
}

func TestSlackSteerTimeoutReleasesTerminalAndFallsBack(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	bot.steerTimeout = 10 * time.Millisecond
	mgr := &steerTestMgr{calls: make(chan steerCall, 1), waitContext: true}
	bot.mgr = mgr
	bot.userCache["U123"] = "Alice"

	hold := bot.reserveThread("C1", "thread.1")
	hold.Wait()
	_, cancel := context.WithCancel(context.Background())
	source := bot.registerActiveTurn("C1", "thread.1", cancel)
	bot.processIncoming(context.Background(), "C1", "thread.1", "100.000002", "do not hang", "U123")
	select {
	case <-mgr.calls:
	case <-time.After(time.Second):
		t.Fatal("steer was not attempted")
	}

	finished := make(chan struct{})
	go func() {
		bot.finishActiveTurn("C1", "thread.1", source)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("timed-out steer pinned terminal sealing")
	}
	cancel()
	bot.releaseThreadReservation("C1", "thread.1", hold)
}

func TestSlackSteerAdmissionIsBounded(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	_, cancel := context.WithCancel(context.Background())
	turn := bot.registerActiveTurn("C1", "thread.1", cancel)
	defer cancel()
	defer bot.unregisterActiveTurn("C1", "thread.1", turn)

	reservations := make([]*activeSteerReservation, 0, maxPendingSteerAdmissions)
	for range maxPendingSteerAdmissions {
		gotTurn, reservation, full := bot.reserveActiveAdmission("C1", "thread.1")
		if gotTurn != turn || reservation == nil || full {
			t.Fatalf("admission unexpectedly rejected at %d", len(reservations))
		}
		reservations = append(reservations, reservation)
	}
	if gotTurn, reservation, full := bot.reserveActiveAdmission("C1", "thread.1"); gotTurn != turn || reservation != nil || !full {
		t.Fatalf("overflow admission = turn:%v reservation:%v full:%v", gotTurn, reservation, full)
	}
	for _, reservation := range reservations {
		reservation.Release()
	}
}

func TestProcessIncomingHandlesFullSecondAdmissionCheck(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	bot.userCache["U123"] = "Alice"
	_, cancel := context.WithCancel(context.Background())
	turn := bot.registerActiveTurn("C1", "thread.1", cancel)
	defer cancel()
	defer bot.unregisterActiveTurn("C1", "thread.1", turn)

	reservations := make([]*activeSteerReservation, 0, maxPendingSteerAdmissions)
	for range maxPendingSteerAdmissions {
		_, reservation, full := bot.reserveActiveAdmission("C1", "thread.1")
		if reservation == nil || full {
			t.Fatal("failed to fill active admission queue")
		}
		reservations = append(reservations, reservation)
	}
	defer func() {
		for _, reservation := range reservations {
			reservation.Release()
		}
	}()

	// This path performs a second admission check after formatting. It must
	// handle (active, nil, full) without calling admission.Wait on nil.
	bot.processIncoming(context.Background(), "C1", "thread.1", "100.000002", "overflow", "U123")
}

func TestStopCancelsUserLookupHoldingActiveAdmission(t *testing.T) {
	lookupStarted := make(chan struct{})
	releaseLookup := make(chan struct{})
	var lookupOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/users.info" {
			lookupOnce.Do(func() { close(lookupStarted) })
			select {
			case <-r.Context().Done():
			case <-releaseLookup:
			}
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
	}))
	t.Cleanup(func() {
		close(releaseLookup)
		server.Close()
	})

	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	bot.api = slack.New("xoxb-test", slack.OptionAPIURL(server.URL+"/"))
	turnCtx, cancel := context.WithCancel(context.Background())
	turn := bot.registerActiveTurn("C1", "thread.1", cancel)
	_, admission, full := bot.reserveActiveAdmission("C1", "thread.1")
	if admission == nil || full {
		t.Fatal("failed to reserve active admission")
	}

	processed := make(chan struct{})
	go func() {
		bot.processIncomingWithReservedAdmission(context.Background(), "C1", "thread.1", "100.000002",
			"continue", "U123", nil, true, turn, admission)
		close(processed)
	}()
	select {
	case <-lookupStarted:
	case <-time.After(time.Second):
		t.Fatal("users.info lookup did not start")
	}

	turn.requestStop()
	finished := make(chan struct{})
	go func() {
		bot.finishActiveTurn("C1", "thread.1", turn)
		close(finished)
	}()
	select {
	case <-processed:
	case <-time.After(time.Second):
		t.Fatal("message processing remained blocked in user lookup")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("user lookup kept terminal sealing blocked")
	}
	if turnCtx.Err() == nil {
		t.Fatal("stop did not cancel active turn context")
	}
}

func TestUncertainSlackSteerIsNotRetriedAsTurn(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	mgr := &steerTestMgr{calls: make(chan steerCall, 1), err: agent.ErrSteerDeliveryUncertain}
	bot.mgr = mgr
	bot.userCache["U123"] = "Alice"

	_, cancel := context.WithCancel(context.Background())
	turn := bot.registerActiveTurn("C1", "thread.1", cancel)
	defer cancel()
	defer bot.unregisterActiveTurn("C1", "thread.1", turn)
	bot.processIncoming(context.Background(), "C1", "thread.1", "100.000002", "maybe once", "U123")
	select {
	case <-mgr.calls:
	case <-time.After(time.Second):
		t.Fatal("steer was not attempted")
	}
	waitSteerQueue(t, turn)
	if got := mgr.oneShots.Load(); got != 0 {
		t.Fatalf("uncertain steer was retried as %d ordinary turns", got)
	}
	if len(turn.steerHistory) != 1 {
		t.Fatalf("uncertain steer must remain in canonical history: %+v", turn.steerHistory)
	}
}

func TestStoppedSourceDiscardsActivatedHandoffArrival(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	mgr := &countingMgr{}
	bot.mgr = mgr

	sourceReservation, arrivalReservation := bot.reserveThreadPair("C1", "thread.1")
	_, sourceCancel := context.WithCancel(context.Background())
	source := bot.registerActiveTurnForUser("C1", "thread.1", "U123", sourceCancel)
	arrival := &slackHandoffReservation{
		bot: bot, channel: "C1", threadTS: "thread.1",
		reservation: arrivalReservation, source: source,
	}
	if err := arrival.Activate(context.Background(), "continue after handoff", "peer-2"); err != nil {
		t.Fatal(err)
	}

	source.requestStop()
	bot.unregisterActiveTurn("C1", "thread.1", source)
	// Recreate the gap after the source stop transaction has completed but
	// before the discarded arrival wakes for its reserved FIFO slot.
	arrivalActive, started, denied := bot.cancelActiveTurnForCommand("C1", "thread.1", "U123")
	if arrivalActive == nil || !started || denied {
		t.Fatal("handoff arrival was not stoppable while awaiting source release")
	}
	arrivalActive.completeStopAck()
	bot.releaseThreadReservation("C1", "thread.1", sourceReservation)

	deadline := time.Now().Add(time.Second)
	for {
		bot.activeTurnsMu.Lock()
		remaining := len(bot.activeTurns[activeTurnKey("C1", "thread.1")])
		stopping := bot.stoppingTurns[activeTurnKey("C1", "thread.1")] != nil
		bot.activeTurnsMu.Unlock()
		// Unregistration precedes stop-notice delivery and transaction cleanup.
		// Wait for the complete transaction before admitting a successor stop.
		if remaining == 0 && !stopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("handoff arrival remained registered after source stop")
		}
		time.Sleep(time.Millisecond)
	}
	if got := mgr.oneShots.Load(); got != 0 {
		t.Fatalf("handoff continuation started %d one-shot turns, want 0", got)
	}
	_, nextCancel := context.WithCancel(context.Background())
	defer nextCancel()
	next := bot.registerActiveTurnForUser("C1", "thread.1", "U123", nextCancel)
	defer bot.unregisterActiveTurn("C1", "thread.1", next)
	got, started, denied := bot.cancelActiveTurnForCommand("C1", "thread.1", "U123")
	if got != next || !started || denied {
		t.Fatal("discarded handoff left a stale stop transaction for the thread")
	}
}

func TestHandoffArrivalIncludesAcceptedSlackSteers(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	mgr := &steerTestMgr{calls: make(chan steerCall, 1)}
	bot.mgr = mgr
	bot.userCache["U123"] = "Alice"

	sourceReservation, arrivalReservation := bot.reserveThreadPair("C1", "thread.1")
	_, sourceCancel := context.WithCancel(context.Background())
	source := bot.registerActiveTurn("C1", "thread.1", sourceCancel)
	bot.processIncoming(context.Background(), "C1", "thread.1", "100.000002", "carry this", "U123")
	select {
	case <-mgr.calls:
	case <-time.After(time.Second):
		t.Fatal("steer was not attempted")
	}
	waitSteerQueue(t, source)

	arrival := &slackHandoffReservation{
		bot: bot, channel: "C1", threadTS: "thread.1",
		reservation: arrivalReservation, source: source,
		history: []chathistory.HistoryMessage{{MessageID: "100.000001", Text: "source"}},
	}
	if err := arrival.Activate(context.Background(), "continue after handoff", "peer-2"); err != nil {
		t.Fatal(err)
	}
	if len(arrival.history) != 2 || arrival.history[1].MessageID != "100.000002" || arrival.history[1].Text != "carry this" {
		t.Fatalf("arrival history = %+v", arrival.history)
	}

	// Stop the source so the test does not actually start its synthetic
	// continuation; the snapshot assertion above is the behavior under test.
	source.requestStop()
	bot.finishActiveTurn("C1", "thread.1", source)
	sourceCancel()
	bot.releaseThreadReservation("C1", "thread.1", sourceReservation)
}

func TestHandoffArrivalActiveOrderMatchesReservedFIFO(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	sourceReservation, arrivalReservation := bot.reserveThreadPair("C1", "thread.1")
	ordinaryReservation := bot.reserveThread("C1", "thread.1")
	_, sourceCancel := context.WithCancel(context.Background())
	_, ordinaryCancel := context.WithCancel(context.Background())
	source := bot.registerActiveTurn("C1", "thread.1", sourceCancel)
	ordinary := bot.registerActiveTurn("C1", "thread.1", ordinaryCancel)
	arrival := &slackHandoffReservation{
		bot: bot, channel: "C1", threadTS: "thread.1",
		reservation: arrivalReservation, source: source,
	}
	if err := arrival.Activate(context.Background(), "continue after handoff", "peer-2"); err != nil {
		t.Fatal(err)
	}

	bot.activeTurnsMu.Lock()
	turns := append([]*activeTurn(nil), bot.activeTurns[activeTurnKey("C1", "thread.1")]...)
	bot.activeTurnsMu.Unlock()
	if len(turns) != 3 || turns[0] != source || turns[2] != ordinary {
		t.Fatalf("active order does not match source→arrival→ordinary reservation: %#v", turns)
	}

	// Once the source completes, !stop must target the reserved arrival rather
	// than the later ordinary Slack message.
	bot.unregisterActiveTurn("C1", "thread.1", source)
	if !bot.cancelActiveTurn("C1", "thread.1") {
		t.Fatal("arrival was not cancellable")
	}
	if ordinary.stopRequested() {
		t.Fatal("ordinary turn overtook reserved handoff arrival in active registry")
	}
	bot.releaseThreadReservation("C1", "thread.1", sourceReservation)

	deadline := time.Now().Add(time.Second)
	for {
		bot.activeTurnsMu.Lock()
		remaining := bot.activeTurns[activeTurnKey("C1", "thread.1")]
		arrivalGone := len(remaining) == 1 && remaining[0] == ordinary
		bot.activeTurnsMu.Unlock()
		if arrivalGone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelled arrival did not release its FIFO slot")
		}
		time.Sleep(time.Millisecond)
	}
	ordinaryReservation.Wait()
	bot.unregisterActiveTurn("C1", "thread.1", ordinary)
	ordinaryCancel()
	bot.releaseThreadReservation("C1", "thread.1", ordinaryReservation)
}

func TestCancelledHandoffArrivalReleasesWhileSemaphoreFull(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()
	mgr := &countingMgr{}
	bot.mgr = mgr
	for range cap(bot.sem) {
		bot.sem <- struct{}{}
	}
	defer func() {
		for range cap(bot.sem) {
			<-bot.sem
		}
	}()

	sourceReservation, arrivalReservation := bot.reserveThreadPair("C1", "thread.1")
	ordinaryReservation := bot.reserveThread("C1", "thread.1")
	_, sourceCancel := context.WithCancel(context.Background())
	source := bot.registerActiveTurn("C1", "thread.1", sourceCancel)
	arrival := &slackHandoffReservation{
		bot: bot, channel: "C1", threadTS: "thread.1",
		reservation: arrivalReservation, source: source,
	}
	if err := arrival.Activate(context.Background(), "continue after handoff", "peer-2"); err != nil {
		t.Fatal(err)
	}
	bot.unregisterActiveTurn("C1", "thread.1", source)
	bot.releaseThreadReservation("C1", "thread.1", sourceReservation)
	if !bot.cancelActiveTurn("C1", "thread.1") {
		t.Fatal("waiting arrival was not cancellable")
	}

	unblocked := make(chan struct{})
	go func() {
		ordinaryReservation.Wait()
		close(unblocked)
	}()
	select {
	case <-unblocked:
	case <-time.After(time.Second):
		t.Fatal("cancelled arrival did not release its reservation while semaphore was full")
	}
	if got := mgr.oneShots.Load(); got != 0 {
		t.Fatalf("cancelled arrival started %d one-shot turns, want 0", got)
	}
	bot.activeTurnsMu.Lock()
	remaining := len(bot.activeTurns[activeTurnKey("C1", "thread.1")])
	bot.activeTurnsMu.Unlock()
	if remaining != 0 {
		t.Fatalf("cancelled arrival left %d active registrations", remaining)
	}
	bot.releaseThreadReservation("C1", "thread.1", ordinaryReservation)
}

func TestHandleMessageEventStopCommandBypassesAutoReply(t *testing.T) {
	var postCalls atomic.Int32
	var gotThread, gotMarkdown string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/chat.postMessage" {
			postCalls.Add(1)
			_ = r.ParseForm()
			gotThread = r.FormValue("thread_ts")
			gotMarkdown = r.FormValue("markdown_text")
		}
		fmt.Fprint(w, `{"ok":true,"channel":"C1","ts":"post.1"}`)
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bot := &Bot{
		agentID: "test-agent", agentDataDir: t.TempDir(),
		config: agent.SlackBotConfig{Enabled: true, ThreadReplies: false},
		api:    api, mgr: &mockMgr{}, logger: testLogger, botUserID: "UBOTTEST",
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
		threadLocks: make(map[string]*threadLock), activeTurns: make(map[string][]*activeTurn),
		userCache: make(map[string]string), sem: make(chan struct{}, maxConcurrentChats),
	}
	turnCtx, turnCancel := context.WithCancel(context.Background())
	turn := bot.registerActiveTurnForUser("C1", "thread.123", "U123", turnCancel)
	defer bot.unregisterActiveTurn("C1", "thread.123", turn)
	defer turnCancel()

	// No history and thread auto-replies are disabled. A normal thread post
	// would be ignored, but the command must still reach the active registry.
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User: "U123", Channel: "C1", ChannelType: "channel",
		ThreadTimeStamp: "thread.123", TimeStamp: "msg.456", Text: "!stop",
	})
	select {
	case <-turnCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("active turn was not cancelled")
	}
	if postCalls.Load() != 1 || gotThread != "thread.123" || gotMarkdown != stopCommandAck {
		t.Fatalf("stop ack calls=%d thread=%q markdown=%q", postCalls.Load(), gotThread, gotMarkdown)
	}
}

// Ownership checks must share the stop registry lock and must not bypass the
// acknowledgement barrier or accidentally target a successor during finalization.
func TestStopCommandOwnershipAcrossFinalization(t *testing.T) {
	for _, tc := range []struct {
		name, owner, requester string
	}{
		{"other user", "UOWNER", "UOTHER"},
		{"missing requester", "UOWNER", ""},
		{"internal turn", "", "UOTHER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bot := &Bot{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			turn := bot.registerActiveTurnForUser("C1", "T1", tc.owner, cancel)
			got, started, denied := bot.cancelActiveTurnForCommand("C1", "T1", tc.requester)
			if got != turn || started || !denied || ctx.Err() != nil || turn.stopRequested() {
				t.Fatalf("unauthorized stop: got=%p started=%v denied=%v cancelled=%v", got, started, denied, ctx.Err())
			}
			if len(bot.stoppingTurns) != 0 {
				t.Fatal("unauthorized command created a stop transaction")
			}
		})
	}
	bot := &Bot{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	turn := bot.registerActiveTurnForUser("C1", "T1", "UOWNER", cancel)
	got, started, denied := bot.cancelActiveTurnForCommand("C1", "T1", "UOWNER")
	if got != turn || !started || denied || ctx.Err() == nil {
		t.Fatal("owner could not stop own turn")
	}
	defer turn.completeStopAck()
	bot.unregisterActiveTurn("C1", "T1", turn)
	nextCtx, nextCancel := context.WithCancel(context.Background())
	defer nextCancel()
	next := bot.registerActiveTurnForUser("C1", "T1", "UNEXT", nextCancel)
	for _, user := range []string{"UOTHER", "UNEXT", "", "UOWNER"} {
		got, started, denied = bot.cancelActiveTurnForCommand("C1", "T1", user)
		if got != turn || started || denied != (user != "UOWNER") || nextCtx.Err() != nil {
			t.Fatalf("finalizing stop for %q: got=%p started=%v denied=%v nextCancelled=%v", user, got, started, denied, nextCtx.Err())
		}
	}
	bot.finishStopTransaction("C1", "T1", turn)
	got, started, denied = bot.cancelActiveTurnForCommand("C1", "T1", "UNEXT")
	if got != next || !started || denied || nextCtx.Err() == nil {
		t.Fatal("successor was not stoppable after finalization")
	}
	next.completeStopAck()
}

func TestStopCompletionWaitsForCommandAck(t *testing.T) {
	seen := make(chan string, 2)
	ackStarted := make(chan struct{})
	releaseAck := make(chan struct{})
	doneStarted := make(chan struct{})
	releaseDone := make(chan struct{})
	var releaseAckOnce sync.Once
	var releaseDoneOnce sync.Once
	release := func() { releaseAckOnce.Do(func() { close(releaseAck) }) }
	finishDone := func() { releaseDoneOnce.Do(func() { close(releaseDone) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = r.ParseForm()
		body := r.FormValue("markdown_text")
		seen <- body
		if body == stopCommandAck {
			close(ackStarted)
			<-releaseAck
		} else if body == stopCommandDone {
			close(doneStarted)
			<-releaseDone
		}
		fmt.Fprint(w, `{"ok":true,"channel":"C1","ts":"post.1"}`)
	}))
	t.Cleanup(func() {
		release()
		finishDone()
		srv.Close()
	})

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bot := &Bot{
		api: api, logger: testLogger, ctx: ctx, cancel: cancel,
		activeTurns: make(map[string][]*activeTurn),
	}
	turnCtx, turnCancel := context.WithCancel(context.Background())
	turn := bot.registerActiveTurnForUser("C1", "thread.123", "U123", turnCancel)
	defer bot.unregisterActiveTurn("C1", "thread.123", turn)
	nextCtx, nextCancel := context.WithCancel(context.Background())
	defer nextCancel()
	next := bot.registerActiveTurnForUser("C1", "thread.123", "U123", nextCancel)
	defer bot.unregisterActiveTurn("C1", "thread.123", next)

	completionAttempted := make(chan struct{})
	completionDone := make(chan struct{})
	go func() {
		<-turnCtx.Done()
		close(completionAttempted)
		// sendToAgent removes a terminal turn before Slack finalization. Recreate
		// that production gap while a queued next turn is already registered.
		bot.unregisterActiveTurn("C1", "thread.123", turn)
		bot.postStopNotice("C1", "thread.123", turn)
		bot.finishStopTransaction("C1", "thread.123", turn)
		close(completionDone)
	}()
	commandDone := make(chan struct{})
	go func() {
		bot.handleSlackCommand(context.Background(), "C1", "thread.123", "msg.456", "U123", "!stop")
		close(commandDone)
	}()

	select {
	case <-ackStarted:
	case <-time.After(time.Second):
		t.Fatal("stop acknowledgement did not start")
	}
	select {
	case <-completionAttempted:
	case <-time.After(time.Second):
		t.Fatal("cancelled terminal path did not attempt completion")
	}
	select {
	case body := <-seen:
		if body != stopCommandAck {
			t.Fatalf("first stop notice = %q, want acknowledgement", body)
		}
	default:
		t.Fatal("acknowledgement request was not recorded")
	}
	select {
	case body := <-seen:
		t.Fatalf("completion %q was posted before acknowledgement finished", body)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-commandDone:
	case <-time.After(time.Second):
		t.Fatal("stop command did not finish")
	}
	select {
	case <-doneStarted:
	case <-time.After(time.Second):
		t.Fatal("stop completion did not start after acknowledgement")
	}
	select {
	case body := <-seen:
		if body != stopCommandDone {
			t.Fatalf("second stop notice = %q, want completion", body)
		}
	default:
		t.Fatal("completion request was not recorded")
	}
	// The original turn is now absent from activeTurns and the next queued turn
	// is the FIFO head, but the stop transaction remains until Stopped finishes.
	if !bot.handleSlackCommand(context.Background(), "C1", "thread.123", "msg.457", "U123", "!stop") {
		t.Fatal("duplicate stop command was not consumed")
	}
	select {
	case <-nextCtx.Done():
		t.Fatal("duplicate stop cancelled the next queued turn")
	default:
	}
	select {
	case body := <-seen:
		t.Fatalf("duplicate stop emitted an extra notice: %q", body)
	default:
	}

	finishDone()
	select {
	case <-completionDone:
	case <-time.After(time.Second):
		t.Fatal("stop completion did not finish")
	}
}

func TestStopCompletionContinuesAfterAckFailure(t *testing.T) {
	seen := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = r.ParseForm()
		body := r.FormValue("markdown_text")
		seen <- body
		if body == stopCommandAck {
			fmt.Fprint(w, `{"ok":false,"error":"channel_not_found"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"channel":"C1","ts":"post.2"}`)
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bot := &Bot{
		api: api, logger: testLogger, ctx: ctx, cancel: cancel,
		activeTurns: make(map[string][]*activeTurn),
	}
	turnCtx, turnCancel := context.WithCancel(context.Background())
	turn := bot.registerActiveTurnForUser("C1", "thread.123", "U123", turnCancel)
	defer bot.unregisterActiveTurn("C1", "thread.123", turn)

	completionDone := make(chan struct{})
	go func() {
		<-turnCtx.Done()
		bot.postStopNotice("C1", "thread.123", turn)
		close(completionDone)
	}()
	bot.handleSlackCommand(context.Background(), "C1", "thread.123", "msg.456", "U123", "!stop")
	select {
	case <-completionDone:
	case <-time.After(time.Second):
		t.Fatal("failed acknowledgement left completion blocked")
	}
	for i, want := range []string{stopCommandAck, stopCommandDone} {
		select {
		case got := <-seen:
			if got != want {
				t.Fatalf("notice %d = %q, want %q", i, got, want)
			}
		default:
			t.Fatalf("notice %d was not posted", i)
		}
	}
}

func TestHandleMessageEventStopCommandHonorsThreadGate(t *testing.T) {
	var postCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			postCalls.Add(1)
		}
		fmt.Fprint(w, `{"ok":true,"channel":"C1","ts":"post.1"}`)
	}))
	defer srv.Close()

	respondThread := false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bot := &Bot{
		agentID: "test-agent", agentDataDir: t.TempDir(),
		config: agent.SlackBotConfig{Enabled: true, RespondThread: &respondThread},
		api:    slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/")), mgr: &mockMgr{}, logger: testLogger,
		botUserID: "UBOTTEST", ctx: ctx, cancel: cancel, done: make(chan struct{}),
		threadLocks: make(map[string]*threadLock), activeTurns: make(map[string][]*activeTurn),
		userCache: make(map[string]string), sem: make(chan struct{}, maxConcurrentChats),
	}
	turnCtx, turnCancel := context.WithCancel(context.Background())
	turn := bot.registerActiveTurnForUser("C1", "thread.123", "U123", turnCancel)
	defer bot.unregisterActiveTurn("C1", "thread.123", turn)
	defer turnCancel()

	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User: "U123", Channel: "C1", ChannelType: "channel",
		ThreadTimeStamp: "thread.123", TimeStamp: "msg.456", Text: "!stop",
	})
	select {
	case <-turnCtx.Done():
		t.Fatal("disabled thread surface accepted stop command")
	case <-time.After(50 * time.Millisecond):
	}
	if postCalls.Load() != 0 {
		t.Fatalf("disabled thread surface posted %d command responses", postCalls.Load())
	}
}

func TestHandleMessageEventStopCommandRequiresTurnOwner(t *testing.T) {
	var postCalls atomic.Int32
	var gotMarkdown string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			postCalls.Add(1)
			_ = r.ParseForm()
			gotMarkdown = r.FormValue("markdown_text")
		}
		fmt.Fprint(w, `{"ok":true,"channel":"C1","ts":"post.1"}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bot := &Bot{
		agentID: "test-agent", agentDataDir: t.TempDir(), config: agent.SlackBotConfig{Enabled: true},
		api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/")), mgr: &mockMgr{}, logger: testLogger,
		botUserID: "UBOTTEST", ctx: ctx, cancel: cancel, done: make(chan struct{}),
		threadLocks: make(map[string]*threadLock), activeTurns: make(map[string][]*activeTurn),
		userCache: make(map[string]string), sem: make(chan struct{}, maxConcurrentChats),
	}
	turnCtx, turnCancel := context.WithCancel(context.Background())
	turn := bot.registerActiveTurnForUser("C1", "thread.123", "UOWNER", turnCancel)
	defer bot.unregisterActiveTurn("C1", "thread.123", turn)
	defer turnCancel()

	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User: "UOTHER", Channel: "C1", ChannelType: "channel",
		ThreadTimeStamp: "thread.123", TimeStamp: "msg.456", Text: "!stop",
	})
	select {
	case <-turnCtx.Done():
		t.Fatal("unrelated Slack user cancelled the active turn")
	case <-time.After(50 * time.Millisecond):
	}
	if postCalls.Load() != 1 || gotMarkdown != stopCommandNotOwner {
		t.Fatalf("unauthorized stop response calls=%d markdown=%q", postCalls.Load(), gotMarkdown)
	}
}

// --- postMessage / chat.update markdown_text tests ---

// TestBotPostMessageSendsMarkdownTextOnly verifies that postMessage emits
// only the markdown_text form field. Pairing it with text triggers Slack's
// markdown_text_conflict error and the call silently fails — observed in
// production (2026-05-19) and the cause of stream finalize truncation in
// multi-chunk replies. Regression guard: if a future change re-adds
// MsgOptionText to postMessage, this test must fail.
func TestBotPostMessageSendsMarkdownTextOnly(t *testing.T) {
	type captured struct {
		text         string
		markdownText string
		called       int
	}
	var got captured

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat.postMessage":
			_ = r.ParseForm()
			got.text = r.FormValue("text")
			got.markdownText = r.FormValue("markdown_text")
			got.called++
			fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":"123.456"}`)
		default:
			fmt.Fprintf(w, `{"ok":true}`)
		}
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bot := &Bot{
		api:    api,
		logger: testLogger,
		ctx:    ctx,
	}

	if !bot.postMessage(context.Background(), "C1", "", "hello world") {
		t.Fatal("postMessage should succeed against a mock returning ok")
	}
	if got.called != 1 {
		t.Fatalf("chat.postMessage called %d times, want 1", got.called)
	}
	if got.markdownText != "hello world" {
		t.Errorf("markdown_text = %q, want %q", got.markdownText, "hello world")
	}
	if got.text != "" {
		t.Errorf("text must be empty to avoid markdown_text_conflict, got %q", got.text)
	}
}

func TestBotPostMessageFallsBackToLegacyTextOnMarkdownTextErrors(t *testing.T) {
	for _, slackError := range []string{"invalid_blocks", "invalid_blocks_format", "markdown_text_conflict"} {
		t.Run(slackError, func(t *testing.T) {
			type captured struct{ text, markdownText, threadTS string }
			var calls []captured
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = r.ParseForm()
				calls = append(calls, captured{r.FormValue("text"), r.FormValue("markdown_text"), r.FormValue("thread_ts")})
				if len(calls) == 1 {
					fmt.Fprintf(w, `{"ok":false,"error":%q}`, slackError)
					return
				}
				fmt.Fprint(w, `{"ok":true,"channel":"C1","ts":"123.456"}`)
			}))
			defer srv.Close()

			bot := &Bot{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/")), logger: testLogger}
			body := "## Heading\n\n**bold** and [link](https://example.com)"
			if !bot.postMessage(context.Background(), "C1", "thread.999", body) {
				t.Fatal("postMessage should succeed via legacy text fallback")
			}
			if len(calls) != 2 {
				t.Fatalf("chat.postMessage called %d times, want 2", len(calls))
			}
			if calls[0].markdownText != body || calls[0].text != "" {
				t.Errorf("first call = %+v, want markdown_text only", calls[0])
			}
			if calls[1].markdownText != "" || calls[1].text != PlainToSlack(body) {
				t.Errorf("fallback call = %+v, want legacy converted text", calls[1])
			}
			if calls[0].threadTS != "thread.999" || calls[1].threadTS != "thread.999" {
				t.Errorf("thread_ts was not preserved: %+v", calls)
			}
		})
	}
}

func TestBotPostMessageDoesNotFallbackToLegacyTextOnUnrelatedSlackError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":false,"error":"channel_not_found"}`)
	}))
	defer srv.Close()
	bot := &Bot{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/")), logger: testLogger}
	if bot.postMessage(context.Background(), "C1", "thread.999", "hello") {
		t.Fatal("postMessage unexpectedly succeeded")
	}
	if calls.Load() != 1 {
		t.Fatalf("chat.postMessage calls = %d, want 1", calls.Load())
	}
}

// TestFinalizeUpdateOptsSendsMarkdownTextOnly verifies that the stream-finalize
// chat.update call wires markdown_text alone, with no text field. Pairing
// MsgOptionText with MsgOptionMarkdownText was the root cause of the
// "{accumulated stream buffer} + {final body}" double-render bug (see the
// IMPORTANT comment in sendToAgent). Mirrors TestBotPostMessageSendsMarkdownTextOnly
// but covers the chat.update path — the postMessage test alone does not
// guard against re-adding MsgOptionText to the finalize update slice.
func TestFinalizeUpdateOptsSendsMarkdownTextOnly(t *testing.T) {
	type captured struct {
		text         string
		markdownText string
		threadTS     string
		called       int
	}
	var got captured

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat.update":
			_ = r.ParseForm()
			got.text = r.FormValue("text")
			got.markdownText = r.FormValue("markdown_text")
			got.threadTS = r.FormValue("thread_ts")
			got.called++
			fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":"123.456"}`)
		default:
			fmt.Fprintf(w, `{"ok":true}`)
		}
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))

	opts := finalizeUpdateOpts("hello body", "thread.999")
	if _, _, _, err := api.UpdateMessageContext(context.Background(), "C1", "stream.111", opts...); err != nil {
		t.Fatalf("UpdateMessageContext failed: %v", err)
	}

	if got.called != 1 {
		t.Fatalf("chat.update called %d times, want 1", got.called)
	}
	if got.markdownText != "hello body" {
		t.Errorf("markdown_text = %q, want %q", got.markdownText, "hello body")
	}
	if got.text != "" {
		t.Errorf("text must be empty to avoid double-render with the streamed buffer, got %q", got.text)
	}
	if got.threadTS != "thread.999" {
		t.Errorf("thread_ts = %q, want %q", got.threadTS, "thread.999")
	}
}

// TestFinalizeUpdateOptsOmitsThreadTSWhenEmpty guards the conditional MsgOptionTS
// append — passing an empty threadTS would otherwise leak `thread_ts=` to the
// wire and chat.update would reject it.
func TestFinalizeUpdateOptsOmitsThreadTSWhenEmpty(t *testing.T) {
	var sawTS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/chat.update" {
			_ = r.ParseForm()
			sawTS = r.FormValue("thread_ts")
		}
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	opts := finalizeUpdateOpts("body", "")
	if _, _, _, err := api.UpdateMessageContext(context.Background(), "C1", "stream.222", opts...); err != nil {
		t.Fatalf("UpdateMessageContext failed: %v", err)
	}
	if sawTS != "" {
		t.Errorf("thread_ts must be empty when input threadTS is empty, got %q", sawTS)
	}
}

// TestDeliveryFailureNoticeDoesNotAttributeCause guards against regressing
// the notice wording back to a cause-specific phrasing like "too long".
// The notice is posted from any deliveredAll=false path in sendToAgent
// (stream-finalize and batch-fallback), and those failures may come from
// chunkPostTimeout expiry, transient Slack API errors, or context cancel
// — not just oversized replies. The text must stay cause-neutral so users
// aren't misled into thinking they hit a length limit when they didn't.
func TestDeliveryFailureNoticeDoesNotAttributeCause(t *testing.T) {
	forbidden := []string{"too long", "too large", "exceeded", "limit"}
	lower := strings.ToLower(deliveryFailureNotice)
	for _, sub := range forbidden {
		if strings.Contains(lower, sub) {
			t.Errorf("deliveryFailureNotice = %q must not imply specific cause %q", deliveryFailureNotice, sub)
		}
	}
	if !strings.Contains(deliveryFailureNotice, "could not be delivered") {
		t.Errorf("deliveryFailureNotice = %q should describe a generic delivery failure", deliveryFailureNotice)
	}
}

// TestPostMessageRateLimitNoExtraSleepOnFinalAttempt guards against the
// rate-limit retry loop sleeping after the last permitted attempt fails.
// Before the fix, an attempt == maxRateLimitRetry that hit a 429 still
// spent attempt+1 seconds in time.After before exiting the loop, eating
// chunkPostTimeout budget for no subsequent retry. After the fix the loop
// must:
//   - perform exactly maxRateLimitRetry+1 PostMessage calls
//   - sleep at most maxRateLimitRetry times (one per gap between attempts)
//   - return false promptly when the final attempt is also rate-limited
func TestPostMessageRateLimitNoExtraSleepOnFinalAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sleeps atomic.Int32
	bot := &Bot{
		api:    api,
		logger: testLogger,
		ctx:    ctx,
		// Use a synchronous "instant fire" sleeper so the test does not
		// depend on real wall-clock waits while still counting how many
		// times the loop tried to sleep.
		rateLimitSleep: func(d time.Duration) <-chan time.Time {
			sleeps.Add(1)
			ch := make(chan time.Time, 1)
			ch <- time.Now()
			return ch
		},
	}

	if bot.postMessage(context.Background(), "C1", "", "hello") {
		t.Fatal("postMessage should return false when all attempts are rate limited")
	}

	if got := calls.Load(); got != int32(maxRateLimitRetry+1) {
		t.Errorf("PostMessage call count = %d, want %d (maxRateLimitRetry+1)", got, maxRateLimitRetry+1)
	}
	if got := sleeps.Load(); got != int32(maxRateLimitRetry) {
		t.Errorf("rateLimitSleep invocations = %d, want %d (one per inter-attempt gap; no sleep after final attempt)", got, maxRateLimitRetry)
	}
}

// scriptedMgr returns a pre-canned event stream from ChatOneShot so
// sendToAgent's event loop can be driven deterministically in tests.
type scriptedMgr struct {
	events          []agent.ChatEvent
	lastOneShotOpts agent.OneShotOpts
}

type countingMgr struct{ oneShots atomic.Int32 }

func (m *countingMgr) Chat(_ context.Context, _, _, _ string, _ []agent.MessageAttachment, _ ...agent.BusySource) (<-chan agent.ChatEvent, error) {
	ch := make(chan agent.ChatEvent)
	close(ch)
	return ch, nil
}

func (m *countingMgr) ChatOneShot(_ context.Context, _, _ string, _ agent.OneShotOpts) (<-chan agent.ChatEvent, error) {
	m.oneShots.Add(1)
	ch := make(chan agent.ChatEvent)
	close(ch)
	return ch, nil
}

// interruptedRelayMgr models the peer relay contract: text already decoded on
// the Hub is delivered, then cancelling the HTTP request closes the stream
// without an authoritative terminal event.
type interruptedRelayMgr struct{ started chan struct{} }

func (m *interruptedRelayMgr) Chat(_ context.Context, _, _, _ string, _ []agent.MessageAttachment, _ ...agent.BusySource) (<-chan agent.ChatEvent, error) {
	panic("unexpected Chat call")
}

func (m *interruptedRelayMgr) ChatOneShot(ctx context.Context, _, _ string, _ agent.OneShotOpts) (<-chan agent.ChatEvent, error) {
	ch := make(chan agent.ChatEvent, 1)
	ch <- agent.ChatEvent{Type: "text", Delta: "partial from holder"}
	close(m.started)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func (m *scriptedMgr) Chat(_ context.Context, _, _, _ string, _ []agent.MessageAttachment, _ ...agent.BusySource) (<-chan agent.ChatEvent, error) {
	ch := make(chan agent.ChatEvent, len(m.events)+1)
	for _, e := range m.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func (m *scriptedMgr) ChatOneShot(_ context.Context, _, _ string, opts agent.OneShotOpts) (<-chan agent.ChatEvent, error) {
	m.lastOneShotOpts = opts
	ch := make(chan agent.ChatEvent, len(m.events)+1)
	for _, e := range m.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// streamScript drives a mockSlackServer for sendToAgent-level tests of
// the stream-restart behavior. It hands out fresh stream timestamps from
// streamTSs in order, fails the appendStream call at the indexes in
// killAt with message_not_in_streaming_state, and counts every relevant
// API call so the test can assert on the resulting sequence. Counters
// and the issued/stopped lists are mutated from the HTTP handler
// goroutine; the test reads them only after sendToAgent returns.
type streamScript struct {
	streamTSs  []string // returned by chat.startStream, in order
	killAt     []int    // 0-based indexes of appendStream calls that should fail with message_not_in_streaming_state
	failUpdate bool     // chat.update returns an error (simulate finalize replacement failure)
	failPost   bool     // chat.postMessage returns an error (simulate batch/fallback delivery failure)
	// 0-based chat.delete call indexes that return HTTP 429. Tests use this
	// to verify Retry-After recovery and failed eager-cleanup handoff.
	rateLimitDeleteAt []int
	repliesJSON       string // optional conversations.replies response body

	startCalls   int
	appendCalls  int
	stopCalls    int
	updateCalls  int
	postCalls    int
	deleteCalls  int
	issuedTS     []string // ts values returned by chat.startStream
	stoppedTS    []string // ts values seen by chat.stopStream
	deletedTS    []string // ts values seen by chat.delete (dead-stream cleanup)
	startSeen    chan struct{}
	startOnce    sync.Once
	appendOnTS   []string // ts the bot tried to append to (in order)
	lastUpdateTS string
	lastUpdateMD string
	lastPostMD   string
	postedMD     []string
}

// newStreamServer returns a mock Slack server that delegates streaming
// calls to script and otherwise mimics mockSlackServer.
func newStreamServer(t *testing.T, script *streamScript) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/chat.startStream":
			idx := script.startCalls
			script.startCalls++
			if idx >= len(script.streamTSs) {
				fmt.Fprintf(w, `{"ok":false,"error":"too_many_streams"}`)
				return
			}
			ts := script.streamTSs[idx]
			script.issuedTS = append(script.issuedTS, ts)
			if script.startSeen != nil {
				script.startOnce.Do(func() { close(script.startSeen) })
			}
			fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":%q}`, ts)
		case "/chat.appendStream":
			idx := script.appendCalls
			script.appendCalls++
			script.appendOnTS = append(script.appendOnTS, r.FormValue("ts"))
			for _, k := range script.killAt {
				if k == idx {
					fmt.Fprintf(w, `{"ok":false,"error":"message_not_in_streaming_state"}`)
					return
				}
			}
			fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":%q}`, r.FormValue("ts"))
		case "/chat.stopStream":
			script.stopCalls++
			script.stoppedTS = append(script.stoppedTS, r.FormValue("ts"))
			fmt.Fprintf(w, `{"ok":true}`)
		case "/chat.update":
			script.updateCalls++
			script.lastUpdateTS = r.FormValue("ts")
			script.lastUpdateMD = r.FormValue("markdown_text")
			if script.failUpdate {
				fmt.Fprintf(w, `{"ok":false,"error":"message_not_found"}`)
				return
			}
			fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":%q}`, r.FormValue("ts"))
		case "/chat.postMessage":
			script.postCalls++
			script.lastPostMD = r.FormValue("markdown_text")
			script.postedMD = append(script.postedMD, script.lastPostMD)
			if script.failPost {
				fmt.Fprintf(w, `{"ok":false,"error":"channel_not_found"}`)
				return
			}
			fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":"post.999"}`)
		case "/chat.delete":
			idx := script.deleteCalls
			script.deleteCalls++
			script.deletedTS = append(script.deletedTS, r.FormValue("ts"))
			if slices.Contains(script.rateLimitDeleteAt, idx) {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":%q}`, r.FormValue("ts"))
		case "/conversations.replies":
			if script.repliesJSON != "" {
				fmt.Fprint(w, script.repliesJSON)
				return
			}
			fmt.Fprintf(w, `{"ok":true,"messages":[]}`)
		case "/conversations.history":
			fmt.Fprintf(w, `{"ok":true,"messages":[]}`)
		case "/assistant.threads.setStatus":
			fmt.Fprintf(w, `{"ok":true}`)
		case "/auth.test":
			fmt.Fprintf(w, `{"ok":true,"user_id":"UBOTTEST","user":"testbot","team":"T1","team_id":"T1"}`)
		default:
			fmt.Fprintf(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newBotWithStream(t *testing.T, mgr ChatManager, srv *httptest.Server) *Bot {
	t.Helper()
	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	sm := socketmode.New(api)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Bot{
		agentID:      "test-agent",
		agentDataDir: "", // empty → skip chathistory writes
		config:       agent.SlackBotConfig{Enabled: true},
		api:          api,
		sm:           sm,
		mgr:          mgr,
		logger:       testLogger,
		botUserID:    "UBOTTEST",
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
		threadLocks:  make(map[string]*threadLock),
		userCache:    make(map[string]string),
		sem:          make(chan struct{}, maxConcurrentChats),
		// Run the dead-stream cleanup goroutine synchronously so the
		// test can observe its API calls after sendToAgent returns.
		runAsync: func(fn func()) { fn() },
	}
}

func TestSendToAgentSuppressesNoReplyToken(t *testing.T) {
	script := &streamScript{streamTSs: []string{"stream.unused"}}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "text", Delta: "[[NO_"},
		{Type: "text", Delta: "REPLY]]"},
		{Type: "done", Message: &agent.Message{Content: noReplyToken}},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if !strings.Contains(mgr.lastOneShotOpts.SystemPromptExtra, noReplyToken) {
		t.Fatalf("Slack system prompt does not advertise no-reply token: %q", mgr.lastOneShotOpts.SystemPromptExtra)
	}
	if script.startCalls != 0 || script.appendCalls != 0 || script.stopCalls != 0 ||
		script.updateCalls != 0 || script.postCalls != 0 || script.deleteCalls != 0 {
		t.Fatalf("no-reply emitted Slack message calls: start=%d append=%d stop=%d update=%d post=%d delete=%d",
			script.startCalls, script.appendCalls, script.stopCalls, script.updateCalls, script.postCalls, script.deleteCalls)
	}
}

func TestSendToAgentNoReplyTerminalMessageWithoutTextEvent(t *testing.T) {
	script := &streamScript{streamTSs: []string{"stream.unused"}}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "done", Message: &agent.Message{Content: " \n" + noReplyToken + "\t"}},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if script.startCalls != 0 || script.updateCalls != 0 || script.postCalls != 0 {
		t.Fatalf("terminal-only no-reply emitted Slack message calls: start=%d update=%d post=%d",
			script.startCalls, script.updateCalls, script.postCalls)
	}
}

func TestSendToAgentUsesTerminalContentForNoReplyDecision(t *testing.T) {
	t.Run("terminal token overrides incomplete ordinary delta", func(t *testing.T) {
		script := &streamScript{streamTSs: []string{"stream.1"}}
		srv := newStreamServer(t, script)
		mgr := &scriptedMgr{events: []agent.ChatEvent{
			{Type: "text", Delta: "partial ordinary text"},
			{Type: "done", Message: &agent.Message{Content: noReplyToken}},
		}}
		bot := newBotWithStream(t, mgr, srv)

		bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

		if script.postCalls != 0 || script.updateCalls != 0 {
			t.Fatalf("authoritative terminal token delivered content: post=%d update=%d", script.postCalls, script.updateCalls)
		}
		if !containsString(script.deletedTS, "stream.1") {
			t.Fatalf("stream containing incomplete delta was not deleted: %v", script.deletedTS)
		}
	})

	t.Run("terminal ordinary text overrides streamed token", func(t *testing.T) {
		script := &streamScript{streamTSs: []string{"stream.unused"}}
		srv := newStreamServer(t, script)
		mgr := &scriptedMgr{events: []agent.ChatEvent{
			{Type: "text", Delta: noReplyToken},
			{Type: "done", Message: &agent.Message{Content: "ordinary final response"}},
		}}
		bot := newBotWithStream(t, mgr, srv)

		bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

		if script.postCalls != 1 || script.lastPostMD != "ordinary final response" {
			t.Fatalf("authoritative ordinary response not delivered: posts=%d body=%q", script.postCalls, script.lastPostMD)
		}
	})
}

func TestSendToAgentNeverLeaksNoReplyTokenOnError(t *testing.T) {
	script := &streamScript{streamTSs: []string{"stream.unused"}}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "text", Delta: noReplyToken},
		{Type: "done", Message: &agent.Message{Content: noReplyToken}, ErrorMessage: "backend failed"},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if script.postCalls != 1 {
		t.Fatalf("error after no-reply token should post one generic failure, got %d", script.postCalls)
	}
	if strings.Contains(script.lastPostMD, noReplyToken) {
		t.Fatalf("no-reply token leaked in failure message: %q", script.lastPostMD)
	}
}

func TestSendToAgentPreservesCancelledDoneContent(t *testing.T) {
	script := &streamScript{}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "text", Delta: "partial"},
		{Type: "done", Message: &agent.Message{Content: "partial work completed before stop"}, ErrorMessage: agent.ErrMsgCancelled},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")
	if script.postCalls != 1 || script.lastPostMD != "partial work completed before stop" {
		t.Fatalf("cancelled partial not delivered: posts=%d body=%q", script.postCalls, script.lastPostMD)
	}
}

func TestSendToAgentPreservesHubDecodedPartialWhenPeerRelayIsCancelled(t *testing.T) {
	streamStarted := make(chan struct{})
	script := &streamScript{streamTSs: []string{"stream.1"}, startSeen: streamStarted}
	srv := newStreamServer(t, script)
	mgr := &interruptedRelayMgr{started: make(chan struct{})}
	bot := newBotWithStream(t, mgr, srv)

	done := make(chan struct{})
	go func() {
		bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")
		close(done)
	}()
	select {
	case <-streamStarted:
	case <-time.After(time.Second):
		t.Fatal("Hub did not decode and stream the relay partial")
	}
	if !bot.cancelActiveTurn("C1", "thread.123") {
		t.Fatal("peer-relayed turn was not registered for cancellation")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("peer-relayed turn did not finalize after cancellation")
	}
	got := script.lastUpdateMD
	if got == "" {
		got = script.lastPostMD
	}
	if got != "partial from holder" {
		t.Fatalf("Hub-decoded partial was not finalized: updates=%d posts=%d update=%q post=%q",
			script.updateCalls, script.postCalls, script.lastUpdateMD, script.lastPostMD)
	}
}

func TestSendToAgentReportsStoppedWhenCancelledWithoutContent(t *testing.T) {
	script := &streamScript{}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{{Type: "done", ErrorMessage: agent.ErrMsgCancelled}}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")
	if script.postCalls != 1 || script.lastPostMD != stopCommandDone {
		t.Fatalf("cancelled empty turn posts=%d body=%q", script.postCalls, script.lastPostMD)
	}
}

func TestSendToAgentDeletesProgressOnlyStreamAfterStopNotice(t *testing.T) {
	script := &streamScript{streamTSs: []string{"stream.1"}}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "tool_use", ToolName: "Bash"},
		{Type: "done", ErrorMessage: agent.ErrMsgCancelled},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")
	if script.postCalls != 1 || script.lastPostMD != stopCommandDone {
		t.Fatalf("stop notice posts=%d body=%q", script.postCalls, script.lastPostMD)
	}
	if !containsString(script.deletedTS, "stream.1") {
		t.Fatalf("progress-only stream was not deleted: %v", script.deletedTS)
	}
}

func TestSendToAgentNeverLeaksIncompleteNoReplyPrefix(t *testing.T) {
	tests := []struct {
		name   string
		events []agent.ChatEvent
	}{
		{
			name: "event stream closes without done",
			events: []agent.ChatEvent{
				{Type: "text", Delta: "[[NO_"},
			},
		},
		{
			name: "done reports backend error",
			events: []agent.ChatEvent{
				{Type: "text", Delta: "[[NO_"},
				{Type: "done", Message: &agent.Message{Content: "[[NO_"}, ErrorMessage: "backend failed"},
			},
		},
		{
			name: "authoritative terminal prefix replaces ordinary delta",
			events: []agent.ChatEvent{
				{Type: "text", Delta: "ordinary streamed text"},
				{Type: "done", Message: &agent.Message{Content: "[[NO_"}, ErrorMessage: "backend failed"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			script := &streamScript{streamTSs: []string{"stream.unused"}}
			srv := newStreamServer(t, script)
			bot := newBotWithStream(t, &scriptedMgr{events: tc.events}, srv)

			bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

			if script.postCalls != 1 {
				t.Fatalf("incomplete control prefix should post one generic failure, got %d", script.postCalls)
			}
			if strings.Contains(script.lastPostMD, "[[NO_") {
				t.Fatalf("incomplete no-reply prefix leaked in failure message: %q", script.lastPostMD)
			}
		})
	}
}

func TestSendToAgentNoReplyDeletesEarlierToolStream(t *testing.T) {
	script := &streamScript{streamTSs: []string{"stream.1"}}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "tool_use", ToolName: "Bash"},
		{Type: "text", Delta: noReplyToken},
		{Type: "done", Message: &agent.Message{Content: noReplyToken}},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if script.startCalls != 1 {
		t.Fatalf("chat.startStream calls = %d, want 1 for the earlier tool indicator", script.startCalls)
	}
	if !containsString(script.stoppedTS, "stream.1") || !containsString(script.deletedTS, "stream.1") {
		t.Fatalf("suppressed tool stream was not stopped and deleted; stopped=%v deleted=%v", script.stoppedTS, script.deletedTS)
	}
	if script.updateCalls != 0 || script.postCalls != 0 {
		t.Fatalf("suppressed tool stream delivered content: update=%d post=%d", script.updateCalls, script.postCalls)
	}
}

func TestSendToAgentNoReplyDeleteRecoversFromRateLimit(t *testing.T) {
	script := &streamScript{
		streamTSs:         []string{"stream.1"},
		rateLimitDeleteAt: []int{0},
	}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "tool_use", ToolName: "Bash"},
		{Type: "text", Delta: noReplyToken},
		{Type: "done", Message: &agent.Message{Content: noReplyToken}},
	}}
	bot := newBotWithStream(t, mgr, srv)
	var sleeps atomic.Int32
	bot.rateLimitSleep = func(time.Duration) <-chan time.Time {
		sleeps.Add(1)
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if script.deleteCalls != 2 {
		t.Fatalf("chat.delete calls = %d, want one rate-limited attempt plus one retry", script.deleteCalls)
	}
	if sleeps.Load() != 1 {
		t.Fatalf("rate-limit sleeps = %d, want 1", sleeps.Load())
	}
	if countString(script.deletedTS, "stream.1") != 2 {
		t.Fatalf("stream.1 delete attempts = %d, want 2; deletedTS=%v", countString(script.deletedTS, "stream.1"), script.deletedTS)
	}
}

func TestSendToAgentNoReplyRetriesFailedSupersededStreamDeletion(t *testing.T) {
	rotations := maxRetainedDeadStreams + 1
	streams := make([]string, rotations)
	killRotations := make([]int, rotations)
	events := make([]agent.ChatEvent, rotations)
	for i := range rotations {
		streams[i] = fmt.Sprintf("stream.%d", i+1)
		killRotations[i] = i
		events[i] = agent.ChatEvent{Type: "tool_use", ToolName: "Bash"}
	}
	events = append(events,
		agent.ChatEvent{Type: "text", Delta: noReplyToken},
		agent.ChatEvent{Type: "done", Message: &agent.Message{Content: noReplyToken}},
	)
	// Exhaust every rate-limit attempt made by the eager cleanup of the
	// first superseded stream. Suppression must retain that TS and make a
	// fifth, successful delete attempt before returning.
	rateLimitedDeletes := make([]int, maxRateLimitRetry+1)
	for i := range rateLimitedDeletes {
		rateLimitedDeletes[i] = i
	}
	script := &streamScript{
		streamTSs:         streams,
		killAt:            killRotations,
		rateLimitDeleteAt: rateLimitedDeletes,
	}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: events}
	bot := newBotWithStream(t, mgr, srv)
	bot.rateLimitSleep = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	var clock atomic.Int64
	bot.streamNow = func() time.Time {
		ns := clock.Add(int64(streamRestartWindow + time.Second))
		return time.Unix(0, ns)
	}

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if got := countString(script.deletedTS, "stream.1"); got != maxRateLimitRetry+2 {
		t.Fatalf("stream.1 delete attempts = %d, want %d (exhausted eager cleanup + suppression retry); deletedTS=%v",
			got, maxRateLimitRetry+2, script.deletedTS)
	}
	for _, ts := range streams {
		if !containsString(script.deletedTS, ts) {
			t.Errorf("suppressed stream %q was never deleted; deletedTS=%v", ts, script.deletedTS)
		}
	}
	if script.updateCalls != 0 || script.postCalls != 0 {
		t.Fatalf("no-reply delivered content after cleanup retry: update=%d post=%d", script.updateCalls, script.postCalls)
	}
}

func TestSendToAgentNoReplyLeavesUserAsLastThreadHistoryEntry(t *testing.T) {
	const (
		channel   = "C1"
		threadTS  = "1700000000.000100"
		messageTS = "1700000001.000200"
	)
	script := &streamScript{
		streamTSs: []string{"stream.unused"},
		// Simulate conversations.replies succeeding before Slack has made the
		// triggering event visible: the response contains only the root.
		repliesJSON: `{"ok":true,"messages":[` +
			`{"type":"message","user":"UROOT","text":"root","ts":"` + threadTS + `","thread_ts":"` + threadTS + `"}` +
			`]}`,
	}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "text", Delta: noReplyToken},
		{Type: "done", Message: &agent.Message{Content: noReplyToken}},
	}}
	bot := newBotWithStream(t, mgr, srv)
	bot.agentDataDir = t.TempDir()

	path := chathistory.HistoryFilePath(bot.agentDataDir, platformSlack, channel, threadTS)
	if err := chathistory.AppendMessages(path, []chathistory.HistoryMessage{
		{Platform: platformSlack, ChannelID: channel, ThreadID: threadTS, MessageID: threadTS, UserID: "UROOT", Text: "root"},
		{Platform: platformSlack, ChannelID: channel, ThreadID: threadTS, MessageID: "1700000000.bot", UserID: bot.botUserID, Text: "prior answer", IsBot: true},
	}); err != nil {
		t.Fatal(err)
	}

	bot.sendToAgent(context.Background(), channel, threadTS, threadTS, messageTS, "ping", "alice", "U123")
	if got := mgr.lastOneShotOpts.History; len(got) != 2 || got[0].Text != "root" || got[1].Text != "prior answer" {
		t.Fatalf("ChatOneShot history = %+v, want canonical prior Slack transcript", got)
	}
	if mgr.lastOneShotOpts.HistorySelfUserID != bot.botUserID {
		t.Fatalf("HistorySelfUserID = %q, want %q", mgr.lastOneShotOpts.HistorySelfUserID, bot.botUserID)
	}

	history, err := chathistory.LoadHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 || history[len(history)-1].MessageID != messageTS || history[len(history)-1].IsBot {
		t.Fatalf("last history entry after no-reply is not the triggering user message: %+v", history)
	}
	for _, msg := range history {
		if strings.Contains(msg.Text, noReplyToken) {
			t.Fatalf("no-reply token was persisted to Slack history: %+v", msg)
		}
	}
	if bot.shouldAutoReply(channel, threadTS, "another unmentioned message") {
		t.Fatal("no-reply should leave the triggering user as last history entry and stop auto-reply chaining")
	}
}

func TestSendToAgentFlushesNoReplyPrefixWhenResponseDiverges(t *testing.T) {
	script := &streamScript{streamTSs: []string{"stream.1"}}
	srv := newStreamServer(t, script)
	text := noReplyToken + " is the control token"
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "text", Delta: "[[NO_"},
		{Type: "text", Delta: "REPLY]] is the control token"},
		{Type: "done", Message: &agent.Message{Content: text}},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if script.startCalls != 1 || script.updateCalls != 1 {
		t.Fatalf("ordinary response after token prefix was not delivered: start=%d update=%d", script.startCalls, script.updateCalls)
	}
	if script.lastUpdateMD != text {
		t.Fatalf("final response = %q, want %q", script.lastUpdateMD, text)
	}
}

// TestSendToAgentRestartsOnDeadStream drives sendToAgent end-to-end with
// a tool_use event that triggers stream open + a doomed append, then a
// text event whose first append hits the stream-closed error. The bot
// must:
//   - open ts-1 on the tool_use,
//   - drop ts-1 after appendStream returns message_not_in_streaming_state,
//   - open ts-2 on the next tool_use (since text events are throttled
//     and won't open a new stream until the throttle clears, we use a
//     second tool_use which bypasses the throttle),
//   - flush the carried-over delta + new indicator on ts-2,
//   - chat.update ts-2 with the full response text,
//   - chat.delete the dead ts-1 during finalize cleanup (the full reply
//     landed on ts-2, so the frozen ts-1 partial is a duplicate).
//
// This is the integration guard for the silent-truncation bug that was
// the original motivation for these changes.
func TestSendToAgentRestartsOnDeadStream(t *testing.T) {
	script := &streamScript{
		streamTSs: []string{"stream.1", "stream.2"},
		// Kill the very first appendStream — that's the indicator
		// append on the first tool_use event. The bot should drop
		// ts-1, then the next tool_use opens ts-2 and lands the
		// indicator there.
		killAt: []int{0},
	}
	srv := newStreamServer(t, script)

	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "tool_use", ToolName: "Bash"},
		{Type: "tool_use", ToolName: "Read"},
		{Type: "text", Delta: "hello world"},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if !mgr.lastOneShotOpts.DisableKojoAttachmentInstructions {
		t.Error("Slack ChatOneShot must disable Kojo response-attachment instructions")
	}

	if script.startCalls != 2 {
		t.Errorf("chat.startStream calls = %d, want 2 (initial + 1 restart)", script.startCalls)
	}
	if len(script.issuedTS) >= 2 && script.issuedTS[0] != "stream.1" {
		t.Errorf("first startStream returned %q, want %q", script.issuedTS[0], "stream.1")
	}
	if !containsString(script.deletedTS, "stream.1") {
		t.Errorf("chat.delete not called on dead stream.1; deletedTS=%v", script.deletedTS)
	}
	if containsString(script.deletedTS, "stream.2") {
		t.Errorf("chat.delete must NOT touch the live final stream.2; deletedTS=%v", script.deletedTS)
	}
	if script.lastUpdateTS != "stream.2" {
		t.Errorf("final chat.update targeted %q, want %q", script.lastUpdateTS, "stream.2")
	}
	if !strings.Contains(script.lastUpdateMD, "hello world") {
		t.Errorf("final chat.update markdown_text = %q, want it to contain %q", script.lastUpdateMD, "hello world")
	}
}

// TestSendToAgentRestartBurstCapFallsBackToBatchPost forces every
// appendStream to die in a tight burst so the bot exhausts its
// maxStreamRestarts budget inside streamRestartWindow. After the circuit
// breaker trips, the remaining text must reach the user via
// chat.postMessage (batch fallback) rather than being silently dropped.
// Also asserts that every dead streamTS gets chat.delete'd during finalize
// cleanup (the full reply landed via batch post, so the N frozen partials
// are duplicates) so the channel doesn't render N stuck streaming messages.
func TestSendToAgentRestartBurstCapFallsBackToBatchPost(t *testing.T) {
	// Generate maxStreamRestarts+2 stream TSs so the bot has more
	// inventory than it can possibly use. The cap check must trip
	// before the inventory runs out.
	streams := make([]string, maxStreamRestarts+2)
	for i := range streams {
		streams[i] = fmt.Sprintf("stream.%d", i+1)
	}
	// Kill every appendStream call.
	killAll := make([]int, 200)
	for i := range killAll {
		killAll[i] = i
	}
	script := &streamScript{streamTSs: streams, killAt: killAll}
	srv := newStreamServer(t, script)

	// Use tool_use events (bypass the 1s throttle on text) so each
	// event reliably triggers a fresh appendStream attempt.
	events := make([]agent.ChatEvent, maxStreamRestarts+3)
	for i := range events {
		events[i] = agent.ChatEvent{Type: "tool_use", ToolName: "Bash"}
	}
	// End with a text event so response.Builder is non-empty and the
	// finalize path actually emits a chunk (no text → "something went
	// wrong" branch instead).
	events = append(events, agent.ChatEvent{Type: "text", Delta: "final words"}, agent.ChatEvent{Type: "done"})

	mgr := &scriptedMgr{events: events}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	// startStream must be called exactly maxStreamRestarts+1 times
	// (initial open + maxStreamRestarts restarts before the cap trips).
	// Equality matters here: the previous `>=` cap condition allowed
	// only maxStreamRestarts opens (off-by-one), and a `> +1` assertion
	// would happily pass that regression. Asserting equality keeps the
	// cap semantics pinned down.
	if script.startCalls != maxStreamRestarts+1 {
		t.Errorf("chat.startStream called %d times, want %d (initial + maxStreamRestarts)",
			script.startCalls, maxStreamRestarts+1)
	}
	// All dead streams should be chat.delete'd during finalize: every
	// issued stream died, the batch post carried the full reply, so each
	// frozen partial is a duplicate that must be removed.
	if len(script.deletedTS) != script.startCalls {
		t.Errorf("chat.delete called on %d streams, want %d (every issued stream)",
			len(script.deletedTS), script.startCalls)
	}
	// Fallback path: at least one chat.postMessage for the final text.
	if script.postCalls == 0 {
		t.Error("expected at least one chat.postMessage as batch-fallback after cap, got 0")
	}
}

func TestTrimStreamDeathsOutsideWindowBoundariesAndRollback(t *testing.T) {
	now := time.Unix(1_000, 0)
	cutoff := now.Add(-streamRestartWindow)
	cases := []struct {
		name   string
		deaths []time.Time
		want   []time.Time
	}{
		{
			name: "before cutoff is expired",
			deaths: []time.Time{
				cutoff.Add(-time.Nanosecond),
			},
			want: nil,
		},
		{
			name: "cutoff is inclusive",
			deaths: []time.Time{
				cutoff,
			},
			want: []time.Time{cutoff},
		},
		{
			name: "after cutoff is recent",
			deaths: []time.Time{
				cutoff.Add(time.Nanosecond),
			},
			want: []time.Time{cutoff.Add(time.Nanosecond)},
		},
		{
			name: "clock rollback retains future deaths conservatively",
			deaths: []time.Time{
				now.Add(time.Second),
				now.Add(-2 * streamRestartWindow),
			},
			want: []time.Time{now.Add(time.Second)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := trimStreamDeathsOutsideWindow(append([]time.Time(nil), tc.deaths...), now)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("trimStreamDeathsOutsideWindow() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSendToAgentAllowsSlowStreamRotation verifies that the restart cap is
// a burst circuit breaker, not a lifetime limit. Slack regularly expires an
// otherwise healthy stream after roughly five minutes. A long-running turn
// may therefore need more than maxStreamRestarts replacements, provided the
// deaths are spaced outside streamRestartWindow. The final live stream must
// still receive chat.update rather than forcing a batch post after ~30 min.
func TestSendToAgentAllowsSlowStreamRotation(t *testing.T) {
	const extraRotations = 2
	rotations := maxStreamRestarts + extraRotations
	streams := make([]string, rotations+1) // one live stream after all rotations
	for i := range streams {
		streams[i] = fmt.Sprintf("stream.%d", i+1)
	}
	killRotations := make([]int, rotations)
	for i := range killRotations {
		killRotations[i] = i
	}
	script := &streamScript{streamTSs: streams, killAt: killRotations}
	srv := newStreamServer(t, script)

	events := make([]agent.ChatEvent, rotations)
	for i := range events {
		events[i] = agent.ChatEvent{Type: "tool_use", ToolName: "Bash"}
	}
	events = append(events, agent.ChatEvent{Type: "text", Delta: "final words"}, agent.ChatEvent{Type: "done"})

	mgr := &scriptedMgr{events: events}
	bot := newBotWithStream(t, mgr, srv)
	var clock atomic.Int64
	bot.streamNow = func() time.Time {
		// startStream and dropStream both consult the clock. Advancing by
		// more than the whole window on each observation guarantees that
		// the preceding death is outside the next start's rolling window.
		ns := clock.Add(int64(streamRestartWindow + time.Second))
		return time.Unix(0, ns)
	}

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if script.startCalls != rotations+1 {
		t.Errorf("chat.startStream called %d times, want %d slow rotations plus final live stream",
			script.startCalls, rotations+1)
	}
	if script.postCalls != 0 {
		t.Errorf("chat.postMessage calls = %d, want 0; slow rotation must not trip batch fallback", script.postCalls)
	}
	if script.lastUpdateTS != streams[len(streams)-1] {
		t.Errorf("final chat.update targeted %q, want final live stream %q",
			script.lastUpdateTS, streams[len(streams)-1])
	}
	if len(script.deletedTS) != rotations {
		t.Errorf("chat.delete called on %d streams, want %d dead rotations", len(script.deletedTS), rotations)
	}
}

func TestSendToAgentBoundsDeadStreamArtifactsDuringSlowRotation(t *testing.T) {
	rotations := maxRetainedDeadStreams + 2
	streams := make([]string, rotations+1)
	for i := range streams {
		streams[i] = fmt.Sprintf("stream.%d", i+1)
	}
	killRotations := make([]int, rotations)
	for i := range killRotations {
		killRotations[i] = i
	}
	script := &streamScript{
		streamTSs:  streams,
		killAt:     killRotations,
		failUpdate: true,
		failPost:   true,
	}
	srv := newStreamServer(t, script)

	events := make([]agent.ChatEvent, rotations)
	for i := range events {
		events[i] = agent.ChatEvent{Type: "tool_use", ToolName: "Bash"}
	}
	events = append(events, agent.ChatEvent{Type: "text", Delta: "final words"}, agent.ChatEvent{Type: "done"})
	mgr := &scriptedMgr{events: events}
	bot := newBotWithStream(t, mgr, srv)
	var clock atomic.Int64
	bot.streamNow = func() time.Time {
		ns := clock.Add(int64(streamRestartWindow + time.Second))
		return time.Unix(0, ns)
	}

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	eagerDeletes := rotations - maxRetainedDeadStreams
	if len(script.deletedTS) != eagerDeletes {
		t.Fatalf("chat.delete called on %d streams, want %d superseded artifacts", len(script.deletedTS), eagerDeletes)
	}
	for _, ts := range streams[:eagerDeletes] {
		if !containsString(script.deletedTS, ts) {
			t.Errorf("superseded stream %q was not deleted; deletedTS=%v", ts, script.deletedTS)
		}
	}
	for _, ts := range streams[eagerDeletes:rotations] {
		if !containsString(script.stoppedTS, ts) {
			t.Errorf("retained failure artifact %q was not stopped; stoppedTS=%v", ts, script.stoppedTS)
		}
	}
}

func TestSendToAgentDeletesStreamWhenUpdateFailsAndFreshPostSucceeds(t *testing.T) {
	script := &streamScript{streamTSs: []string{"stream.1"}, failUpdate: true}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{{Type: "text", Delta: "hello world"}}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")
	if script.postCalls == 0 {
		t.Fatal("expected fresh post fallback after chat.update failure")
	}
	if !containsString(script.deletedTS, "stream.1") {
		t.Fatalf("stale live stream was not deleted; deletedTS=%v", script.deletedTS)
	}
}

// TestSendToAgentKeepsDeadStreamWhenDeliveryFails verifies the safety net:
// if the final reply could NOT be delivered (chat.update fails AND the
// chat.postMessage fallback fails), the dead partial is preserved as a
// debugging/retry artifact — StopStream'd, not chat.delete'd. Deleting it
// there would leave the user with no content at all.
func TestSendToAgentKeepsDeadStreamWhenDeliveryFails(t *testing.T) {
	script := &streamScript{
		streamTSs:  []string{"stream.1", "stream.2"},
		killAt:     []int{0}, // kill the first append → drop stream.1, restart to stream.2
		failUpdate: true,     // finalize chat.update on stream.2 fails
		failPost:   true,     // batch/fallback postMessage also fails
	}
	srv := newStreamServer(t, script)

	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "tool_use", ToolName: "Bash"},
		{Type: "tool_use", ToolName: "Read"},
		{Type: "text", Delta: "hello world"},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	// Delivery failed, so the dead stream.1 must be kept (StopStream),
	// not deleted.
	if containsString(script.deletedTS, "stream.1") {
		t.Errorf("dead stream.1 was deleted despite delivery failure; deletedTS=%v", script.deletedTS)
	}
	if !containsString(script.stoppedTS, "stream.1") {
		t.Errorf("dead stream.1 should be StopStream'd as a retry artifact; stoppedTS=%v", script.stoppedTS)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func countString(haystack []string, needle string) int {
	count := 0
	for _, s := range haystack {
		if s == needle {
			count++
		}
	}
	return count
}

// TestIsStreamClosedErr verifies the typed-error detection used by
// appendStream to decide whether to abandon a dead stream. The detection
// goes through errors.As + slack.SlackErrorResponse so a future slack-go
// change that wraps the error still trips the same code path.
func TestIsStreamClosedErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated error", errors.New("boom"), false},
		{"wrong slack error code", slack.SlackErrorResponse{Err: "channel_not_found"}, false},
		{"raw stream-closed", slack.SlackErrorResponse{Err: slackErrNotStreaming}, true},
		{"wrapped stream-closed", fmt.Errorf("append failed: %w", slack.SlackErrorResponse{Err: slackErrNotStreaming}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStreamClosedErr(tc.err); got != tc.want {
				t.Fatalf("isStreamClosedErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestAppendStreamReturnsFalseOnStreamClosed verifies the critical bug-fix
// signal: when Slack rejects chat.appendStream with
// message_not_in_streaming_state, appendStream returns false so the
// caller can drop the dead streamTS and restart a fresh stream. Before
// this fix the error was silently logged at Debug and the caller kept
// pushing into the dead stream, producing the 30+ identical failures
// observed in production (2026-06-03 12:34-12:41).
func TestAppendStreamReturnsFalseOnStreamClosed(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "appendStream") {
			calls.Add(1)
			fmt.Fprintf(w, `{"ok":false,"error":"message_not_in_streaming_state"}`)
			return
		}
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	bot := &Bot{api: api, logger: testLogger}

	if bot.appendStream(context.Background(), "C1", "stream-ts", "delta") {
		t.Fatal("appendStream must return false on message_not_in_streaming_state")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("AppendStream call count = %d, want 1 (no retry on stream-closed)", got)
	}
}

// TestAppendStreamReturnsTrueOnUnknownError covers the conservative
// fallback for non-rate-limit, non-stream-closed errors. A transient
// Slack 5xx must NOT abandon the stream — the next call might succeed
// and prematurely churning the streamTS leaves an extra orphaned
// message in the channel. Caller keeps the same streamTS; if the error
// WAS terminal, the next append will return the typed stream-closed
// signal and we'll restart then.
func TestAppendStreamReturnsTrueOnUnknownError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "appendStream") {
			fmt.Fprintf(w, `{"ok":false,"error":"internal_error"}`)
			return
		}
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	bot := &Bot{api: api, logger: testLogger}

	if !bot.appendStream(context.Background(), "C1", "stream-ts", "delta") {
		t.Fatal("appendStream must return true on unknown errors (don't churn the stream)")
	}
}

// TestAppendStreamRateLimitNoExtraSleepOnFinalAttempt mirrors the
// postMessage guard for appendStream's identical retry loop. Both sites
// must stop sleeping once attempt == maxRateLimitRetry — a stray sleep
// there delays the entire stream-finalize path with no follow-up
// AppendStream call to justify the wait.
func TestAppendStreamRateLimitNoExtraSleepOnFinalAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slack streaming sends chat.appendStream — return 429 on any
		// path that isn't an irrelevant info call so the test stays
		// resilient to URL routing changes in slack-go.
		if strings.Contains(r.URL.Path, "appendStream") {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sleeps atomic.Int32
	bot := &Bot{
		api:    api,
		logger: testLogger,
		ctx:    ctx,
		rateLimitSleep: func(d time.Duration) <-chan time.Time {
			sleeps.Add(1)
			ch := make(chan time.Time, 1)
			ch <- time.Now()
			return ch
		},
	}

	bot.appendStream(context.Background(), "C1", "stream-ts", "delta")

	if got := calls.Load(); got != int32(maxRateLimitRetry+1) {
		t.Errorf("AppendStream call count = %d, want %d (maxRateLimitRetry+1)", got, maxRateLimitRetry+1)
	}
	if got := sleeps.Load(); got != int32(maxRateLimitRetry) {
		t.Errorf("rateLimitSleep invocations = %d, want %d (one per inter-attempt gap; no sleep after final attempt)", got, maxRateLimitRetry)
	}
}

func TestTextSegmentSeparator(t *testing.T) {
	cases := []struct{ prev, next, want string }{
		{"", "next", ""},
		{"  \n", "next", ""},
		{"answer?", "next", "\n\n"},
		{"answer\n", "next", "\n"},
		{"answer\n\n", "next", ""},
		{"answer", "\nnext", "\n"},
		{"answer\n", "\nnext", ""},
	}
	for _, c := range cases {
		if got := textSegmentSeparator(c.prev, c.next); got != c.want {
			t.Errorf("textSegmentSeparator(%q, %q) = %q, want %q", c.prev, c.next, got, c.want)
		}
	}
}

func finalSlackBody(script *streamScript) string {
	if script.updateCalls > 0 {
		return script.lastUpdateMD
	}
	return script.lastPostMD
}

func TestSendToAgentSeparatesTextSegmentsAroundTools(t *testing.T) {
	t.Run("terminal matches streamed text", func(t *testing.T) {
		script := &streamScript{streamTSs: []string{"stream.1"}}
		srv := newStreamServer(t, script)
		mgr := &scriptedMgr{events: []agent.ChatEvent{
			{Type: "text", Delta: "まとめてよいですか？"},
			{Type: "tool_use", ToolName: "Bash", ToolInput: `{"command":"true"}`},
			{Type: "tool_result", ToolName: "Bash"},
			{Type: "text", Delta: "次の段落"},
			{Type: "done", Message: &agent.Message{Content: "まとめてよいですか？次の段落"}},
		}}
		bot := newBotWithStream(t, mgr, srv)

		bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

		if got, want := finalSlackBody(script), "まとめてよいですか？\n\n次の段落"; got != want {
			t.Fatalf("final body = %q, want %q", got, want)
		}
	})

	t.Run("terminal carries an undelivered tail", func(t *testing.T) {
		script := &streamScript{streamTSs: []string{"stream.1"}}
		srv := newStreamServer(t, script)
		mgr := &scriptedMgr{events: []agent.ChatEvent{
			{Type: "text", Delta: "first"},
			{Type: "tool_use", ToolName: "Bash", ToolInput: `{"command":"true"}`},
			{Type: "text", Delta: "second"},
			{Type: "done", Message: &agent.Message{Content: "firstsecond and tail"}},
		}}
		bot := newBotWithStream(t, mgr, srv)

		bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

		if got, want := finalSlackBody(script), "first\n\nsecond and tail"; got != want {
			t.Fatalf("final body = %q, want %q", got, want)
		}
	})

	t.Run("unstreamed segment after a tool", func(t *testing.T) {
		script := &streamScript{streamTSs: []string{"stream.1"}}
		srv := newStreamServer(t, script)
		mgr := &scriptedMgr{events: []agent.ChatEvent{
			{Type: "text", Delta: "first"},
			{Type: "tool_use", ToolName: "Bash", ToolInput: `{"command":"true"}`},
			{Type: "done", Message: &agent.Message{Content: "firstsecond"}},
		}}
		bot := newBotWithStream(t, mgr, srv)

		bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

		if got, want := finalSlackBody(script), "first\n\nsecond"; got != want {
			t.Fatalf("final body = %q, want %q", got, want)
		}
	})

	t.Run("terminal prepends unstreamed text", func(t *testing.T) {
		script := &streamScript{streamTSs: []string{"stream.1"}}
		srv := newStreamServer(t, script)
		mgr := &scriptedMgr{events: []agent.ChatEvent{
			{Type: "text", Delta: "first"},
			{Type: "tool_use", ToolName: "Bash", ToolInput: `{"command":"true"}`},
			{Type: "text", Delta: "second"},
			{Type: "done", Message: &agent.Message{Content: "earlier\n\nfirstsecond"}},
		}}
		bot := newBotWithStream(t, mgr, srv)

		bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

		if got, want := finalSlackBody(script), "earlier\n\nfirst\n\nsecond"; got != want {
			t.Fatalf("final body = %q, want %q", got, want)
		}
	})

	t.Run("subagent text is not part of the reply", func(t *testing.T) {
		script := &streamScript{streamTSs: []string{"stream.1"}}
		srv := newStreamServer(t, script)
		mgr := &scriptedMgr{events: []agent.ChatEvent{
			{Type: "text", Delta: "main"},
			{Type: "text", Delta: " child", ParentToolUseID: "toolu_parent"},
			{Type: "done", Message: &agent.Message{Content: "main"}},
		}}
		bot := newBotWithStream(t, mgr, srv)

		bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

		if got, want := finalSlackBody(script), "main"; got != want {
			t.Fatalf("final body = %q, want %q", got, want)
		}
	})

	t.Run("diverging terminal stays authoritative", func(t *testing.T) {
		script := &streamScript{streamTSs: []string{"stream.1"}}
		srv := newStreamServer(t, script)
		mgr := &scriptedMgr{events: []agent.ChatEvent{
			{Type: "text", Delta: "first"},
			{Type: "tool_use", ToolName: "Bash", ToolInput: `{"command":"true"}`},
			{Type: "text", Delta: "second"},
			{Type: "done", Message: &agent.Message{Content: "rewritten body"}},
		}}
		bot := newBotWithStream(t, mgr, srv)

		bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

		if got, want := finalSlackBody(script), "rewritten body"; got != want {
			t.Fatalf("final body = %q, want %q", got, want)
		}
	})

	t.Run("subagent tools do not split parent text", func(t *testing.T) {
		script := &streamScript{streamTSs: []string{"stream.1"}}
		srv := newStreamServer(t, script)
		mgr := &scriptedMgr{events: []agent.ChatEvent{
			{Type: "text", Delta: "one "},
			{Type: "tool_use", ToolName: "Bash", ToolInput: `{"command":"true"}`, ParentToolUseID: "toolu_parent"},
			{Type: "text", Delta: "sentence"},
			{Type: "done", Message: &agent.Message{Content: "one sentence"}},
		}}
		bot := newBotWithStream(t, mgr, srv)

		bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

		if got, want := finalSlackBody(script), "one sentence"; got != want {
			t.Fatalf("final body = %q, want %q", got, want)
		}
	})
}

func TestSlackSystemPromptSaysAllTextIsPosted(t *testing.T) {
	script := &streamScript{streamTSs: []string{"stream.1"}}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "text", Delta: "hi"},
		{Type: "done", Message: &agent.Message{Content: "hi"}},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if !strings.Contains(mgr.lastOneShotOpts.SystemPromptExtra, "Every text you output during this turn is posted, not only the last one") {
		t.Fatalf("Slack prompt does not explain that all turn text is posted: %q", mgr.lastOneShotOpts.SystemPromptExtra)
	}
}
