package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"

	"github.com/loppo-llc/kojo/internal/atomicfile"
)

// codexThreadIDPattern matches Codex app-server thread IDs. Current
// Codex builds use UUIDv7-shaped lowercase hex IDs; keeping the
// validation strict prevents poisoned .codex/threads refs from
// reaching thread/resume or filesystem joins.
var codexThreadIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func isCodexThreadID(s string) bool {
	return codexThreadIDPattern.MatchString(s)
}

type codexThreadRef struct {
	ThreadID    string `json:"thread_id"`
	RolloutPath string `json:"rollout_path,omitempty"`
}

func codexThreadRefDir(agentID string) string {
	return filepath.Join(agentDir(agentID), ".codex", "threads")
}

func codexThreadRefName(sessionKey string) string {
	if sessionKey == "" {
		return "main.json"
	}
	return "key-" + agentIDToUUID(sessionKey) + ".json"
}

func codexThreadRefPath(agentID, sessionKey string) string {
	return filepath.Join(codexThreadRefDir(agentID), codexThreadRefName(sessionKey))
}

func readCodexThreadRef(agentID, sessionKey string) (*codexThreadRef, error) {
	body, err := os.ReadFile(codexThreadRefPath(agentID, sessionKey))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ref codexThreadRef
	if err := json.Unmarshal(body, &ref); err != nil {
		return nil, err
	}
	if !isCodexThreadID(ref.ThreadID) {
		_ = os.Remove(codexThreadRefPath(agentID, sessionKey))
		return nil, fmt.Errorf("invalid codex thread_id %q", ref.ThreadID)
	}
	return &ref, nil
}

func writeCodexThreadRef(agentID, sessionKey string, ref codexThreadRef, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if !isCodexThreadID(ref.ThreadID) {
		logger.Warn("codex: refusing to persist invalid thread id", "agent", agentID, "thread_id", ref.ThreadID)
		return
	}
	dir := codexThreadRefDir(agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.Warn("codex: mkdir thread ref dir failed", "agent", agentID, "err", err)
		return
	}
	body, err := json.MarshalIndent(ref, "", "  ")
	if err != nil {
		logger.Warn("codex: marshal thread ref failed", "agent", agentID, "err", err)
		return
	}
	if err := atomicfile.WriteBytes(codexThreadRefPath(agentID, sessionKey), append(body, '\n'), 0o644); err != nil {
		logger.Warn("codex: write thread ref failed", "agent", agentID, "err", err)
	}
}

func deleteCodexThreadRef(agentID, sessionKey string, logger *slog.Logger) {
	if err := os.Remove(codexThreadRefPath(agentID, sessionKey)); err != nil && !os.IsNotExist(err) {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("codex: remove stale thread ref failed", "agent", agentID, "err", err)
	}
}

func cloneStringAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func waitCodexRPCResponse(scanner *bufio.Scanner, id int64) (*rpcMessage, bool) {
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.ID != nil && *msg.ID == id {
			return &msg, true
		}
	}
	return nil, false
}

func decodeCodexThreadResult(raw *json.RawMessage) (threadID, rolloutPath string) {
	if raw == nil {
		return "", ""
	}
	var result struct {
		Thread struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(*raw, &result); err != nil {
		return "", ""
	}
	return result.Thread.ID, result.Thread.Path
}

func codexEffortForProtocol(effort string) string {
	switch effort {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return effort
	default:
		return ""
	}
}

func codexCanResumeSession(agentID, sessionKey string) bool {
	if sessionKey == "" {
		return false
	}
	ref, err := readCodexThreadRef(agentID, sessionKey)
	if err != nil || ref == nil || ref.ThreadID == "" {
		return false
	}
	path := ref.RolloutPath
	if path == "" {
		path = lookupCodexRolloutPath(ref.ThreadID)
	}
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular() && info.Size() > 0
}
