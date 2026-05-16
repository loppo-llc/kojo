package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/notifysource"
	"github.com/loppo-llc/kojo/internal/store"
)

const (
	notifyPollInterval  = 1 * time.Minute
	notifyRetryInterval = 3 * time.Minute
	notifyMaxRetries    = 3
	notifySrcTimeout    = 2 * time.Minute
	// notifyCursorDBTimeout caps the synchronous DB I/O the poller does
	// for cursor load/upsert/delete. Cursor writes happen on the polling
	// thread (cursor advance after a successful Poll, purge during
	// RebuildSources/RemoveAgent) so a stuck DB would otherwise block the
	// poll loop or a Manager mutation. 10s is generous for a single-row
	// upsert against a local SQLite — anything slower means the DB is
	// genuinely sick and we'd rather log+continue than hang.
	notifyCursorDBTimeout = 10 * time.Second
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
	cursors   map[string]string                         // "agentID:sourceID" → opaque cursor token
	// sourceTypes records the canonical cfg.Type for each in-memory
	// "agentID:sourceID" key. Stored separately from the cursor token
	// so the persist / purge paths can recompose the v1 DB primary key
	// "<agentID>:<cfg.Type>:<sourceID>" without re-walking the agent's
	// config — and without trusting src.Type() (which is set by the
	// source plugin's factory and could in principle disagree with
	// cfg.Type if a future plugin author misconfigured it). cfg.Type is
	// the single source of truth because the importer ALSO uses it to
	// compose ids in slice 8a (notify_cursors importer reads cfg.Type
	// from agents.json's NotifySources[].Type field).
	//
	// Populated on RebuildSources, cleared alongside cursors on the
	// purge paths. An entry can exist here without a matching `cursors`
	// entry (a configured source that has not yet returned its first
	// cursor, or one whose DB read failed) — that's the canonical
	// indicator of "known source, fresh start" vs. an unknown key.
	sourceTypes map[string]string
	lastPoll    map[string]time.Time // "agentID:sourceID" → last poll time
	// reloadFailed records (agentID:sourceID) keys whose DB cursor read
	// failed during the most recent RebuildSources. The poll loop skips
	// these entries to avoid re-delivering already-seen items: a fresh
	// poll with an empty cursor would make the source plugin treat it
	// as "fresh start" and re-fetch from the beginning. The flag is
	// cleared on the next successful reload via reloadCursorsForAgentLocked.
	reloadFailed map[string]bool
	pending      []pendingNotify
	logger       *slog.Logger
	stopCh       chan struct{}
	stopCtx      context.Context    // cancelled on Stop(); parent for delivery goroutines
	stopFn       context.CancelFunc // cancels stopCtx
	sendWg       sync.WaitGroup     // tracks in-flight sendSystemMessage goroutines
	done         chan struct{}
}

func newNotifyPoller(mgr *Manager, logger *slog.Logger) *notifyPoller {
	ctx, cancel := context.WithCancel(context.Background())
	return &notifyPoller{
		mgr:          mgr,
		factories:    make(map[string]notifysource.Factory),
		sources:      make(map[string]map[string]notifysource.Source),
		cursors:      make(map[string]string),
		sourceTypes:  make(map[string]string),
		lastPoll:     make(map[string]time.Time),
		reloadFailed: make(map[string]bool),
		logger:       logger,
		stopCh:       make(chan struct{}),
		stopCtx:      ctx,
		stopFn:       cancel,
		done:         make(chan struct{}),
	}
}

// notifyCursorDBID composes the v1 notify_cursors primary key from
// (agentID, source, sourceID). Mirrors the importer's composition
// (internal/migrate/importers/notify_cursors.go) so a row inserted by
// the v0→v1 importer is read back by the live poller using the same id.
func notifyCursorDBID(agentID, source, sourceID string) string {
	return agentID + ":" + source + ":" + sourceID
}

// store returns the SQL store handle, or nil if the manager has none
// (e.g. tests that construct a *Manager via &Manager{} without wiring
// AgentStore). Callers MUST nil-check; the live runtime always has a
// store, but per-process tests may not.
func (p *notifyPoller) store() *store.Store {
	if p == nil || p.mgr == nil {
		return nil
	}
	return p.mgr.Store()
}

// dbCtx returns a bounded child of stopCtx for synchronous cursor I/O.
// Caller MUST defer the returned cancel. Using stopCtx as the parent
// means a Manager.Close (which cancels stopCtx via Stop) aborts pending
// cursor writes promptly instead of letting them block shutdown.
func (p *notifyPoller) dbCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(p.stopCtx, notifyCursorDBTimeout)
}

// RegisterFactory registers a factory for a source type.
func (p *notifyPoller) RegisterFactory(sourceType string, f notifysource.Factory) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.factories[sourceType] = f
}

// Start begins the polling loop. Cursor state is loaded lazily per
// agent via RebuildSources rather than up-front: the manager calls
// RebuildSources for every active (non-archived) agent right after
// Start(), which is when we know which (agentID, sourceType, sourceID)
// triples to fetch from notify_cursors. An archived agent's cursors
// stay on disk untouched until Unarchive triggers RebuildSources.
func (p *notifyPoller) Start() {
	go p.loop()
}

// Stop stops the polling loop, cancels in-flight delivery goroutines, and
// waits for everything to finish.
func (p *notifyPoller) Stop() {
	p.stopFn() // cancel all in-flight sendSystemMessage goroutines
	close(p.stopCh)
	<-p.done
	p.sendWg.Wait() // wait for delivery goroutines to return
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
	// activeTypes records the source type for each kept sourceID so the
	// post-build cursor reload picks the right DB id (recall the in-memory
	// key is "agentID:sourceID" — type is needed only at the DB boundary).
	activeTypes := make(map[string]string)
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
		activeTypes[cfg.ID] = cfg.Type
	}

	// Purge state for sources that are no longer active. Done BEFORE
	// seeding sourceTypes for the new active set so the purge sees the
	// previous run's types (needed to recompose DB ids) rather than the
	// new run's. keepKeys is the in-memory key shape "agentID:sourceID".
	activeKeys := make(map[string]bool)
	for id := range agentSources {
		activeKeys[agentID+":"+id] = true
	}
	p.purgeKeysLocked(agentID, activeKeys)

	if len(agentSources) > 0 {
		p.sources[agentID] = agentSources
	}

	// Seed / refresh sourceTypes for the active set BEFORE the cursor
	// reload so any DB read failure can still be retried later (the
	// type is needed to recompose the DB id). A type change for the
	// same sourceID drops the prior cursor — the new type's source
	// plugin wouldn't understand the old token anyway, and the stale
	// DB row at the previous type id has just been swept by
	// purgeKeysLocked above.
	for sourceID, sourceType := range activeTypes {
		key := agentID + ":" + sourceID
		if prev, ok := p.sourceTypes[key]; ok && prev != sourceType {
			delete(p.cursors, key)
			delete(p.lastPoll, key)
		}
		p.sourceTypes[key] = sourceType
	}

	// Refresh in-memory cursor state from the DB for each retained
	// source. Done unconditionally so a manual SQL update (e.g. operator
	// rewinding a cursor) is picked up at the next config touch.
	p.reloadCursorsForAgentLocked(agentID, activeTypes)
}

// RemoveAgent removes all sources and state for an agent. Unlike
// DetachAgent (archive path), this is destructive: the agent itself is
// gone, so persisted cursors are no longer meaningful and the DB rows
// are purged en masse to prevent orphans. Best-effort: a DB error is
// logged but not returned — the eventual operator-driven hard-delete
// pass (planned `--clean` target; not yet implemented) mops up any
// survivors.
//
// In-memory purge goes through purgeAgentMemoryStateLocked (NOT
// purgeKeysLocked) so we don't issue redundant per-row deletes that
// the subsequent DeleteNotifyCursorsByAgent would re-do — the
// agent-wide DELETE is one round-trip vs. N round-trips per cursor,
// and it ALSO catches DB rows that never made it into the in-memory
// cache (e.g. an archived agent whose cursors were never reloaded
// because RebuildSources was never called).
func (p *notifyPoller) RemoveAgent(agentID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.sources, agentID)
	p.purgeAgentMemoryStateLocked(agentID)

	if st := p.store(); st != nil {
		ctx, cancel := p.dbCtx()
		defer cancel()
		if _, err := st.DeleteNotifyCursorsByAgent(ctx, agentID); err != nil {
			// Best-effort. The in-memory state is already gone, so the
			// runtime is correct; only the on-disk row leak is at risk.
			p.logger.Warn("failed to delete notify cursors for removed agent", "agent", agentID, "err", err)
		}
	}
}

// DetachAgent stops polling for an agent's sources but preserves cursors and
// lastPoll so that resuming via RebuildSources later doesn't replay
// already-delivered notifications. Used by Manager.Archive: archived agents
// are dormant, but their progress markers stay valid for when they wake up.
// Pending retry deliveries for the agent are dropped — the agent can't
// receive them while archived.
func (p *notifyPoller) DetachAgent(agentID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.sources, agentID)

	filtered := p.pending[:0]
	for _, pn := range p.pending {
		if pn.agentID == agentID {
			continue
		}
		filtered = append(filtered, pn)
	}
	p.pending = filtered
}

// purgeAgentStateLocked removes all cursors, lastPoll, and pending for an agent.
// Issues per-row DB deletes for whatever cursors are in memory.
// Caller must hold p.mu.
//
// Used by RebuildSources's "no-config / no-creds" early-out path. For
// the agent-deletion path (RemoveAgent), use purgeAgentMemoryStateLocked
// followed by DeleteNotifyCursorsByAgent — that combo also wipes DB
// rows that never made it into the in-memory cache.
func (p *notifyPoller) purgeAgentStateLocked(agentID string) {
	p.purgeKeysLocked(agentID, nil)
}

// purgeAgentMemoryStateLocked drops all in-memory state for an agent
// WITHOUT touching the DB. Used by RemoveAgent immediately before
// issuing a single agent-wide DeleteNotifyCursorsByAgent — the bulk
// DELETE is faster than N individual DeleteNotifyCursor calls and
// catches uncached rows too. Caller must hold p.mu.
func (p *notifyPoller) purgeAgentMemoryStateLocked(agentID string) {
	prefix := agentID + ":"
	for k := range p.cursors {
		if strings.HasPrefix(k, prefix) {
			delete(p.cursors, k)
		}
	}
	for k := range p.sourceTypes {
		if strings.HasPrefix(k, prefix) {
			delete(p.sourceTypes, k)
		}
	}
	for k := range p.lastPoll {
		if strings.HasPrefix(k, prefix) {
			delete(p.lastPoll, k)
		}
	}
	for k := range p.reloadFailed {
		if strings.HasPrefix(k, prefix) {
			delete(p.reloadFailed, k)
		}
	}
	filtered := p.pending[:0]
	for _, pn := range p.pending {
		if pn.agentID == agentID {
			continue
		}
		filtered = append(filtered, pn)
	}
	p.pending = filtered
}

// purgeKeysLocked removes cursors/lastPoll/pending entries for agentID.
// If keepKeys is non-nil, only entries NOT in keepKeys are removed.
// If keepKeys is nil, all entries for the agent are removed.
// Caller must hold p.mu.
//
// In-memory state (cursors / sourceTypes / lastPoll / reloadFailed /
// pending) is purged first; DB rows are deleted afterwards in two
// passes:
//
//  1. Per-key DELETE for entries we know about (using the cached
//     cfg.Type from sourceTypes to recompose the DB id).
//  2. List-and-diff against the agent's current DB rows so any rows
//     we *don't* know about (e.g. rows whose reload failed earlier
//     this run, or rows from a previous boot whose source has been
//     removed from config and never made it into memory) are also
//     removed when keepKeys is non-nil. Without this second pass the
//     archived-then-config-edited path would leak DB rows until the
//     agent itself is deleted.
//
// Both DB passes are best-effort: a transient SQL error logs and the
// purge continues, leaving in-memory state already consistent. `kojo
// clean` mops up survivors.
func (p *notifyPoller) purgeKeysLocked(agentID string, keepKeys map[string]bool) {
	prefix := agentID + ":"
	st := p.store()

	type pendingDelete struct{ id, source string }
	var dbDeletes []pendingDelete

	for k := range p.cursors {
		if strings.HasPrefix(k, prefix) && !keepKeys[k] {
			sourceID := strings.TrimPrefix(k, prefix)
			// Read the canonical cfg.Type from sourceTypes — that's the
			// id the importer would have used and the same value
			// reloadCursorsForAgentLocked seeded into sourceTypes during
			// the most recent RebuildSources. Falling back to "" would
			// produce a malformed DB id (":sourceID") that DeleteNotifyCursor
			// rejects, so a missing sourceTypes entry means we can't safely
			// issue the DB delete; log and skip in that case.
			sourceType, ok := p.sourceTypes[k]
			if !ok || sourceType == "" {
				p.logger.Warn("purge: missing source type for cursor key, skipping DB delete (drift detection: in-memory cursor without recorded type)",
					"agent", agentID, "key", k)
			} else {
				dbDeletes = append(dbDeletes, pendingDelete{
					id:     notifyCursorDBID(agentID, sourceType, sourceID),
					source: sourceType,
				})
			}
			delete(p.cursors, k)
		}
	}
	for k := range p.sourceTypes {
		if strings.HasPrefix(k, prefix) && !keepKeys[k] {
			delete(p.sourceTypes, k)
		}
	}
	for k := range p.lastPoll {
		if strings.HasPrefix(k, prefix) && !keepKeys[k] {
			delete(p.lastPoll, k)
		}
	}
	for k := range p.reloadFailed {
		if strings.HasPrefix(k, prefix) && !keepKeys[k] {
			delete(p.reloadFailed, k)
		}
	}

	filtered := p.pending[:0]
	for _, pn := range p.pending {
		if pn.agentID == agentID && !keepKeys[agentID+":"+pn.sourceID] {
			continue
		}
		filtered = append(filtered, pn)
	}
	p.pending = filtered

	// Defer DB deletes until after we've mutated the in-memory state so
	// a slow DB doesn't extend the time the lock is held doing pure
	// memory work. The poll loop / RebuildSources callers all take p.mu
	// so the lock is held during the DB calls — that's intentional:
	// a concurrent RebuildSources mid-purge could otherwise race the
	// active-key reload against the delete.
	if st == nil {
		return
	}
	ctx, cancel := p.dbCtx()
	defer cancel()
	for _, d := range dbDeletes {
		if err := st.DeleteNotifyCursor(ctx, d.id); err != nil {
			p.logger.Warn("failed to delete notify cursor row",
				"agent", agentID, "source", d.source, "id", d.id, "err", err)
		}
	}

	// keepKeys == nil means "full-agent purge" — wipe every DB cursor
	// for this agent, including rows that never made it into the
	// in-memory cache (e.g. archived agent unarchived → notify config
	// cleared before any successful reload). The bulk DELETE inside
	// DeleteNotifyCursorsByAgent fires per-row delete events for live
	// rows so peers / WS subscribers see the same "removed" stream
	// they would have from individual DeleteNotifyCursor calls.
	//
	// Note: RemoveAgent reaches this purge path indirectly only via
	// RebuildSources's no-config branch (when an agent's notify
	// configs are emptied without deleting the agent itself). The
	// dedicated RemoveAgent codepath bypasses purgeKeysLocked
	// entirely (see purgeAgentMemoryStateLocked + the explicit
	// DeleteNotifyCursorsByAgent it issues), so this branch is the
	// safety net for the in-flight reconfiguration case rather than
	// the agent-deletion case.
	if keepKeys == nil {
		if _, err := st.DeleteNotifyCursorsByAgent(ctx, agentID); err != nil {
			p.logger.Warn("failed to bulk-delete notify cursors during full purge",
				"agent", agentID, "err", err)
		}
		return
	}
	live, err := st.ListNotifyCursorsByAgent(ctx, agentID)
	if err != nil {
		p.logger.Warn("failed to list notify cursors for orphan diff",
			"agent", agentID, "err", err)
		return
	}
	for _, rec := range live {
		// Recompose the in-memory key from the DB row's source/id —
		// rec.ID is "<agentID>:<source>:<sourceID>", so the suffix
		// after the agent prefix gives "<source>:<sourceID>". Strip the
		// "<source>:" head to get the in-memory sourceID. We trust
		// rec.Source as the type because it's what the row actually
		// stores (any drift between cfg.Type and rec.Source would be a
		// migration bug we want surfaced, not silently masked).
		sourceID := strings.TrimPrefix(strings.TrimPrefix(rec.ID, prefix), rec.Source+":")
		if sourceID == "" || sourceID == rec.ID {
			// Defensive: a row whose id doesn't start with our prefix
			// shouldn't have come back from a WHERE agent_id = ?
			// query, but log + skip rather than mis-delete.
			p.logger.Warn("orphan diff: unexpected cursor id shape",
				"agent", agentID, "id", rec.ID, "source", rec.Source)
			continue
		}
		key := agentID + ":" + sourceID
		if keepKeys[key] {
			continue
		}
		if err := st.DeleteNotifyCursor(ctx, rec.ID); err != nil {
			p.logger.Warn("failed to delete orphan notify cursor row",
				"agent", agentID, "source", rec.Source, "id", rec.ID, "err", err)
		}
	}
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
		if IsInSilentHours(a.SilentStart, a.SilentEnd) {
			continue
		}

		// Find the matching interval config for each source
		for sourceID, src := range agentSources {
			key := agentID + ":" + sourceID
			// Skip sources whose DB cursor read failed in the most
			// recent RebuildSources: polling with the empty/stale
			// in-memory cursor would make the source plugin re-fetch
			// from the beginning, which is strictly worse than
			// pausing this source until its next RebuildSources
			// successfully reloads it. Cleared by reloadCursorsForAgentLocked
			// on the next config touch (or by RegisterFactory + the
			// next agent config edit).
			if p.reloadFailed[key] {
				continue
			}
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

	// If the agent was archived (or its source was removed / re-attached as
	// a fresh instance via RebuildSources) while we were polling, skip both
	// the cursor advance and the delivery: the items we just fetched will
	// be redelivered from the same cursor on the next poll, instead of
	// being silently dropped or attributed to a now-replaced source.
	if a, ok := p.mgr.Get(agentID); !ok || a.Archived {
		p.mu.Lock()
		p.lastPoll[key] = time.Now()
		p.mu.Unlock()
		return
	}
	p.mu.Lock()
	current, stillAttached := p.sources[agentID][sourceID]
	// Compare the actual source pointer, not just the key: an Archive →
	// Unarchive cycle (or RebuildSources after a config change) replaces
	// the source instance under the same key, and an in-flight poll on the
	// old instance must not advance the new instance's cursor.
	if !stillAttached || current != src {
		p.lastPoll[key] = time.Now()
		p.mu.Unlock()
		return
	}
	p.lastPoll[key] = time.Now()
	if result.Cursor != "" {
		// Use cfg.Type from sourceTypes (canonical) rather than
		// src.Type() so persisted DB ids agree with the importer's
		// composition. Falling back to src.Type() if the entry is
		// missing handles the unlikely race where the source was
		// removed and re-added between RebuildSources and pollSource —
		// the empty fallback would still produce a parseable id, just
		// possibly under a different type than RebuildSources expected,
		// which the next reload will reconcile.
		sourceType := p.sourceTypes[key]
		if sourceType == "" {
			sourceType = src.Type()
			p.logger.Warn("pollSource: missing sourceTypes entry, falling back to src.Type()",
				"agent", agentID, "sourceID", sourceID, "fallback", sourceType)
			p.sourceTypes[key] = sourceType
		}
		p.cursors[key] = result.Cursor
		p.persistCursorLocked(agentID, sourceType, sourceID, result.Cursor)
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

	p.sendWg.Add(1)
	go func() {
		defer p.sendWg.Done()
		p.sendSystemMessage(agentID, sourceID, message, 0)
	}()
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
		// Re-check silent hours at retry time — the notification may
		// have been queued before the agent entered silent hours.
		// Requeue without consuming the retry budget so messages are
		// not permanently lost during long silent windows.
		if a, ok := p.mgr.Get(pn.agentID); ok && IsInSilentHours(a.SilentStart, a.SilentEnd) {
			pn.nextAt = time.Now().Add(notifyRetryInterval)
			p.mu.Lock()
			p.pending = append(p.pending, pn)
			p.mu.Unlock()
			continue
		}
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
		p.sendWg.Add(1)
		go func() {
			defer p.sendWg.Done()
			p.sendSystemMessage(pn.agentID, pn.sourceID, pn.message, pn.retries)
		}()
	}
}

func (p *notifyPoller) sendSystemMessage(agentID, sourceID, message string, retries int) {
	ctx, cancel := context.WithTimeout(p.stopCtx, notifyTimeout)
	defer cancel()

	events, err := p.mgr.Chat(ctx, agentID, message, "system", nil, BusySourceNotification)
	if err != nil {
		// Archived is a terminal state: re-queueing would just spin on
		// retries until the cap. Drop without warn — DetachAgent already
		// preserves cursors so the message will not be redelivered after
		// Unarchive.
		if errors.Is(err, ErrAgentArchived) {
			p.logger.Debug("notification dropped: agent archived", "agent", agentID)
			return
		}
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
//
// The poller's persistence model is "DB is source of truth". The
// in-memory map is a write-through cache populated lazily per agent
// via reloadCursorsForAgentLocked at RebuildSources time, and
// invalidated on RemoveAgent / config changes via purgeKeysLocked.
// Cursor advances inside pollSource go straight to the DB through
// persistCursorLocked.

// reloadCursorsForAgentLocked refreshes p.cursors for one agent from
// notify_cursors. activeTypes maps each retained sourceID → its source
// type ("slack"|"gmail"|...) so the function can compute each row's
// composite primary key without a second config walk.
//
// Behaviour:
//   - For each (sourceID, type) in activeTypes, fetch the row at
//     "<agentID>:<type>:<sourceID>".
//   - Hit → populate p.cursors[agentID:sourceID] with the loaded token,
//     clear any prior reloadFailed flag.
//   - Miss (ErrNotFound) → drop any stale in-memory cursor under this
//     key (so a re-attached source doesn't replay an old token) and
//     clear reloadFailed: an absent DB row is a definitive "fresh
//     start", not a recoverable read failure.
//   - DB error (other than ErrNotFound) → log + mark reloadFailed.
//     The poll loop skips reloadFailed sources to avoid re-delivering
//     items with an empty cursor (see tick()'s reloadFailed check).
//
// Caller MUST hold p.mu — we mutate p.cursors / p.reloadFailed directly.
func (p *notifyPoller) reloadCursorsForAgentLocked(agentID string, activeTypes map[string]string) {
	st := p.store()
	if st == nil {
		// No DB wired (e.g. tests via &Manager{}). Clear stale
		// reloadFailed flags so a subsequent RegisterFactory + tick
		// doesn't pause the poll forever, but otherwise leave cursors
		// untouched.
		for sourceID := range activeTypes {
			delete(p.reloadFailed, agentID+":"+sourceID)
		}
		return
	}
	ctx, cancel := p.dbCtx()
	defer cancel()

	for sourceID, sourceType := range activeTypes {
		key := agentID + ":" + sourceID
		id := notifyCursorDBID(agentID, sourceType, sourceID)
		rec, err := st.GetNotifyCursor(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Fresh source: drop any stale in-memory entry under this
				// key so a re-attached source doesn't replay against a
				// cursor from a deleted-then-recreated row.
				delete(p.cursors, key)
				delete(p.reloadFailed, key)
				continue
			}
			p.logger.Warn("failed to load notify cursor (poll paused for this source until next reload)",
				"agent", agentID, "source", sourceType, "id", id, "err", err)
			p.reloadFailed[key] = true
			continue
		}
		p.cursors[key] = rec.Cursor
		delete(p.reloadFailed, key)
	}
}

// persistCursorLocked writes a single cursor advance to notify_cursors.
// Caller MUST hold p.mu (the in-memory map was just updated and we
// want the DB write to land in the same critical section so a
// concurrent RebuildSources sees a consistent view of memory + DB).
//
// Best-effort: a DB error is logged but doesn't roll back the in-memory
// cursor advance — the next successful poll will retry the upsert with
// an even fresher cursor token, which is strictly better than treating
// the cursor as un-advanced and re-fetching the same items.
func (p *notifyPoller) persistCursorLocked(agentID, sourceType, sourceID, cursor string) {
	st := p.store()
	if st == nil {
		return
	}
	ctx, cancel := p.dbCtx()
	defer cancel()

	rec := &store.NotifyCursorRecord{
		ID:      notifyCursorDBID(agentID, sourceType, sourceID),
		Source:  sourceType,
		AgentID: &agentID,
		Cursor:  cursor,
	}
	if err := st.UpsertNotifyCursor(ctx, rec, store.NotifyCursorInsertOptions{}); err != nil {
		p.logger.Warn("failed to persist notify cursor",
			"agent", agentID, "source", sourceType, "id", rec.ID, "err", err)
	}
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
