package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/slack-go/slack"

	"github.com/loppo-llc/kojo/internal/agent"
)

type contextKey int

const (
	mcpAgentIDKey contextKey = iota
	mcpReqIDKey
)

// newMCPHandler creates the MCP HTTP handler that serves Slack tools.
// A single MCPServer + StreamableHTTPServer is shared across all agents;
// the agent ID is injected into the request context via the URL path value
// and used to resolve agent-specific credentials at call time.
func newMCPHandler(agents *agent.Manager, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	srv := mcpserver.NewMCPServer("kojo-tools", "1.0.0",
		mcpserver.WithToolCapabilities(true),
	)

	// --- slack_list_channels ---
	listTool := mcp.NewTool("slack_list_channels",
		mcp.WithDescription("List Slack channels the bot can access. Returns channel ID, name, topic, and member count. By default returns only channels the bot has joined. Use member_only=false to include all visible channels (may be slow for large workspaces)."),
		mcp.WithNumber("limit", mcp.Description("Maximum number of channels to return (default 200, max 1000)")),
		mcp.WithString("name_contains", mcp.Description("Filter channels whose name contains this substring (case-insensitive)")),
		mcp.WithBoolean("member_only", mcp.Description("If true (default), only return channels the bot has joined")),
	)
	srv.AddTool(listTool, slackListChannelsHandler(agents, logger))

	// --- slack_post_message ---
	postTool := mcp.NewTool("slack_post_message",
		mcp.WithDescription("Post a message to a Slack channel. The channel parameter can be a channel ID (C0123ABC) or a #channel-name."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel ID or #channel-name to post to")),
		mcp.WithString("text", mcp.Required(), mcp.Description("Message text (supports Slack mrkdwn formatting)")),
	)
	srv.AddTool(postTool, slackPostMessageHandler(agents, logger))

	// --- slack_reply_to_thread ---
	replyTool := mcp.NewTool("slack_reply_to_thread",
		mcp.WithDescription("Post a reply to a specific thread in a Slack channel. Use this when you want to reply in a thread rather than posting a new top-level message."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel ID or #channel-name where the thread exists")),
		mcp.WithString("thread_ts", mcp.Required(), mcp.Description("Timestamp of the parent message (thread root)")),
		mcp.WithString("text", mcp.Required(), mcp.Description("Reply text (supports Slack mrkdwn formatting)")),
		mcp.WithBoolean("broadcast", mcp.Description("If true, also post the reply to the channel (default false)")),
	)
	srv.AddTool(replyTool, slackReplyToThreadHandler(agents, logger))

	// --- slack_add_reaction ---
	addReactionTool := mcp.NewTool("slack_add_reaction",
		mcp.WithDescription("Add an emoji reaction to a message. The emoji name should NOT include colons (e.g. use 'thumbsup' not ':thumbsup:')."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel ID where the message is")),
		mcp.WithString("timestamp", mcp.Required(), mcp.Description("Timestamp of the message to react to")),
		mcp.WithString("emoji", mcp.Required(), mcp.Description("Emoji name without colons (e.g. 'thumbsup', 'heart', 'eyes')")),
	)
	srv.AddTool(addReactionTool, slackAddReactionHandler(agents, logger))

	// --- slack_remove_reaction ---
	removeReactionTool := mcp.NewTool("slack_remove_reaction",
		mcp.WithDescription("Remove an emoji reaction from a message."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel ID where the message is")),
		mcp.WithString("timestamp", mcp.Required(), mcp.Description("Timestamp of the message")),
		mcp.WithString("emoji", mcp.Required(), mcp.Description("Emoji name without colons (e.g. 'thumbsup')")),
	)
	srv.AddTool(removeReactionTool, slackRemoveReactionHandler(agents, logger))

	// --- slack_list_emoji ---
	listEmojiTool := mcp.NewTool("slack_list_emoji",
		mcp.WithDescription("List custom emoji available in the Slack workspace. Returns emoji names and their image URLs or aliases. Standard Unicode emoji (like :thumbsup:) are always available and not included in this list."),
		mcp.WithString("name_contains", mcp.Description("Filter emoji whose name contains this substring (case-insensitive)")),
	)
	srv.AddTool(listEmojiTool, slackListEmojiHandler(agents, logger))

	// --- slack_get_channel_history ---
	historyTool := mcp.NewTool("slack_get_channel_history",
		mcp.WithDescription("Get recent messages from a Slack channel. Returns messages in reverse chronological order (newest first)."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel ID or #channel-name")),
		mcp.WithNumber("limit", mcp.Description("Number of messages to retrieve (default 20, max 100)")),
		mcp.WithString("oldest", mcp.Description("Only messages after this Unix timestamp")),
		mcp.WithString("latest", mcp.Description("Only messages before this Unix timestamp")),
	)
	srv.AddTool(historyTool, slackGetChannelHistoryHandler(agents, logger))

	// --- slack_get_thread_replies ---
	threadRepliesTool := mcp.NewTool("slack_get_thread_replies",
		mcp.WithDescription("Get all replies in a message thread."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel ID where the thread exists")),
		mcp.WithString("thread_ts", mcp.Required(), mcp.Description("Timestamp of the parent message (thread root)")),
		mcp.WithNumber("limit", mcp.Description("Maximum number of replies to return (default 50, max 200)")),
	)
	srv.AddTool(threadRepliesTool, slackGetThreadRepliesHandler(agents, logger))

	// --- slack_list_users ---
	listUsersTool := mcp.NewTool("slack_list_users",
		mcp.WithDescription("List users in the Slack workspace. Returns user ID, name, display name, real name, and bot/admin flags. Useful for resolving @mentions or finding user IDs."),
		mcp.WithString("name_contains", mcp.Description("Filter users whose name or display name contains this substring (case-insensitive)")),
		mcp.WithNumber("limit", mcp.Description("Maximum number of users to return (default 200, max 500)")),
		mcp.WithBoolean("include_bots", mcp.Description("If true, include bot users in the result (default false)")),
	)
	srv.AddTool(listUsersTool, slackListUsersHandler(agents, logger))

	// --- slack_upload_file ---
	uploadFileTool := mcp.NewTool("slack_upload_file",
		mcp.WithDescription("Upload a file to a Slack channel. Provide file_path (preferred for binary/large files) or content (for text snippets). Only one source allowed."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel ID or #channel-name to upload to")),
		mcp.WithString("filename", mcp.Required(), mcp.Description("Name of the file (e.g. 'report.csv', 'image.png')")),
		mcp.WithString("file_path", mcp.Description("Path to a local file to upload. Must be inside the kojo upload directory ({tmp}/kojo/upload). Preferred for binary and large files — avoids base64 overhead.")),
		mcp.WithString("content", mcp.Description("Plain text content for the file (for text/code snippets). Use this OR file_path.")),
		mcp.WithString("title", mcp.Description("Title of the file (defaults to filename)")),
		mcp.WithString("initial_comment", mcp.Description("Message to post alongside the file")),
		mcp.WithString("thread_ts", mcp.Description("Thread timestamp to upload the file as a reply")),
	)
	srv.AddTool(uploadFileTool, slackUploadFileHandler(agents, logger))

	httpSrv := mcpserver.NewStreamableHTTPServer(srv,
		mcpserver.WithStateLess(true),
		mcpserver.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			agentID := r.PathValue("id")
			ctx = context.WithValue(ctx, mcpAgentIDKey, agentID)
			if reqID, ok := r.Context().Value(mcpReqIDKey).(string); ok {
				ctx = context.WithValue(ctx, mcpReqIDKey, reqID)
			}
			return ctx
		}),
	)

	// Wrap with logging middleware: request/response + body peek.
	return logMCPMiddleware(httpSrv, logger)
}

// statusRecorder captures HTTP status and bytes written for logging.
// Request/response bodies are intentionally NOT captured here, because MCP
// tool arguments/results may contain sensitive data (Slack tokens, channel
// contents, user messages) that must not leak into logs.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Flush passes through to the underlying ResponseWriter if it supports flushing.
// This is required for SSE/streaming responses (MCP StreamableHTTP uses them).
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// maxMCPBodyBytes caps how much of the request body we buffer for rpcMethod
// peeking. MCP JSON-RPC envelopes are small; anything larger is almost
// certainly tool-call arguments we don't want to parse ourselves, and a cap
// prevents a malicious client from forcing us to allocate unbounded memory.
const maxMCPBodyBytes = 1 << 20 // 1 MiB

// readCloser pairs an arbitrary reader with an explicit close function.
// Used when splicing a peek buffer in front of an existing body so we can
// still propagate Close() to the underlying body.
type readCloser struct {
	io.Reader
	close func() error
}

func (rc *readCloser) Close() error {
	if rc.close == nil {
		return nil
	}
	return rc.close()
}

func logMCPMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := newReqID()
		agentID := r.PathValue("id")

		// Read at most maxMCPBodyBytes so we can peek the JSON-RPC method name.
		// Anything larger is passed through untouched.
		origBody := r.Body
		limited := io.LimitReader(origBody, maxMCPBodyBytes+1)
		peekBuf, _ := io.ReadAll(limited)
		if len(peekBuf) > maxMCPBodyBytes {
			// We hit the cap; splice the remaining body back so the MCP server
			// still sees the full request. Keep delegating Close to the
			// original body so the underlying connection is not leaked.
			r.Body = &readCloser{
				Reader: io.MultiReader(bytes.NewReader(peekBuf), origBody),
				close:  origBody.Close,
			}
		} else {
			_ = origBody.Close()
			r.Body = io.NopCloser(bytes.NewReader(peekBuf))
		}

		// Peek JSON-RPC method name if body looks like JSON. The body itself
		// is NOT logged — MCP tool arguments can contain sensitive data.
		rpcMethod := ""
		if len(peekBuf) > 0 && len(peekBuf) <= maxMCPBodyBytes && peekBuf[0] == '{' {
			var peek struct {
				Method string          `json:"method"`
				ID     json.RawMessage `json:"id"`
			}
			if err := json.Unmarshal(peekBuf, &peek); err == nil {
				rpcMethod = peek.Method
			}
		}

		logger.Info("mcp request",
			"reqID", reqID,
			"agent", agentID,
			"method", r.Method,
			"rpcMethod", rpcMethod,
			"bodySize", len(peekBuf),
			"ua", r.Header.Get("User-Agent"),
		)

		ctx := context.WithValue(r.Context(), mcpReqIDKey, reqID)
		r = r.WithContext(ctx)

		rw := &statusRecorder{ResponseWriter: w, status: 200}
		start := time.Now()
		next.ServeHTTP(rw, r)
		dur := time.Since(start)

		// Response body is NOT logged — it may contain Slack data (channel
		// names, topics, message contents) returned by tool calls.
		logger.Info("mcp response",
			"reqID", reqID,
			"agent", agentID,
			"rpcMethod", rpcMethod,
			"status", rw.status,
			"bytes", rw.bytes,
			"ms", dur.Milliseconds(),
		)
	})
}

func newReqID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func reqIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(mcpReqIDKey).(string); ok {
		return v
	}
	return ""
}

// getSlackClient resolves the Slack bot token for the agent in context
// and returns a Slack API client. Returns nil and an error message if
// the agent has no Slack bot configured.
func getSlackClient(ctx context.Context, agents *agent.Manager) (*slack.Client, string) {
	agentID, _ := ctx.Value(mcpAgentIDKey).(string)
	if agentID == "" {
		return nil, "agent ID not found in request context"
	}

	creds := agents.Credentials()
	if creds == nil {
		return nil, "credential store not available"
	}

	token, err := creds.GetToken("slack", agentID, "", "bot_token")
	if err != nil || token == "" {
		return nil, fmt.Sprintf("Slack bot token not configured for agent %s", agentID)
	}

	return slack.New(token), ""
}

// listChannelsDefaults centralizes the tunables for slack_list_channels so
// they're easy to find and reason about (and so tests can depend on the
// named constants instead of the raw numbers).
const (
	// listChannelsDefaultLimit is the default cap on returned channels when
	// the caller doesn't specify one.
	listChannelsDefaultLimit = 200
	// listChannelsMaxLimit clamps the caller-supplied limit. Anything larger
	// is silently reduced to this value.
	listChannelsMaxLimit = 1000
	// listChannelsPageSize is the per-request page size sent to Slack. 200 is
	// the Slack-recommended tier-2 page size.
	listChannelsPageSize = 200
	// listChannelsMaxPages caps pagination to avoid exhausting Slack Tier 2
	// rate limits in large workspaces that span many hundreds of channels.
	listChannelsMaxPages = 5
)

// listChannelsArgs is the parsed representation of slack_list_channels
// MCP tool arguments.
type listChannelsArgs struct {
	Limit      int
	NameFilter string // already lowercased
	MemberOnly bool
}

// parseListChannelsArgs converts the loose MCP argument map into a typed
// struct, applying defaults and clamping the limit.
func parseListChannelsArgs(args map[string]any) listChannelsArgs {
	out := listChannelsArgs{
		Limit:      listChannelsDefaultLimit,
		MemberOnly: true,
	}
	if v, ok := args["limit"].(float64); ok && v > 0 {
		out.Limit = int(v)
		if out.Limit > listChannelsMaxLimit {
			out.Limit = listChannelsMaxLimit
		}
	}
	if v, ok := args["name_contains"].(string); ok {
		out.NameFilter = strings.ToLower(v)
	}
	if v, ok := args["member_only"].(bool); ok {
		out.MemberOnly = v
	}
	return out
}

// channelInfo is the JSON shape returned by slack_list_channels.
type channelInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Topic      string `json:"topic,omitempty"`
	Purpose    string `json:"purpose,omitempty"`
	NumMembers int    `json:"numMembers"`
	IsMember   bool   `json:"isMember"`
}

// matchChannel returns (info, true) if ch passes the filters in opts, or
// (zero, false) otherwise. Pure on its inputs — no I/O.
func matchChannel(ch slack.Channel, opts listChannelsArgs) (channelInfo, bool) {
	if opts.MemberOnly && !ch.IsMember {
		return channelInfo{}, false
	}
	if opts.NameFilter != "" && !strings.Contains(strings.ToLower(ch.Name), opts.NameFilter) {
		return channelInfo{}, false
	}
	return channelInfo{
		ID:         ch.ID,
		Name:       ch.Name,
		Topic:      ch.Topic.Value,
		Purpose:    ch.Purpose.Value,
		NumMembers: ch.NumMembers,
		IsMember:   ch.IsMember,
	}, true
}

// slackConversationLister is the small subset of slack.Client that the
// channel-listing logic needs. Declared here so tests can inject a fake.
type slackConversationLister interface {
	GetConversationsContext(ctx context.Context, params *slack.GetConversationsParameters) ([]slack.Channel, string, error)
}

// listSlackChannels paginates through Slack's conversations.list API and
// applies opts. It is wired up to a real *slack.Client in production, but
// tests can inject any slackConversationLister.
//
// Partial-success semantics: if we've already collected at least one channel
// and a subsequent page fails, the error is reported to the caller (logged)
// and the channels collected so far are returned.
func listSlackChannels(
	ctx context.Context,
	api slackConversationLister,
	opts listChannelsArgs,
	onPartial func(err error, collected int),
) ([]channelInfo, error) {
	var channels []channelInfo
	cursor := ""
	for page := 0; page < listChannelsMaxPages; page++ {
		params := &slack.GetConversationsParameters{
			Types:           []string{"public_channel", "private_channel"},
			Limit:           listChannelsPageSize,
			Cursor:          cursor,
			ExcludeArchived: true,
		}
		chs, nextCursor, err := api.GetConversationsContext(ctx, params)
		if err != nil {
			if len(channels) > 0 {
				if onPartial != nil {
					onPartial(err, len(channels))
				}
				break
			}
			return nil, err
		}
		for _, ch := range chs {
			info, ok := matchChannel(ch, opts)
			if !ok {
				continue
			}
			channels = append(channels, info)
			if len(channels) >= opts.Limit {
				break
			}
		}
		if len(channels) >= opts.Limit || nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	if len(channels) > opts.Limit {
		channels = channels[:opts.Limit]
	}
	return channels, nil
}

func slackListChannelsHandler(agents *agent.Manager, logger *slog.Logger) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reqID := reqIDFromCtx(ctx)
		agentID, _ := ctx.Value(mcpAgentIDKey).(string)
		opts := parseListChannelsArgs(req.GetArguments())

		logger.Info("mcp tool invoked", "reqID", reqID, "agent", agentID, "tool", "slack_list_channels",
			"limit", opts.Limit, "nameFilter", opts.NameFilter, "memberOnly", opts.MemberOnly)

		api, errMsg := getSlackClient(ctx, agents)
		if api == nil {
			logger.Warn("mcp tool aborted", "reqID", reqID, "agent", agentID, "tool", "slack_list_channels", "err", errMsg)
			return mcp.NewToolResultError(errMsg), nil
		}

		channels, err := listSlackChannels(ctx, api, opts, func(err error, collected int) {
			logger.Warn("mcp tool partial", "reqID", reqID, "agent", agentID,
				"tool", "slack_list_channels", "err", err, "collected", collected)
		})
		if err != nil {
			logger.Warn("mcp tool error", "reqID", reqID, "agent", agentID, "tool", "slack_list_channels", "err", err)
			return mcp.NewToolResultError(fmt.Sprintf("Slack API error: %v", err)), nil
		}

		data, _ := json.MarshalIndent(channels, "", "  ")
		logger.Info("mcp tool result", "reqID", reqID, "agent", agentID, "tool", "slack_list_channels", "channelCount", len(channels))
		return mcp.NewToolResultText(string(data)), nil
	}
}

func slackPostMessageHandler(agents *agent.Manager, logger *slog.Logger) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reqID := reqIDFromCtx(ctx)
		agentID, _ := ctx.Value(mcpAgentIDKey).(string)
		args := req.GetArguments()
		channel, _ := args["channel"].(string)
		text, _ := args["text"].(string)
		logger.Info("mcp tool invoked",
			"reqID", reqID, "agent", agentID, "tool", "slack_post_message",
			"channel", channel, "textLen", len(text),
		)

		api, errMsg := getSlackClient(ctx, agents)
		if api == nil {
			logger.Warn("mcp tool aborted", "reqID", reqID, "agent", agentID, "tool", "slack_post_message", "err", errMsg)
			return mcp.NewToolResultError(errMsg), nil
		}

		if channel == "" || text == "" {
			logger.Warn("mcp tool missing args", "reqID", reqID, "agent", agentID, "tool", "slack_post_message", "channelEmpty", channel == "", "textEmpty", text == "")
			return mcp.NewToolResultError("both 'channel' and 'text' are required"), nil
		}

		resolvedChannel, err := resolveSlackChannel(ctx, api, channel)
		if err != nil {
			logger.Warn("mcp tool resolve failed", "reqID", reqID, "agent", agentID, "tool", "slack_post_message", "channel", channel, "err", err)
			return mcp.NewToolResultError(fmt.Sprintf("failed to resolve channel %q: %v", channel, err)), nil
		}

		_, ts, err := api.PostMessageContext(ctx, resolvedChannel, slack.MsgOptionText(text, false))
		if err != nil {
			logger.Warn("mcp tool post failed", "reqID", reqID, "agent", agentID, "tool", "slack_post_message", "channel", resolvedChannel, "err", err)
			return mcp.NewToolResultError(fmt.Sprintf("failed to post message: %v", err)), nil
		}

		logger.Info("mcp tool result",
			"reqID", reqID, "agent", agentID, "tool", "slack_post_message",
			"channel", resolvedChannel, "ts", ts,
		)

		result := map[string]string{
			"channel":   resolvedChannel,
			"timestamp": ts,
			"status":    "posted",
		}
		data, _ := json.Marshal(result)
		return mcp.NewToolResultText(string(data)), nil
	}
}

// resolveSlackChannel converts a #channel-name to a channel ID.
// If the input already looks like a conversation ID (starts with C/G/D),
// it's returned as-is. User IDs (U/W prefix) are NOT accepted here —
// chat.postMessage expects a conversation ID, not a user ID, so passing a
// user ID would fail at Slack. Callers who want to DM a user must open a
// conversation first and pass the resulting D... ID.
func resolveSlackChannel(ctx context.Context, api *slack.Client, channel string) (string, error) {
	channel = strings.TrimPrefix(channel, "#")

	if len(channel) >= 9 && !strings.Contains(channel, " ") {
		switch channel[0] {
		case 'C', 'G', 'D':
			return channel, nil
		}
	}

	cursor := ""
	for {
		params := &slack.GetConversationsParameters{
			Types:  []string{"public_channel", "private_channel"},
			Limit:  200,
			Cursor: cursor,
		}
		chs, nextCursor, err := api.GetConversationsContext(ctx, params)
		if err != nil {
			return "", fmt.Errorf("list channels: %w", err)
		}
		for _, ch := range chs {
			if ch.Name == channel {
				return ch.ID, nil
			}
		}
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}

	return "", fmt.Errorf("channel %q not found", channel)
}

// ---------------------------------------------------------------------------
// slack_reply_to_thread
// ---------------------------------------------------------------------------

func slackReplyToThreadHandler(agents *agent.Manager, logger *slog.Logger) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reqID := reqIDFromCtx(ctx)
		agentID, _ := ctx.Value(mcpAgentIDKey).(string)
		args := req.GetArguments()
		channel, _ := args["channel"].(string)
		threadTS, _ := args["thread_ts"].(string)
		text, _ := args["text"].(string)
		broadcast, _ := args["broadcast"].(bool)

		logger.Info("mcp tool invoked",
			"reqID", reqID, "agent", agentID, "tool", "slack_reply_to_thread",
			"channel", channel, "threadTS", threadTS, "textLen", len(text),
		)

		api, errMsg := getSlackClient(ctx, agents)
		if api == nil {
			logger.Warn("mcp tool aborted", "reqID", reqID, "agent", agentID, "tool", "slack_reply_to_thread", "err", errMsg)
			return mcp.NewToolResultError(errMsg), nil
		}

		if channel == "" || threadTS == "" || text == "" {
			return mcp.NewToolResultError("'channel', 'thread_ts', and 'text' are all required"), nil
		}

		resolvedChannel, err := resolveSlackChannel(ctx, api, channel)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to resolve channel %q: %v", channel, err)), nil
		}

		opts := []slack.MsgOption{
			slack.MsgOptionText(text, false),
			slack.MsgOptionTS(threadTS),
		}
		if broadcast {
			opts = append(opts, slack.MsgOptionBroadcast())
		}

		_, ts, err := api.PostMessageContext(ctx, resolvedChannel, opts...)
		if err != nil {
			logger.Warn("mcp tool post failed", "reqID", reqID, "agent", agentID, "tool", "slack_reply_to_thread", "err", err)
			return mcp.NewToolResultError(fmt.Sprintf("failed to post reply: %v", err)), nil
		}

		logger.Info("mcp tool result",
			"reqID", reqID, "agent", agentID, "tool", "slack_reply_to_thread",
			"channel", resolvedChannel, "ts", ts,
		)

		result := map[string]string{
			"channel":   resolvedChannel,
			"timestamp": ts,
			"thread_ts": threadTS,
			"status":    "replied",
		}
		data, _ := json.Marshal(result)
		return mcp.NewToolResultText(string(data)), nil
	}
}

// ---------------------------------------------------------------------------
// slack_add_reaction
// ---------------------------------------------------------------------------

func slackAddReactionHandler(agents *agent.Manager, logger *slog.Logger) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reqID := reqIDFromCtx(ctx)
		agentID, _ := ctx.Value(mcpAgentIDKey).(string)
		args := req.GetArguments()
		channel, _ := args["channel"].(string)
		timestamp, _ := args["timestamp"].(string)
		emoji, _ := args["emoji"].(string)

		logger.Info("mcp tool invoked",
			"reqID", reqID, "agent", agentID, "tool", "slack_add_reaction",
			"channel", channel, "timestamp", timestamp, "emoji", emoji,
		)

		api, errMsg := getSlackClient(ctx, agents)
		if api == nil {
			return mcp.NewToolResultError(errMsg), nil
		}

		if channel == "" || timestamp == "" || emoji == "" {
			return mcp.NewToolResultError("'channel', 'timestamp', and 'emoji' are all required"), nil
		}

		// Strip colons if the caller includes them (e.g. ":thumbsup:" → "thumbsup")
		emoji = strings.Trim(emoji, ":")

		ref := slack.NewRefToMessage(channel, timestamp)
		if err := api.AddReactionContext(ctx, emoji, ref); err != nil {
			logger.Warn("mcp tool error", "reqID", reqID, "agent", agentID, "tool", "slack_add_reaction", "err", err)
			return mcp.NewToolResultError(fmt.Sprintf("failed to add reaction: %v", err)), nil
		}

		logger.Info("mcp tool result", "reqID", reqID, "agent", agentID, "tool", "slack_add_reaction", "status", "added")

		result := map[string]string{"status": "added", "emoji": emoji}
		data, _ := json.Marshal(result)
		return mcp.NewToolResultText(string(data)), nil
	}
}

// ---------------------------------------------------------------------------
// slack_remove_reaction
// ---------------------------------------------------------------------------

func slackRemoveReactionHandler(agents *agent.Manager, logger *slog.Logger) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reqID := reqIDFromCtx(ctx)
		agentID, _ := ctx.Value(mcpAgentIDKey).(string)
		args := req.GetArguments()
		channel, _ := args["channel"].(string)
		timestamp, _ := args["timestamp"].(string)
		emoji, _ := args["emoji"].(string)

		logger.Info("mcp tool invoked",
			"reqID", reqID, "agent", agentID, "tool", "slack_remove_reaction",
			"channel", channel, "timestamp", timestamp, "emoji", emoji,
		)

		api, errMsg := getSlackClient(ctx, agents)
		if api == nil {
			return mcp.NewToolResultError(errMsg), nil
		}

		if channel == "" || timestamp == "" || emoji == "" {
			return mcp.NewToolResultError("'channel', 'timestamp', and 'emoji' are all required"), nil
		}

		emoji = strings.Trim(emoji, ":")

		ref := slack.NewRefToMessage(channel, timestamp)
		if err := api.RemoveReactionContext(ctx, emoji, ref); err != nil {
			logger.Warn("mcp tool error", "reqID", reqID, "agent", agentID, "tool", "slack_remove_reaction", "err", err)
			return mcp.NewToolResultError(fmt.Sprintf("failed to remove reaction: %v", err)), nil
		}

		logger.Info("mcp tool result", "reqID", reqID, "agent", agentID, "tool", "slack_remove_reaction", "status", "removed")

		result := map[string]string{"status": "removed", "emoji": emoji}
		data, _ := json.Marshal(result)
		return mcp.NewToolResultText(string(data)), nil
	}
}

// ---------------------------------------------------------------------------
// slack_list_emoji
// ---------------------------------------------------------------------------

// emojiInfo is the JSON shape returned by slack_list_emoji.
type emojiInfo struct {
	Name  string `json:"name"`
	Value string `json:"value"` // URL or "alias:name"
}

func slackListEmojiHandler(agents *agent.Manager, logger *slog.Logger) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reqID := reqIDFromCtx(ctx)
		agentID, _ := ctx.Value(mcpAgentIDKey).(string)
		args := req.GetArguments()
		nameFilter, _ := args["name_contains"].(string)
		nameFilter = strings.ToLower(nameFilter)

		logger.Info("mcp tool invoked",
			"reqID", reqID, "agent", agentID, "tool", "slack_list_emoji",
			"nameFilter", nameFilter,
		)

		api, errMsg := getSlackClient(ctx, agents)
		if api == nil {
			return mcp.NewToolResultError(errMsg), nil
		}

		emojiMap, err := api.GetEmojiContext(ctx)
		if err != nil {
			logger.Warn("mcp tool error", "reqID", reqID, "agent", agentID, "tool", "slack_list_emoji", "err", err)
			return mcp.NewToolResultError(fmt.Sprintf("Slack API error: %v", err)), nil
		}

		emojis := filterEmoji(emojiMap, nameFilter)

		// Sort by name for deterministic output.
		sort.Slice(emojis, func(i, j int) bool { return emojis[i].Name < emojis[j].Name })

		logger.Info("mcp tool result", "reqID", reqID, "agent", agentID, "tool", "slack_list_emoji", "emojiCount", len(emojis))

		data, _ := json.MarshalIndent(emojis, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

// ---------------------------------------------------------------------------
// slack_get_channel_history
// ---------------------------------------------------------------------------

const (
	historyDefaultLimit = 20
	historyMaxLimit     = 100
)

// messageInfo is the JSON shape returned by slack_get_channel_history and
// slack_get_thread_replies.
type messageInfo struct {
	User      string `json:"user"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	ReplyCount int   `json:"reply_count,omitempty"`
	SubType   string `json:"subtype,omitempty"`
}

func slackGetChannelHistoryHandler(agents *agent.Manager, logger *slog.Logger) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reqID := reqIDFromCtx(ctx)
		agentID, _ := ctx.Value(mcpAgentIDKey).(string)
		args := req.GetArguments()
		channel, _ := args["channel"].(string)
		oldest, _ := args["oldest"].(string)
		latest, _ := args["latest"].(string)

		limit := clampLimit(args["limit"], historyDefaultLimit, historyMaxLimit)

		logger.Info("mcp tool invoked",
			"reqID", reqID, "agent", agentID, "tool", "slack_get_channel_history",
			"channel", channel, "limit", limit,
		)

		api, errMsg := getSlackClient(ctx, agents)
		if api == nil {
			return mcp.NewToolResultError(errMsg), nil
		}

		if channel == "" {
			return mcp.NewToolResultError("'channel' is required"), nil
		}

		resolvedChannel, err := resolveSlackChannel(ctx, api, channel)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to resolve channel %q: %v", channel, err)), nil
		}

		params := &slack.GetConversationHistoryParameters{
			ChannelID: resolvedChannel,
			Limit:     limit,
			Oldest:    oldest,
			Latest:    latest,
		}

		resp, err := api.GetConversationHistoryContext(ctx, params)
		if err != nil {
			logger.Warn("mcp tool error", "reqID", reqID, "agent", agentID, "tool", "slack_get_channel_history", "err", err)
			return mcp.NewToolResultError(fmt.Sprintf("Slack API error: %v", err)), nil
		}

		messages := make([]messageInfo, 0, len(resp.Messages))
		for _, m := range resp.Messages {
			messages = append(messages, messageInfo{
				User:       m.User,
				Text:       m.Text,
				Timestamp:  m.Timestamp,
				ThreadTS:   m.ThreadTimestamp,
				ReplyCount: m.ReplyCount,
				SubType:    m.SubType,
			})
		}

		logger.Info("mcp tool result", "reqID", reqID, "agent", agentID, "tool", "slack_get_channel_history", "messageCount", len(messages))

		data, _ := json.MarshalIndent(messages, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

// ---------------------------------------------------------------------------
// slack_get_thread_replies
// ---------------------------------------------------------------------------

const (
	threadRepliesDefaultLimit = 50
	threadRepliesMaxLimit     = 200
)

func slackGetThreadRepliesHandler(agents *agent.Manager, logger *slog.Logger) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reqID := reqIDFromCtx(ctx)
		agentID, _ := ctx.Value(mcpAgentIDKey).(string)
		args := req.GetArguments()
		channel, _ := args["channel"].(string)
		threadTS, _ := args["thread_ts"].(string)

		limit := clampLimit(args["limit"], threadRepliesDefaultLimit, threadRepliesMaxLimit)

		logger.Info("mcp tool invoked",
			"reqID", reqID, "agent", agentID, "tool", "slack_get_thread_replies",
			"channel", channel, "threadTS", threadTS, "limit", limit,
		)

		api, errMsg := getSlackClient(ctx, agents)
		if api == nil {
			return mcp.NewToolResultError(errMsg), nil
		}

		if channel == "" || threadTS == "" {
			return mcp.NewToolResultError("'channel' and 'thread_ts' are required"), nil
		}

		params := &slack.GetConversationRepliesParameters{
			ChannelID: channel,
			Timestamp: threadTS,
			Limit:     limit,
		}

		msgs, _, _, err := api.GetConversationRepliesContext(ctx, params)
		if err != nil {
			logger.Warn("mcp tool error", "reqID", reqID, "agent", agentID, "tool", "slack_get_thread_replies", "err", err)
			return mcp.NewToolResultError(fmt.Sprintf("Slack API error: %v", err)), nil
		}

		messages := make([]messageInfo, 0, len(msgs))
		for _, m := range msgs {
			messages = append(messages, messageInfo{
				User:      m.User,
				Text:      m.Text,
				Timestamp: m.Timestamp,
				ThreadTS:  m.ThreadTimestamp,
				SubType:   m.SubType,
			})
		}

		logger.Info("mcp tool result", "reqID", reqID, "agent", agentID, "tool", "slack_get_thread_replies", "messageCount", len(messages))

		data, _ := json.MarshalIndent(messages, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

// ---------------------------------------------------------------------------
// slack_list_users
// ---------------------------------------------------------------------------

const (
	listUsersDefaultLimit = 200
	listUsersMaxLimit     = 500
)

// userInfo is the JSON shape returned by slack_list_users.
type userInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	RealName    string `json:"realName"`
	DisplayName string `json:"displayName"`
	IsBot       bool   `json:"isBot,omitempty"`
	IsAdmin     bool   `json:"isAdmin,omitempty"`
	Deleted     bool   `json:"deleted,omitempty"`
}

func slackListUsersHandler(agents *agent.Manager, logger *slog.Logger) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reqID := reqIDFromCtx(ctx)
		agentID, _ := ctx.Value(mcpAgentIDKey).(string)
		args := req.GetArguments()
		nameFilter, _ := args["name_contains"].(string)
		nameFilter = strings.ToLower(nameFilter)
		includeBots, _ := args["include_bots"].(bool)

		limit := clampLimit(args["limit"], listUsersDefaultLimit, listUsersMaxLimit)

		logger.Info("mcp tool invoked",
			"reqID", reqID, "agent", agentID, "tool", "slack_list_users",
			"nameFilter", nameFilter, "limit", limit, "includeBots", includeBots,
		)

		api, errMsg := getSlackClient(ctx, agents)
		if api == nil {
			return mcp.NewToolResultError(errMsg), nil
		}

		allUsers, err := api.GetUsersContext(ctx)
		if err != nil {
			logger.Warn("mcp tool error", "reqID", reqID, "agent", agentID, "tool", "slack_list_users", "err", err)
			return mcp.NewToolResultError(fmt.Sprintf("Slack API error: %v", err)), nil
		}

		var users []userInfo
		for _, u := range allUsers {
			info, ok := matchUser(u, nameFilter, includeBots)
			if !ok {
				continue
			}
			users = append(users, info)
			if len(users) >= limit {
				break
			}
		}

		logger.Info("mcp tool result", "reqID", reqID, "agent", agentID, "tool", "slack_list_users", "userCount", len(users))

		data, _ := json.MarshalIndent(users, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

// ---------------------------------------------------------------------------
// slack_upload_file
// ---------------------------------------------------------------------------

func slackUploadFileHandler(agents *agent.Manager, logger *slog.Logger) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reqID := reqIDFromCtx(ctx)
		agentID, _ := ctx.Value(mcpAgentIDKey).(string)
		args := req.GetArguments()
		channel, _ := args["channel"].(string)
		filename, _ := args["filename"].(string)
		filePath, _ := args["file_path"].(string)
		content, _ := args["content"].(string)
		title, _ := args["title"].(string)
		initialComment, _ := args["initial_comment"].(string)
		threadTS, _ := args["thread_ts"].(string)

		logger.Info("mcp tool invoked",
			"reqID", reqID, "agent", agentID, "tool", "slack_upload_file",
			"channel", channel, "filename", filename,
			"hasFilePath", filePath != "", "contentLen", len(content),
		)

		api, errMsg := getSlackClient(ctx, agents)
		if api == nil {
			return mcp.NewToolResultError(errMsg), nil
		}

		if channel == "" || filename == "" {
			return mcp.NewToolResultError("'channel' and 'filename' are required"), nil
		}

		// Exactly one source must be provided.
		if filePath == "" && content == "" {
			return mcp.NewToolResultError("one of 'file_path' or 'content' is required"), nil
		}
		if filePath != "" && content != "" {
			return mcp.NewToolResultError("provide only one of 'file_path' or 'content'"), nil
		}

		resolvedChannel, err := resolveSlackChannel(ctx, api, channel)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to resolve channel %q: %v", channel, err)), nil
		}

		if title == "" {
			title = filename
		}

		params := slack.UploadFileParameters{
			Filename:        filename,
			Title:           title,
			Channel:         resolvedChannel,
			InitialComment:  initialComment,
			ThreadTimestamp: threadTS,
		}

		// On the file_path branch we open the validated file ourselves and
		// hand a *os.File to the Slack SDK. Passing the path would re-open
		// it inside the SDK, leaving a TOCTOU window where a symlink swap
		// (or rename) between validation and re-open could redirect the
		// upload to a file outside the upload directory. Streaming through
		// the fd we already pinned closes that window.
		if filePath != "" {
			f, size, errKind := openUploadPath(filePath)
			if errKind != "" {
				logger.Warn("mcp tool aborted",
					"reqID", reqID, "agent", agentID, "tool", "slack_upload_file",
					"reason", errKind, "basename", filepath.Base(filePath),
				)
				return mcp.NewToolResultError(uploadPathUserMessage(errKind)), nil
			}
			defer f.Close()
			params.Reader = f
			params.FileSize = int(size)
		} else {
			params.Content = content
			params.FileSize = len(content)
		}

		fileSummary, err := api.UploadFileContext(ctx, params)
		if err != nil {
			logger.Warn("mcp tool error", "reqID", reqID, "agent", agentID, "tool", "slack_upload_file", "err", err)
			return mcp.NewToolResultError(fmt.Sprintf("failed to upload file: %v", err)), nil
		}

		logger.Info("mcp tool result",
			"reqID", reqID, "agent", agentID, "tool", "slack_upload_file",
			"fileID", fileSummary.ID, "channel", resolvedChannel,
		)

		result := map[string]string{
			"status":  "uploaded",
			"file_id": fileSummary.ID,
			"title":   fileSummary.Title,
			"channel": resolvedChannel,
		}
		data, _ := json.Marshal(result)
		return mcp.NewToolResultText(string(data)), nil
	}
}

// ---------------------------------------------------------------------------
// Shared helpers (extracted for testability)
// ---------------------------------------------------------------------------

// clampLimit reads a JSON-decoded numeric "limit" argument and clamps it to
// [1, max]. Inputs are floats because mcp-go decodes JSON numbers as float64.
//
// Returns def when the argument is missing, non-numeric, non-positive, or
// rounds down to zero (e.g. 0.5).
func clampLimit(raw any, def, max int) int {
	v, ok := raw.(float64)
	if !ok || v <= 0 {
		return def
	}
	n := int(v)
	if n < 1 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// filterEmoji returns emojiInfo entries from the Slack emoji map whose name
// contains nameFilter (case-insensitive). An empty filter matches all.
func filterEmoji(emojiMap map[string]string, nameFilter string) []emojiInfo {
	filter := strings.ToLower(nameFilter)
	var emojis []emojiInfo
	for name, value := range emojiMap {
		if filter != "" && !strings.Contains(strings.ToLower(name), filter) {
			continue
		}
		emojis = append(emojis, emojiInfo{Name: name, Value: value})
	}
	return emojis
}

// matchUser applies the slack_list_users filter rules to a Slack user and
// returns its userInfo projection on match. nameFilter is matched
// case-insensitively against Name, RealName, and DisplayName; it does not
// need to be pre-lowercased by the caller.
//
// Rules (in order):
//  1. Skip users marked Deleted.
//  2. Skip bot users (IsBot or USLACKBOT) unless includeBots is true.
//  3. If nameFilter is non-empty, the user's Name, RealName, or DisplayName
//     must contain it as a case-insensitive substring.
func matchUser(u slack.User, nameFilter string, includeBots bool) (userInfo, bool) {
	if u.Deleted {
		return userInfo{}, false
	}
	if (u.IsBot || u.ID == "USLACKBOT") && !includeBots {
		return userInfo{}, false
	}
	if nameFilter != "" {
		needle := strings.ToLower(nameFilter)
		match := strings.Contains(strings.ToLower(u.Name), needle) ||
			strings.Contains(strings.ToLower(u.RealName), needle) ||
			strings.Contains(strings.ToLower(u.Profile.DisplayName), needle)
		if !match {
			return userInfo{}, false
		}
	}
	return userInfo{
		ID:          u.ID,
		Name:        u.Name,
		RealName:    u.RealName,
		DisplayName: u.Profile.DisplayName,
		IsBot:       u.IsBot,
		IsAdmin:     u.IsAdmin,
	}, true
}

// upload path validation error kinds. These are constants (not formatted
// errors) so they can be safely logged without leaking absolute paths or
// other host details.
const (
	uploadErrEmpty     = "empty"
	uploadErrInvalid   = "invalid_path"
	uploadErrNotFound  = "not_found"
	uploadErrOutside   = "outside_upload_dir"
	uploadErrIsDir     = "is_directory"
	uploadErrNotFile   = "not_regular_file"
	uploadErrOpenFail  = "open_failed"
	uploadErrStatFail  = "stat_failed"
	uploadErrSwapped   = "swapped_during_validation"
)

// uploadPathUserMessage maps an internal error kind to a fixed,
// path-free user-facing message.
func uploadPathUserMessage(kind string) string {
	switch kind {
	case uploadErrEmpty:
		return "'file_path' must not be empty"
	case uploadErrOutside:
		return "'file_path' must be inside the kojo upload directory"
	case uploadErrIsDir:
		return "'file_path' must be a regular file, not a directory"
	case uploadErrNotFile:
		return "'file_path' must be a regular file"
	case uploadErrNotFound:
		return "'file_path' does not exist or is not accessible"
	case uploadErrSwapped:
		return "'file_path' changed during validation; refusing to upload"
	default:
		return "'file_path' is invalid"
	}
}

// openUploadPath validates that p points to a regular file inside the
// kojo upload directory and returns an *os.File already opened on the
// validated inode. Callers MUST Close the returned file.
//
// Opening up-front (rather than just resolving a path string) eliminates
// the TOCTOU window between validation and Slack SDK re-open: even if a
// symlink/rename races our check, the SDK reads from the fd we already
// pinned to the verified inode.
//
// On error, returns (nil, 0, kind) where kind is one of the uploadErr*
// constants — never a formatted error containing a host path.
func openUploadPath(p string) (*os.File, int64, string) {
	if p == "" {
		return nil, 0, uploadErrEmpty
	}

	// Canonicalize the upload root once (handles /tmp → /private/tmp on macOS).
	canonicalRoot, err := filepath.EvalSymlinks(uploadDir)
	if err != nil {
		canonicalRoot = uploadDir
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, 0, uploadErrInvalid
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, 0, uploadErrNotFound
	}

	// Must be inside the upload directory (canonical-prefix check).
	if resolved != canonicalRoot &&
		!strings.HasPrefix(resolved, canonicalRoot+string(filepath.Separator)) {
		return nil, 0, uploadErrOutside
	}

	// Open the resolved (canonical) path.
	f, err := os.Open(resolved)
	if err != nil {
		return nil, 0, uploadErrOpenFail
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, uploadErrStatFail
	}
	if info.IsDir() {
		f.Close()
		return nil, 0, uploadErrIsDir
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, 0, uploadErrNotFile
	}

	// Close the residual TOCTOU window between EvalSymlinks and Open: if
	// the path (or any of its parents) was swapped to a symlink or to a
	// different inode in that gap, reject the upload.
	//
	// Three checks together cover the realistic attack vectors:
	//  1. Re-resolve symlinks: if any component was made a symlink after
	//     the first EvalSymlinks, the canonical form changes.
	//  2. Lstat regular-file: reject if the final component is now a
	//     symlink or non-regular type.
	//  3. SameFile inode match: reject if the path now points to a
	//     different inode than the open fd.
	reResolved, err := filepath.EvalSymlinks(resolved)
	if err != nil || reResolved != resolved {
		f.Close()
		return nil, 0, uploadErrSwapped
	}
	lstatInfo, err := os.Lstat(resolved)
	if err != nil {
		f.Close()
		return nil, 0, uploadErrSwapped
	}
	if !lstatInfo.Mode().IsRegular() {
		f.Close()
		return nil, 0, uploadErrSwapped
	}
	if !os.SameFile(info, lstatInfo) {
		f.Close()
		return nil, 0, uploadErrSwapped
	}

	return f, info.Size(), ""
}
