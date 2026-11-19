package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/chathistory"
)

// codexCacheColdIdle is the idle gap after which the prompt cache behind a
// codex thread is assumed gone. OpenAI's cache retention is best-effort and
// varies: on kojo agents' rollouts, turns resumed after 26, 88 and 136
// minutes still read almost the whole context from cache, a few after
// 100-150 minutes did not, and every one after 200+ minutes started with
// cached_input_tokens at or near 0. Like sessionCacheColdIdle for claude,
// this is a ceiling: it never resets a thread whose cache could still be
// warm.
const codexCacheColdIdle = 3 * time.Hour

// codexColdResetFloorTokens is the context size over which a cold codex
// thread is dropped instead of resumed. A cold resume prefills the whole
// context at full input price (no cache write premium); a fresh thread
// starts near 17k. Same judgement as agyColdResetFloorTokens.
//
// Warm threads are never reset by kojo (warmTokens 0): codex compacts them
// itself, keeping continuity, and resetting a warm thread would throw away
// a cache that is still paying for itself.
const codexColdResetFloorTokens = 100_000

// codexRolloutTailReadBytes caps the rollout tail read. Rollout lines can
// be several MiB (compaction records carry the replacement history), so
// the window is wider than the agy / claude ones.
const codexRolloutTailReadBytes = 8 * 1024 * 1024

var codexResetPolicy = idleResetPolicy{
	tool:            ToolCodex,
	coldIdle:        codexCacheColdIdle,
	coldFloorTokens: codexColdResetFloorTokens,
}

// lastCodexContextTokens returns the input size of the last model call in
// a codex rollout (last_token_usage.input_tokens of the last token_count
// event, cached input included). A compaction record after it means the
// context has just shrunk, so it reads as 0 until the next call.
func lastCodexContextTokens(path string) (int, bool) {
	// Non-blocking open + fstat regular-file gate: a FIFO swapped in for
	// the log must not hang the turn.
	f, info, err := openStagedAttachment(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	var start int64
	if info.Size() > codexRolloutTailReadBytes {
		start = info.Size() - codexRolloutTailReadBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return 0, false
	}
	first := start > 0
	last, seen := 0, false
	tokenCount := []byte(`"token_count"`)
	compacted := []byte(`"compacted"`)
	err = chathistory.ScanJSONLLines(io.LimitReader(f, codexRolloutTailReadBytes), func(line []byte) {
		if first {
			first = false
			return
		}
		isCount := bytes.Contains(line, tokenCount)
		if !isCount && !bytes.Contains(line, compacted) {
			return
		}
		var rec struct {
			Type    string `json:"type"`
			Payload struct {
				Type string `json:"type"`
				Info *struct {
					Last struct {
						InputTokens int `json:"input_tokens"`
					} `json:"last_token_usage"`
				} `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &rec) != nil {
			return
		}
		switch {
		case rec.Type == "compacted":
			last, seen = 0, true
		case rec.Type == "event_msg" && rec.Payload.Type == "token_count" && rec.Payload.Info != nil && rec.Payload.Info.Last.InputTokens > 0:
			last, seen = rec.Payload.Info.Last.InputTokens, true
		}
	})
	if err != nil {
		return 0, false
	}
	if !seen && start > 0 {
		return 0, false
	}
	return last, true
}

// codexRolloutUsable accepts only a rollout path codex itself would have
// written for threadID: absolute, a .jsonl named after the thread, inside
// $CODEX_HOME/sessions once symlinks in its directory are resolved. The ref
// lives in the agent-writable workspace, so anything else is ignored. (The
// file itself must also be a regular file; idleAwareResetDue Lstats it.)
func codexRolloutUsable(path, threadID string) bool {
	if path == "" || !filepath.IsAbs(path) || !isCodexThreadID(threadID) {
		return false
	}
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "rollout-") || !strings.HasSuffix(base, "-"+threadID+".jsonl") {
		return false
	}
	root := codexHome()
	if root == "" {
		return false
	}
	sessions, err := filepath.EvalSymlinks(filepath.Join(root, "sessions"))
	if err != nil {
		return false
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(sessions, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// dropCodexThreadRefForReset removes the session's thread ref after a reset
// decision, under the same lock goal binding updates take. It re-reads the
// ref first and leaves it alone (ok=false) when it no longer points at
// threadID or a goal got bound to it since the decision.
func dropCodexThreadRefForReset(agentID, sessionKey, threadID string) (bool, error) {
	path := codexThreadRefPath(agentID, sessionKey)
	unlock := goalRefLocks.Lock(path)
	defer unlock()
	ref, err := readCodexThreadRef(agentID, sessionKey)
	if err != nil {
		return false, err
	}
	// The caller (Chat) holds this key's codexGoalRuntimes slot, so no goal
	// run can start on the thread; only a binding written through
	// updateGoalBinding can race the decision, and that takes this lock.
	if ref == nil || ref.ThreadID != threadID || ref.Goal != nil {
		return false, nil
	}
	return true, deleteCodexThreadRefStrict(agentID, sessionKey)
}

// codexThreadResetDue reports whether the stored thread ref should be
// dropped before this turn (see idleAwareResetDue). Goal-bound threads are
// never reset: the goal's continuation lives in the native thread.
func codexThreadResetDue(ref *codexThreadRef, sessionKey string, automatedTrigger bool, agentID string, idleThreshold time.Duration, logger *slog.Logger) bool {
	if ref == nil || ref.Goal != nil || !codexRolloutUsable(ref.RolloutPath, ref.ThreadID) {
		return false
	}
	return idleAwareResetDue(codexResetPolicy, ref.RolloutPath, lastCodexContextTokens, ref.ThreadID, sessionKey, automatedTrigger, agentID, idleThreshold, logger)
}
