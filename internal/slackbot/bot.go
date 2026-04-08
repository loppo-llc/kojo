package slackbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/chathistory"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// ChatEvent represents a streaming event from the agent chat backend.
// This mirrors agent.ChatEvent but is defined here to avoid a circular import
// (agent imports slackbot indirectly via manager → hub).
// The adapter in agent/manager wires the concrete types together.
type ChatEvent struct {
	Type         string
	Delta        string
	ErrorMessage string
}

// ChatManager is the interface the bot uses to interact with agents.
type ChatManager interface {
	ChatForSlack(ctx context.Context, agentID, message, role string) (<-chan ChatEvent, error)
	ChatForSlackOneShot(ctx context.Context, agentID, message, role string) (<-chan ChatEvent, error)
	IsBusy(agentID string) bool
}

// Bot manages a single Slack Socket Mode connection for one agent.
type Bot struct {
	agentID      string
	agentDataDir string // agent data directory for history file storage
	config       Config
	api          *slack.Client
	sm           *socketmode.Client
	mgr          ChatManager
	logger       *slog.Logger
	botUserID    string

	cancel context.CancelFunc
	done   chan struct{}

	// pending tracks messages received while agent is busy
	pendingMu sync.Mutex
	pending   []pendingMsg

	// userCache caches Slack user ID → display name
	userCacheMu sync.RWMutex
	userCache   map[string]string
}

type pendingMsg struct {
	channel   string
	threadTS  string
	text      string
	userID    string
	retries   int
	messageTS string // for removing reaction
}

const (
	maxPendingRetries = 3
	maxPendingQueue   = 20
	pendingRetryDelay = 5 * time.Second
	slackMaxMsgLen    = 3000
	maxRateLimitRetry = 3
	userCacheTTL      = 10 * time.Minute
)

// NewBot creates a new Bot instance. Call Run() to start it.
// agentDataDir is the agent's data directory used for storing conversation history files.
func NewBot(agentID string, agentDataDir string, cfg Config, appToken, botToken string, mgr ChatManager, logger *slog.Logger) *Bot {
	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	sm := socketmode.New(api, socketmode.OptionLog(slog.NewLogLogger(logger.Handler(), slog.LevelWarn)))

	return &Bot{
		agentID:      agentID,
		agentDataDir: agentDataDir,
		config:       cfg,
		api:          api,
		sm:           sm,
		mgr:          mgr,
		logger:       logger.With("component", "slackbot", "agent", agentID),
		done:         make(chan struct{}),
		userCache:    make(map[string]string),
	}
}

// Run starts the Socket Mode event loop. It blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) {
	defer close(b.done)

	ctx, b.cancel = context.WithCancel(ctx)

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
	if b.cancel != nil {
		b.cancel()
	}
	<-b.done
}

// Done returns a channel that is closed when the bot exits.
func (b *Bot) Done() <-chan struct{} {
	return b.done
}

// TestConnection performs auth.test to validate the tokens.
func TestConnection(appToken, botToken string) (team, botUser string, err error) {
	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	resp, err := api.AuthTest()
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
	// Ignore bot's own messages and message edits/deletes
	if ev.User == b.botUserID || ev.User == "" || ev.SubType != "" {
		return
	}

	// Direct messages
	if ev.ChannelType == "im" {
		if !b.config.ReactDM() {
			return
		}
		b.processIncoming(ctx, ev.Channel, ev.ThreadTimeStamp, ev.TimeStamp, ev.Text, ev.User)
		return
	}

	// Channel thread replies: respond without mention if this is a thread
	// we've previously participated in (history exists), the last message
	// in history was from us, and the new message doesn't mention someone else.
	if b.config.ReactThread() && ev.ThreadTimeStamp != "" && b.shouldAutoReply(ev.Channel, ev.ThreadTimeStamp, ev.Text) {
		b.processIncoming(ctx, ev.Channel, ev.ThreadTimeStamp, ev.TimeStamp, ev.Text, ev.User)
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
	path := chathistory.HistoryFilePath(b.agentDataDir, "slack", channelID, threadTS)
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

	// Fetch conversation history from Slack for context injection
	var history []chathistory.HistoryMessage
	if threadTS != "" {
		history = FetchThreadHistory(ctx, b.api, b.agentDataDir, channel, threadTS, b.resolveUserName, b.logger)
	} else {
		history = FetchChannelHistory(ctx, b.api, b.agentDataDir, channel, channelHistoryLimit, b.resolveUserName, b.logger)
	}

	// Build enriched message with conversation history
	var sb strings.Builder
	if len(history) > 0 {
		sb.WriteString(chathistory.FormatForInjection(history, b.botUserID, chathistory.DefaultMaxMessages, chathistory.DefaultMaxChars))
		sb.WriteString("\n---\n\n")
	}
	sb.WriteString(fmt.Sprintf("[Slack @%s] %s", displayName, text))
	formattedMsg := sb.String()

	// Determine reply thread: use existing thread or start new one
	replyTS := threadTS
	if replyTS == "" && b.config.ThreadReplies {
		replyTS = messageTS // reply in a thread starting from this message
	}

	// When creating a new thread (threadTS was empty), save the user's message
	// to the thread history file so it appears as the first entry.
	if threadTS == "" && replyTS != "" && b.agentDataDir != "" {
		userMsg := chathistory.HistoryMessage{
			Platform:  "slack",
			ChannelID: channel,
			ThreadID:  replyTS,
			MessageID: messageTS,
			UserID:    userID,
			UserName:  displayName,
			Text:      text,
			Timestamp: time.Now().Format(time.RFC3339),
			IsBot:     false,
		}
		path := chathistory.HistoryFilePath(b.agentDataDir, "slack", channel, replyTS)
		if err := chathistory.WriteMessages(path, []chathistory.HistoryMessage{userMsg}); err != nil {
			b.logger.Warn("failed to save initial user message to thread history", "err", err)
		}
	}

	if b.mgr.IsBusy(b.agentID) {
		// Check queue limit
		b.pendingMu.Lock()
		if len(b.pending) >= maxPendingQueue {
			b.pendingMu.Unlock()
			b.logger.Warn("slack pending queue full, dropping message", "user", userID)
			b.postMessage(ctx, channel, replyTS, "Sorry, too many messages are queued. Please try again later.")
			return
		}
		// Add hourglass reaction and queue
		_ = b.api.AddReactionContext(ctx, "hourglass_flowing_sand", slack.ItemRef{
			Channel:   channel,
			Timestamp: messageTS,
		})
		b.pending = append(b.pending, pendingMsg{
			channel:   channel,
			threadTS:  replyTS,
			text:      formattedMsg,
			userID:    userID,
			messageTS: messageTS,
		})
		b.pendingMu.Unlock()
		b.logger.Debug("agent busy, queued slack message", "user", userID)
		return
	}

	go b.sendToAgent(ctx, channel, replyTS, messageTS, formattedMsg)
}

// streamAppendInterval is the minimum interval between AppendStream calls.
const streamAppendInterval = 1 * time.Second

func (b *Bot) sendToAgent(ctx context.Context, channel, threadTS, messageTS, message string) {
	// Show typing indicator (best-effort; requires Agents & Assistants + assistant:write scope)
	_ = b.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
		ChannelID: channel,
		ThreadTS:  threadTS,
		Status:    "考え中…",
	})

	events, err := b.mgr.ChatForSlackOneShot(ctx, b.agentID, message, "user")
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

	for evt := range events {
		switch evt.Type {
		case "text":
			response.WriteString(evt.Delta)
			pendingDelta.WriteString(evt.Delta)

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

	// Flush remaining delta
	if streamTS != "" && pendingDelta.Len() > 0 {
		b.appendStream(ctx, channel, streamTS, pendingDelta.String())
	}

	// Finalize
	if streamTS != "" {
		// Stop stream → message becomes permanent, typing indicator disappears.
		// Send final full text with StopStream for reliability (in case some
		// AppendStream calls were lost due to rate limiting or transient errors).
		stopOpts := []slack.MsgOption{}
		if response.Len() > 0 {
			stopOpts = append(stopOpts, slack.MsgOptionMarkdownText(PlainToSlack(response.String())))
		}
		if _, _, err := b.api.StopStreamContext(ctx, channel, streamTS, stopOpts...); err != nil {
			b.logger.Warn("failed to stop slack stream", "err", err)
		}
	} else if response.Len() > 0 {
		// Fallback: traditional batch post (StartStream failed or no streaming support)
		text := PlainToSlack(response.String())
		chunks := SplitMessage(text, slackMaxMsgLen)
		for _, chunk := range chunks {
			b.postMessage(ctx, channel, threadTS, chunk)
		}
	} else if hasError {
		b.postMessage(ctx, channel, threadTS, "Sorry, something went wrong while processing your request.")
	}

	// Clear typing indicator (auto-clears on message post, but explicit clear as safety net)
	b.clearAssistantStatus(ctx, channel, threadTS)

	// Save bot response to thread history so shouldAutoReply can detect
	// that the last message was from the bot on subsequent thread messages.
	if response.Len() > 0 && threadTS != "" && b.agentDataDir != "" {
		botMsg := chathistory.HistoryMessage{
			Platform:  "slack",
			ChannelID: channel,
			ThreadID:  threadTS,
			MessageID: fmt.Sprintf("%d.bot", time.Now().Unix()),
			UserID:    b.botUserID,
			UserName:  "assistant",
			Text:      response.String(),
			Timestamp: time.Now().Format(time.RFC3339),
			IsBot:     true,
		}
		path := chathistory.HistoryFilePath(b.agentDataDir, "slack", channel, threadTS)
		if err := chathistory.AppendMessages(path, []chathistory.HistoryMessage{botMsg}); err != nil {
			b.logger.Warn("failed to save bot response to thread history", "err", err)
		}
	}

	// Remove hourglass if this was from a pending message
	if messageTS != "" {
		_ = b.api.RemoveReactionContext(ctx, "hourglass_flowing_sand", slack.ItemRef{
			Channel:   channel,
			Timestamp: messageTS,
		})
	}

	// Process any pending messages
	b.processPending(ctx)
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

	for attempt := range maxRateLimitRetry {
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

func (b *Bot) processPending(ctx context.Context) {
	b.pendingMu.Lock()
	if len(b.pending) == 0 {
		b.pendingMu.Unlock()
		return
	}
	// Take the first pending message
	msg := b.pending[0]
	b.pending = b.pending[1:]
	b.pendingMu.Unlock()

	if b.mgr.IsBusy(b.agentID) {
		msg.retries++
		if msg.retries >= maxPendingRetries {
			b.logger.Warn("slack pending message dropped after max retries", "user", msg.userID)
			b.postMessage(ctx, msg.channel, msg.threadTS, "Sorry, I'm currently busy. Please try again later.")
			// Remove hourglass reaction
			_ = b.api.RemoveReactionContext(ctx, "hourglass_flowing_sand", slack.ItemRef{
				Channel:   msg.channel,
				Timestamp: msg.messageTS,
			})
			// Continue processing remaining pending messages
			b.processPending(ctx)
			return
		}
		// Re-queue and schedule a retry
		b.pendingMu.Lock()
		b.pending = append([]pendingMsg{msg}, b.pending...)
		b.pendingMu.Unlock()

		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pendingRetryDelay):
				b.processPending(ctx)
			}
		}()
		return
	}

	b.sendToAgent(ctx, msg.channel, msg.threadTS, msg.messageTS, msg.text)
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

	// Expire cache entry after TTL
	go func() {
		time.Sleep(userCacheTTL)
		b.userCacheMu.Lock()
		delete(b.userCache, userID)
		b.userCacheMu.Unlock()
	}()

	return name
}
