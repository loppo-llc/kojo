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
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
	head   []byte // first 200 bytes for logging
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if len(r.head) < 200 {
		room := 200 - len(r.head)
		if room > len(b) {
			room = len(b)
		}
		r.head = append(r.head, b[:room]...)
	}
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

func logMCPMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := newReqID()
		agentID := r.PathValue("id")

		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))

		// Peek JSON-RPC method name if body looks like JSON.
		rpcMethod := ""
		if len(body) > 0 && body[0] == '{' {
			var peek struct {
				Method string          `json:"method"`
				ID     json.RawMessage `json:"id"`
			}
			if err := json.Unmarshal(body, &peek); err == nil {
				rpcMethod = peek.Method
			}
		}

		logger.Info("mcp request",
			"reqID", reqID,
			"agent", agentID,
			"method", r.Method,
			"rpcMethod", rpcMethod,
			"bodySize", len(body),
			"bodyHead", headString(body, 300),
			"ua", r.Header.Get("User-Agent"),
		)

		ctx := context.WithValue(r.Context(), mcpReqIDKey, reqID)
		r = r.WithContext(ctx)

		rw := &statusRecorder{ResponseWriter: w, status: 200}
		start := time.Now()
		next.ServeHTTP(rw, r)
		dur := time.Since(start)

		logger.Info("mcp response",
			"reqID", reqID,
			"agent", agentID,
			"rpcMethod", rpcMethod,
			"status", rw.status,
			"bytes", rw.bytes,
			"ms", dur.Milliseconds(),
			"respHead", headString(rw.head, 300),
		)
	})
}

func newReqID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func headString(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
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

func slackListChannelsHandler(agents *agent.Manager, logger *slog.Logger) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reqID := reqIDFromCtx(ctx)
		agentID, _ := ctx.Value(mcpAgentIDKey).(string)
		args := req.GetArguments()

		// Parse optional parameters.
		limit := 200
		if v, ok := args["limit"].(float64); ok && v > 0 {
			limit = int(v)
			if limit > 1000 {
				limit = 1000
			}
		}
		nameFilter := ""
		if v, ok := args["name_contains"].(string); ok {
			nameFilter = strings.ToLower(v)
		}
		memberOnly := true
		if v, ok := args["member_only"].(bool); ok {
			memberOnly = v
		}

		logger.Info("mcp tool invoked", "reqID", reqID, "agent", agentID, "tool", "slack_list_channels",
			"limit", limit, "nameFilter", nameFilter, "memberOnly", memberOnly)

		api, errMsg := getSlackClient(ctx, agents)
		if api == nil {
			logger.Warn("mcp tool aborted", "reqID", reqID, "agent", agentID, "tool", "slack_list_channels", "err", errMsg)
			return mcp.NewToolResultError(errMsg), nil
		}

		type channelInfo struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Topic      string `json:"topic,omitempty"`
			Purpose    string `json:"purpose,omitempty"`
			NumMembers int    `json:"numMembers"`
			IsMember   bool   `json:"isMember"`
		}

		var channels []channelInfo
		cursor := ""
		// Cap pagination to avoid exhausting Slack Tier 2 rate limits.
		// Large workspaces can have thousands of channels across 20+ pages.
		const maxPages = 5
		for page := 0; page < maxPages; page++ {
			params := &slack.GetConversationsParameters{
				Types:           []string{"public_channel", "private_channel"},
				Limit:           200,
				Cursor:          cursor,
				ExcludeArchived: true,
			}
			chs, nextCursor, err := api.GetConversationsContext(ctx, params)
			if err != nil {
				// If we already collected some channels, return them with a warning
				// instead of failing completely.
				if len(channels) > 0 {
					logger.Warn("mcp tool partial", "reqID", reqID, "agent", agentID,
						"tool", "slack_list_channels", "err", err, "collected", len(channels))
					break
				}
				logger.Warn("mcp tool error", "reqID", reqID, "agent", agentID, "tool", "slack_list_channels", "err", err)
				return mcp.NewToolResultError(fmt.Sprintf("Slack API error: %v", err)), nil
			}
			for _, ch := range chs {
				if memberOnly && !ch.IsMember {
					continue
				}
				if nameFilter != "" && !strings.Contains(strings.ToLower(ch.Name), nameFilter) {
					continue
				}
				channels = append(channels, channelInfo{
					ID:         ch.ID,
					Name:       ch.Name,
					Topic:      ch.Topic.Value,
					Purpose:    ch.Purpose.Value,
					NumMembers: ch.NumMembers,
					IsMember:   ch.IsMember,
				})
				if len(channels) >= limit {
					break
				}
			}
			if len(channels) >= limit || nextCursor == "" {
				break
			}
			cursor = nextCursor
		}

		// Truncate to exact limit.
		if len(channels) > limit {
			channels = channels[:limit]
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
// If the input already looks like a channel ID (starts with C/G/D), it's returned as-is.
func resolveSlackChannel(ctx context.Context, api *slack.Client, channel string) (string, error) {
	channel = strings.TrimPrefix(channel, "#")

	if len(channel) > 0 && (channel[0] == 'C' || channel[0] == 'G' || channel[0] == 'D' || channel[0] == 'U' || channel[0] == 'W') && !strings.Contains(channel, " ") && len(channel) >= 9 {
		return channel, nil
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
