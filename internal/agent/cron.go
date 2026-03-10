package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

const cronTimeout = 10 * time.Minute
const cronMinInterval = 50 * time.Second // minimum interval between runs for same agent
const cronLockFile = ".cron_last"
func cronPrompt() string {
	now := time.Now()
	return "[system message] " + now.Format("2006年1月2日 15:04") + "の定期チェックインです。最近の出来事や気づきがあれば memory/" + now.Format("2006-01-02") + ".md に記録し、必要なタスクを実行してください。"
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

	// Check active hours
	if a, ok := cs.mgr.Get(agentID); ok {
		if !IsWithinActiveHours(a.ActiveStart, a.ActiveEnd) {
			cs.logger.Debug("cron job skipped (outside active hours)", "agent", agentID,
				"activeStart", a.ActiveStart, "activeEnd", a.ActiveEnd)
			return
		}
	}

	// Cross-process guard: atomic lock file prevents duplicate execution
	if !acquireCronLock(agentID) {
		cs.logger.Debug("cron job skipped (lock held)", "agent", agentID)
		return
	}

	cs.logger.Info("cron job triggered", "agent", agentID)

	ctx, cancel := context.WithTimeout(context.Background(), cronTimeout)
	defer cancel()

	events, err := cs.mgr.Chat(ctx, agentID, cronPrompt(), "system", nil)
	if err != nil {
		cs.logger.Warn("cron chat failed", "agent", agentID, "err", err)
		return
	}

	// Drain events (we don't stream cron results anywhere, just persist them)
	for range events {
	}

	cs.logger.Info("cron job completed", "agent", agentID)
}
