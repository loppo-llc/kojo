package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildMCPServers(t *testing.T) {
	t.Run("empty apiBase returns nil", func(t *testing.T) {
		got := BuildMCPServers("ag_123", "", true)
		if got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})

	t.Run("no slack bot produces empty map", func(t *testing.T) {
		got := BuildMCPServers("ag_123", "http://127.0.0.1:8080", false)
		if got == nil {
			t.Fatalf("expected non-nil map, got nil")
		}
		if len(got) != 0 {
			t.Fatalf("expected empty map, got %v", got)
		}
	})

	t.Run("slack bot produces http entry with agent-scoped URL", func(t *testing.T) {
		got := BuildMCPServers("ag_abc", "http://127.0.0.1:8080", true)
		entry, ok := got["slack"]
		if !ok {
			t.Fatalf("expected slack entry, got %v", got)
		}
		if entry.Type != "http" {
			t.Errorf("entry.Type = %q, want %q", entry.Type, "http")
		}
		wantURL := "http://127.0.0.1:8080/api/v1/agents/ag_abc/mcp"
		if entry.URL != wantURL {
			t.Errorf("entry.URL = %q, want %q", entry.URL, wantURL)
		}
	})
}

func TestMCPConfigJSON(t *testing.T) {
	t.Run("empty servers produce mcpServers:{}", func(t *testing.T) {
		s, err := mcpConfigJSON(map[string]mcpServerEntry{})
		if err != nil {
			t.Fatalf("mcpConfigJSON error: %v", err)
		}
		// Claude expects an object (possibly empty) under mcpServers.
		var round struct {
			MCPServers map[string]mcpServerEntry `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(s), &round); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if round.MCPServers == nil {
			t.Errorf("mcpServers unmarshaled as nil; expected empty map")
		}
		if !strings.Contains(s, `"mcpServers"`) {
			t.Errorf("output %q missing mcpServers key", s)
		}
	})

	t.Run("slack entry is serialized with url and type", func(t *testing.T) {
		servers := map[string]mcpServerEntry{
			"slack": {URL: "http://127.0.0.1:8080/api/v1/agents/ag_abc/mcp", Type: "http"},
		}
		s, err := mcpConfigJSON(servers)
		if err != nil {
			t.Fatalf("mcpConfigJSON error: %v", err)
		}
		var round struct {
			MCPServers map[string]mcpServerEntry `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(s), &round); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		slack, ok := round.MCPServers["slack"]
		if !ok {
			t.Fatalf("slack entry missing: %v", round.MCPServers)
		}
		if slack.URL != servers["slack"].URL {
			t.Errorf("url mismatch: got %q, want %q", slack.URL, servers["slack"].URL)
		}
		if slack.Type != "http" {
			t.Errorf("type mismatch: got %q, want %q", slack.Type, "http")
		}
	})

	t.Run("nil servers normalized to empty object", func(t *testing.T) {
		// Claude Code rejects `{"mcpServers": null}`; a nil input must be
		// serialized as `{"mcpServers": {}}` so it's safe to always pass
		// through --mcp-config.
		s, err := mcpConfigJSON(nil)
		if err != nil {
			t.Fatalf("mcpConfigJSON(nil) error: %v", err)
		}
		if !strings.Contains(s, `"mcpServers":{}`) {
			t.Errorf("output %q should contain mcpServers:{}", s)
		}
	})
}
