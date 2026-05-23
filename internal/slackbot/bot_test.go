package slackbot

import (
	"context"
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

// --- postMessage tests ---

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
