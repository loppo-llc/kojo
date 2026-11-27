package agent

import (
	"log/slog"
	"os"
	"time"
)

// idleResetPolicy configures idleAwareResetDue for one CLI backend. It is
// the agy / codex counterpart of claude's sessionFileUsableReset, which
// predates it and keeps its own copy of the same rules.
type idleResetPolicy struct {
	tool string
	// warmTokens resets a session over this context size even while its
	// prompt cache may be warm. 0 leaves warm sessions to the CLI's own
	// compaction.
	warmTokens int
	// coldIdle is the idle gap after which the CLI's prompt cache is
	// assumed gone and resuming prefills the whole context at full price.
	coldIdle time.Duration
	// coldFloorTokens replaces warmTokens once the session is cold.
	coldFloorTokens int
}

// idleAwareResetDue reports whether the stored session whose CLI log is at
// logPath should be dropped before this turn. measure returns the context
// size of the last model call in that log (ok=false: inconclusive, keep).
// Idle time is the log's mtime, roughly the end of the last turn.
//
// Over p.warmTokens the session is dropped; once idle past p.coldIdle the
// bar falls to p.coldFloorTokens, since resuming then buys nothing but a
// full-price prefill. An interactive turn (automatedTrigger=false) keeps a
// session active within idleThreshold (<=0: sessionResetMinIdleDuration),
// so a reset never lands mid-rally.
//
// The dropped session is not summarized; the fresh session gets the
// caller's bootstrap context like any new one.
func idleAwareResetDue(p idleResetPolicy, logPath string, measure func(string) (int, bool), sessionID, sessionKey string, automatedTrigger bool, agentID string, idleThreshold time.Duration, logger *slog.Logger) bool {
	if logger == nil {
		logger = slog.Default()
	}
	// Lstat + regular: the path comes from state an agent can write, so
	// refuse symlinks and FIFOs (an open on a FIFO would block the turn).
	info, err := os.Lstat(logPath)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	ctx, ok := measure(logPath)
	if !ok {
		return false
	}
	idle := time.Since(info.ModTime())
	threshold := p.warmTokens
	cacheCold := idle >= p.coldIdle
	if cacheCold {
		threshold = p.coldFloorTokens
	}
	if threshold <= 0 || ctx <= threshold {
		return false
	}
	if !automatedTrigger {
		guard := idleThreshold
		if guard <= 0 {
			guard = sessionResetMinIdleDuration
		}
		if idle < guard {
			logger.Debug("session over threshold but recently active, keeping",
				"tool", p.tool, "agent", agentID, "session", sessionID,
				"contextTokens", ctx, "idle", idle, "idleThreshold", guard)
			return false
		}
	}
	logger.Info("session context over threshold, resetting",
		"tool", p.tool, "agent", agentID, "session", sessionID, "sessionKey", sessionKey,
		"contextTokens", ctx, "threshold", threshold, "cacheCold", cacheCold,
		"idle", idle, "automatedTrigger", automatedTrigger)
	return true
}
