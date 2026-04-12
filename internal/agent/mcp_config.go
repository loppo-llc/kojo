package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
// Claude Code uses {"mcpServers": {...}} format.
func mcpConfigJSON(servers map[string]mcpServerEntry) (string, error) {
	cfg := struct {
		MCPServers map[string]mcpServerEntry `json:"mcpServers"`
	}{MCPServers: servers}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// WriteCodexMCPConfig writes .codex/config.toml with [mcp_servers.*] sections
// for Codex CLI's project-level MCP configuration.
// Codex reads this file from the working directory on startup.
// If servers is empty, the file is removed.
func WriteCodexMCPConfig(dir string, servers map[string]mcpServerEntry, logger *slog.Logger) {
	codexDir := filepath.Join(dir, ".codex")
	configPath := filepath.Join(codexDir, "config.toml")

	if len(servers) == 0 {
		os.Remove(configPath)
		return
	}

	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		logger.Warn("failed to create .codex dir", "err", err)
		return
	}

	var b strings.Builder
	for name, s := range servers {
		fmt.Fprintf(&b, "[mcp_servers.%s]\n", name)
		fmt.Fprintf(&b, "url = %q\n\n", s.URL)
	}

	if err := os.WriteFile(configPath, []byte(b.String()), 0o644); err != nil {
		logger.Warn("failed to write .codex/config.toml", "err", err)
	}
}
