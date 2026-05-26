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
	Chat(ctx context.Context, agentID, message, role string, attachments []agent.MessageAttachment, source ...agent.BusySource) (<-chan agent.ChatEvent, error)
	ChatOneShot(ctx context.Context, agentID, message string, opts ...agent.OneShotOpts) (<-chan agent.ChatEvent, error)
	// CanResumeSession reports whether the next ChatOneShot for this
	// (agentID, sessionKey) pair is likely to resume an existing
	// backend session. True when the backend honors SessionKey AND
	// the on-disk session artifact exists AND is non-empty. The
	// Slack bot uses this to gate the "skip FormatForInjection(history)"
	// optimization: when false (backend runs OneShot, or the session
	// file was removed/empty), history must be re-injected because
	// the backend will see no prior context. See Manager.CanResumeSession
	// for the threshold-reset edge case where this returns true but
	// the backend ultimately starts a fresh session.
	CanResumeSession(agentID, sessionKey string) bool
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

	// rateLimitSleep is overridable in tests to bypass real wall-clock
	// waits in the postMessage / appendStream rate-limit backoff. nil
	// means use time.After (production default).
	rateLimitSleep func(d time.Duration) <-chan time.Time
}

const (
	slackMaxMsgLen    = 3000
	maxRateLimitRetry = 3

	// chunkPostTimeoutBase / chunkPostTimeoutPerChunk / chunkPostTimeoutMax
	// shape the finalize-time chunk-posting budget. The per-chunk slice
	// (7s) covers postMessage's worst-case rate-limit retry chain
	// (1+2+3 = 6s) plus headroom for the actual HTTP round-trip. The
	// max cap prevents huge replies from holding a context (and the
	// finalize goroutine) open for several minutes.
	chunkPostTimeoutBase     = 10 * time.Second
	chunkPostTimeoutPerChunk = 7 * time.Second
	chunkPostTimeoutMax      = 90 * time.Second

	// finalizeShortTimeout is the budget for short, single-call finalize
	// operations (StopStream, chat.update, clearAssistantStatus, and the
	// delivery-failure notice). Long-running chunk posting uses
	// chunkPostTimeout* above instead.
	finalizeShortTimeout = 5 * time.Second

	// deliveryFailureNotice is shown when one or more reply chunks fail to
	// reach Slack (chunkPostTimeout expiry, Slack API error, context cancel,
	// etc.). The wording deliberately avoids implying a specific cause —
	// "too long" would be misleading when a transient API/network error or
	// rate-limit storm is to blame. Defined as a constant so the streamed
	// finalize path and the batch-fallback path stay in lockstep.
	deliveryFailureNotice = "_⚠️ The full response could not be delivered to Slack. Check kojo logs for details._"

	// maxConcurrentChats is the maximum number of concurrent sendToAgent
	// goroutines per Bot (i.e. per agent). This prevents resource exhaustion
	// when many Slack threads send messages simultaneously.
	maxConcurrentChats = 10

	// platformSlack is the platform identifier used in chat history entries.
	platformSlack = "slack"

	// typingStatus is the assistant status text shown while processing a message.
	typingStatus = "Thinking…"

	// slackSystemPrompt is appended to the system prompt when the message
	// originates from Slack. It tells the agent how Slack replies work so it
	// responds naturally instead of trying to use MCP tools to reply.
	slackSystemPrompt = `## Slack Conversation

This message was received via Slack. Your text response will be automatically posted to the Slack thread — just respond normally. Do NOT use Slack MCP tools (slack_post_message, slack_reply_to_thread, etc.) to reply to this conversation.

Slack MCP tools are still available for other actions: posting to a different channel, adding reactions, uploading files, listing channels/users, etc.`
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

	// Build a session key that maps 1:1 to the chat_history file unit
	// (per-thread or per-channel). This gives each Slack conversation its
	// own resumable session with full context across messages. Computed
	// here (rather than inline at the ChatOneShot call site) because the
	// inject-history decision below needs to know whether the backend's
	// session artifact for this conversation actually exists on disk.
	slackSessionKey := b.agentID + ":slack:" + channel + ":" + replyTS

	// Decide whether to inject history into the user message.
	//
	// When the backend supports SessionKey-based resumption (Claude), the
	// first turn seeds the session with FormatForInjection(history); the
	// backend then carries that context internally on every subsequent
	// resume. Re-injecting on later turns duplicates every prior Slack
	// message — once in the resumed transcript, once in the new user
	// payload — burning context and risking confusion. So skip injection
	// once both signals agree:
	//
	//   (a) the chat_history records a prior bot reply (we already had a
	//       turn in this conversation), AND
	//   (b) the backend's session artifact for this conversation still
	//       exists on disk (claude /clear, upgrade or manual cleanup
	//       can remove it independently of Slack-side history, so signal
	//       (a) alone is not safe).
	//
	// For backends that do not support resume (codex, gemini, …) the
	// ChatOneShot call falls back to OneShot:true, which carries no prior
	// context across turns. Those backends must keep receiving injected
	// history on every turn or they lose the conversation entirely.
	//
	// Remaining tradeoff: user messages that landed in the Slack thread
	// between the last bot reply and this turn are not delta-injected;
	// Claude sees them only as referenced text in the new user message.
	// We accept this to avoid duplicating the full transcript.
	injectHistory := true
	if len(history) > 0 && b.mgr.CanResumeSession(b.agentID, slackSessionKey) {
		// Match our own bot replies only. Two reliable signals are OR'd:
		//
		//   (1) UserID == b.botUserID — set by every AppendMessages write
		//       below, and also by Slack's API for modern apps that expose
		//       User on bot-posted messages.
		//   (2) MessageID has a ".bot" suffix — the local sentinel that
		//       AppendMessages assigns (see "%d.bot" formatting in the
		//       bot-history append below). This catches replies that Slack
		//       returns with an empty User and only BotID set, where (1)
		//       alone would miss them.
		//
		// We deliberately do NOT match on IsBot alone, because unrelated
		// bot posts in the same channel (GitHub, Datadog, …) would falsely
		// suppress injection on the very first turn and start the resumed
		// Claude session with no Slack context.
		for i := range history {
			if !history[i].IsBot {
				continue
			}
			if history[i].UserID == b.botUserID || strings.HasSuffix(history[i].MessageID, ".bot") {
				injectHistory = false
				break
			}
		}
	}

	// Build enriched message with conversation history.
	var sb strings.Builder
	if injectHistory && len(history) > 0 {
		sb.WriteString(chathistory.FormatForInjection(history, b.botUserID, chathistory.DefaultMaxMessages, chathistory.DefaultMaxChars))
		sb.WriteString("\n---\n\n")
	}
	if replyTS != "" {
		sb.WriteString(fmt.Sprintf("[Slack @%s | channel:%s thread:%s] %s", displayName, channel, replyTS, text))
	} else {
		sb.WriteString(fmt.Sprintf("[Slack @%s | channel:%s] %s", displayName, channel, text))
	}
	message := sb.String()

	// From here on, the thread handle used for posting/streaming.
	threadTS := replyTS

	// Show typing indicator (best-effort; requires Agents & Assistants + assistant:write scope)
	_ = b.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
		ChannelID: channel,
		ThreadTS:  threadTS,
		Status:    typingStatus,
	})

	// slackSessionKey was computed above (1:1 with the chat_history unit)
	// so the inject-history decision could check for the backend session
	// artifact on disk. threadTS is replyTS so the key is identical.
	events, err := b.mgr.ChatOneShot(ctx, b.agentID, message, agent.OneShotOpts{
		SessionKey:        slackSessionKey,
		SystemPromptExtra: slackSystemPrompt,
	})
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

	// startStream initializes the Slack stream if not already started.
	// Returns true if the stream is active (either already started or just created).
	startStream := func() bool {
		if streamTS != "" {
			return true
		}
		if streamFailed {
			return false
		}
		opts := []slack.MsgOption{}
		if threadTS != "" {
			opts = append(opts, slack.MsgOptionTS(threadTS))
		}
		_, ts, err := b.api.StartStreamContext(ctx, channel, opts...)
		if err != nil {
			b.logger.Warn("failed to start slack stream, falling back to batch post", "err", err)
			streamFailed = true
			return false
		}
		streamTS = ts
		lastAppend = time.Now()
		return true
	}

	for evt := range events {
		switch evt.Type {
		case "text":
			response.WriteString(evt.Delta)
			pendingDelta.WriteString(evt.Delta)

			// Start stream on first text event
			if !startStream() {
				continue
			}

			// Throttled append
			if pendingDelta.Len() > 0 && time.Since(lastAppend) >= streamAppendInterval {
				b.appendStream(ctx, channel, streamTS, pendingDelta.String())
				pendingDelta.Reset()
				lastAppend = time.Now()
			}

		case "tool_use":
			// Update assistant status to show which tool is running
			status := toolStatusText(evt.ToolName)
			_ = b.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
				ChannelID: channel,
				ThreadTS:  threadTS,
				Status:    status,
			})

			// Append tool status indicator to the stream so the user can
			// see progress even during long tool executions. The final
			// UpdateMessage replaces the stream with clean response text,
			// so these ephemeral indicators are automatically removed.
			//
			// Note: status indicators bypass the streamAppendInterval throttle
			// because tool_use events fire at most once per tool invocation
			// (not in a tight loop like text deltas) and a user who sees no
			// updates during a long-running tool has no way to tell the agent
			// is still working. If the very first event is tool_use,
			// throttling would suppress the indicator until any subsequent
			// text — which may never come if the tool takes minutes.
			if startStream() {
				// Flush any pending text delta first so the status appears after
				// whatever the assistant has said so far.
				if pendingDelta.Len() > 0 {
					b.appendStream(ctx, channel, streamTS, pendingDelta.String())
					pendingDelta.Reset()
				}
				b.appendStream(ctx, channel, streamTS, "\n\n_⏳ "+status+"_")
				lastAppend = time.Now()
			}

		case "tool_result":
			// Revert assistant status to "Thinking…" while the agent
			// processes the tool result and decides the next action.
			_ = b.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
				ChannelID: channel,
				ThreadTS:  threadTS,
				Status:    typingStatus,
			})

		case "error":
			hasError = true
			b.logger.Warn("agent returned error during slack chat", "err", evt.ErrorMessage)
		}
	}

	// Use a separate context for finalization so that cleanup API calls
	// (StopStream, UpdateMessage, etc.) complete even if the Bot's context
	// was cancelled (e.g. during shutdown or reconfiguration). This budget
	// covers the short, single-call ops only — chunk posting via
	// postMessage gets its own larger context per call site (see
	// chunkPostTimeout) so rate-limit backoff doesn't truncate the reply.
	finCtx, finCancel := context.WithTimeout(context.Background(), finalizeShortTimeout)
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
		// Use MsgOptionMarkdownText (markdown_text param) so Slack uses the
		// same full-Markdown renderer as chat.appendStream; the legacy mrkdwn
		// renderer (text param) does not support tables, headings, etc.
		//
		// IMPORTANT: send markdown_text ALONE — do NOT pair it with
		// MsgOptionText. Slack's chat.update docs only state that
		// markdown_text may be sent without text (it does not document
		// the streamed-buffer interaction directly), but empirically
		// pairing both leaves Slack rendering as "{accumulated stream
		// markdown_text} + {final body}" — i.e. the chat.update text
		// field is overwritten while the streamed markdown_text buffer
		// stays intact. Sending markdown_text alone empirically yields
		// the desired replacement (this matched the working behavior
		// observed up to 2026-05-17, before MsgOptionText was added).
		// Push notification previews lose their body text as a side
		// effect; handled outside the stream-finalize path.
		if response.Len() > 0 {
			text := response.String()
			chunks := SplitMessage(text, slackMaxMsgLen)
			updateOpts := finalizeUpdateOpts(chunks[0], threadTS)

			deliveredAll := true
			_, _, _, updateErr := b.api.UpdateMessageContext(finCtx, channel, streamTS, updateOpts...)
			if updateErr != nil {
				b.logger.Warn("failed to update stream message with final text", "err", updateErr)
			}

			// chunks[1:] (and any postMessage fallback for chunks[0]) need
			// their own context — finCtx only has finalizeShortTimeout
			// covering StopStream + chat.update + clearAssistantStatus, and
			// postMessage's rate-limit retry alone can consume 1+2+3s.
			// chunkPostTimeout scales with chunk count and caps at
			// chunkPostTimeoutMax so huge replies don't hold the goroutine
			// open for several minutes.
			//
			// Allocate AFTER UpdateMessageContext returns so a slow
			// chat.update round-trip doesn't eat into the chunk-posting
			// budget. (chat.update itself uses finCtx and has its own
			// short timeout.)
			chunkCtx, chunkCancel := context.WithTimeout(context.Background(), chunkPostTimeout(len(chunks)))
			defer chunkCancel()

			if updateErr != nil {
				// Fallback: post the first chunk as a fresh message so the
				// final reply still reaches the user. Without this, a
				// chat.update failure leaves the channel with whatever
				// partial AppendStream output happened to land, possibly
				// truncated. Symmetric with the empty-response branch below.
				//
				// If even chunks[0] fails, do NOT post the remaining
				// chunks — emitting chunks[1:] without their lead would
				// just confuse the user. Skip straight to the delivery
				// failure notice.
				if !b.postMessage(chunkCtx, channel, threadTS, chunks[0]) {
					deliveredAll = false
				}
			}
			// Remaining chunks: post as follow-up messages, but only if
			// chunks[0] reached the user. Stop on the first failure —
			// once chunkCtx is cancelled or Slack is hard rate-limiting
			// us, subsequent posts will fail the same way.
			if deliveredAll {
				for _, chunk := range chunks[1:] {
					if !b.postMessage(chunkCtx, channel, threadTS, chunk) {
						deliveredAll = false
						break
					}
				}
			}
			if !deliveredAll {
				// Surface the delivery failure to the user with a fresh
				// context — chunkCtx may already be expired at this point.
				// Best effort; if this also fails the log entries from
				// postMessage are the trail.
				noticeCtx, noticeCancel := context.WithTimeout(context.Background(), finalizeShortTimeout)
				b.postMessage(noticeCtx, channel, threadTS,
					deliveryFailureNotice)
				noticeCancel()
			}
		} else {
			// Stream was started — usually by the first tool_use event —
			// but the assistant never produced any reply text. Keep the
			// stream content (e.g. "_⏳ {tool}_" indicators) intact so
			// the user can see how far the turn got — which tool_use
			// was emitted is the most useful debugging artifact when
			// this path triggers. Surface the failure as a new message
			// (threaded when threadTS is set, top-level otherwise — same
			// behavior as the non-empty path's fallback above) instead
			// of overwriting the stream via chat.update, which would
			// erase the execution trail. The StopStream call above is
			// best-effort: if it failed the stream may briefly remain
			// live next to the error, but Slack auto-finalizes it via
			// TTL; the non-empty path treats StopStream the same way.
			// The else-if branches below cannot run because streamTS
			// != "" already matched, so this is the only place to
			// surface the failure.
			b.postMessage(finCtx, channel, threadTS,
				"Sorry, something went wrong while processing your request.")
		}
	} else if response.Len() > 0 {
		// Fallback: traditional batch post (StartStream failed or no streaming support)
		text := response.String()
		chunks := SplitMessage(text, slackMaxMsgLen)
		// Same rationale as the streamed path: chunk posting needs its
		// own context so the finCtx budget doesn't truncate large
		// replies via rate-limit backoff.
		chunkCtx, chunkCancel := context.WithTimeout(context.Background(), chunkPostTimeout(len(chunks)))
		defer chunkCancel()
		deliveredAll := true
		for _, chunk := range chunks {
			if !b.postMessage(chunkCtx, channel, threadTS, chunk) {
				deliveredAll = false
				break
			}
		}
		if !deliveredAll {
			noticeCtx, noticeCancel := context.WithTimeout(context.Background(), finalizeShortTimeout)
			b.postMessage(noticeCtx, channel, threadTS,
				deliveryFailureNotice)
			noticeCancel()
		}
	} else if hasError {
		b.postMessage(finCtx, channel, threadTS, "Sorry, something went wrong while processing your request.")
	}

	// Clear typing indicator (auto-clears on message post, but explicit clear as safety net).
	// Use a fresh short context — finCtx may already be expired if we
	// went through a long chunk-posting path above.
	clearCtx, clearCancel := context.WithTimeout(context.Background(), finalizeShortTimeout)
	b.clearAssistantStatus(clearCtx, channel, threadTS)
	clearCancel()

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

// toolStatusText returns a human-readable status string for the given tool name.
func toolStatusText(toolName string) string {
	switch toolName {
	case "Bash":
		return "Running command…"
	case "Read":
		return "Reading file…"
	case "Write":
		return "Writing file…"
	case "Edit":
		return "Editing file…"
	case "Grep":
		return "Searching code…"
	case "Glob":
		return "Finding files…"
	case "Agent":
		return "Running sub-agent…"
	case "WebFetch":
		return "Fetching web page…"
	case "WebSearch":
		return "Searching the web…"
	case "NotebookEdit":
		return "Editing notebook…"
	default:
		if toolName == "" {
			return "Working…"
		}
		return "Using " + toolName + "…"
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
			// No retries left — return without sleeping. Sleeping
			// past the final attempt has no follow-up call to wait
			// for and just delays the rest of the stream finalize
			// path. Mirrors the same guard in postMessage so both
			// retry sites stay in lockstep.
			//
			// Log on exhaustion so a sustained 429 storm leaves a
			// trail — without this, stream deltas stop appearing in
			// the channel with no log entry to correlate against
			// (non-rate-limit errors hit the Debug log below).
			if attempt == maxRateLimitRetry {
				b.logger.Warn("failed to append slack stream after rate limit retries",
					"channel", channel, "streamTS", streamTS,
					"retryAfter", rlErr.RetryAfter, "err", err)
				return
			}
			delay := rlErr.RetryAfter
			if delay == 0 {
				delay = time.Duration(attempt+1) * time.Second
			}
			sleep := b.rateLimitSleep
			if sleep == nil {
				sleep = time.After
			}
			select {
			case <-sleep(delay):
				continue
			case <-ctx.Done():
				return
			}
		}
		b.logger.Debug("append stream failed", "err", err)
		return
	}
}

// finalizeUpdateOpts returns the slack.MsgOption slice used by the
// stream-finalize chat.update call. Centralized so the wire shape
// (markdown_text alone, no MsgOptionText) is asserted from tests
// without invoking the full sendToAgent path. See the IMPORTANT
// comment in sendToAgent for why MsgOptionText must not be paired
// with MsgOptionMarkdownText on this code path.
func finalizeUpdateOpts(text, threadTS string) []slack.MsgOption {
	opts := []slack.MsgOption{
		slack.MsgOptionMarkdownText(text),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	return opts
}

// chunkPostTimeout returns the timeout budget for posting nChunks
// messages via postMessage, capped at chunkPostTimeoutMax.
func chunkPostTimeout(nChunks int) time.Duration {
	d := chunkPostTimeoutBase + chunkPostTimeoutPerChunk*time.Duration(nChunks)
	if d > chunkPostTimeoutMax {
		d = chunkPostTimeoutMax
	}
	return d
}

// clearAssistantStatus clears the assistant typing indicator (best-effort).
func (b *Bot) clearAssistantStatus(ctx context.Context, channel, threadTS string) {
	_ = b.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
		ChannelID: channel,
		ThreadTS:  threadTS,
		Status:    "", // empty = clear
	})
}

// postMessage sends a message to Slack with rate-limit retry. Returns
// true if Slack accepted the post, false if the call ultimately failed
// (rate-limit retries exhausted, context cancelled, or any non-rate-limit
// error). Failure reasons are logged at Warn. Callers in the finalize
// block use the return value to detect chunk-level delivery failures and
// surface a user-visible notice; callers that only need best-effort
// delivery may ignore it.
func (b *Bot) postMessage(ctx context.Context, channel, threadTS, text string) bool {
	// Send markdown_text alone. Pairing MsgOptionText with
	// MsgOptionMarkdownText was empirically observed (2026-05-19
	// production logs) to make Slack return markdown_text_conflict, so
	// every chat.postMessage call silently fails. This broke the
	// finalize block: stream chunks[1:] (multi-chunk replies) and the
	// delivery-failure notice fallback were both dropped, leaving the
	// channel with only chunks[0] visible. The streaming-update path already
	// went markdown_text-alone for an unrelated reason (3987158); make
	// chat.postMessage symmetric.
	//
	// Trade-off: push notification previews, link unfurls and other
	// surfaces that ignore markdown_text now show whatever fallback
	// Slack auto-generates from the blocks (typically incomplete for
	// tables/code blocks). That is strictly better than the previous
	// behavior, where the message itself never reached Slack at all.
	//
	// markdown_text is sent raw — Slack parses Markdown directly and
	// resolves mention tokens the LLM intentionally emits (<!channel>,
	// <@U…>, …) the same way as the chat.update path. Mention misuse
	// is controlled by the agent's system prompt, not by escaping at
	// this layer.
	opts := []slack.MsgOption{
		slack.MsgOptionMarkdownText(text),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}

	for attempt := 0; attempt <= maxRateLimitRetry; attempt++ {
		_, _, err := b.api.PostMessageContext(ctx, channel, opts...)
		if err == nil {
			return true
		}
		var rlErr *slack.RateLimitedError
		if errors.As(err, &rlErr) {
			// No retries left — return immediately. Sleeping here
			// would burn 1+ s of chunkPostTimeout for no subsequent
			// attempt, exceeding the documented 1+2+3 s backoff
			// chain and risking a cascade where later chunks lose
			// their budget too.
			if attempt == maxRateLimitRetry {
				// Include err and RetryAfter so production logs
				// can distinguish a Slack hard 429 from a slow
				// recovery — without these the Warn is opaque.
				b.logger.Warn("failed to post slack message after rate limit retries",
					"channel", channel, "threadTS", threadTS,
					"retryAfter", rlErr.RetryAfter, "err", err)
				return false
			}
			wait := rlErr.RetryAfter
			if wait <= 0 {
				wait = time.Duration(attempt+1) * time.Second
			}
			b.logger.Debug("slack rate limited, waiting", "retryAfter", wait)
			sleep := b.rateLimitSleep
			if sleep == nil {
				sleep = time.After
			}
			select {
			case <-ctx.Done():
				b.logger.Warn("slack post cancelled while waiting on rate limit",
					"channel", channel, "threadTS", threadTS, "err", ctx.Err())
				return false
			case <-sleep(wait):
				continue
			}
		}
		b.logger.Warn("failed to post slack message",
			"channel", channel, "threadTS", threadTS, "err", err)
		return false
	}
	// Unreachable: the rate-limited branch above returns on the final
	// attempt rather than falling through. Kept to satisfy the compiler.
	return false
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
