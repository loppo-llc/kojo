package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

const cronTimeout = 10 * time.Minute
const cronPrompt = "定期チェックの時間です。最近の記憶を振り返り、気づいたことや考えたことがあれば記録してください。"

// cronScheduler manages periodic agent executions.
type cronScheduler struct {
	mu      sync.Mutex
	c       *cron.Cron
	entries map[string]cron.EntryID // agent ID -> cron entry
	mgr     *Manager
	logger  *slog.Logger
}

func newCronScheduler(mgr *Manager, logger *slog.Logger) *cronScheduler {
	return &cronScheduler{
		c:       cron.New(),
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

func (cs *cronScheduler) runCronJob(agentID string) {
	cs.logger.Info("cron job triggered", "agent", agentID)

	ctx, cancel := context.WithTimeout(context.Background(), cronTimeout)
	defer cancel()

	events, err := cs.mgr.Chat(ctx, agentID, cronPrompt, "system")
	if err != nil {
		cs.logger.Warn("cron chat failed", "agent", agentID, "err", err)
		return
	}

	// Drain events (we don't stream cron results anywhere, just persist them)
	for range events {
	}

	cs.logger.Info("cron job completed", "agent", agentID)
}
