package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/slack-go/slack"

	"github.com/loppo-llc/kojo/internal/agent"
)

type contextKey int

const mcpAgentIDKey contextKey = iota

// newMCPHandler creates the MCP HTTP handler that serves Slack tools.
// A single MCPServer + StreamableHTTPServer is shared across all agents;
// the agent ID is injected into the request context via the URL path value
// and used to resolve agent-specific credentials at call time.
func newMCPHandler(agents *agent.Manager) http.Handler {
	srv := mcpserver.NewMCPServer("kojo-tools", "1.0.0",
		mcpserver.WithToolCapabilities(true),
	)

	// --- slack_list_channels ---
	listTool := mcp.NewTool("slack_list_channels",
		mcp.WithDescription("List Slack channels the bot can access. Returns channel ID, name, topic, and member count."),
	)
	srv.AddTool(listTool, slackListChannelsHandler(agents))

	// --- slack_post_message ---
	postTool := mcp.NewTool("slack_post_message",
		mcp.WithDescription("Post a message to a Slack channel. The channel parameter can be a channel ID (C0123ABC) or a #channel-name."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel ID or #channel-name to post to")),
		mcp.WithString("text", mcp.Required(), mcp.Description("Message text (supports Slack mrkdwn formatting)")),
	)
	srv.AddTool(postTool, slackPostMessageHandler(agents))

	httpSrv := mcpserver.NewStreamableHTTPServer(srv,
		mcpserver.WithStateLess(true),
		mcpserver.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			agentID := r.PathValue("id")
			return context.WithValue(ctx, mcpAgentIDKey, agentID)
		}),
	)

	return httpSrv
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

func slackListChannelsHandler(agents *agent.Manager) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		api, errMsg := getSlackClient(ctx, agents)
		if api == nil {
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
		for {
			params := &slack.GetConversationsParameters{
				Types:           []string{"public_channel", "private_channel"},
				Limit:           200,
				Cursor:          cursor,
				ExcludeArchived: true,
			}
			chs, nextCursor, err := api.GetConversationsContext(ctx, params)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("Slack API error: %v", err)), nil
			}
			for _, ch := range chs {
				channels = append(channels, channelInfo{
					ID:         ch.ID,
					Name:       ch.Name,
					Topic:      ch.Topic.Value,
					Purpose:    ch.Purpose.Value,
					NumMembers: ch.NumMembers,
					IsMember:   ch.IsMember,
				})
			}
			if nextCursor == "" {
				break
			}
			cursor = nextCursor
		}

		data, _ := json.MarshalIndent(channels, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}
}

func slackPostMessageHandler(agents *agent.Manager) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		api, errMsg := getSlackClient(ctx, agents)
		if api == nil {
			return mcp.NewToolResultError(errMsg), nil
		}

		args := req.GetArguments()
		channel, _ := args["channel"].(string)
		text, _ := args["text"].(string)

		if channel == "" || text == "" {
			return mcp.NewToolResultError("both 'channel' and 'text' are required"), nil
		}

		resolvedChannel, err := resolveSlackChannel(ctx, api, channel)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to resolve channel %q: %v", channel, err)), nil
		}

		_, ts, err := api.PostMessageContext(ctx, resolvedChannel, slack.MsgOptionText(text, false))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to post message: %v", err)), nil
		}

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

	if len(channel) > 0 && (channel[0] == 'C' || channel[0] == 'G' || channel[0] == 'D') && !strings.Contains(channel, " ") && len(channel) >= 9 {
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
