package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

const cronTimeout = 10 * time.Minute // default timeout; per-agent override via TimeoutMinutes
const cronMinInterval = 50 * time.Second // minimum interval between runs for same agent
const cronLockFile = ".cron_last"

// cronPrompt builds the periodic check-in prompt. See cronPromptAt for details.
func cronPrompt(nextRun time.Time, timeoutMinutes int, customMessage string) string {
	return cronPromptAt(time.Now(), nextRun, timeoutMinutes, customMessage)
}

// cronPromptAt is the time-injectable form of cronPrompt for unit testing.
// If customMessage is non-empty it replaces the default trailing instruction;
// the literal "{date}" inside customMessage is replaced with today's date in
// YYYY-MM-DD form. The custom section is separated from the meta header by a
// blank line so an injected "[system message]" prefix cannot blend in with the
// surrounding meta text.
func cronPromptAt(now, nextRun time.Time, timeoutMinutes int, customMessage string) string {
	today := now.Format("2006-01-02")
	msg := "[system message] " + now.Format("2006年1月2日 15:04") + "の定期チェックインです。"

	msg += fmt.Sprintf("（今回のタイムアウトは%d分", timeoutMinutes)
	if !nextRun.IsZero() {
		var nextFmt string
		if nextRun.YearDay() == now.YearDay() && nextRun.Year() == now.Year() {
			nextFmt = nextRun.Format("15:04")
		} else {
			nextFmt = nextRun.Format("1月2日 15:04")
		}
		msg += "。完了後の次回のチェックインは最短" + formatUntil(nextRun, now) + "後 (" + nextFmt + ")"
	}
	msg += "）"
	if trimmed := strings.TrimSpace(customMessage); trimmed != "" {
		// Blank line + heading make the user-supplied section visually
		// distinguishable from the meta header even if the value contains
		// "[system message]" or similar prompt-bracketing.
		msg += "\n\n--- 指示 ---\n" + strings.ReplaceAll(trimmed, "{date}", today)
	} else {
		msg += "最近の出来事や気づきがあれば memory/" + today + ".md に記録し、必要なタスクを実行してください。"
	}
	return msg
}

// checkinPrompt builds the manual check-in prompt. Unlike cronPromptAt this
// is fired on demand from the UI and has no scheduled successor, so it omits
// the "次回のチェックイン" footer and uses the wording "チェックイン" (not
// "定期チェックイン") to make the source visible in the transcript.
func checkinPrompt(now time.Time, timeoutMinutes int, customMessage string) string {
	today := now.Format("2006-01-02")
	msg := "[system message] " + now.Format("2006年1月2日 15:04") + "のチェックインです。"
	msg += fmt.Sprintf("（今回のタイムアウトは%d分）", timeoutMinutes)
	if trimmed := strings.TrimSpace(customMessage); trimmed != "" {
		msg += "\n\n--- 指示 ---\n" + strings.ReplaceAll(trimmed, "{date}", today)
	} else {
		msg += "最近の出来事や気づきがあれば memory/" + today + ".md に記録し、必要なタスクを実行してください。"
	}
	return msg
}

// formatUntil returns a Japanese-formatted duration from now until t,
// rounded to the nearest minute (minimum 1分).
func formatUntil(t, now time.Time) string {
	mins := int((t.Sub(now) + 30*time.Second) / time.Minute)
	if mins < 1 {
		mins = 1
	}
	if mins < 60 {
		return fmt.Sprintf("%d分", mins)
	}
	h := mins / 60
	m := mins % 60
	if m == 0 {
		return fmt.Sprintf("%d時間", h)
	}
	return fmt.Sprintf("%d時間%d分", h, m)
}

// cronScheduler manages periodic agent executions.
type cronScheduler struct {
	mu      sync.Mutex
	c       *cron.Cron
	entries map[string]cron.EntryID // agent ID -> cron entry
	mgr     *Manager
	logger  *slog.Logger
}

func newCronScheduler(mgr *Manager, logger *slog.Logger) *cronScheduler {
	cronLogger := cron.PrintfLogger(slog.NewLogLogger(logger.Handler(), slog.LevelDebug))
	return &cronScheduler{
		c:       cron.New(cron.WithChain(cron.SkipIfStillRunning(cronLogger))),
		entries: make(map[string]cron.EntryID),
		mgr:     mgr,
		logger:  logger,
	}
}

func (cs *cronScheduler) Start() {
	cs.c.Start()
}

func (cs *cronScheduler) Stop() {
	ctx := cs.c.Stop()
	// Wait for running jobs to finish (with timeout)
	select {
	case <-ctx.Done():
	case <-time.After(cronTimeout + 30*time.Second):
		cs.logger.Warn("cron shutdown timed out waiting for jobs")
	}
}

// Schedule adds or updates a cron schedule for an agent.
// If cronExpr is empty, any existing schedule is removed.
// The cronExpr is generated internally by intervalToCron.
func (cs *cronScheduler) Schedule(agentID string, cronExpr string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Remove existing entry
	if entryID, ok := cs.entries[agentID]; ok {
		cs.c.Remove(entryID)
		delete(cs.entries, agentID)
	}

	if cronExpr == "" {
		return nil
	}

	entryID, err := cs.c.AddFunc(cronExpr, func() {
		cs.runCronJob(agentID)
	})
	if err != nil {
		return err
	}

	cs.entries[agentID] = entryID
	return nil
}

// nextRun returns the next scheduled run time for an agent, adjusted for
// silent hours. If the cron's Next falls inside the silent window, it
// iterates the cron schedule forward to find the first tick outside it.
// Returns zero time if no schedule exists.
func (cs *cronScheduler) nextRun(agentID string, silentStart, silentEnd string) time.Time {
	cs.mu.Lock()
	var entry cron.Entry
	if entryID, ok := cs.entries[agentID]; ok {
		entry = cs.c.Entry(entryID)
	}
	cs.mu.Unlock()

	if entry.Schedule == nil || entry.Next.IsZero() {
		return time.Time{}
	}

	if silentStart == "" || silentEnd == "" {
		return entry.Next
	}

	return nextRunOutsideSilentWindow(entry.Schedule, entry.Next, silentStart, silentEnd)
}

// nextRunOutsideSilentWindow finds the first cron tick at or after t that falls
// outside the silent hours window. Tries up to 300 iterations (covers 25h at
// 5-minute intervals) to avoid returning a tick inside a long silent window.
func nextRunOutsideSilentWindow(sched cron.Schedule, t time.Time, start, end string) time.Time {
	s, _ := time.Parse("15:04", start)
	e, _ := time.Parse("15:04", end)
	startMin := s.Hour()*60 + s.Minute()
	endMin := e.Hour()*60 + e.Minute()

	candidate := t
	for i := 0; i < 300; i++ {
		cMin := candidate.Hour()*60 + candidate.Minute()
		inSilent := false
		if startMin <= endMin {
			inSilent = cMin >= startMin && cMin < endMin
		} else {
			inSilent = cMin >= startMin || cMin < endMin
		}
		if !inSilent {
			return candidate
		}
		candidate = sched.Next(candidate)
	}
	// Couldn't find a non-silent tick; return zero so the UI shows "—"
	// instead of a misleading time that would be skipped.
	return time.Time{}
}

// Remove removes the cron schedule for an agent.
func (cs *cronScheduler) Remove(agentID string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if entryID, ok := cs.entries[agentID]; ok {
		cs.c.Remove(entryID)
		delete(cs.entries, agentID)
	}
}

// cronLockPath returns the path to the lock file for an agent's cron job.
func cronLockPath(agentID string) string {
	return filepath.Join(agentDir(agentID), cronLockFile)
}

// acquireCronLock atomically creates a lock file using O_CREATE|O_EXCL.
// Returns true if the lock was acquired (this process should run the job).
// If a lock file already exists but is older than cronMinInterval, it is
// treated as stale (previous run completed or crashed) and reclaimed.
// Fail-closed: any unexpected error returns false.
func acquireCronLock(agentID string) bool {
	dir := agentDir(agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	p := cronLockPath(agentID)

	// Fast path: atomically create lock file (OS guarantees only one succeeds)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		f.Close()
		return true
	}

	// File exists — check if it's stale
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	if time.Since(info.ModTime()) < cronMinInterval {
		return false // recent lock, another process is handling it
	}

	// Stale lock — reclaim: remove and retry atomically
	os.Remove(p)
	f, err = os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return false // another process reclaimed it first
	}
	f.Close()
	return true
}

func (cs *cronScheduler) runCronJob(agentID string) {
	// Check global pause
	if cs.mgr.CronPaused() {
		cs.logger.Debug("cron job skipped (globally paused)", "agent", agentID)
		return
	}

	// Check silent hours and read agent config
	var silentStart, silentEnd string
	var timeoutMinutes int
	if a, ok := cs.mgr.Get(agentID); ok {
		// Archived guard: a tick may have queued just before Archive ran
		// cron.Remove. Bail out before ever calling Chat (which would log a
		// warn for ErrAgentArchived).
		if a.Archived {
			cs.logger.Debug("cron job skipped (archived)", "agent", agentID)
			return
		}
		silentStart, silentEnd = a.SilentStart, a.SilentEnd
		timeoutMinutes = a.TimeoutMinutes
		if IsInSilentHours(silentStart, silentEnd) {
			cs.logger.Debug("cron job skipped (silent hours)", "agent", agentID,
				"silentStart", silentStart, "silentEnd", silentEnd)
			return
		}
	}

	cronMessage := readCheckinFile(agentID)

	// Cross-process guard: atomic lock file prevents duplicate execution
	if !acquireCronLock(agentID) {
		cs.logger.Debug("cron job skipped (lock held)", "agent", agentID)
		return
	}

	cs.logger.Info("cron job triggered", "agent", agentID)

	nextRun := cs.nextRun(agentID, silentStart, silentEnd)

	// Per-agent timeout (0 = use default)
	timeout := cronTimeout
	if timeoutMinutes > 0 {
		timeout = time.Duration(timeoutMinutes) * time.Minute
	}
	effectiveTimeoutMin := int(timeout / time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	events, err := cs.mgr.Chat(ctx, agentID, cronPrompt(nextRun, effectiveTimeoutMin, cronMessage), "system", nil, BusySourceCron)
	if err != nil {
		cs.logger.Warn("cron chat failed", "agent", agentID, "err", err)
		return
	}

	// Drain events (we don't stream cron results anywhere, just persist them)
	for range events {
	}

	if ctx.Err() == context.DeadlineExceeded {
		cs.logger.Warn("cron job timed out", "agent", agentID, "timeout", timeout)
	} else {
		cs.logger.Info("cron job completed", "agent", agentID)
	}
}
