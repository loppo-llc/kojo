package agent

import (
	"encoding/json"
	"fmt"
)

// mcpServerEntry represents one MCP server configuration.
// For HTTP transport, only URL (and optionally Type) are set.
// Headers are required when the MCP target sits behind kojo's auth
// listener — every /api/v1/* request needs the per-agent token.
type mcpServerEntry struct {
	URL     string            `json:"url,omitempty"`
	Type    string            `json:"type,omitempty"` // "http" for Gemini/Codex
	Headers map[string]string `json:"headers,omitempty"`
}

// BuildMCPServers returns the set of MCP servers that should be available
// to the given agent. apiBase is the kojo server URL (e.g. "http://127.0.0.1:8080").
//
// When the agent has its own kojo auth token (i.e. agentTokenLookup is
// wired up by the server) the per-agent /mcp endpoint requires the
// X-Kojo-Token header. Without it the call lands as a Guest principal
// and is denied by the auth middleware.
func BuildMCPServers(agentID, apiBase string, hasSlackBot bool) map[string]mcpServerEntry {
	if apiBase == "" {
		return nil
	}

	servers := make(map[string]mcpServerEntry)

	if hasSlackBot {
		entry := mcpServerEntry{
			URL:  fmt.Sprintf("%s/api/v1/agents/%s/mcp", apiBase, agentID),
			Type: "http",
		}
		if agentTokenLookup != nil {
			if tok, ok := agentTokenLookup(agentID); ok && tok != "" {
				entry.Headers = map[string]string{"X-Kojo-Token": tok}
			}
		}
		servers["slack"] = entry
	}

	return servers
}

// mcpConfigJSON returns inline JSON for Claude's --mcp-config flag.
// Claude Code uses {"mcpServers": {...}} format and expects mcpServers to be
// an object (never null), so a nil input map is normalized to an empty map.
func mcpConfigJSON(servers map[string]mcpServerEntry) (string, error) {
	if servers == nil {
		servers = map[string]mcpServerEntry{}
	}
	cfg := struct {
		MCPServers map[string]mcpServerEntry `json:"mcpServers"`
	}{MCPServers: servers}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

