package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

const cronTimeout = 10 * time.Minute // default timeout; per-agent override via TimeoutMinutes
const cronMinInterval = 50 * time.Second // minimum interval between runs for same agent
const cronLockFile = ".cron_last"

func cronPrompt(nextRun time.Time, timeoutMinutes int) string {
	now := time.Now()
	msg := "[system message] " + now.Format("2006年1月2日 15:04") + "の定期チェックインです。"

	msg += fmt.Sprintf("（制限時間: %d分", timeoutMinutes)
	if !nextRun.IsZero() {
		var nextFmt string
		if nextRun.YearDay() == now.YearDay() && nextRun.Year() == now.Year() {
			nextFmt = nextRun.Format("15:04")
		} else {
			nextFmt = nextRun.Format("1月2日 15:04")
		}
		msg += "、次回予定: " + nextFmt
	}
	msg += "）"
	msg += "最近の出来事や気づきがあれば memory/" + now.Format("2006-01-02") + ".md に記録し、必要なタスクを実行してください。"
	return msg
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
// active hours. If the cron's Next falls outside the active window, it
// iterates the cron schedule forward to find the first tick inside the window.
// Returns zero time if no schedule exists.
func (cs *cronScheduler) nextRun(agentID string, activeStart, activeEnd string) time.Time {
	cs.mu.Lock()
	var entry cron.Entry
	if entryID, ok := cs.entries[agentID]; ok {
		entry = cs.c.Entry(entryID)
	}
	cs.mu.Unlock()

	if entry.Schedule == nil || entry.Next.IsZero() {
		return time.Time{}
	}

	if activeStart == "" || activeEnd == "" {
		return entry.Next
	}

	return nextRunInActiveWindow(entry.Schedule, entry.Next, activeStart, activeEnd)
}

// nextRunInActiveWindow finds the first cron tick at or after t that falls
// within the active hours window. Tries up to 48 iterations to avoid infinite loops.
func nextRunInActiveWindow(sched cron.Schedule, t time.Time, start, end string) time.Time {
	s, _ := time.Parse("15:04", start)
	e, _ := time.Parse("15:04", end)
	startMin := s.Hour()*60 + s.Minute()
	endMin := e.Hour()*60 + e.Minute()

	candidate := t
	for i := 0; i < 48; i++ {
		cMin := candidate.Hour()*60 + candidate.Minute()
		inWindow := false
		if startMin <= endMin {
			inWindow = cMin >= startMin && cMin < endMin
		} else {
			inWindow = cMin >= startMin || cMin < endMin
		}
		if inWindow {
			return candidate
		}
		candidate = sched.Next(candidate)
	}
	// Couldn't find one within 48 ticks; return raw next
	return t
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

	// Check active hours and read agent config
	var activeStart, activeEnd string
	var timeoutMinutes int
	if a, ok := cs.mgr.Get(agentID); ok {
		activeStart, activeEnd = a.ActiveStart, a.ActiveEnd
		timeoutMinutes = a.TimeoutMinutes
		if !IsWithinActiveHours(activeStart, activeEnd) {
			cs.logger.Debug("cron job skipped (outside active hours)", "agent", agentID,
				"activeStart", activeStart, "activeEnd", activeEnd)
			return
		}
	}

	// Cross-process guard: atomic lock file prevents duplicate execution
	if !acquireCronLock(agentID) {
		cs.logger.Debug("cron job skipped (lock held)", "agent", agentID)
		return
	}

	cs.logger.Info("cron job triggered", "agent", agentID)

	nextRun := cs.nextRun(agentID, activeStart, activeEnd)

	// Per-agent timeout (0 = use default)
	timeout := cronTimeout
	if timeoutMinutes > 0 {
		timeout = time.Duration(timeoutMinutes) * time.Minute
	}
	effectiveTimeoutMin := int(timeout / time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	events, err := cs.mgr.Chat(ctx, agentID, cronPrompt(nextRun, effectiveTimeoutMin), "system", nil)
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
