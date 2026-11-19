package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/loppo-llc/kojo/internal/chathistory"
)

// agyResetThresholdTokens is the context size (input + cache_read of the
// last planner call) at which kojo drops a stored agy conversation and
// starts a fresh one. agy compacts on its own (a CHECKPOINT step) only
// near the model's limit — observed around 500k — so this sits below it:
// kojo's reset, which keeps long-term state in the agent's files rather
// than in an in-context summary, fires first, and agy's CHECKPOINT stays a
// safety net. Same reasoning as sessionResetThresholdTokens for claude.
const agyResetThresholdTokens = 400_000

// agyCacheColdIdle is the idle gap after which the implicit prompt cache
// behind an agy conversation is assumed gone. Measured on agy transcripts:
// a turn that starts within about 5-7 minutes of the previous one reads
// 95-99% of its context from cache, while one that starts 10 minutes or
// more later has cache_read_tokens=0 on its first planner call and
// prefills the whole conversation at the full input price.
//
// Measured against the transcript's mtime, which agy rewrites on every
// step, i.e. roughly the end of the last turn.
const agyCacheColdIdle = 10 * time.Minute

// agyColdResetFloorTokens replaces agyResetThresholdTokens once the idle
// gap exceeds agyCacheColdIdle. With the cache cold, resuming a
// conversation of size C prefills C at full price, while a fresh one
// prefills only the rules, tools and bootstrap context (15-25k measured on
// agy main conversations). Gemini's implicit cache has no write premium,
// so the break-even sits higher than claude's 30k floor: below 100k the
// saving per cold wake is small next to the lost in-context continuity.
// The number is a judgement, not a measurement.
const agyColdResetFloorTokens = 100_000

// agyTranscriptTailReadBytes caps how much of agy's transcript.jsonl is
// read to find the last planner call. agy truncates long fields in that
// log (records stay a few KiB), so the window holds many steps.
const agyTranscriptTailReadBytes = 1 * 1024 * 1024

// agyTranscriptPath is the per-conversation step log agy writes under its
// global state directory.
func agyTranscriptPath(convID string) string {
	home := agyHome()
	if home == "" || !isAgyConversationID(convID) {
		return ""
	}
	return filepath.Join(home, "brain", convID, ".system_generated", "logs", "transcript.jsonl")
}

// lastAgyContextTokens returns the context size of the last PLANNER_RESPONSE
// in an agy transcript: input_tokens + cache_read_tokens of that call.
// CHECKPOINT records are skipped — their counts describe the compaction
// call over the old context, not the context that follows it. ok=false
// means the read was inconclusive and no decision should be made.
func lastAgyContextTokens(path string) (int, bool) {
	// Non-blocking open + fstat regular-file gate: a FIFO swapped in for
	// the log must not hang the turn.
	f, info, err := openStagedAttachment(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	var start int64
	if info.Size() > agyTranscriptTailReadBytes {
		start = info.Size() - agyTranscriptTailReadBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return 0, false
	}
	first := start > 0
	last := 0
	planner := []byte(`"PLANNER_RESPONSE"`)
	err = chathistory.ScanJSONLLines(io.LimitReader(f, agyTranscriptTailReadBytes), func(line []byte) {
		if first {
			// Seeked into the middle of a record: drop the partial line.
			first = false
			return
		}
		if !bytes.Contains(line, planner) {
			return
		}
		var rec struct {
			Type            string `json:"type"`
			InputTokens     int    `json:"input_tokens"`
			CacheReadTokens int    `json:"cache_read_tokens"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.Type != "PLANNER_RESPONSE" {
			return
		}
		if n := rec.InputTokens + rec.CacheReadTokens; n > 0 {
			last = n
		}
	})
	if err != nil {
		return 0, false
	}
	if last == 0 && start > 0 {
		return 0, false
	}
	return last, true
}

// agyResetPolicy is the idle-aware reset policy for agy conversations.
var agyResetPolicy = idleResetPolicy{
	tool:            ToolGeminiAgy,
	warmTokens:      agyResetThresholdTokens,
	coldIdle:        agyCacheColdIdle,
	coldFloorTokens: agyColdResetFloorTokens,
}

// agyConversationResetDue reports whether the stored conversation convID
// should be dropped before this turn (see idleAwareResetDue).
func agyConversationResetDue(convID, sessionKey string, automatedTrigger bool, agentID string, idleThreshold time.Duration, logger *slog.Logger) bool {
	path := agyTranscriptPath(convID)
	if path == "" {
		return false
	}
	return idleAwareResetDue(agyResetPolicy, path, lastAgyContextTokens, convID, sessionKey, automatedTrigger, agentID, idleThreshold, logger)
}

// dropAgyConversation forgets the conversation a session key points at and
// removes agy's state for it unless another ref still uses it. It reports
// false when the ref could not be removed, in which case the caller keeps
// resuming. A failed state removal is only logged: the id stays in the
// known-conversations ledger, so a later session reset retries it.
func dropAgyConversation(agentDirPath, sessionKey, convID string, logger *slog.Logger) bool {
	if err := os.Remove(agyConversationRefPath(agentDirPath, sessionKey)); err != nil && !os.IsNotExist(err) {
		logger.Warn("agy reset: remove conversation ref failed, keeping conversation",
			"conversation", convID, "err", err)
		return false
	}
	if agyConversationIDPersisted(agentDirPath, convID) {
		return true
	}
	if _, err := removeAgyConversation(convID); err != nil {
		logger.Warn("agy reset: remove conversation state failed", "conversation", convID, "err", err)
		return true
	}
	if agyRefuseStateSymlinks(agentDirPath) == nil {
		_ = os.Remove(agyExtraFile(agentDirPath, convID))
	}
	return true
}

// agyTranscriptHasErrorAfter reports whether an agy transcript holds an
// ERROR_MESSAGE step with an index above afterStep. ok=false means the
// read was inconclusive: the file could not be read, or the tail window
// did not reach back to afterStep.
func agyTranscriptHasErrorAfter(path string, afterStep int) (found, ok bool) {
	if path == "" {
		return false, false
	}
	f, info, err := openStagedAttachment(path)
	if err != nil {
		return false, false
	}
	defer f.Close()
	var start int64
	if info.Size() > agyTranscriptTailReadBytes {
		start = info.Size() - agyTranscriptTailReadBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return false, false
	}
	first := start > 0
	reached := start == 0
	err = chathistory.ScanJSONLLines(io.LimitReader(f, agyTranscriptTailReadBytes), func(line []byte) {
		if first {
			first = false
			return
		}
		var rec struct {
			StepIndex int    `json:"step_index"`
			Type      string `json:"type"`
		}
		if json.Unmarshal(line, &rec) != nil {
			return
		}
		if rec.StepIndex <= afterStep {
			reached = true
		} else if rec.Type == "ERROR_MESSAGE" {
			found = true
		}
	})
	if err != nil {
		return false, false
	}
	if found {
		return true, true
	}
	return false, reached
}
