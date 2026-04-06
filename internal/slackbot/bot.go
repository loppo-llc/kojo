package slackbot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

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
	IsBusy(agentID string) bool
}

// Bot manages a single Slack Socket Mode connection for one agent.
type Bot struct {
	agentID   string
	config    Config
	api       *slack.Client
	sm        *socketmode.Client
	mgr       ChatManager
	logger    *slog.Logger
	botUserID string

	cancel context.CancelFunc
	done   chan struct{}

	// pending tracks messages received while agent is busy
	pendingMu sync.Mutex
	pending   []pendingMsg
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
	pendingRetryDelay = 5 * time.Second
	slackMaxMsgLen    = 3000
)

// NewBot creates a new Bot instance. Call Run() to start it.
func NewBot(agentID string, cfg Config, appToken, botToken string, mgr ChatManager, logger *slog.Logger) *Bot {
	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	sm := socketmode.New(api, socketmode.OptionLog(slog.NewLogLogger(logger.Handler(), slog.LevelWarn)))

	return &Bot{
		agentID: agentID,
		config:  cfg,
		api:     api,
		sm:      sm,
		mgr:     mgr,
		logger:  logger.With("component", "slackbot", "agent", agentID),
		done:    make(chan struct{}),
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
	// Only handle direct messages (im)
	if ev.ChannelType != "im" {
		return
	}
	// Ignore bot's own messages and message edits/deletes
	if ev.User == b.botUserID || ev.User == "" || ev.SubType != "" {
		return
	}
	b.processIncoming(ctx, ev.Channel, ev.ThreadTimeStamp, ev.TimeStamp, ev.Text, ev.User)
}

func (b *Bot) handleAppMentionEvent(ctx context.Context, ev *slackevents.AppMentionEvent) {
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

	// Convert Slack formatting to plain text
	text = SlackToPlain(text)

	// Build message with Slack context
	formattedMsg := fmt.Sprintf("[Slack @%s] %s", userID, text)

	// Determine reply thread: use existing thread or start new one
	replyTS := threadTS
	if replyTS == "" && b.config.ThreadReplies {
		replyTS = messageTS // reply in a thread starting from this message
	}

	if b.mgr.IsBusy(b.agentID) {
		// Add hourglass reaction and queue
		_ = b.api.AddReactionContext(ctx, "hourglass_flowing_sand", slack.ItemRef{
			Channel:   channel,
			Timestamp: messageTS,
		})
		b.pendingMu.Lock()
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

func (b *Bot) sendToAgent(ctx context.Context, channel, threadTS, messageTS, message string) {
	events, err := b.mgr.ChatForSlack(ctx, b.agentID, message, "user")
	if err != nil {
		b.logger.Warn("failed to start agent chat from slack", "err", err)
		b.postMessage(ctx, channel, threadTS, fmt.Sprintf("Error: %s", err.Error()))
		return
	}

	// Accumulate response
	var response strings.Builder
	for evt := range events {
		switch evt.Type {
		case "text":
			response.WriteString(evt.Delta)
		case "error":
			if evt.ErrorMessage != "" {
				response.WriteString("\n[Error: " + evt.ErrorMessage + "]")
			}
		}
	}

	if response.Len() == 0 {
		return
	}

	// Convert markdown to Slack mrkdwn and post
	text := PlainToSlack(response.String())
	chunks := SplitMessage(text, slackMaxMsgLen)
	for _, chunk := range chunks {
		b.postMessage(ctx, channel, threadTS, chunk)
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

func (b *Bot) postMessage(ctx context.Context, channel, threadTS, text string) {
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, _, err := b.api.PostMessageContext(ctx, channel, opts...)
	if err != nil {
		b.logger.Warn("failed to post slack message", "channel", channel, "err", err)
	}
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
			return
		}
		// Re-queue
		b.pendingMu.Lock()
		b.pending = append(b.pending, msg)
		b.pendingMu.Unlock()
		return
	}

	b.sendToAgent(ctx, msg.channel, msg.threadTS, msg.messageTS, msg.text)
}
