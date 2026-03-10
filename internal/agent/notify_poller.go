package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/notifysource"
)

const (
	notifyPollInterval  = 1 * time.Minute
	notifyRetryInterval = 3 * time.Minute
	notifyMaxRetries    = 3
	notifySrcTimeout    = 2 * time.Minute
	notifyCursorFile    = "notify_cursors.json"
)

// pendingNotify represents a notification waiting to be delivered to a busy agent.
type pendingNotify struct {
	agentID  string
	sourceID string
	message  string
	retries  int
	nextAt   time.Time
}

// notifyPoller periodically polls notification sources for each agent.
type notifyPoller struct {
	mu        sync.Mutex
	mgr       *Manager
	factories map[string]notifysource.Factory
	sources   map[string]map[string]notifysource.Source // agentID → sourceID → Source
	cursors   map[string]string                         // "agentID:sourceID" → cursor
	lastPoll  map[string]time.Time                      // "agentID:sourceID" → last poll time
	pending   []pendingNotify
	logger    *slog.Logger
	stopCh    chan struct{}
	done      chan struct{}
}

func newNotifyPoller(mgr *Manager, logger *slog.Logger) *notifyPoller {
	return &notifyPoller{
		mgr:       mgr,
		factories: make(map[string]notifysource.Factory),
		sources:   make(map[string]map[string]notifysource.Source),
		cursors:   make(map[string]string),
		lastPoll:  make(map[string]time.Time),
		logger:    logger,
		stopCh:    make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// RegisterFactory registers a factory for a source type.
func (p *notifyPoller) RegisterFactory(sourceType string, f notifysource.Factory) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.factories[sourceType] = f
}

// Start begins the polling loop.
func (p *notifyPoller) Start() {
	p.loadCursors()
	go p.loop()
}

// Stop stops the polling loop and waits for completion.
func (p *notifyPoller) Stop() {
	close(p.stopCh)
	<-p.done
}

// RebuildSources rebuilds source instances for an agent from its config.
func (p *notifyPoller) RebuildSources(agentID string, configs []notifysource.Config) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Remove old sources
	delete(p.sources, agentID)

	// Purge all state if no configs or no credential store
	if len(configs) == 0 || p.mgr.creds == nil {
		p.purgeAgentStateLocked(agentID)
		return
	}

	agentSources := make(map[string]notifysource.Source)
	for _, cfg := range configs {
		if !cfg.Enabled {
			continue
		}
		factory, ok := p.factories[cfg.Type]
		if !ok {
			p.logger.Warn("unknown notify source type", "type", cfg.Type, "agent", agentID)
			continue
		}
		ta := &tokenAccessorImpl{
			store:    p.mgr.creds,
			provider: cfg.Type,
			agentID:  agentID,
			sourceID: cfg.ID,
		}
		src, err := factory(cfg, ta)
		if err != nil {
			p.logger.Warn("failed to create notify source", "type", cfg.Type, "agent", agentID, "err", err)
			continue
		}
		agentSources[cfg.ID] = src
	}

	// Purge state for sources that are no longer active
	prefix := agentID + ":"
	activeKeys := make(map[string]bool)
	for id := range agentSources {
		activeKeys[prefix+id] = true
	}
	for k := range p.cursors {
		if strings.HasPrefix(k, prefix) && !activeKeys[k] {
			delete(p.cursors, k)
		}
	}
	for k := range p.lastPoll {
		if strings.HasPrefix(k, prefix) && !activeKeys[k] {
			delete(p.lastPoll, k)
		}
	}
	// Remove pending notifications for removed sources (match by sourceID)
	filtered := p.pending[:0]
	for _, pn := range p.pending {
		if pn.agentID == agentID {
			if _, ok := agentSources[pn.sourceID]; !ok {
				continue // source was removed
			}
		}
		filtered = append(filtered, pn)
	}
	p.pending = filtered

	if len(agentSources) > 0 {
		p.sources[agentID] = agentSources
	}

	p.saveCursorsLocked()
}

// RemoveAgent removes all sources and state for an agent.
func (p *notifyPoller) RemoveAgent(agentID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.sources, agentID)
	p.purgeAgentStateLocked(agentID)
}

// purgeAgentStateLocked removes all cursors, lastPoll, and pending for an agent.
// Caller must hold p.mu.
func (p *notifyPoller) purgeAgentStateLocked(agentID string) {
	prefix := agentID + ":"
	for k := range p.cursors {
		if strings.HasPrefix(k, prefix) {
			delete(p.cursors, k)
		}
	}
	for k := range p.lastPoll {
		if strings.HasPrefix(k, prefix) {
			delete(p.lastPoll, k)
		}
	}

	filtered := p.pending[:0]
	for _, pn := range p.pending {
		if pn.agentID != agentID {
			filtered = append(filtered, pn)
		}
	}
	p.pending = filtered

	p.saveCursorsLocked()
}

func (p *notifyPoller) loop() {
	defer close(p.done)

	ticker := time.NewTicker(notifyPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.tick()
		}
	}
}

func (p *notifyPoller) tick() {
	// Process retries first
	p.processRetries()

	// Check global pause
	if p.mgr.CronPaused() {
		return
	}

	p.mu.Lock()
	// Snapshot source configs
	type pollTask struct {
		agentID  string
		sourceID string
		source   notifysource.Source
	}
	var tasks []pollTask

	for agentID, agentSources := range p.sources {
		// Get agent config for active hours check
		a, ok := p.mgr.Get(agentID)
		if !ok {
			continue
		}
		if !IsWithinActiveHours(a.ActiveStart, a.ActiveEnd) {
			continue
		}

		// Find the matching interval config for each source
		for sourceID, src := range agentSources {
			key := agentID + ":" + sourceID
			interval := p.getSourceIntervalLocked(a, sourceID)
			if interval <= 0 {
				continue
			}
			if last, ok := p.lastPoll[key]; ok {
				if time.Since(last) < time.Duration(interval)*time.Minute {
					continue
				}
			}
			tasks = append(tasks, pollTask{agentID, sourceID, src})
		}
	}
	p.mu.Unlock()

	// Execute polls outside the lock
	for _, t := range tasks {
		p.pollSource(t.agentID, t.sourceID, t.source)
	}
}

func (p *notifyPoller) getSourceIntervalLocked(a *Agent, sourceID string) int {
	for _, cfg := range a.NotifySources {
		if cfg.ID == sourceID {
			return cfg.IntervalMinutes
		}
	}
	return 0
}

func (p *notifyPoller) pollSource(agentID, sourceID string, src notifysource.Source) {
	key := agentID + ":" + sourceID

	p.mu.Lock()
	cursor := p.cursors[key]
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), notifySrcTimeout)
	defer cancel()

	result, err := src.Poll(ctx, cursor)
	if err != nil {
		p.logger.Warn("notify poll failed", "agent", agentID, "source", sourceID, "err", err)
		// Update lastPoll even on failure to avoid hammering
		p.mu.Lock()
		p.lastPoll[key] = time.Now()
		p.mu.Unlock()
		return
	}

	p.mu.Lock()
	p.lastPoll[key] = time.Now()
	if result.Cursor != "" {
		p.cursors[key] = result.Cursor
		p.saveCursorsLocked()
	}
	p.mu.Unlock()

	if len(result.Items) == 0 {
		return
	}

	// Build notification message
	msg := formatNotifyMessage(src.Type(), result.Items)
	p.deliverToAgent(agentID, sourceID, msg)
}

func (p *notifyPoller) deliverToAgent(agentID, sourceID, message string) {
	if p.mgr.IsBusy(agentID) {
		p.mu.Lock()
		p.pending = append(p.pending, pendingNotify{
			agentID:  agentID,
			sourceID: sourceID,
			message:  message,
			retries:  0,
			nextAt:   time.Now().Add(notifyRetryInterval),
		})
		p.mu.Unlock()
		p.logger.Debug("agent busy, queued notification for retry", "agent", agentID)
		return
	}

	p.sendSystemMessage(agentID, sourceID, message, 0)
}

func (p *notifyPoller) processRetries() {
	p.mu.Lock()
	now := time.Now()
	var remaining []pendingNotify
	var ready []pendingNotify

	for _, pn := range p.pending {
		if now.Before(pn.nextAt) {
			remaining = append(remaining, pn)
			continue
		}
		ready = append(ready, pn)
	}
	p.pending = remaining
	p.mu.Unlock()

	for _, pn := range ready {
		if p.mgr.IsBusy(pn.agentID) {
			pn.retries++
			if pn.retries >= notifyMaxRetries {
				p.logger.Warn("notification dropped after max retries", "agent", pn.agentID)
				continue
			}
			pn.nextAt = time.Now().Add(notifyRetryInterval)
			p.mu.Lock()
			p.pending = append(p.pending, pn)
			p.mu.Unlock()
			continue
		}
		p.sendSystemMessage(pn.agentID, pn.sourceID, pn.message, pn.retries)
	}
}

func (p *notifyPoller) sendSystemMessage(agentID, sourceID, message string, retries int) {
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()

	events, err := p.mgr.Chat(ctx, agentID, message, "system", nil)
	if err != nil {
		retries++
		if retries >= notifyMaxRetries {
			p.logger.Warn("notification dropped after max retries", "agent", agentID)
			return
		}
		p.logger.Warn("failed to send notification, queuing retry", "agent", agentID, "retries", retries, "err", err)
		p.mu.Lock()
		p.pending = append(p.pending, pendingNotify{
			agentID:  agentID,
			sourceID: sourceID,
			message:  message,
			retries:  retries,
			nextAt:   time.Now().Add(notifyRetryInterval),
		})
		p.mu.Unlock()
		return
	}

	// Drain events
	for range events {
	}

	p.logger.Info("notification delivered to agent", "agent", agentID)
}

// cursor persistence

func (p *notifyPoller) loadCursors() {
	path := filepath.Join(configDir(), notifyCursorFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := json.Unmarshal(data, &p.cursors); err != nil {
		p.logger.Warn("failed to parse notify cursors", "path", path, "err", err)
	}
}

func (p *notifyPoller) saveCursorsLocked() {
	path := filepath.Join(configDir(), notifyCursorFile)
	data, err := json.Marshal(p.cursors)
	if err != nil {
		p.logger.Warn("failed to marshal notify cursors", "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		p.logger.Warn("failed to create cursor dir", "err", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		p.logger.Warn("failed to write notify cursors", "path", path, "err", err)
	}
}

func configDir() string {
	return kojoConfigDir()
}

// formatNotifyMessage builds a system message from notification items.
func formatNotifyMessage(sourceType string, items []notifysource.Notification) string {
	var b strings.Builder
	now := time.Now().Format("2006-01-02 15:04 -0700 MST")
	switch sourceType {
	case "gmail":
		fmt.Fprintf(&b, "[system] 新着メール (%d件, 現在時刻: %s):\n", len(items), now)
	default:
		fmt.Fprintf(&b, "[system] 新着通知 (%d件, %s, 現在時刻: %s):\n", len(items), sourceType, now)
	}
	for _, item := range items {
		if !item.ReceivedAt.IsZero() {
			fmt.Fprintf(&b, "- [%s] %s", item.ReceivedAt.Local().Format("2006-01-02 15:04"), item.Title)
		} else {
			fmt.Fprintf(&b, "- %s", item.Title)
		}
		if item.Body != "" {
			fmt.Fprintf(&b, " / %s", item.Body)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// tokenAccessorImpl bridges the notifysource.TokenAccessor interface to CredentialStore.
type tokenAccessorImpl struct {
	store    *CredentialStore
	provider string
	agentID  string
	sourceID string
}

func (t *tokenAccessorImpl) GetToken(key string) (string, error) {
	return t.store.GetToken(t.provider, t.agentID, t.sourceID, key)
}

func (t *tokenAccessorImpl) SetToken(key, value string, expiresAt time.Time) error {
	return t.store.SetToken(t.provider, t.agentID, t.sourceID, key, value, expiresAt)
}

func (t *tokenAccessorImpl) GetTokenExpiry(key string) (string, time.Time, error) {
	return t.store.GetTokenExpiry(t.provider, t.agentID, t.sourceID, key)
}
