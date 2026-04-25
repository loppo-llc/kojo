package slackbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/chathistory"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// ChatManager is the interface the bot uses to interact with agents.
// agent.Manager satisfies this interface directly — no adapter needed.
type ChatManager interface {
	Chat(ctx context.Context, agentID, message, role string, attachments []agent.MessageAttachment) (<-chan agent.ChatEvent, error)
	ChatOneShot(ctx context.Context, agentID, message string) (<-chan agent.ChatEvent, error)
}

// Bot manages a single Slack Socket Mode connection for one agent.
type Bot struct {
	agentID      string
	agentDataDir string // agent data directory for history file storage
	config       agent.SlackBotConfig
	api          *slack.Client
	sm           *socketmode.Client
	mgr          ChatManager
	logger       *slog.Logger
	botUserID    string
	botToken     string // stored for file downloads (slack.Client.token is private)

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// threadLocks serializes processing per thread to maintain history consistency.
	// Each threadLock carries a reference count so the map entry is only deleted
	// when the last goroutine releases the lock.
	threadLocksMu sync.Mutex
	threadLocks   map[string]*threadLock // key: "channel:threadTS"

	// userCache caches Slack user ID → display name for the Bot's lifetime.
	// Display names rarely change; the cache is cleared on Bot restart.
	userCacheMu sync.RWMutex
	userCache   map[string]string

	// sem limits the number of concurrent sendToAgent goroutines.
	sem chan struct{}
}

const (
	slackMaxMsgLen    = 3000
	maxRateLimitRetry = 3

	// maxConcurrentChats is the maximum number of concurrent sendToAgent
	// goroutines per Bot (i.e. per agent). This prevents resource exhaustion
	// when many Slack threads send messages simultaneously.
	maxConcurrentChats = 10

	// platformSlack is the platform identifier used in chat history entries.
	platformSlack = "slack"

	// typingStatus is the assistant status text shown while processing a message.
	typingStatus = "Thinking…"
)

// NewBot creates a new Bot instance. Call Run() to start it.
// agentDataDir is the agent's data directory used for storing conversation history files.
// parentCtx controls the Bot's lifetime: cancelling it will stop the event loop.
func NewBot(parentCtx context.Context, agentID string, agentDataDir string, cfg agent.SlackBotConfig, appToken, botToken string, mgr ChatManager, logger *slog.Logger, extraSlackOpts ...slack.Option) *Bot {
	opts := append([]slack.Option{slack.OptionAppLevelToken(appToken)}, extraSlackOpts...)
	api := slack.New(botToken, opts...)
	sm := socketmode.New(api, socketmode.OptionLog(slog.NewLogLogger(logger.Handler(), slog.LevelWarn)))

	ctx, cancel := context.WithCancel(parentCtx)
	return &Bot{
		agentID:      agentID,
		agentDataDir: agentDataDir,
		config:       cfg,
		api:          api,
		sm:           sm,
		mgr:          mgr,
		logger:       logger.With("component", "slackbot", "agent", agentID),
		botToken:     botToken,
		ctx:          ctx,
		cancel:       cancel,
		done:        make(chan struct{}),
		threadLocks: make(map[string]*threadLock),
		userCache:   make(map[string]string),
		sem:         make(chan struct{}, maxConcurrentChats),
	}
}

// Run starts the Socket Mode event loop. It blocks until the Bot's context is cancelled.
func (b *Bot) Run() {
	defer close(b.done)

	ctx := b.ctx

	// Resolve our own user ID
	authResp, err := b.api.AuthTestContext(ctx)
	if err != nil {
		b.logger.Error("slack auth.test failed", "err", err)
		return
	}
	b.botUserID = authResp.UserID
	b.logger.Info("slack bot connected", "botUser", b.botUserID, "team", authResp.Team)

	go func() {
		if err := b.sm.RunContext(ctx); err != nil && ctx.Err() == nil {
			b.logger.Error("socketmode.Run exited with error", "err", err)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-b.sm.Events:
			if !ok {
				return
			}
			b.handleEvent(ctx, evt)
		}
	}
}

// Stop cancels the bot's context and waits for it to finish.
func (b *Bot) Stop() {
	b.cancel()
	<-b.done
}

// Done returns a channel that is closed when the bot exits.
func (b *Bot) Done() <-chan struct{} {
	return b.done
}

// TestConnection performs auth.test to validate the tokens.
// The provided context controls the request timeout.
func TestConnection(ctx context.Context, appToken, botToken string) (team, botUser string, err error) {
	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	resp, err := api.AuthTestContext(ctx)
	if err != nil {
		return "", "", fmt.Errorf("auth.test failed: %w", err)
	}
	return resp.Team, resp.User, nil
}

func (b *Bot) handleEvent(ctx context.Context, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeEventsAPI:
		evtAPI, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		b.sm.Ack(*evt.Request)
		b.handleEventsAPI(ctx, evtAPI)

	case socketmode.EventTypeConnectionError:
		b.logger.Warn("slack connection error", "data", evt.Data)

	case socketmode.EventTypeConnecting:
		b.logger.Debug("connecting to slack")

	case socketmode.EventTypeConnected:
		b.logger.Debug("connected to slack")

	case socketmode.EventTypeDisconnect:
		b.logger.Info("disconnected from slack")
	}
}

func (b *Bot) handleEventsAPI(ctx context.Context, evt slackevents.EventsAPIEvent) {
	switch evt.Type {
	case slackevents.CallbackEvent:
		b.handleCallbackEvent(ctx, evt.InnerEvent)
	}
}

func (b *Bot) handleCallbackEvent(ctx context.Context, inner slackevents.EventsAPIInnerEvent) {
	switch ev := inner.Data.(type) {
	case *slackevents.MessageEvent:
		b.handleMessageEvent(ctx, ev)
	case *slackevents.AppMentionEvent:
		b.handleAppMentionEvent(ctx, ev)
	}
}

func (b *Bot) handleMessageEvent(ctx context.Context, ev *slackevents.MessageEvent) {
	// Ignore bot's own messages
	if ev.User == b.botUserID || ev.User == "" {
		return
	}
	// Ignore edits, deletes, and other meta subtypes — but allow file_share.
	if ev.SubType != "" && ev.SubType != "file_share" {
		b.logger.Debug("slack message ignored", "subType", ev.SubType, "user", ev.User)
		return
	}

	// Extract files from the message (populated by UnmarshalJSON into ev.Message).
	text := ev.Text
	if ev.Message != nil && len(ev.Message.Files) > 0 {
		b.logger.Debug("slack files attached", "count", len(ev.Message.Files))
		downloaded, errs := b.downloadSlackFiles(ctx, ev.Message.Files)
		text = appendFileInfo(text, downloaded, errs)
	}

	// Direct messages
	if ev.ChannelType == "im" {
		if !b.config.ReactDM() {
			return
		}
		b.processIncoming(ctx, ev.Channel, ev.ThreadTimeStamp, ev.TimeStamp, text, ev.User)
		return
	}

	// Channel thread replies: respond without mention if this is a thread
	// we've previously participated in (history exists), the last message
	// in history was from us, and the new message doesn't mention someone else.
	if b.config.ReactThread() && ev.ThreadTimeStamp != "" && b.shouldAutoReply(ev.Channel, ev.ThreadTimeStamp, ev.Text) {
		b.processIncoming(ctx, ev.Channel, ev.ThreadTimeStamp, ev.TimeStamp, text, ev.User)
	}
}

// shouldAutoReply checks whether the bot should respond to a thread message
// without being explicitly mentioned. Returns true when:
//  1. The thread has existing conversation history (bot was mentioned before)
//  2. The last message in that history was from the bot (direct follow-up)
//  3. The message does not mention another user (not directed at someone else)
func (b *Bot) shouldAutoReply(channelID, threadTS, rawText string) bool {
	if b.agentDataDir == "" {
		return false
	}

	// 1. Check history exists for this thread
	path := chathistory.HistoryFilePath(b.agentDataDir, platformSlack, channelID, threadTS)
	if !chathistory.HasHistory(path) {
		return false
	}

	// 2. Last message in history must be from the bot
	last := chathistory.LastMessage(path)
	if last == nil || !last.IsBot || last.UserID != b.botUserID {
		return false
	}

	// 3. Message must not mention another user (bot's own mention is OK)
	mentions := reUserMention.FindAllStringSubmatch(rawText, -1)
	for _, m := range mentions {
		if len(m) > 1 && m[1] != b.botUserID {
			return false // mentions someone other than the bot
		}
	}

	return true
}

func (b *Bot) handleAppMentionEvent(ctx context.Context, ev *slackevents.AppMentionEvent) {
	if !b.config.ReactMention() {
		return
	}
	// Ignore our own messages
	if ev.User == b.botUserID || ev.User == "" {
		return
	}
	// Strip the bot mention from the message
	text := StripBotMention(ev.Text, b.botUserID)

	// Download attached files (same as DM handling in handleMessageEvent)
	if len(ev.Files) > 0 {
		b.logger.Debug("slack files attached to mention", "count", len(ev.Files))
		downloaded, errs := b.downloadSlackFiles(ctx, ev.Files)
		text = appendFileInfo(text, downloaded, errs)
	}

	b.processIncoming(ctx, ev.Channel, ev.ThreadTimeStamp, ev.TimeStamp, text, ev.User)
}

func (b *Bot) processIncoming(ctx context.Context, channel, threadTS, messageTS, text, userID string) {
	if strings.TrimSpace(text) == "" {
		return
	}

	// Convert Slack formatting to plain text, resolving user mentions to display names
	text = SlackToPlain(text, b.resolveUserName)

	// Resolve user display name
	displayName := b.resolveUserName(userID)

	// Determine reply thread: use existing thread or start new one
	replyTS := threadTS
	if replyTS == "" && b.config.ThreadReplies {
		replyTS = messageTS // reply in a thread starting from this message
	}

	select {
	case b.sem <- struct{}{}:
		go func() {
			defer func() { <-b.sem }()
			b.sendToAgent(ctx, channel, threadTS, replyTS, messageTS, text, displayName, userID)
		}()
	default:
		b.logger.Warn("too many concurrent chats, dropping message", "channel", channel)
		b.postMessage(ctx, channel, replyTS, "I'm currently handling too many conversations. Please try again shortly.")
	}
}

// streamAppendInterval is the minimum interval between AppendStream calls.
const streamAppendInterval = 1 * time.Second

func (b *Bot) sendToAgent(ctx context.Context, channel, origThreadTS, replyTS, messageTS, text, displayName, userID string) {
	// Serialize processing within the same thread to maintain history
	// consistency. The lock must cover both history fetching and prompt
	// construction so that concurrent messages to the same thread observe
	// each other's updates rather than building prompts from stale history.
	tl := b.acquireThreadLock(channel, replyTS)
	tl.mu.Lock()
	defer func() {
		tl.mu.Unlock()
		b.releaseThreadLock(channel, replyTS, tl)
	}()

	// When creating a new thread (origThreadTS was empty), save the user's
	// message to the thread history file so it appears as the first entry.
	if origThreadTS == "" && replyTS != "" && b.agentDataDir != "" {
		userMsg := chathistory.HistoryMessage{
			Platform:  platformSlack,
			ChannelID: channel,
			ThreadID:  replyTS,
			MessageID: messageTS,
			UserID:    userID,
			UserName:  displayName,
			Text:      text,
			Timestamp: time.Now().Format(time.RFC3339),
			IsBot:     false,
		}
		path := chathistory.HistoryFilePath(b.agentDataDir, platformSlack, channel, replyTS)
		if err := chathistory.WriteMessages(path, []chathistory.HistoryMessage{userMsg}); err != nil {
			b.logger.Warn("failed to save initial user message to thread history", "err", err)
		}
	}

	// Fetch conversation history from Slack for context injection.
	var history []chathistory.HistoryMessage
	if origThreadTS != "" {
		history = FetchThreadHistory(ctx, b.api, b.agentDataDir, channel, origThreadTS, b.resolveUserName, b.logger)
	} else {
		history = FetchChannelHistory(ctx, b.api, b.agentDataDir, channel, channelHistoryLimit, b.resolveUserName, b.logger)
	}

	// Build enriched message with conversation history.
	var sb strings.Builder
	if len(history) > 0 {
		sb.WriteString(chathistory.FormatForInjection(history, b.botUserID, chathistory.DefaultMaxMessages, chathistory.DefaultMaxChars))
		sb.WriteString("\n---\n\n")
	}
	sb.WriteString(fmt.Sprintf("[Slack @%s] %s", displayName, text))
	message := sb.String()

	// From here on, the thread handle used for posting/streaming.
	threadTS := replyTS

	// Show typing indicator (best-effort; requires Agents & Assistants + assistant:write scope)
	_ = b.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
		ChannelID: channel,
		ThreadTS:  threadTS,
		Status:    typingStatus,
	})

	events, err := b.mgr.ChatOneShot(ctx, b.agentID, message)
	if err != nil {
		b.clearAssistantStatus(ctx, channel, threadTS)
		b.logger.Warn("failed to start agent chat from slack", "err", err)
		b.postMessage(ctx, channel, threadTS, "Sorry, I couldn't process your message right now. Please try again later.")
		return
	}

	var response strings.Builder    // full response text
	var pendingDelta strings.Builder // text not yet sent via AppendStream
	var streamTS string              // ts of the streaming message (empty = not started or fallback)
	var lastAppend time.Time
	hasError := false
	streamFailed := false // true if StartStream failed, use fallback
	filter := &agent.ReplyTagFilter{}

	for evt := range events {
		switch evt.Type {
		case "text":
			// Only forward text inside <reply>...</reply> tags.
			delta := filter.Feed(evt.Delta)
			if delta == "" {
				continue
			}
			response.WriteString(delta)
			pendingDelta.WriteString(delta)

			// Start stream on first text event
			if streamTS == "" && !streamFailed {
				opts := []slack.MsgOption{}
				if threadTS != "" {
					opts = append(opts, slack.MsgOptionTS(threadTS))
				}
				_, ts, err := b.api.StartStreamContext(ctx, channel, opts...)
				if err != nil {
					b.logger.Warn("failed to start slack stream, falling back to batch post", "err", err)
					streamFailed = true
					continue
				}
				streamTS = ts
				lastAppend = time.Now()
			}

			// Throttled append
			if streamTS != "" && pendingDelta.Len() > 0 && time.Since(lastAppend) >= streamAppendInterval {
				b.appendStream(ctx, channel, streamTS, pendingDelta.String())
				pendingDelta.Reset()
				lastAppend = time.Now()
			}

		case "error":
			hasError = true
			b.logger.Warn("agent returned error during slack chat", "err", evt.ErrorMessage)
		}
	}

	// Flush any remaining buffered reply content.
	if remaining := filter.Flush(); remaining != "" {
		response.WriteString(remaining)
		pendingDelta.WriteString(remaining)
	}

	// Use a separate context for finalization so that cleanup API calls
	// (StopStream, UpdateMessage, etc.) complete even if the Bot's context
	// was cancelled (e.g. during shutdown or reconfiguration).
	finCtx, finCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finCancel()

	// Flush remaining delta
	if streamTS != "" && pendingDelta.Len() > 0 {
		b.appendStream(finCtx, channel, streamTS, pendingDelta.String())
	}

	// Finalize
	if streamTS != "" {
		// Stop stream (no text — just finalize the typing indicator)
		if _, _, err := b.api.StopStreamContext(finCtx, channel, streamTS); err != nil {
			b.logger.Warn("failed to stop slack stream", "err", err)
		}

		// Replace stream content with the full response via chat.update.
		// This ensures complete text even if AppendStream calls were lost
		// due to rate limiting, stream timeout, or transient errors.
		if response.Len() > 0 {
			text := PlainToSlack(response.String())
			chunks := SplitMessage(text, slackMaxMsgLen)
			// First chunk: update the streaming message in-place
			updateOpts := []slack.MsgOption{slack.MsgOptionText(chunks[0], false)}
			if threadTS != "" {
				updateOpts = append(updateOpts, slack.MsgOptionTS(threadTS))
			}
			if _, _, _, err := b.api.UpdateMessageContext(finCtx, channel, streamTS, updateOpts...); err != nil {
				b.logger.Warn("failed to update stream message with final text", "err", err)
			}
			// Remaining chunks: post as follow-up messages
			for _, chunk := range chunks[1:] {
				b.postMessage(finCtx, channel, threadTS, chunk)
			}
		}
	} else if response.Len() > 0 {
		// Fallback: traditional batch post (StartStream failed or no streaming support)
		text := PlainToSlack(response.String())
		chunks := SplitMessage(text, slackMaxMsgLen)
		for _, chunk := range chunks {
			b.postMessage(finCtx, channel, threadTS, chunk)
		}
	} else if hasError {
		b.postMessage(finCtx, channel, threadTS, "Sorry, something went wrong while processing your request.")
	}

	// Clear typing indicator (auto-clears on message post, but explicit clear as safety net)
	b.clearAssistantStatus(finCtx, channel, threadTS)

	// Save bot response to thread history so shouldAutoReply can detect
	// that the last message was from the bot on subsequent thread messages.
	if response.Len() > 0 && threadTS != "" && b.agentDataDir != "" {
		botMsg := chathistory.HistoryMessage{
			Platform:  platformSlack,
			ChannelID: channel,
			ThreadID:  threadTS,
			MessageID: fmt.Sprintf("%d.bot", time.Now().Unix()),
			UserID:    b.botUserID,
			UserName:  "assistant",
			Text:      response.String(),
			Timestamp: time.Now().Format(time.RFC3339),
			IsBot:     true,
		}
		path := chathistory.HistoryFilePath(b.agentDataDir, platformSlack, channel, threadTS)
		if err := chathistory.AppendMessages(path, []chathistory.HistoryMessage{botMsg}); err != nil {
			b.logger.Warn("failed to save bot response to thread history", "err", err)
		}
	}

}

// appendStream appends text to a streaming Slack message with rate limit retry.
func (b *Bot) appendStream(ctx context.Context, channel, streamTS, text string) {
	for attempt := 0; attempt <= maxRateLimitRetry; attempt++ {
		_, _, err := b.api.AppendStreamContext(ctx, channel, streamTS, slack.MsgOptionMarkdownText(text))
		if err == nil {
			return
		}
		var rlErr *slack.RateLimitedError
		if errors.As(err, &rlErr) {
			delay := rlErr.RetryAfter
			if delay == 0 {
				delay = time.Duration(attempt+1) * time.Second
			}
			select {
			case <-time.After(delay):
				continue
			case <-ctx.Done():
				return
			}
		}
		b.logger.Debug("append stream failed", "err", err)
		return
	}
}

// clearAssistantStatus clears the assistant typing indicator (best-effort).
func (b *Bot) clearAssistantStatus(ctx context.Context, channel, threadTS string) {
	_ = b.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
		ChannelID: channel,
		ThreadTS:  threadTS,
		Status:    "", // empty = clear
	})
}

func (b *Bot) postMessage(ctx context.Context, channel, threadTS, text string) {
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}

	for attempt := 0; attempt <= maxRateLimitRetry; attempt++ {
		_, _, err := b.api.PostMessageContext(ctx, channel, opts...)
		if err == nil {
			return
		}
		var rlErr *slack.RateLimitedError
		if errors.As(err, &rlErr) {
			wait := rlErr.RetryAfter
			if wait <= 0 {
				wait = time.Duration(attempt+1) * time.Second
			}
			b.logger.Debug("slack rate limited, waiting", "retryAfter", wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
				continue
			}
		}
		b.logger.Warn("failed to post slack message", "channel", channel, "err", err)
		return
	}
	b.logger.Warn("failed to post slack message after rate limit retries", "channel", channel)
}

// threadLock is a reference-counted mutex for serializing per-thread processing.
// The map entry is only removed when the last holder releases it, preventing a
// race where a new mutex is created while another goroutine is still waiting on
// the previous one.
type threadLock struct {
	mu      sync.Mutex
	waiters int
}

// acquireThreadLock returns the threadLock for the given channel+thread,
// creating one if needed, and increments its reference count.
// Must be paired with releaseThreadLock after tl.mu.Unlock().
func (b *Bot) acquireThreadLock(channel, threadTS string) *threadLock {
	key := channel + ":" + threadTS
	b.threadLocksMu.Lock()
	defer b.threadLocksMu.Unlock()
	tl, ok := b.threadLocks[key]
	if !ok {
		tl = &threadLock{}
		b.threadLocks[key] = tl
	}
	tl.waiters++
	return tl
}

// releaseThreadLock decrements the reference count and removes the map entry
// when no goroutines are waiting or holding the lock.
func (b *Bot) releaseThreadLock(channel, threadTS string, tl *threadLock) {
	key := channel + ":" + threadTS
	b.threadLocksMu.Lock()
	defer b.threadLocksMu.Unlock()
	tl.waiters--
	if tl.waiters == 0 {
		delete(b.threadLocks, key)
	}
}

// resolveUserName resolves a Slack user ID to a display name, with caching.
func (b *Bot) resolveUserName(userID string) string {
	b.userCacheMu.RLock()
	if name, ok := b.userCache[userID]; ok {
		b.userCacheMu.RUnlock()
		return name
	}
	b.userCacheMu.RUnlock()

	user, err := b.api.GetUserInfo(userID)
	if err != nil {
		b.logger.Debug("failed to resolve slack user", "userID", userID, "err", err)
		return userID // fallback to raw ID
	}

	name := user.Profile.DisplayName
	if name == "" {
		name = user.RealName
	}
	if name == "" {
		name = user.Name
	}

	b.userCacheMu.Lock()
	b.userCache[userID] = name
	b.userCacheMu.Unlock()

	return name
}
