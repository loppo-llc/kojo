package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	markerStart = "<!-- kojo-agent:start -->"
	markerEnd   = "<!-- kojo-agent:end -->"
)

// injectCLAUDEmd writes the agent's SOUL.md into the workDir's CLAUDE.local.md.
// Any previously injected block (from a crash) is stripped first.
// Returns a restore function that must be called when the session ends.
func injectCLAUDEmd(workDir, soul string) (restoreFunc func(), err error) {
	path := filepath.Join(workDir, "CLAUDE.local.md")

	// Read existing file
	existing, readErr := os.ReadFile(path)
	hasExisting := readErr == nil

	// Strip any stale injected block from a previous crash
	base := string(existing)
	if hasExisting {
		base = stripInjectedBlock(base)
	}

	// Build injected content
	injected := fmt.Sprintf("\n%s\n%s\n%s\n", markerStart, soul, markerEnd)

	var content string
	if hasExisting {
		content = base + injected
	} else {
		content = injected
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("failed to write CLAUDE.local.md: %w", err)
	}

	// Return restore function
	return func() {
		if hasExisting {
			os.WriteFile(path, []byte(base), 0o644)
		} else {
			os.Remove(path)
		}
	}, nil
}

// stripInjectedBlock removes the kojo-agent marker block from content.
func stripInjectedBlock(content string) string {
	start := strings.Index(content, markerStart)
	if start < 0 {
		return content
	}
	end := strings.Index(content[start:], markerEnd)
	if end < 0 {
		return content
	}
	// Remove from the newline before markerStart to the end of markerEnd + newline
	before := content[:start]
	after := content[start+end+len(markerEnd):]
	// Trim the leading newline we prepend during injection
	before = strings.TrimRight(before, "\n")
	if strings.HasPrefix(after, "\n") {
		after = after[1:]
	}
	result := before + after
	if result == "" {
		return ""
	}
	return result
}
