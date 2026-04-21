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
