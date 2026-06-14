package slackbot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/chathistory"
	"github.com/slack-go/slack"
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
	tl1 := bot.acquireThreadLock("C1", "1234.5678")
	tl1.mu.Lock()

	// Acquire again for the same thread — same lock, higher refcount.
	tl2 := bot.acquireThreadLock("C1", "1234.5678")
	if tl1 != tl2 {
		t.Fatal("expected same lock for same thread")
	}

	// Release once — should still exist (refcount > 0).
	bot.releaseThreadLock("C1", "1234.5678", tl1)
	bot.threadLocksMu.Lock()
	_, exists := bot.threadLocks["C1:1234.5678"]
	bot.threadLocksMu.Unlock()
	if !exists {
		t.Fatal("lock should still exist after one release")
	}

	tl1.mu.Unlock()

	// Release again — refcount hits 0, entry removed.
	bot.releaseThreadLock("C1", "1234.5678", tl2)
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

	tl1 := bot.acquireThreadLock("C1", "ts1")
	tl2 := bot.acquireThreadLock("C1", "ts2")
	tl3 := bot.acquireThreadLock("C2", "ts1")

	if tl1 == tl2 || tl1 == tl3 || tl2 == tl3 {
		t.Fatal("different channel/thread combos should get different locks")
	}

	bot.releaseThreadLock("C1", "ts1", tl1)
	bot.releaseThreadLock("C1", "ts2", tl2)
	bot.releaseThreadLock("C2", "ts1", tl3)
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
	for _, slackError := range []string{"invalid_blocks_format", "markdown_text_conflict"} {
		t.Run(slackError, func(t *testing.T) {
			type captured struct {
				text         string
				markdownText string
				threadTS     string
			}
			var calls []captured

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/chat.postMessage":
					_ = r.ParseForm()
					calls = append(calls, captured{
						text:         r.FormValue("text"),
						markdownText: r.FormValue("markdown_text"),
						threadTS:     r.FormValue("thread_ts"),
					})
					if len(calls) == 1 {
						fmt.Fprintf(w, `{"ok":false,"error":%q}`, slackError)
						return
					}
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

			body := "## Heading\n\n**bold** and [link](https://example.com)"
			if !bot.postMessage(context.Background(), "C1", "thread.999", body) {
				t.Fatal("postMessage should succeed via legacy text fallback")
			}
			if len(calls) != 2 {
				t.Fatalf("chat.postMessage called %d times, want 2", len(calls))
			}
			if calls[0].markdownText != body {
				t.Errorf("first call markdown_text = %q, want %q", calls[0].markdownText, body)
			}
			if calls[0].text != "" {
				t.Errorf("first call text must be empty, got %q", calls[0].text)
			}
			if calls[1].markdownText != "" {
				t.Errorf("fallback call markdown_text must be empty, got %q", calls[1].markdownText)
			}
			wantText := PlainToSlack(body)
			if calls[1].text != wantText {
				t.Errorf("fallback text = %q, want %q", calls[1].text, wantText)
			}
			for i, call := range calls {
				if call.threadTS != "thread.999" {
					t.Errorf("call %d thread_ts = %q, want thread.999", i+1, call.threadTS)
				}
			}
		})
	}
}

func TestBotPostMessageDoesNotFallbackToLegacyTextOnUnrelatedSlackError(t *testing.T) {
	var called int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat.postMessage":
			called++
			fmt.Fprintf(w, `{"ok":false,"error":"invalid_auth"}`)
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

	if bot.postMessage(context.Background(), "C1", "", "hello") {
		t.Fatal("postMessage should fail on invalid_auth")
	}
	if called != 1 {
		t.Fatalf("chat.postMessage called %d times, want 1", called)
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
	events []agent.ChatEvent
}

func (m *scriptedMgr) Chat(_ context.Context, _, _, _ string, _ []agent.MessageAttachment, _ ...agent.BusySource) (<-chan agent.ChatEvent, error) {
	ch := make(chan agent.ChatEvent, len(m.events)+1)
	for _, e := range m.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func (m *scriptedMgr) ChatOneShot(_ context.Context, _, _ string, _ agent.OneShotOpts) (<-chan agent.ChatEvent, error) {
	ch := make(chan agent.ChatEvent, len(m.events)+1)
	for _, e := range m.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func (m *scriptedMgr) CanResumeSession(_, _ string) bool { return false }

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

	startCalls   int
	appendCalls  int
	stopCalls    int
	updateCalls  int
	postCalls    int
	deleteCalls  int
	issuedTS     []string // ts values returned by chat.startStream
	stoppedTS    []string // ts values seen by chat.stopStream
	deletedTS    []string // ts values seen by chat.delete (dead-stream cleanup)
	appendOnTS   []string // ts the bot tried to append to (in order)
	lastUpdateTS string
	lastUpdateMD string
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
			if script.failPost {
				fmt.Fprintf(w, `{"ok":false,"error":"channel_not_found"}`)
				return
			}
			fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":"post.999"}`)
		case "/chat.delete":
			script.deleteCalls++
			script.deletedTS = append(script.deletedTS, r.FormValue("ts"))
			fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":%q}`, r.FormValue("ts"))
		case "/conversations.history", "/conversations.replies":
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

// TestSendToAgentRestartCapFallsBackToBatchPost forces every appendStream
// to die so the bot exhausts its maxStreamRestarts budget. After the cap
// trips, the remaining text must reach the user via chat.postMessage
// (batch fallback) rather than being silently dropped. Also asserts that
// every dead streamTS gets chat.delete'd during finalize cleanup (the
// full reply landed via batch post, so the N frozen partials are
// duplicates) so the channel doesn't render N stuck streaming messages.
func TestSendToAgentRestartCapFallsBackToBatchPost(t *testing.T) {
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
	events = append(events, agent.ChatEvent{Type: "text", Delta: "final words"})

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
