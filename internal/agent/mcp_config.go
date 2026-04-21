package agent

import (
	"encoding/json"
	"fmt"
)

// mcpServerEntry represents one MCP server configuration.
// For HTTP transport, only URL (and optionally Type) are set.
type mcpServerEntry struct {
	URL  string `json:"url,omitempty"`
	Type string `json:"type,omitempty"` // "http" for Gemini/Codex
}

// BuildMCPServers returns the set of MCP servers that should be available
// to the given agent. apiBase is the kojo server URL (e.g. "http://127.0.0.1:8080").
func BuildMCPServers(agentID, apiBase string, hasSlackBot bool) map[string]mcpServerEntry {
	if apiBase == "" {
		return nil
	}

	servers := make(map[string]mcpServerEntry)

	if hasSlackBot {
		servers["slack"] = mcpServerEntry{
			URL:  fmt.Sprintf("%s/api/v1/agents/%s/mcp", apiBase, agentID),
			Type: "http",
		}
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

