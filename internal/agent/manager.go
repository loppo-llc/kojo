package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/notifysource"
	"github.com/loppo-llc/kojo/internal/notifysource/gmail"
	"github.com/loppo-llc/kojo/internal/store"
)

// BusySource identifies what triggered a busy state.
type BusySource int

const (
	// BusySourceUser is an interactive chat initiated by the user.
	BusySourceUser BusySource = iota
	// BusySourceCron is a scheduled cron check-in.
	BusySourceCron
	// BusySourceNotification is an automated notification (group DM, etc.)
	// that should not surface as "busy" in member status displays.
	BusySourceNotification
)

type busyEntry struct {
	cancel      context.CancelFunc
	startedAt   time.Time
	broadcaster *chatBroadcaster // fan-out for reconnecting clients
	source      BusySource
}

// Manager manages agent CRUD, chat orchestration, and lifecycle.
type Manager struct {
	mu       sync.Mutex
	agents   map[string]*Agent
	backends map[string]ChatBackend
	store    *agentStore
	creds    *CredentialStore
	cron     *cronScheduler
	logger   *slog.Logger

	// groupdms is set after construction to avoid circular dependency.
	groupdms *GroupDMManager

	// busy tracks which agents have an active chat.
	busy   map[string]busyEntry
	busyMu sync.Mutex

	// resetting tracks agents currently being reset (blocks new chats).
	resetting map[string]bool

	// editing tracks agents whose transcript is being edited; blocks new
	// chats but is invisible to chat WebSocket subscribers.
	editing map[string]bool

	// switching tracks agents in the middle of a §3.7 device
	// switch (begin → sync → pull → complete). Blocks new
	// Chat starts on this peer so no transcript / JSONL gets
	// written between Step -1's quiesce and the post-complete
	// drain — without the gate a cron tick or WS frame mid-
	// switch would land state on source that target never
	// receives. Cleared by SetSwitching(false) on every exit
	// path of switch_device_handler.
	switching map[string]bool

	// mutating tracks the per-agent in-flight count of state
	// mutations (persona / settings / notify / slackbot / task
	// / credential / avatar / OAuth token) that don't route
	// through Chat → busy. Every mutation entry uses
	// AcquireMutation to bump the counter under busyMu (which
	// also refuses when switching is set), and defers
	// releaseMutation to bring it back down. WaitChatIdle
	// drains the counter so Step -1's snapshot can't race a
	// mutation that started just before SetSwitching landed.
	mutating map[string]int

	// preparing tracks agents whose Chat / ChatOneShot /
	// Regenerate is INSIDE prepareChat (memory sync, system
	// prompt build) but hasn't yet inserted its busy entry. A
	// SetSwitching(true) that lands during this window would
	// pass busy=empty in WaitChatIdle even though prepareChat
	// has already done disk writes. Step -1's quiesce now
	// waits for `preparing` to drain alongside busy, closing
	// that race. Incremented per call (a map[id]int) so
	// concurrent Slack one-shots don't clobber each other's
	// guards.
	preparing map[string]int

	// profileGen tracks agents with in-flight publicProfile generation.
	profileGen map[string]bool

	// cronPaused globally pauses all cron jobs when true.
	cronPaused bool

	// cronToggleMu serializes SetCronPaused calls. Without it two
	// concurrent toggles could interleave between the kv write and the
	// in-memory update — the order of kv writes (last-wins) and memory
	// writes (also last-wins) is independent, so a (false, true)
	// concurrent pair could land kv="true" but cronPaused=false.
	// Single-toggle granularity is fine: this is an operator-driven
	// path, no contention pressure.
	cronToggleMu sync.Mutex

	// memIndexes caches open MemoryIndex instances per agent.
	memIndexes   map[string]*MemoryIndex
	memIndexesMu sync.Mutex

	// notifyPoller polls external notification sources.
	notifyPoller *notifyPoller

	// OnChatDone is called when an agent finishes its response.
	OnChatDone func(agent *Agent, message *Message)

	// chatWatchers tracks per-agent channels notified when a new chat starts.
	chatWatchers   map[string]map[*chatWatcher]struct{}
	chatWatchersMu sync.Mutex

	// oneShotCancels tracks cancel functions for in-flight one-shot chats
	// (e.g. Slack) so they can be cleaned up on Shutdown or agent Delete.
	// Unlike busy (which allows only one chat per agent), multiple one-shot
	// chats may run concurrently for the same agent.
	// Keyed by a unique int64 ID since context.CancelFunc is not comparable.
	oneShotSeq       int64
	oneShotCancels   map[string]map[int64]context.CancelFunc // agentID → id → cancel
	oneShotCancelsMu sync.Mutex

	// tokenStore, if set, is kept in sync with agent lifecycle: a per-agent
	// token is created on Create/Fork and removed on Delete. The store is
	// owned by the auth subsystem; agent.Manager only calls into the
	// minimal AgentTokenStore interface.
	tokenStore AgentTokenStore

	// blobStore is the native blob store handle (Phase 3). Wired post-
	// construction via SetBlobStore so Manager doesn't have to import
	// internal/blob solely for the field type to be reachable. Avatar
	// reads (avatarMeta, ServeAvatar fallback) and writes (SaveAvatar,
	// fork copy, reset cleanup) consult this when non-nil; nil keeps
	// the read paths fail-soft (no avatar = SVG fallback) so tests that
	// don't wire blob still work.
	blobStore *blob.Store

	// patchMus serializes If-Match-gated mutations per agent. The HTTP
	// PATCH handler holds the per-agent lock across precondition-check
	// → Manager.Update → ETag echo so two concurrent requests carrying
	// the same If-Match cannot both pass the check (they'd otherwise
	// race because the precheck reads the store without holding m.mu,
	// and m.mu itself is released between Update's in-memory mutation
	// and m.save()'s store write).
	//
	// This is single-process serialization only — multi-device write
	// coordination is the store-level optimistic-concurrency layer's
	// job (see store.UpdateAgent's ifMatchETag parameter, which a
	// future cutover slice will thread through Manager.Update).
	patchMusMu sync.Mutex
	patchMus   map[string]*sync.Mutex

	// startSchedulersOnce guards StartSchedulers from double-firing
	// the cron / notify-poller boot. schedulersStarted gates
	// Shutdown so it short-circuits when the schedulers were never
	// started (the notifyPoller.Stop path would otherwise wait on
	// a never-launched goroutine).
	startSchedulersOnce sync.Once
	schedulersStarted   bool
}

// AgentTokenStore is the minimal contract the agent manager needs from
// the auth token store, narrowed so the agent package does not import
// internal/auth (avoids a layering cycle).
type AgentTokenStore interface {
	EnsureAgentToken(agentID string) error
	RemoveAgentToken(agentID string)
}

// SetTokenStore wires the per-agent token store. Calling this after
// agents have been loaded triggers token bootstrap for each existing
// agent (so a kojo upgrade gets every agent a token without an explicit
// migration step).
func (m *Manager) SetTokenStore(ts AgentTokenStore) {
	m.tokenStore = ts
	if ts == nil {
		return
	}
	m.mu.Lock()
	ids := make([]string, 0, len(m.agents))
	for id := range m.agents {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		if err := ts.EnsureAgentToken(id); err != nil {
			m.logger.Warn("failed to bootstrap agent token", "agent", id, "err", err)
		}
	}
}

// AgentTokenStore returns the wired-in token store (may be nil during
// tests or before SetTokenStore is called).
func (m *Manager) AgentTokenStore() AgentTokenStore { return m.tokenStore }

// SetBlobStore wires the native blob store handle. Called post-
// construction by cmd/kojo after the blob.Store is built (it depends
// on agentMgr.Store(), so the order is Manager → store → blob.Store
// → SetBlobStore). Tests that don't exercise avatar I/O may leave
// it nil; avatar read paths fall back to the generated-SVG branch
// when blobStore is nil so a Get on an agent in those tests still
// completes.
func (m *Manager) SetBlobStore(bs *blob.Store) {
	m.blobStore = bs
}

// BlobStore returns the wired blob store (may be nil during tests or
// before SetBlobStore is called). Mirrors Store() / AgentTokenStore()
// for subsystem composition.
func (m *Manager) BlobStore() *blob.Store { return m.blobStore }

// HydrateAgentBlobsAtLoad copies every loaded agent's blob_refs
// entries (global + local scope, excluding avatar / index /
// credentials) into agentDir(id) so the v1 CLI process — which
// runs with cmd.Dir = agentDir — can `ls books/`, `cat
// outputs/result.bvh`, etc. just like it could on v0. Without this
// step, post-migration agent CWDs would appear empty to the CLI
// even though the blob store has the files (under
// <configdir>/{global,local}/agents/<id>/ rather than
// agents/<id>/).
//
// Idempotent: existing leaves whose sha256 matches the blob_refs
// row are skipped, so re-runs at every Load are O(stat) per ref.
// Best-effort across agents: one agent's failure logs and the rest
// proceed.
//
// Must be called AFTER SetBlobStore. cmd/kojo wires this in
// initAgents() right after SetBlobStore so the first Chat picks up
// a populated CWD.
func (m *Manager) HydrateAgentBlobsAtLoad() {
	if m.blobStore == nil || m.store == nil {
		return
	}
	st := m.store.Store()
	if st == nil {
		return
	}
	m.mu.Lock()
	ids := make([]string, 0, len(m.agents))
	for id := range m.agents {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		hydrateAgentBlobsAtLoad(st, m.blobStore, id, m.logger)
	}
}

// IsPrivileged returns whether the agent has the Privileged flag set.
// Used by auth.Resolver to map a per-agent token to RoleAgent vs
// RolePrivAgent.
func (m *Manager) IsPrivileged(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	return ok && a.Privileged
}

// SetPrivileged toggles the Privileged flag on the named agent and
// persists the change. Owner-only mutation enforced at the API layer.
func (m *Manager) SetPrivileged(id string, privileged bool) error {
	releaseMut, err := m.AcquireMutation(id)
	if err != nil {
		return err
	}
	defer releaseMut()
	m.mu.Lock()
	a, ok := m.agents[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}
	if a.Privileged == privileged {
		m.mu.Unlock()
		return nil
	}
	a.Privileged = privileged
	a.UpdatedAt = time.Now().Format(time.RFC3339)
	m.mu.Unlock()
	m.save()
	return nil
}

// NewManager creates a new agent manager.
//
// Returns an error only if kojo.db cannot be opened — that path is
// fatal for the daemon (without the store there is no agent metadata
// at all), so callers should bubble the error to main and exit.
// Per-agent load failures are logged and skipped, not returned, so a
// single corrupt row doesn't prevent the rest of the daemon from
// coming up.
func NewManager(logger *slog.Logger) (*Manager, error) {
	creds, err := NewCredentialStore()
	if err != nil {
		logger.Warn("failed to open credential store", "err", err)
	}

	st, err := newStore(logger)
	if err != nil {
		return nil, fmt.Errorf("open agent store: %w", err)
	}

	m := &Manager{
		agents: make(map[string]*Agent),
		backends: map[string]ChatBackend{
			"claude":    NewClaudeBackend(logger),
			"codex":     NewCodexBackend(logger),
			"gemini":    NewGeminiBackend(logger),
			"custom":    NewCustomBackend(logger),
			"llama.cpp": NewLlamaCppBackend(logger),
		},
		store:          st,
		creds:          creds,
		logger:         logger,
		busy:           make(map[string]busyEntry),
		resetting:      make(map[string]bool),
		switching:      make(map[string]bool),
		preparing:      make(map[string]int),
		mutating:       make(map[string]int),
		editing:        make(map[string]bool),
		profileGen:     make(map[string]bool),
		memIndexes:     make(map[string]*MemoryIndex),
		chatWatchers:   make(map[string]map[*chatWatcher]struct{}),
		oneShotCancels: make(map[string]map[int64]context.CancelFunc),
		patchMus:       make(map[string]*sync.Mutex),
	}

	m.cron = newCronScheduler(m, logger)
	m.cronPaused = m.store.LoadCronPaused()

	// Initialize notify poller
	m.notifyPoller = newNotifyPoller(m, logger)
	m.notifyPoller.RegisterFactory("gmail", func(cfg notifysource.Config, tokens notifysource.TokenAccessor) (notifysource.Source, error) {
		return gmail.New(cfg, tokens)
	})

	// Load persisted agents
	agents, err := m.store.Load()
	if err != nil {
		logger.Warn("failed to load agents", "err", err)
	}
	for _, a := range agents {
		has, hash := m.avatarMeta(a.ID)
		applyAvatarMeta(a, has, hash)
		// Load last message preview
		if msgs, err := loadMessages(a.ID, 1); err == nil && len(msgs) > 0 {
			last := msgs[len(msgs)-1]
			a.LastMessage = &MessagePreview{
				Content:   truncatePreview(last.Content, 100),
				Role:      last.Role,
				Timestamp: last.Timestamp,
			}
		}
		m.agents[a.ID] = a
	}

	return m, nil
}

// StartSchedulers boots the cron loop and the notify poller and
// schedules every non-archived agent's interval / notify sources.
// Split out of NewManager (Phase G follow-up to codex review) so
// cmd/kojo can interpose Phase D's HydrateAgentBlobsAtLoad between
// agent load and scheduler start — without the gap, a cron tick
// that fires before hydrate finishes would spawn the CLI process
// in an empty CWD (books/, outputs/, etc. not yet on disk).
//
// Idempotent: a sync.Once guard pins the boot to a single execution
// regardless of how many callers race here, so a stray double Start
// can't double-launch the notify poller goroutine. Shutdown
// short-circuits when StartSchedulers never ran (would otherwise
// hang on notifyPoller.Stop's wait-for-loop-exit).
func (m *Manager) StartSchedulers() {
	if m == nil {
		return
	}
	m.startSchedulersOnce.Do(func() {
		// Cron: skip archived agents — their cron stays detached
		// until Unarchive re-schedules.
		m.cron.Start()
		m.mu.Lock()
		agents := make([]*Agent, 0, len(m.agents))
		for _, a := range m.agents {
			agents = append(agents, a)
		}
		m.mu.Unlock()
		for _, a := range agents {
			if a.Archived {
				continue
			}
			if expr := a.CronExpr; expr != "" {
				if err := m.cron.Schedule(a.ID, expr); err != nil {
					m.logger.Warn("failed to schedule cron", "agent", a.ID, "err", err)
				}
			}
		}

		// Notify poller: rebuild sources for all agents (skip archived).
		m.notifyPoller.Start()
		for _, a := range agents {
			if a.Archived {
				continue
			}
			if len(a.NotifySources) > 0 {
				m.notifyPoller.RebuildSources(a.ID, a.NotifySources)
			}
		}
		m.schedulersStarted = true
	})
}

// Close releases manager-owned resources (the kojo.db connection).
// Safe to call on a nil receiver and idempotent.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	return m.store.Close()
}

// Store returns the underlying *store.Store so subsystems built on the
// agent manager (groupdm manager, follow-up cutover slices) can share
// the connection rather than each opening their own *sql.DB. May be
// nil in tests that constructed a *Manager via &Manager{}.
func (m *Manager) Store() *store.Store {
	if m == nil || m.store == nil {
		return nil
	}
	return m.store.Store()
}

// SetGroupDMManager sets the group DM manager reference.
// Called after both managers are created to avoid circular dependency.
func (m *Manager) SetGroupDMManager(gdm *GroupDMManager) {
	m.groupdms = gdm
}

// Create creates a new agent.
func (m *Manager) Create(cfg AgentConfig) (*Agent, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("name is required")
	}

	a, err := newAgent(cfg)
	if err != nil {
		return nil, err
	}

	if err := ensureAgentDir(a); err != nil {
		return nil, fmt.Errorf("create agent dir: %w", err)
	}

	has, hash := m.avatarMeta(a.ID)
	applyAvatarMeta(a, has, hash)

	// Provision the auth token first. A tokenless agent is silently
	// broken (its $KOJO_AGENT_TOKEN env is unset, so any self-API
	// curl from the PTY hits the auth listener as Guest and 403s).
	// Better to fail Create loudly than hand back a half-wired agent.
	if m.tokenStore != nil {
		if err := m.tokenStore.EnsureAgentToken(a.ID); err != nil {
			// Roll back the agent dir we created above.
			_ = os.RemoveAll(agentDir(a.ID))
			return nil, fmt.Errorf("provision agent token: %w", err)
		}
	}

	m.mu.Lock()
	m.agents[a.ID] = a
	m.mu.Unlock()

	m.save()

	// Sync the freshly-minted MEMORY.md / memory/ tree into the DB.
	// ensureAgentDir already wrote the initial MEMORY.md, but the
	// inline sync there was a no-op because the parent agent row
	// didn't exist yet (m.save above is what creates it). Rerun
	// here so the DB row is populated from the start of the agent's
	// life — without it, the first cross-device reader would 404.
	// Best-effort: a sync failure is logged but doesn't abort
	// Create (the file is canonical regardless; the next sync hook
	// will reconcile).
	if st := getGlobalStore(); st != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := SyncAgentMemoryFromDisk(ctx, st, a.ID, m.logger); err != nil {
			m.logger.Warn("memory sync after create failed", "agent", a.ID, "err", err)
		}
		cancel()
	}

	if expr := a.CronExpr; expr != "" {
		if err := m.cron.Schedule(a.ID, expr); err != nil {
			m.logger.Warn("failed to schedule cron", "agent", a.ID, "err", err)
		}
	}

	m.logger.Info("agent created", "id", a.ID, "name", a.Name)

	// Generate public profile in background
	if a.Persona != "" {
		go m.regeneratePublicProfile(a.ID, a.Persona)
	}

	return a, nil
}

// syncPersona reconciles the on-disk persona.md with the in-memory
// Agent.Persona and the DB row. Post-cutover the DB is canonical;
// disk is a hydrated mirror. The CLI process can edit persona.md and
// this function picks up that edit on the next chat-done /
// scheduled-hook tick:
//   - file present, content differs from in-memory → update memory
//     and DB (CLI-edit propagation)
//   - file missing + live DB row → hydrate disk from DB (post-
//     migration first boot or manual wipe; not a clear)
//   - file missing + no DB row → no-op
//
// CLI users that genuinely want to clear persona must round-trip
// through Manager.Update (PATCH) or PutAgentPersona (Web UI / API
// PUT) — both of those write through to the DB row, so the next
// sync sees DB body="" and the hydrate guard `prev.Body != ""`
// correctly skips re-hydration.
//
// Locking: holds personaSyncMu(agentID) for the entire disk-read
// → DB-upsert critical section so a concurrent PutAgentPersona /
// Manager.Update can't sneak its write in between. The lock is
// released BEFORE SaveAgentRowOnly (which takes store.mu) to
// preserve a single canonical lock order: store.mu is NEVER
// acquired while personaSyncMu is held, eliminating the
// inversion-deadlock against m.save() callers (which take store.mu
// → personaSyncMu via upsertAgent).
func (m *Manager) syncPersona(agentID string) {
	// §3.7 device-switch gate via AcquireMutation. Two
	// invariants this gives us at once:
	//   1. If switching is set, AcquireMutation refuses — sync
	//      bails out before any disk read / DB write that
	//      could land post-snapshot.
	//   2. While sync runs, mutating[agentID] is incremented
	//      so WaitChatIdle observes this work as non-idle.
	//      Without that drain point a sync that passed the
	//      check microseconds before SetSwitching could still
	//      write under m.mu after the snapshot.
	// Best-effort: callers (Get/List/Directory) ignore the
	// skip silently; the in-memory cache is the most-recent
	// pre-switch state, which is what those read paths want.
	releaseMut, mutErr := m.AcquireMutation(agentID)
	if mutErr != nil {
		return
	}
	defer releaseMut()

	// Check existence under lock, then release for file I/O
	m.mu.Lock()
	_, exists := m.agents[agentID]
	m.mu.Unlock()
	if !exists {
		return
	}

	releasePersonaSync := lockPersonaSync(agentID)

	// Read file under personaSyncMu (atomic w.r.t. concurrent writers).
	// readPersonaForSync distinguishes (body, exists, real-error):
	//   - exists  → use body (may be empty for an intentionally-empty file)
	//   - !exists → ENOENT, hydrate-trigger candidate
	//   - err     → real I/O failure, bail
	//
	// readPersonaFile (which collapses ENOENT to ("", true)) cannot be
	// used here because the empty/missing distinction is the whole
	// point: post-cutover, missing-disk is hydrate intent and
	// empty-file is clear intent. The two callsites in this file
	// previously both used readPersonaFile and TOCTOU'd on the
	// follow-up readPersonaForSync — collapsed into one atomic read.
	content, fileExists, readErr := readPersonaForSync(agentID)
	if readErr != nil {
		m.logger.Warn("syncPersona: persona.md read failed", "agent", agentID, "err", readErr)
		releasePersonaSync()
		return
	}

	// Missing-disk + live-DB hydrate path. Symmetrical with the
	// upsertAgent persona path in store.go; see that function's doc
	// comment for the DB-canonical / disk-mirror rationale.
	//
	// DB-error handling: distinguish ErrNotFound (no row, fall
	// through and treat missing-disk as clear) from real I/O
	// errors (transient lock contention, corruption, etc.) — for
	// the latter we MUST NOT proceed to "treat as clear" which
	// would upsert body="" and silently clobber the live DB row
	// once the transient error resolved. Bail out with a warn
	// instead; the next sync retries.
	if !fileExists {
		if st := getGlobalStore(); st != nil {
			ctx, cancel := dbContextWithCancel(nil, 5*time.Second)
			prev, perr := st.GetAgentPersona(ctx, agentID)
			cancel()
			switch {
			case perr == nil && prev != nil && prev.DeletedAt == nil && prev.Body != "":
				if werr := writePersonaFile(agentID, prev.Body); werr != nil {
					m.logger.Warn("syncPersona: hydrate persona disk from DB failed",
						"agent", agentID, "err", werr)
				}
				releasePersonaSync()
				return
			case perr != nil && !errors.Is(perr, store.ErrNotFound):
				m.logger.Warn("syncPersona: persona DB read failed; skipping sync to avoid clobber",
					"agent", agentID, "err", perr)
				releasePersonaSync()
				return
			}
		}
		// No live DB row to hydrate from; treat missing as clear.
		// content is "" already from readPersonaForSync.
	}

	// Re-acquire lock to compare and update
	m.mu.Lock()
	a, exists := m.agents[agentID]
	if !exists {
		m.mu.Unlock()
		releasePersonaSync()
		return
	}
	if a.Persona == content {
		// Persona unchanged — but backfill publicProfile if missing
		if content != "" && a.PublicProfile == "" && !a.PublicProfileOverride {
			persona := content
			m.mu.Unlock()
			releasePersonaSync()
			go m.regeneratePublicProfile(agentID, persona)
		} else {
			m.mu.Unlock()
			releasePersonaSync()
		}
		return
	}
	a.Persona = content
	tool := a.Tool
	override := a.PublicProfileOverride
	a.UpdatedAt = time.Now().Format(time.RFC3339)
	m.mu.Unlock()

	// Persona DB row update inline, while still holding
	// personaSyncMu(agentID): the disk read above + this DB
	// write together form the atomic "disk → DB" sync that the
	// lock guarantees against concurrent PutAgentPersona.
	if st := getGlobalStore(); st != nil {
		ctx, cancel := dbContextWithCancel(nil, 5*time.Second)
		if _, err := st.UpsertAgentPersona(ctx, agentID, content, "", store.AgentInsertOptions{
			AllowOverwrite: true,
		}); err != nil {
			m.logger.Warn("syncPersona: persona DB upsert failed", "agent", agentID, "err", err)
		}
		cancel()
	}

	// Release personaSyncMu BEFORE crossing into store.mu via
	// SaveAgentRowOnly. m.save() and friends take store.mu first
	// and personaSyncMu second (via upsertAgent); a path that
	// holds personaSyncMu and waits for store.mu would invert
	// that order and deadlock.
	releasePersonaSync()

	// Flush the agents-row metadata via the single-agent helper.
	m.mu.Lock()
	var snapshot *Agent
	if cur, ok := m.agents[agentID]; ok {
		snapshot = copyAgent(cur)
	}
	m.mu.Unlock()
	if snapshot != nil {
		if err := m.store.SaveAgentRowOnly(snapshot); err != nil {
			m.logger.Warn("syncPersona: agents-row update failed", "agent", agentID, "err", err)
		}
	}

	// Pre-generate persona summary in background so it's cached for next chat
	if len([]rune(content)) > maxPersonaSummaryRunes {
		go getPersonaSummary(agentID, content, tool, m.logger)
	}

	// Regenerate or clear public profile when persona changes via file edit (unless overridden)
	if !override {
		if content != "" {
			go m.regeneratePublicProfile(agentID, content)
		} else {
			m.mu.Lock()
			if a, ok := m.agents[agentID]; ok {
				a.PublicProfile = ""
			}
			var snap *Agent
			if cur, ok := m.agents[agentID]; ok {
				snap = copyAgent(cur)
			}
			m.mu.Unlock()
			// Single-agent save. personaSyncMu has already been
			// released above (before the agents-row flush) so
			// this call doesn't risk the personaSyncMu →
			// store.mu inversion against m.save callers.
			if snap != nil {
				if err := m.store.SaveAgentRowOnly(snap); err != nil {
					m.logger.Warn("syncPersona: clear publicProfile save failed",
						"agent", agentID, "err", err)
				}
			}
		}
	}
}

// Get returns a deep copy of an agent by ID.
func (m *Manager) Get(id string) (*Agent, bool) {
	// Skip persona disk-sync (and the publicProfile regen it can spawn) for
	// archived agents — they are dormant and we mustn't trigger LLM calls or
	// state writes just because someone called Get to inspect an archived agent.
	m.mu.Lock()
	a, ok := m.agents[id]
	archived := ok && a.Archived
	m.mu.Unlock()
	if !archived {
		m.syncPersona(id)
	}
	has, hash := m.avatarMeta(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok = m.agents[id]
	if !ok {
		return nil, false
	}
	applyAvatarMeta(a, has, hash)
	return copyAgent(a), true
}

// GetRemote returns an agent whose runtime was released from the
// local manager via §3.7 device-switch but whose DB row still
// exists. HolderPeer is populated from agent_locks. Returns nil
// when the agent is either in-memory (use Get) or not in the DB.
func (m *Manager) GetRemote(id string) *Agent {
	if m == nil || m.store == nil || id == "" {
		return nil
	}
	// If it's in the in-memory map, it's not remote.
	m.mu.Lock()
	_, inMem := m.agents[id]
	m.mu.Unlock()
	if inMem {
		return nil
	}
	a, err := m.store.LoadByID(id)
	if err != nil {
		return nil
	}
	// Populate runtime-cached fields that LoadByID doesn't cover.
	if msgs, merr := loadMessages(id, 1); merr == nil && len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		a.LastMessage = &MessagePreview{
			Content:   truncatePreview(last.Content, 100),
			Role:      last.Role,
			Timestamp: last.Timestamp,
		}
	}
	has, hash := m.avatarMeta(id)
	applyAvatarMeta(a, has, hash)
	st := m.Store()
	if st != nil {
		if lock, err := st.GetAgentLock(context.Background(), a.ID); err == nil && lock.HolderPeer != "" {
			a.HolderPeer = lock.HolderPeer
		}
	}
	return a
}

// List returns deep copies of all agents.
func (m *Manager) List() []*Agent {
	// Collect IDs (skipping archived for syncPersona) outside the main lock
	m.mu.Lock()
	ids := make([]string, 0, len(m.agents))
	syncIDs := make([]string, 0, len(m.agents))
	for id, a := range m.agents {
		ids = append(ids, id)
		if !a.Archived {
			syncIDs = append(syncIDs, id)
		}
	}
	m.mu.Unlock()

	for _, id := range syncIDs {
		m.syncPersona(id)
	}

	// Pre-fetch avatar info outside lock (disk I/O)
	type avInfo struct {
		has  bool
		hash string
	}
	avMap := make(map[string]avInfo, len(ids))
	for _, id := range ids {
		has, hash := m.avatarMeta(id)
		avMap[id] = avInfo{has, hash}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]*Agent, 0, len(m.agents))
	for _, a := range m.agents {
		if info, ok := avMap[a.ID]; ok {
			applyAvatarMeta(a, info.has, info.hash)
		}
		list = append(list, copyAgent(a))
	}
	return list
}

// ListRemote returns agents that exist in the store but are NOT in
// the in-memory manager (i.e. their runtime was released via §3.7
// device-switch). Each returned Agent has HolderPeer populated from
// agent_locks so the UI knows which peer hosts them. Returns nil
// when no remote agents exist or the store is unavailable.
func (m *Manager) ListRemote() []*Agent {
	if m == nil || m.store == nil {
		return nil
	}
	st := m.Store()
	if st == nil {
		return nil
	}

	// Snapshot local IDs under the lock.
	m.mu.Lock()
	local := make(map[string]struct{}, len(m.agents))
	for id := range m.agents {
		local[id] = struct{}{}
	}
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbRecs, err := st.ListAgents(ctx)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("ListRemote: store.ListAgents failed", "err", err)
		}
		return nil
	}

	var remote []*Agent
	for _, rec := range dbRecs {
		if _, ok := local[rec.ID]; ok {
			continue
		}
		a, err := m.store.LoadByID(rec.ID)
		if err != nil {
			continue
		}
		// Populate runtime-cached fields that LoadByID doesn't cover.
		if msgs, merr := loadMessages(rec.ID, 1); merr == nil && len(msgs) > 0 {
			last := msgs[len(msgs)-1]
			a.LastMessage = &MessagePreview{
				Content:   truncatePreview(last.Content, 100),
				Role:      last.Role,
				Timestamp: last.Timestamp,
			}
		}
		has, hash := m.avatarMeta(rec.ID)
		applyAvatarMeta(a, has, hash)
		// Look up the holder from agent_locks.
		lock, err := st.GetAgentLock(ctx, rec.ID)
		if err == nil && lock.HolderPeer != "" {
			a.HolderPeer = lock.HolderPeer
		}
		remote = append(remote, a)
	}
	return remote
}

// Directory returns minimal public info for all agents (for agent-to-agent discovery).
func (m *Manager) Directory() []DirectoryEntry {
	// Sync persona first (may trigger publicProfile regeneration). Skip
	// archived agents — they're hidden from the directory anyway and we
	// mustn't trigger LLM calls / file writes on dormant agents.
	m.mu.Lock()
	ids := make([]string, 0, len(m.agents))
	for id, a := range m.agents {
		if a.Archived {
			continue
		}
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		m.syncPersona(id)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	entries := make([]DirectoryEntry, 0, len(m.agents))
	for _, a := range m.agents {
		// Hide archived agents from agent-to-agent discovery so other
		// agents don't try to DM or invite them to group chats.
		if a.Archived {
			continue
		}
		entries = append(entries, DirectoryEntry{
			ID:            a.ID,
			Name:          a.Name,
			PublicProfile: a.PublicProfile,
		})
	}
	return entries
}

// regeneratePublicProfile generates a public profile from persona in the background.
// Only writes the result if the agent's persona hasn't changed since generation started.
// Uses profileGen map to prevent duplicate concurrent generations for the same agent.
//
// §3.7 device-switch race: this runs as a goroutine spawned from
// Update / CreateAgent / SetPersona paths AFTER those return. If a
// switch begins while the LLM round-trip is in flight, naively
// writing the result would land PublicProfile on the source AFTER
// the snapshot — stranding the new profile on the wrong peer. We
// gate both ends: refuse to start if switching is set on entry,
// and re-check switching under busyMu before the write.
func (m *Manager) regeneratePublicProfile(agentID, persona string) {
	// Refuse to even start if a switch is mid-flight. The target
	// peer will run its own regen after finalize if needed.
	m.busyMu.Lock()
	if m.switching != nil && m.switching[agentID] {
		m.busyMu.Unlock()
		return
	}
	m.busyMu.Unlock()

	// Dedupe: skip if generation is already in flight for this agent
	m.mu.Lock()
	if m.profileGen[agentID] {
		m.mu.Unlock()
		return
	}
	m.profileGen[agentID] = true
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.profileGen, agentID)
		m.mu.Unlock()
	}()

	profile, err := GeneratePublicProfile(persona)
	if err != nil {
		m.logger.Warn("failed to generate public profile", "agent", agentID, "err", err)
		return
	}
	// Re-check switching under m.mu + busyMu, atomically with
	// the assignment. An LLM round-trip can take many seconds;
	// a switch may have landed mid-flight. Acquiring m.mu first
	// then busyMu inside it matches the lock order used
	// elsewhere (AcquireMutation / WaitChatIdle take busyMu
	// without m.mu, so m.mu → busyMu is the established
	// direction). Discarding under both locks closes the
	// previous gap where SetSwitching could land between the
	// switching check and the m.mu acquire.
	m.mu.Lock()
	m.busyMu.Lock()
	switching := m.switching != nil && m.switching[agentID]
	m.busyMu.Unlock()
	if switching {
		m.mu.Unlock()
		m.logger.Info("public profile regen aborted: switch in progress", "agent", agentID)
		return
	}
	a, ok := m.agents[agentID]
	if ok && a.Persona == persona && !a.PublicProfileOverride {
		a.PublicProfile = profile
	} else {
		ok = false // persona changed or override enabled, discard stale result
	}
	m.mu.Unlock()
	if ok {
		m.save()
		m.logger.Info("public profile generated", "agent", agentID)
	}
}

// Update updates an agent's configuration. Only non-nil fields are applied.
//
// Validation order (codex review): every cfg field with a Valid*
// predicate is checked BEFORE any side-effecting write — disk
// writes (persona.md), avatar fetches, in-memory mutations. A
// malformed PATCH payload (effort, model+effort combo, workDir,
// intervalMinutes, timeoutMinutes, silentHours, etc.) returns its
// error with zero side effects. Without this barrier, a payload
// that set Persona="..." plus an invalid Effort would land the
// persona write to disk + DB, then fail on Effort, leaving the
// caller with a half-applied PATCH whose visible state diverges
// from the response code.
func (m *Manager) Update(id string, cfg AgentUpdateConfig) (*Agent, error) {
	// Reject pre-CronExpr clients up front (parallel with newAgent) so an
	// old mobile build can't accidentally clobber a freshly-set cronExpr by
	// re-PATCHing an intervalMinutes value the server now ignores.
	if cfg.LegacyIntervalMinutes != nil {
		return nil, fmt.Errorf("%w: intervalMinutes is no longer supported; use cronExpr",
			ErrInvalidCronExpr)
	}
	// §3.7 device switch gate. AcquireMutation refuses when
	// switching is set AND bumps the mutating counter so
	// WaitChatIdle observes this write in flight.
	releaseMut, err := m.AcquireMutation(id)
	if err != nil {
		return nil, err
	}
	defer releaseMut()
	// Pure-input validations that don't depend on existing state run first so
	// a malformed payload can't trigger any I/O (persona.md write, avatar
	// fetch) or partial in-memory mutations below.
	var nextCronMessage string
	cronMessageDirty := false
	if cfg.CronMessage != nil {
		v, err := validateCronMessage(*cfg.CronMessage)
		if err != nil {
			return nil, err
		}
		nextCronMessage = v
		cronMessageDirty = true
	}
	if cfg.ResumeIdleMinutes != nil && !ValidResumeIdle(*cfg.ResumeIdleMinutes) {
		return nil, fmt.Errorf("unsupported resumeIdle: %d minutes", *cfg.ResumeIdleMinutes)
	}
	// Empty CronExpr is a valid value (= disable scheduling); only non-empty
	// values are run through the parser. Validated up-front like the other
	// pure-input fields so a malformed expression can't land partial mutations.
	if cfg.CronExpr != nil && *cfg.CronExpr != "" {
		if err := ValidateCronExpr(*cfg.CronExpr); err != nil {
			return nil, err
		}
	}
	if cfg.TimeoutMinutes != nil && !ValidTimeout(*cfg.TimeoutMinutes) {
		return nil, fmt.Errorf("%w: %d minutes", ErrUnsupportedTimeout, *cfg.TimeoutMinutes)
	}
	if cfg.Effort != nil && !ValidEffort(*cfg.Effort) {
		return nil, fmt.Errorf("unsupported effort level: %q", *cfg.Effort)
	}
	if cfg.WorkDir != nil && *cfg.WorkDir != "" {
		if !filepath.IsAbs(*cfg.WorkDir) {
			return nil, fmt.Errorf("workDir must be an absolute path: %s", *cfg.WorkDir)
		}
		if info, err := os.Stat(*cfg.WorkDir); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("workDir does not exist or is not a directory: %s", *cfg.WorkDir)
		}
	}
	if cfg.ThinkingMode != nil && !ValidThinkingMode(*cfg.ThinkingMode) {
		return nil, fmt.Errorf("unsupported thinkingMode: %q", *cfg.ThinkingMode)
	}
	// Same reasoning for TTS — checked here so a bad model/voice can't
	// trigger persona.md write or partial sibling-field mutations below.
	if cfg.TTS != nil {
		if err := ValidateTTS(cfg.TTS); err != nil {
			return nil, err
		}
	}

	// Check agent exists before any file I/O
	m.mu.Lock()
	a, ok := m.agents[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}
	// Cross-field checks against the live agent state run under m.mu
	// since Effort / Model can also be patched in this PATCH; we
	// compute the prospective values and validate the combination
	// before releasing the lock to the persona-write path.
	prospEffort := a.Effort
	if cfg.Effort != nil {
		prospEffort = *cfg.Effort
	}
	prospModel := a.Model
	if cfg.Model != nil {
		prospModel = *cfg.Model
	}
	if !ValidModelEffort(prospModel, prospEffort) {
		m.mu.Unlock()
		return nil, fmt.Errorf("unsupported effort level %q for model %q", prospEffort, prospModel)
	}
	prospSilentS, prospSilentE := a.SilentStart, a.SilentEnd
	if cfg.SilentStart != nil {
		prospSilentS = *cfg.SilentStart
	}
	if cfg.SilentEnd != nil {
		prospSilentE = *cfg.SilentEnd
	}
	if err := ValidSilentHours(prospSilentS, prospSilentE); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	// Prospective Tool + CustomBaseURL: tool=custom / llama.cpp
	// require a non-empty CustomBaseURL. Both can be patched in
	// the same PATCH so we have to compute the post-PATCH pair
	// and validate the combination here, not at the per-field
	// site below where a.Tool may already be the new value.
	prospTool := a.Tool
	if cfg.Tool != nil {
		prospTool = *cfg.Tool
	}
	prospBaseURL := a.CustomBaseURL
	if cfg.CustomBaseURL != nil {
		prospBaseURL = *cfg.CustomBaseURL
	}
	if (prospTool == "custom" || prospTool == "llama.cpp") && prospBaseURL == "" {
		m.mu.Unlock()
		return nil, fmt.Errorf("customBaseURL is required for %s tool", prospTool)
	}
	m.mu.Unlock()

	// Persona write: disk + DB row in one personaSyncMu critical section.
	//
	// Post-cutover the DB is canonical; disk is a hydrated mirror. PATCH
	// semantics require the caller's intent (including clear via "") to
	// take effect, but the hydrate-from-DB recovery path in upsertAgent
	// / syncPersona would silently reverse a clear (writePersonaFile("")
	// removes disk → next sync sees missing disk + live DB and
	// re-hydrates the prior body). Compensating moves:
	//   1. Hold personaSyncMu across BOTH the disk write and the DB
	//      upsert so a concurrent syncPersona / upsertAgent (which take
	//      the same mutex) cannot interleave between the two writes.
	//   2. Snapshot disk state before the write so a DB-upsert failure
	//      can roll the disk back. Disk-success + DB-failure was
	//      previously documented as "unsalvageable, the next sync
	//      hydrates disk back" — but that path only runs if the prior
	//      DB body is non-empty, so a "set persona='' on a never-set
	//      agent + DB transient error" combo would land disk-empty
	//      + DB-missing, exposing PATCH-success while the body is in
	//      fact the operator's intended new value. Snapshot + rollback
	//      is the only failure mode that preserves the pre-PATCH state
	//      across all combinations.
	//
	// CRITICAL ordering: personaSyncMu must be released BEFORE we cross
	// into store.mu via SaveAgentRowOnly / m.save() further below.
	// agentStore-internal paths take store.mu THEN personaSyncMu via
	// upsertAgent; a path holding personaSyncMu and waiting for store.mu
	// would invert that order and deadlock.
	if cfg.Persona != nil {
		releasePersonaSync := lockPersonaSync(id)

		// Snapshot pre-PATCH disk state for rollback.
		priorBody, priorExisted, readErr := readPersonaForSync(id)
		if readErr != nil {
			releasePersonaSync()
			return nil, fmt.Errorf("read persona.md (pre-PATCH): %w", readErr)
		}

		if err := writePersonaFile(id, *cfg.Persona); err != nil {
			releasePersonaSync()
			return nil, fmt.Errorf("write persona.md: %w", err)
		}
		if st := getGlobalStore(); st != nil {
			ctx, cancel := dbContextWithCancel(nil, 5*time.Second)
			_, err := st.UpsertAgentPersona(ctx, id, *cfg.Persona, "", store.AgentInsertOptions{
				AllowOverwrite: true,
			})
			cancel()
			if err != nil {
				// Rollback disk to the pre-PATCH state. Three cases:
				//   priorExisted=true, priorBody!=""  → restore body
				//   priorExisted=true, priorBody==""  → restore as
				//                                        an EMPTY FILE
				//                                        (writePersonaFile("")
				//                                        deletes; we use
				//                                        atomicfile.WriteBytes
				//                                        directly to preserve
				//                                        the file's existence)
				//   priorExisted=false                → ensure no file
				//                                        (writePersonaFile("")
				//                                        deletes which is OK)
				if rbErr := rollbackPersonaDisk(id, priorBody, priorExisted); rbErr != nil {
					m.logger.Warn("Manager.Update: persona disk rollback failed",
						"agent", id, "priorExisted", priorExisted, "err", rbErr)
				}
				releasePersonaSync()
				return nil, fmt.Errorf("upsert persona DB row: %w", err)
			}
		}
		releasePersonaSync()
	}

	// Pre-fetch avatar info outside lock (disk I/O)
	avHas, avHash := m.avatarMeta(id)

	m.mu.Lock()
	// Re-check: agent may have been deleted concurrently
	a, ok = m.agents[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}

	oldPersona := a.Persona
	oldOverride := a.PublicProfileOverride
	if cfg.Persona != nil {
		a.Persona = *cfg.Persona
	}
	if cfg.Name != nil {
		a.Name = *cfg.Name
	}
	if cfg.PublicProfileOverride != nil {
		a.PublicProfileOverride = *cfg.PublicProfileOverride
	}
	if cfg.PublicProfile != nil {
		a.PublicProfile = *cfg.PublicProfile
	}
	if cfg.Model != nil {
		a.Model = *cfg.Model
	}
	if cfg.Effort != nil {
		// Already validated upstream (single + Model+Effort combo).
		a.Effort = *cfg.Effort
	}
	if cfg.Tool != nil {
		a.Tool = *cfg.Tool
	}
	if cfg.WorkDir != nil {
		// Already validated upstream (abs path + IsDir).
		a.WorkDir = *cfg.WorkDir
	}
	// CronExpr / TimeoutMinutes / ResumeIdleMinutes / SilentHours
	// validated up-front (before any I/O / mutation).
	{
		s, e := a.SilentStart, a.SilentEnd
		if cfg.SilentStart != nil {
			s = *cfg.SilentStart
		}
		if cfg.SilentEnd != nil {
			e = *cfg.SilentEnd
		}
		_ = s
		_ = e
		// SilentHours combo validated upstream against the prospective
		// (s, e) computed there.
	}
	oldExpr := a.CronExpr
	if cfg.CronExpr != nil {
		// Resolve "@preset:N" sentinels here so the persisted CronExpr is
		// always a real 5-field string with the per-agent offset baked in.
		// Same expansion as newAgent — the runtime cron scheduler never
		// sees the sentinel.
		a.CronExpr = ResolveCronPreset(*cfg.CronExpr, a.ID)
	}
	if cfg.TimeoutMinutes != nil {
		a.TimeoutMinutes = *cfg.TimeoutMinutes
	}
	if cfg.ResumeIdleMinutes != nil {
		a.ResumeIdleMinutes = *cfg.ResumeIdleMinutes
	}
	if cfg.SilentStart != nil {
		a.SilentStart = *cfg.SilentStart
	}
	if cfg.SilentEnd != nil {
		a.SilentEnd = *cfg.SilentEnd
	}
	if cfg.NotifyDuringSilent != nil {
		a.NotifyDuringSilent = cfg.NotifyDuringSilent
	}
	if cfg.DeviceSwitchEnabled != nil {
		a.DeviceSwitchEnabled = cfg.DeviceSwitchEnabled
	}
	if cfg.CustomBaseURL != nil {
		// Validated upstream against the prospective (Tool, CustomBaseURL) combo.
		a.CustomBaseURL = *cfg.CustomBaseURL
	}
	if cfg.AllowedTools != nil {
		a.AllowedTools = cfg.AllowedTools
	}
	if cfg.AllowProtectedPaths != nil {
		a.AllowProtectedPaths = normalizeAllowProtectedPaths(*cfg.AllowProtectedPaths)
	}
	if cfg.ThinkingMode != nil {
		// Validated upstream.
		a.ThinkingMode = NormalizeThinkingMode(*cfg.ThinkingMode)
	}
	if cronMessageDirty {
		a.CronMessage = nextCronMessage
	}
	if cfg.TTS != nil {
		// Already validated up-front before any I/O / mutation.
		// Defensive copy so callers can't mutate the stored config out-of-band.
		t := *cfg.TTS
		a.TTS = &t
	}
	newExpr := a.CronExpr
	a.UpdatedAt = time.Now().Format(time.RFC3339)
	applyAvatarMeta(a, avHas, avHash)

	needsRegen := resolvePublicProfile(a, cfg, oldPersona, oldOverride)

	// Take a copy for return and post-lock operations
	cp := copyAgent(a)
	currentPersona := a.Persona
	m.mu.Unlock()

	if oldExpr != newExpr {
		// Pre-check + post-check pattern (mirrors UpdateNotifySources).
		// We must NOT hold m.mu across cron.Schedule because cron callbacks
		// reach back into the manager (runCronJob → Manager.Get) and would
		// deadlock if Schedule's internal cs.mu happened to hold across a
		// callback invocation. Cheap to re-verify after.
		m.mu.Lock()
		archived := false
		if a, ok := m.agents[id]; ok {
			archived = a.Archived
		}
		m.mu.Unlock()

		if !archived {
			expr := newExpr
			if err := m.cron.Schedule(id, expr); err != nil {
				m.logger.Warn("failed to update cron", "agent", id, "err", err)
			}
			// Undo if Archive raced in between.
			m.mu.Lock()
			racedArchive := false
			if a, ok := m.agents[id]; ok && a.Archived {
				racedArchive = true
			}
			m.mu.Unlock()
			if racedArchive {
				m.cron.Remove(id)
			}
		}
	}

	// Skip publicProfile regen for archived agents — generation calls the LLM
	// (real cost) and writes back into agent state on a dormant agent.
	// Unarchive can request regeneration explicitly if persona was edited.
	if needsRegen && !cp.Archived {
		go m.regeneratePublicProfile(id, currentPersona)
	}

	m.save()

	// §3.7 device-switch skill: if the toggle was touched, re-sync
	// the SKILL.md on disk now so a flip-off cleans up promptly
	// without waiting for the next prepareChat. claude/custom only
	// — other backends never had a .claude/ tree to begin with.
	if cfg.DeviceSwitchEnabled != nil && (cp.Tool == "claude" || cp.Tool == "custom") {
		SyncDeviceSwitchSkill(id, cp.IsDeviceSwitchEnabled(), m.logger)
	}

	return cp, nil
}

// UpdateNotifySources updates the notification source configs for an agent
// and rebuilds the poller's source instances.
func (m *Manager) UpdateNotifySources(id string, sources []notifysource.Config) error {
	releaseMut, err := m.AcquireMutation(id)
	if err != nil {
		return err
	}
	defer releaseMut()
	return m.updateNotifySourcesUnguarded(id, sources)
}

// UpdateNotifySourcesAlreadyGuarded is the no-guard variant for
// callers that already hold AcquireMutation for the agent
// (e.g. handleDeleteNotifySource, which needs to wrap both
// UpdateNotifySources + DeleteTokensBySource in one mutation
// hold so a switching flip between them can't escape §3.7
// quiesce). MUST NOT be called without an outer AcquireMutation.
func (m *Manager) UpdateNotifySourcesAlreadyGuarded(id string, sources []notifysource.Config) error {
	return m.updateNotifySourcesUnguarded(id, sources)
}

func (m *Manager) updateNotifySourcesUnguarded(id string, sources []notifysource.Config) error {
	m.mu.Lock()
	a, ok := m.agents[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}
	a.NotifySources = sources
	a.UpdatedAt = time.Now().Format(time.RFC3339)
	archived := a.Archived
	m.mu.Unlock()

	m.save()

	// Pre-check + post-check pattern. RebuildSources MUST NOT be called under
	// m.mu — the poller's tick goroutine takes p.mu first and then calls
	// Manager.Get (acquiring m.mu), so holding m.mu while taking p.mu would
	// deadlock against an in-flight tick.
	if archived {
		return nil
	}
	m.notifyPoller.RebuildSources(id, sources)

	// Defensive re-check: a concurrent Archive may have run between the
	// snapshot above and the RebuildSources call. If so, undo the rebuild —
	// DetachAgent and RebuildSources are both safe to call repeatedly.
	m.mu.Lock()
	racedArchive := false
	if a, ok := m.agents[id]; ok && a.Archived {
		racedArchive = true
	}
	m.mu.Unlock()
	if racedArchive {
		m.notifyPoller.DetachAgent(id)
	}
	return nil
}

// UpdateSlackBot updates the Slack bot configuration for an agent.
// Pass nil to remove the configuration.
func (m *Manager) UpdateSlackBot(id string, cfg *SlackBotConfig) error {
	releaseMut, err := m.AcquireMutation(id)
	if err != nil {
		return err
	}
	defer releaseMut()
	return m.updateSlackBotUnguarded(id, cfg)
}

// UpdateSlackBotAlreadyGuarded is the no-guard variant of
// UpdateSlackBot for callers that already hold AcquireMutation
// for the agent (e.g. handleSetSlackBot / handleDeleteSlackBot,
// which wrap token Save + UpdateSlackBot + hub Reconfigure in
// one outer mutation hold). Calling UpdateSlackBot from inside
// the outer hold races with SetSwitching: switching can flip
// to true between the outer Acquire and the inner Acquire,
// leaving the token saved but the config row unwritten. This
// variant skips the inner guard so the whole handler is one
// transactional unit under the outer mutation.
//
// MUST NOT be called from any path that does NOT already hold
// AcquireMutation for this agent.
func (m *Manager) UpdateSlackBotAlreadyGuarded(id string, cfg *SlackBotConfig) error {
	return m.updateSlackBotUnguarded(id, cfg)
}

func (m *Manager) updateSlackBotUnguarded(id string, cfg *SlackBotConfig) error {
	m.mu.Lock()
	a, ok := m.agents[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}
	a.SlackBot = cfg
	a.UpdatedAt = time.Now().Format(time.RFC3339)
	m.mu.Unlock()

	m.save()
	return nil
}

// loadSlackBotToken retrieves the Slack bot token for an agent from the
// credential store.  Returns "" if Slack bot is not configured/enabled or
// the token is unavailable.
func (m *Manager) loadSlackBotToken(agentID string, a *Agent) string {
	if a.SlackBot == nil || !a.SlackBot.Enabled || m.creds == nil {
		return ""
	}
	botToken, err := m.creds.GetToken("slack", agentID, "", "bot_token")
	if err != nil || botToken == "" {
		return ""
	}
	return botToken
}

// Credentials returns the credential store. Returns nil if the store failed to initialize.
func (m *Manager) Credentials() *CredentialStore {
	return m.creds
}

// HasCredentials returns true if the credential store is available.
func (m *Manager) HasCredentials() bool {
	return m.creds != nil
}

// chatPrep holds the common setup result shared by Chat and ChatOneShot.
//
// volatileContext is the per-turn block prepended to the user message
// (current time, active todos, recent diary, query-based memory hits).
// It is intentionally NOT folded into sysPrompt: Claude's prompt cache
// keys on the system prompt prefix, and any per-turn change there
// invalidates the entire cache and inflates input cost.
type chatPrep struct {
	agentCopy       Agent
	backend         ChatBackend
	sysPrompt       string
	volatileContext string
	mcpServers      map[string]mcpServerEntry
}

// prepareChat performs the common setup for Chat and ChatOneShot:
// persona sync, agent snapshot, backend resolution, system prompt construction,
// and memory context injection.
//
// skipMemoryContext disables query-based memory hints. Use when the transcript
// is about to be truncated (e.g. regenerate), since the index would still
// contain entries from messages that are being removed.
func (m *Manager) prepareChat(ctx context.Context, agentID, query string, indexNewMessages bool, skipMemoryContext bool) (*chatPrep, error) {
	// Cheap pre-check: refuse archived agents (and unknown ones) before any
	// disk I/O like syncPersona, so dormant agents don't leak side effects
	// into persona files / publicProfile regeneration.
	m.mu.Lock()
	preA, ok := m.agents[agentID]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, agentID)
	}
	if preA.Archived {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAgentArchived, agentID)
	}
	m.mu.Unlock()

	// Refuse mid-reset agents BEFORE the persona / memory sync side
	// effects fire. Without this, a chat that's about to be rejected
	// for ErrAgentResetting would still trigger a syncPersona and a
	// memory DB sync — the latter would either race with ResetData's
	// own post-reset sync (potentially resurrecting a tombstoned row
	// from a stale FS view) or just waste work. Chat / ChatOneShot /
	// Regenerate all funnel through prepareChat, so one guard here
	// covers every entry.
	m.busyMu.Lock()
	resetting := m.resetting[agentID]
	m.busyMu.Unlock()
	if resetting {
		return nil, ErrAgentResetting
	}

	m.syncPersona(agentID)

	// Mirror the persona sync for MEMORY.md and memory/*.md so a CLI
	// edit that landed during the previous conversation is reflected
	// in the DB before we touch the next message. The disk file is
	// canonical for the CLI; the DB row is the read path for Web UI
	// / multi-device — without this hook a cross-device read between
	// turns would observe stale memory.
	//
	// Use the best-effort variant: if a long-running sync (Load,
	// fork) is already holding the per-agent gate, we skip this turn
	// rather than block. The system-prompt build below still reads
	// MEMORY.md from disk so the CLI prompt itself is fresh; the
	// next prepareChat / scheduled hook will retry the DB sync. A 5s
	// ctx caps the wait when the lock IS available but the sync's
	// own DB writes are slow.
	if st := getGlobalStore(); st != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ran, err := SyncAgentMemoryFromDiskBestEffort(ctx, st, agentID, m.logger)
		cancel()
		if !ran {
			m.logger.Debug("memory sync at prepareChat skipped (busy)", "agent", agentID)
		} else if err != nil {
			m.logger.Debug("memory sync at prepareChat failed", "agent", agentID, "err", err)
		}
	}

	m.mu.Lock()
	a, ok := m.agents[agentID]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, agentID)
	}
	if a.Archived {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAgentArchived, agentID)
	}
	agentCopy := *a
	m.mu.Unlock()

	backend, err := m.resolveBackend(agentID, &agentCopy)
	if err != nil {
		return nil, err
	}

	var apiBase string
	var groups []*GroupDM
	if m.groupdms != nil {
		apiBase = m.groupdms.APIBase()
		groups = m.groupdms.GroupsForAgent(agentID)
	}
	if agentCopy.Tool == "claude" || agentCopy.Tool == "custom" {
		PrepareClaudeSettings(agentID, apiBase, agentCopy.AllowProtectedPaths, m.logger)
		// §3.7 device-switch skill. Toggle defaults to true via
		// IsDeviceSwitchEnabled; SyncDeviceSwitchSkill internally
		// gates installation on LookupPeerCount() > 0 so a single-
		// node install never sees the skill. Removes any stale
		// SKILL.md when the toggle is off or the last peer was
		// dropped.
		SyncDeviceSwitchSkill(agentID, agentCopy.IsDeviceSwitchEnabled(), m.logger)
	}

	// Build MCP server list (backend-agnostic, URL-based).
	hasSlackBot := m.loadSlackBotToken(agentID, &agentCopy) != ""
	mcpServers := BuildMCPServers(agentID, apiBase, hasSlackBot)

	// MCP servers are injected per-backend:
	// - Claude: --mcp-config CLI arg (in backend_claude.go)
	// - Codex: -c flag override (in backend_codex.go)
	// - Gemini: .gemini/settings.json mcpServers (in backend_gemini.go)

	sysPrompt := buildSystemPrompt(&agentCopy, m.logger, apiBase, groups, m.creds != nil)

	// Refresh the memory index, but emit query-based recall through the
	// volatile context (per-turn user message), NOT the system prompt —
	// otherwise every distinct user query would invalidate the prompt
	// cache. IndexNewMessages is called for interactive chat (so the index
	// is current) but skipped for one-shot chats which don't persist to
	// the main transcript.
	var queryContext string
	if idx := m.getOrOpenIndex(agentID); idx != nil {
		idx.IndexFilesIfStale(agentID)
		if indexNewMessages {
			idx.IndexNewMessages(agentID)
		}
		if !skipMemoryContext {
			queryContext = idx.BuildContextFromQuery(query)
		}
	}
	volatileContext := m.BuildVolatileContext(ctx, agentID, queryContext)

	return &chatPrep{
		agentCopy:       agentCopy,
		backend:         backend,
		sysPrompt:       sysPrompt,
		volatileContext: volatileContext,
		mcpServers:      mcpServers,
	}, nil
}

// Chat sends a message to an agent and returns a channel of streaming events.
// The role parameter controls how the input message is stored in the transcript
// ("user" for interactive chat, "system" for cron-triggered messages).
// An optional BusySource may be passed to tag the busy entry; defaults to
// BusySourceUser when omitted.
func (m *Manager) Chat(ctx context.Context, agentID string, userMessage string, role string, attachments []MessageAttachment, source ...BusySource) (<-chan ChatEvent, error) {
	// acquirePreparing checks switching AND increments the
	// preparing counter under one busyMu lock — Step -1's
	// WaitChatIdle observes the counter so a race between
	// "switching pre-check passed" and "prepareChat finished"
	// can't slip a disk write past the snapshot.
	if err := m.acquirePreparing(agentID); err != nil {
		return nil, err
	}
	prepReleased := false
	defer func() {
		if !prepReleased {
			m.releasePreparing(agentID)
		}
	}()

	prep, err := m.prepareChat(ctx, agentID, userMessage, true, false)
	if err != nil {
		return nil, err
	}

	// Check if agent is busy, editing, being reset, or switching
	m.busyMu.Lock()
	if m.resetting[agentID] {
		m.busyMu.Unlock()
		return nil, ErrAgentResetting
	}
	if m.editing[agentID] {
		m.busyMu.Unlock()
		return nil, ErrAgentBusy
	}
	if m.switching[agentID] {
		// Re-check post-prepare: a SetSwitching(true) could
		// have landed between the first check and now. Refuse
		// rather than push the chat through.
		m.busyMu.Unlock()
		return nil, fmt.Errorf("%w: device switch in progress", ErrAgentBusy)
	}
	if _, busy := m.busy[agentID]; busy {
		m.busyMu.Unlock()
		return nil, ErrAgentBusy
	}
	chatCtx, cancel := context.WithCancel(ctx)
	// Create outCh and broadcaster upfront so they're available from the
	// moment the busy entry is visible. This prevents a reconnecting
	// WebSocket from falling back to polling during setup.
	outCh := make(chan ChatEvent, 64)
	bc := newChatBroadcaster(outCh)
	src := BusySourceUser
	if len(source) > 0 {
		src = source[0]
	}
	m.busy[agentID] = busyEntry{cancel: cancel, startedAt: time.Now(), broadcaster: bc, source: src}
	// Hand off the preparing counter to the busy entry under
	// the same lock so WaitChatIdle never observes a window
	// where neither preparing nor busy is set for this chat.
	if m.preparing != nil && m.preparing[agentID] > 0 {
		m.preparing[agentID]--
		if m.preparing[agentID] == 0 {
			delete(m.preparing, agentID)
		}
	}
	prepReleased = true
	m.busyMu.Unlock()

	// Notify any WebSocket clients watching this agent.
	m.notifyChatStart(agentID)

	// Save input message to transcript (after memory search to avoid self-injection)
	var inputMsg *Message
	if role == "system" {
		inputMsg = newSystemMessage(userMessage)
	} else {
		inputMsg = newUserMessage(userMessage, attachments)
	}
	if err := appendMessage(agentID, inputMsg); err != nil {
		m.logger.Warn("failed to save input message", "err", err)
	}
	// Stream the system message to WebSocket clients so it appears before
	// the assistant response. User messages are added optimistically by the
	// frontend, so only system messages need injection here.
	if role == "system" {
		outCh <- ChatEvent{Type: "message", Message: inputMsg}
	}

	// Build the effective message for the backend.
	// When attachments are present, prepend file references so the CLI
	// can access them (e.g. via Read tool for images/text).
	effectiveMessage := userMessage
	if len(attachments) > 0 {
		effectiveMessage = formatMessageWithAttachments(userMessage, attachments)
	}
	// Prepend the per-turn volatile context (current time, active todos,
	// recent-diary summary, query-based memory hits). Goes here — not in
	// sysPrompt — to keep the prompt cache prefix stable.
	if prep.volatileContext != "" {
		effectiveMessage = prep.volatileContext + effectiveMessage
	}

	// Start chat. role=="system" marks automated triggers (cron, groupdm,
	// notify poller) where there is no interactive user waiting on the
	// previous turn — backends may drop idle-window protections and prefer
	// aggressive session reset for token conservation.
	backendCh, err := prep.backend.Chat(chatCtx, &prep.agentCopy, effectiveMessage, prep.sysPrompt, ChatOptions{
		MCPServers:       prep.mcpServers,
		AutomatedTrigger: role == "system",
	})
	if err != nil {
		outCh <- ChatEvent{Type: "error", ErrorMessage: err.Error()}
		close(outCh)
		m.clearBusy(agentID)
		cancel()
		return nil, err
	}

	// Return a subscriber channel for the immediate caller.
	// The raw outCh is consumed exclusively by the broadcaster.
	_, callerCh, _ := bc.Subscribe()

	go func() {
		defer close(outCh)
		defer m.clearBusy(agentID)
		defer cancel()
		m.processChatEvents(chatCtx, agentID, backendCh, outCh)

		m.updatePostChatIndex(agentID)
	}()

	return callerCh, nil
}

// ChatOneShot runs a one-shot chat that does not save to the agent's
// transcript (agent_messages) and does not resume the CLI session. Used
// for external platform conversations (Slack, Discord) that carry their
// own context. Memory (MEMORY.md, diary) access is still available via
// system prompt.
func (m *Manager) ChatOneShot(ctx context.Context, agentID string, userMessage string) (<-chan ChatEvent, error) {
	// acquirePreparing: see Chat() for the contract — gates
	// switching AND increments the preparing counter so Step
	// -1's WaitChatIdle observes the in-flight prepareChat.
	if err := m.acquirePreparing(agentID); err != nil {
		return nil, err
	}
	defer m.releasePreparing(agentID)

	prep, err := m.prepareChat(ctx, agentID, userMessage, false, false)
	if err != nil {
		return nil, err
	}

	m.busyMu.Lock()
	if m.resetting[agentID] {
		m.busyMu.Unlock()
		return nil, ErrAgentResetting
	}
	if m.switching[agentID] {
		m.busyMu.Unlock()
		return nil, fmt.Errorf("%w: device switch in progress", ErrAgentBusy)
	}
	m.busyMu.Unlock()

	chatCtx, cancel := context.WithCancel(ctx)
	osID := m.trackOneShot(agentID, cancel)
	outCh := make(chan ChatEvent, 64)

	// Slack messages: instruct the agent to separate thinking from reply.
	if strings.Contains(userMessage, "[Slack @") {
		prep.sysPrompt += "\n\n## Slack Response Format\n\n" +
			"This message is from Slack. Your text output will be posted to Slack.\n" +
			"Wrap ONLY your final reply in <reply>...</reply> tags.\n" +
			"Text outside these tags is your internal workspace — use it freely to think, reason, plan, and execute tools.\n" +
			"Only the content inside <reply> will be shown to the Slack user.\n" +
			"Always include exactly one <reply> block at the end of your response.\n"
	}

	// NOTE: No appendMessage — one-shot chats are not saved to transcript.

	// Prepend volatile context for the same reason as the persistent
	// chat path: keep dynamic data out of the system prompt to preserve
	// the prompt cache.
	effectiveMessage := userMessage
	if prep.volatileContext != "" {
		effectiveMessage = prep.volatileContext + userMessage
	}
	backendCh, err := prep.backend.Chat(chatCtx, &prep.agentCopy, effectiveMessage, prep.sysPrompt, ChatOptions{OneShot: true, MCPServers: prep.mcpServers})
	if err != nil {
		outCh <- ChatEvent{Type: "error", ErrorMessage: err.Error()}
		close(outCh)
		cancel()
		m.untrackOneShot(agentID, osID)
		return nil, err
	}

	go func() {
		defer close(outCh)
		defer cancel()
		defer m.untrackOneShot(agentID, osID)
		m.processOneShotEvents(chatCtx, agentID, backendCh, outCh)
	}()

	return outCh, nil
}

// processOneShotEvents is like processChatEvents but does not persist
// messages to the transcript. It still forwards events to outCh.
func (m *Manager) processOneShotEvents(ctx context.Context, agentID string, backendCh <-chan ChatEvent, outCh chan<- ChatEvent) {
	for {
		select {
		case event, ok := <-backendCh:
			if !ok {
				return
			}
			// Terminal events use blocking send; streaming events are non-blocking.
			if event.Type == "done" || event.Type == "error" {
				// Sync persona in case agent edited it during this chat
				if event.Type == "done" {
					m.syncPersona(agentID)
				}
				select {
				case outCh <- event:
				case <-ctx.Done():
					return
				}
			} else {
				select {
				case outCh <- event:
				default:
				}
			}
		case <-ctx.Done():
			for range backendCh {
			}
			return
		}
	}
}

// processChatEvents reads events from the backend channel, persists messages,
// and forwards events to outCh for the broadcaster.
func (m *Manager) processChatEvents(ctx context.Context, agentID string, backendCh <-chan ChatEvent, outCh chan<- ChatEvent) {
	// Accumulate streaming data so we can persist a partial message if
	// the chat is aborted before a "done" event arrives.
	var accText strings.Builder
	var accThinking strings.Builder
	var accToolUses []ToolUse
	receivedDone := false

	defer func() {
		// §3.7 release guard: a source-release that ran while
		// this goroutine was mid-flight (post-complete drain
		// failure) evicted the agent from m.agents. Every
		// downstream write (persistDoneEvent / appendMessage)
		// uses agentID to compute file paths, so they would
		// land on source's disk despite target now owning the
		// agent. Skip the entire defer body if the agent is
		// no longer here.
		m.mu.Lock()
		_, stillHere := m.agents[agentID]
		m.mu.Unlock()
		if !stillHere {
			return
		}
		if receivedDone {
			if ctx.Err() == context.DeadlineExceeded {
				errMsg := newSystemMessage("⚠️ この応答は制限時間超過により中断されました。")
				if err := appendMessage(agentID, errMsg); err != nil {
					m.logger.Warn("failed to save timeout message", "err", err)
				}
			}
			return
		}
		if accText.Len() > 0 || accThinking.Len() > 0 || len(accToolUses) > 0 {
			msg := newAssistantMessage()
			msg.Content = accText.String()
			msg.Thinking = accThinking.String()
			msg.ToolUses = accToolUses
			m.persistDoneEvent(agentID, msg)
		}
		if ctx.Err() == context.DeadlineExceeded {
			errMsg := newSystemMessage("⚠️ この応答は制限時間超過により中断されました。")
			if err := appendMessage(agentID, errMsg); err != nil {
				m.logger.Warn("failed to save timeout message", "err", err)
			}
		}
	}()

	// accumulate records streaming data for abort recovery.
	accumulate := func(event *ChatEvent) {
		switch event.Type {
		case "text":
			accText.WriteString(event.Delta)
		case "thinking":
			accThinking.WriteString(event.Delta)
		case "tool_use":
			accToolUses = append(accToolUses, ToolUse{
				ID:    event.ToolUseID,
				Name:  event.ToolName,
				Input: event.ToolInput,
			})
		case "tool_result":
			matchToolOutput(accToolUses, event.ToolUseID, event.ToolName, event.ToolOutput)
		}
	}

	// handleTerminal persists terminal events (done/error) to the transcript.
	handleTerminal := func(event *ChatEvent) {
		if event.Type == "done" && event.Message != nil && isRateLimitMessage(event.Message) {
			event.Message.Role = "system"
		}
		if event.Type == "done" && event.Message != nil {
			receivedDone = true
			// §3.7 release guard: if a source-release evicted
			// the agent while this goroutine was mid-process
			// (drain failed at switch), skip every downstream
			// write so we don't strand transcript / JSONL
			// state on the released peer. Target's chat
			// goroutine (re-started after handoff) owns the
			// canonical state from here on.
			m.mu.Lock()
			_, stillHere := m.agents[agentID]
			m.mu.Unlock()
			if !stillHere {
				return
			}
			m.persistDoneEvent(agentID, event.Message)

			if m.OnChatDone != nil && event.ErrorMessage == "" {
				// §3.7 race: a source-release that ran while
				// this chat goroutine was still processing
				// (post-complete drain failure path) evicted
				// the agent from m.agents. Dereferencing a
				// missing entry would panic; skip the
				// OnChatDone callback so post-release writes
				// don't fire for an agent target now owns.
				m.mu.Lock()
				ag, ok := m.agents[agentID]
				var agCopy Agent
				if ok {
					agCopy = *ag
				}
				m.mu.Unlock()
				if ok {
					msgCopy := *event.Message
					go m.OnChatDone(&agCopy, &msgCopy)
				}
			}
		}
		if event.ErrorMessage != "" {
			// §3.7 release guard: skip if evicted (see done-event
			// guard above for rationale).
			m.mu.Lock()
			_, stillHere := m.agents[agentID]
			m.mu.Unlock()
			if stillHere {
				errMsg := newSystemMessage("⚠️ Error: " + event.ErrorMessage)
				if err := appendMessage(agentID, errMsg); err != nil {
					m.logger.Warn("failed to save error message", "err", err)
				}
			}
		}
	}

	for {
		select {
		case event, ok := <-backendCh:
			if !ok {
				return
			}

			accumulate(&event)
			handleTerminal(&event)

			// Terminal events (done/error) use blocking send so the
			// client always receives them. Streaming events use
			// non-blocking send — if no reader (WS disconnected),
			// they are dropped but processing continues.
			if event.Type == "done" || event.Type == "error" {
				select {
				case outCh <- event:
				case <-ctx.Done():
					return
				}
			} else {
				select {
				case outCh <- event:
				default:
				}
			}
		case <-ctx.Done():
			// Abort: drain remaining events from backendCh to capture
			// any data buffered before the backend goroutine noticed
			// the cancellation. Don't forward to outCh (no readers).
			for event := range backendCh {
				accumulate(&event)
				handleTerminal(&event)
			}
			return
		}
	}
}

// persistDoneEvent saves the assistant message and updates agent state.
func (m *Manager) persistDoneEvent(agentID string, msg *Message) {
	if err := appendMessage(agentID, msg); err != nil {
		m.logger.Warn("failed to save assistant message", "err", err)
	}

	// Sync persona.md → Agent.Persona (agent may have edited it)
	m.syncPersona(agentID)

	// Update last message preview
	m.mu.Lock()
	if ag, ok := m.agents[agentID]; ok {
		ag.LastMessage = &MessagePreview{
			Content:   truncatePreview(msg.Content, 100),
			Role:      msg.Role,
			Timestamp: msg.Timestamp,
		}
		ag.UpdatedAt = time.Now().Format(time.RFC3339)
	}
	m.mu.Unlock()
	m.save()
}

// resolveBackend selects the ChatBackend for the agent's tool.
func (m *Manager) resolveBackend(agentID string, agentCopy *Agent) (ChatBackend, error) {
	backend, ok := m.backends[agentCopy.Tool]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedTool, agentCopy.Tool)
	}
	return backend, nil
}

// updatePostChatIndex updates the memory index after a chat completes.
// Skips if the agent was deleted or is being reset to avoid reopening a closed index.
func (m *Manager) updatePostChatIndex(agentID string) {
	m.busyMu.Lock()
	isResetting := m.resetting[agentID]
	m.busyMu.Unlock()
	m.mu.Lock()
	_, agentExists := m.agents[agentID]
	m.mu.Unlock()
	if agentExists && !isResetting {
		if idx := m.getOrOpenIndex(agentID); idx != nil {
			idx.IndexNewMessages(agentID)
			idx.IndexFilesIfStale(agentID)
		}
	}
}

// NextCronRun returns the next *configured* run time for an agent,
// adjusted for silent hours. Returns the zero Time when the agent has no
// schedule, is archived, or doesn't exist.
//
// Semantics: this is the schedule the cron entry would fire AT, NOT a
// guarantee the run will actually happen. Global cron-paused state is
// deliberately ignored here so the UI can render "what would be next"
// (suffixed with "(paused)" via the cronPausedGlobal indicator on the
// agent response). Callers that need "will-actually-fire" semantics
// must AND the return with !Manager.CronPaused() — the only such caller
// today is the runtime scheduler itself, which gates via runCronJob's
// CronPaused check before invoking Chat. The HTTP layer uses this for
// display only.
func (m *Manager) NextCronRun(agentID string) time.Time {
	m.mu.Lock()
	a, ok := m.agents[agentID]
	if !ok || a.Archived || a.CronExpr == "" {
		m.mu.Unlock()
		return time.Time{}
	}
	silentStart, silentEnd := a.SilentStart, a.SilentEnd
	m.mu.Unlock()
	return m.cron.nextRun(agentID, silentStart, silentEnd)
}

// Checkin triggers a manual check-in for the agent. Unlike the periodic
// cron job, this does not acquire the cron lock and does not check the
// active-hours window — the user explicitly asked for it. The check-in
// runs asynchronously: events are drained in a background goroutine and
// the assistant reply is persisted to the transcript like any other chat.
//
// Returns ErrAgentNotFound, ErrAgentArchived, or ErrAgentBusy on rejection.
func (m *Manager) Checkin(agentID string) error {
	m.mu.Lock()
	a, ok := m.agents[agentID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAgentNotFound, agentID)
	}
	if a.Archived {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAgentArchived, agentID)
	}
	timeoutMinutes := a.TimeoutMinutes
	cronMessage := a.CronMessage
	m.mu.Unlock()

	timeout := cronTimeout
	if timeoutMinutes > 0 {
		timeout = time.Duration(timeoutMinutes) * time.Minute
	}
	effectiveTimeoutMin := int(timeout / time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	prompt := checkinPrompt(time.Now(), effectiveTimeoutMin, cronMessage)
	events, err := m.Chat(ctx, agentID, prompt, "system", nil, BusySourceCron)
	if err != nil {
		cancel()
		return err
	}

	go func() {
		defer cancel()
		for range events {
		}
		if ctx.Err() == context.DeadlineExceeded {
			m.logger.Warn("manual checkin timed out", "agent", agentID, "timeout", timeout)
		} else {
			m.logger.Info("manual checkin completed", "agent", agentID)
		}
	}()
	return nil
}

// CronPaused returns whether all cron jobs are globally paused.
func (m *Manager) CronPaused() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cronPaused
}

// SetCronPaused persists the global cron pause state and, on success,
// updates the in-memory flag. Returns an error if the store rejected the
// write so the HTTP handler can surface it as 5xx — acknowledging a
// toggle that hasn't been persisted would let a transient kv-write
// failure desync the UI from the truth across a restart.
//
// Concurrency: cronToggleMu serializes the (memory ↔ kv) update so two
// concurrent toggles can't interleave their kv-writes and memory-writes
// in opposite orders and leave the two stores disagreeing.
//
// Ordering policy is asymmetric — biased toward "fail closed" in flight:
//
//   - paused == true (operator pausing): set memory FIRST so any cron
//     tick happening during the kv write sees the pause immediately.
//     If the kv write fails we revert memory back to false and surface
//     the error; the operator's request didn't land but at most one
//     in-flight tick saw the pause briefly (harmless: paused never
//     starts work, only blocks new work).
//
//   - paused == false (operator resuming): persist FIRST so we never
//     advertise "running" until the kv row actually says so. A kv
//     write failure leaves both memory and kv at "paused" — the
//     operator's request fails closed.
//
// In both branches the post-condition on success is (memory == kv ==
// requested value). On failure: (memory == kv == previous value).
func (m *Manager) SetCronPaused(paused bool) error {
	m.cronToggleMu.Lock()
	defer m.cronToggleMu.Unlock()

	if paused {
		// Pause: memory first.
		m.mu.Lock()
		prev := m.cronPaused
		m.cronPaused = true
		m.mu.Unlock()

		if err := m.store.SaveCronPaused(true); err != nil {
			m.mu.Lock()
			m.cronPaused = prev
			m.mu.Unlock()
			return err
		}
	} else {
		// Resume: kv first.
		if err := m.store.SaveCronPaused(false); err != nil {
			return err
		}
		m.mu.Lock()
		m.cronPaused = false
		m.mu.Unlock()
	}

	m.logger.Info("cron pause toggled", "paused", paused)
	return nil
}

// IsAgentActive reports whether an agent is currently "active":
//   - Global cron is running (not paused)
//   - Agent is not archived
//   - Current time is NOT within the agent's silent hours
//
// Returns (active, found). found is false when the agent ID is unknown.
func (m *Manager) IsAgentActive(agentID string) (bool, bool) {
	m.mu.Lock()
	paused := m.cronPaused
	a, ok := m.agents[agentID]
	if !ok {
		m.mu.Unlock()
		return false, false
	}
	if paused || a.Archived {
		m.mu.Unlock()
		return false, true
	}
	start, end := a.SilentStart, a.SilentEnd
	m.mu.Unlock()

	return !IsInSilentHours(start, end), true
}

// IsAgentDMAvailable reports whether an agent can receive DM notifications.
// Unlike IsAgentActive, this ignores global cron pause (DM delivery is
// independent of cron) and respects NotifyDuringSilent: an agent in silent
// hours is still "available" for DMs if they opted in.
//
// Returns (available, found). found is false when the agent ID is unknown.
func (m *Manager) IsAgentDMAvailable(agentID string) (bool, bool) {
	m.mu.Lock()
	a, ok := m.agents[agentID]
	if !ok {
		m.mu.Unlock()
		return false, false
	}
	if a.Archived {
		m.mu.Unlock()
		return false, true
	}
	start, end := a.SilentStart, a.SilentEnd
	notifyDuring := a.ShouldNotifyDuringSilent()
	m.mu.Unlock()

	if IsInSilentHours(start, end) && !notifyDuring {
		return false, true
	}
	return true, true
}

// trackOneShot registers a one-shot chat's cancel func so it can be
// cleaned up on Shutdown or agent Delete. Returns an ID for untracking.
func (m *Manager) trackOneShot(agentID string, cancel context.CancelFunc) int64 {
	m.oneShotCancelsMu.Lock()
	defer m.oneShotCancelsMu.Unlock()
	m.oneShotSeq++
	id := m.oneShotSeq
	if m.oneShotCancels[agentID] == nil {
		m.oneShotCancels[agentID] = make(map[int64]context.CancelFunc)
	}
	m.oneShotCancels[agentID][id] = cancel
	return id
}

// untrackOneShot removes a one-shot chat's cancel func after it completes.
func (m *Manager) untrackOneShot(agentID string, id int64) {
	m.oneShotCancelsMu.Lock()
	defer m.oneShotCancelsMu.Unlock()
	if set, ok := m.oneShotCancels[agentID]; ok {
		delete(set, id)
		if len(set) == 0 {
			delete(m.oneShotCancels, agentID)
		}
	}
}

// cancelOneShots cancels all in-flight one-shot chats for an agent.
// The map entry is left intact so callers that need to wait for the
// goroutines to finish (see waitOneShotClear) can observe completion as
// each goroutine removes itself via untrackOneShot.
func (m *Manager) cancelOneShots(agentID string) {
	m.oneShotCancelsMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(m.oneShotCancels[agentID]))
	for _, cancel := range m.oneShotCancels[agentID] {
		cancels = append(cancels, cancel)
	}
	m.oneShotCancelsMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// cancelAllOneShots cancels all in-flight one-shot chats across all agents.
func (m *Manager) cancelAllOneShots() {
	m.oneShotCancelsMu.Lock()
	all := m.oneShotCancels
	m.oneShotCancels = make(map[string]map[int64]context.CancelFunc)
	m.oneShotCancelsMu.Unlock()
	for _, cancels := range all {
		for _, cancel := range cancels {
			cancel()
		}
	}
}

// chatWatcher is a handle for a chat-start notification subscription.
type chatWatcher struct {
	ch chan struct{}
}

// WatchChatStart returns a channel that receives a signal whenever a new chat
// starts for the given agent. Call the returned function to unsubscribe.
func (m *Manager) WatchChatStart(agentID string) (<-chan struct{}, func()) {
	w := &chatWatcher{ch: make(chan struct{}, 1)}
	m.chatWatchersMu.Lock()
	if m.chatWatchers[agentID] == nil {
		m.chatWatchers[agentID] = make(map[*chatWatcher]struct{})
	}
	m.chatWatchers[agentID][w] = struct{}{}
	m.chatWatchersMu.Unlock()
	return w.ch, func() {
		m.chatWatchersMu.Lock()
		delete(m.chatWatchers[agentID], w)
		if len(m.chatWatchers[agentID]) == 0 {
			delete(m.chatWatchers, agentID)
		}
		m.chatWatchersMu.Unlock()
	}
}

// notifyChatStart signals all watchers that a new chat has started for agentID.
func (m *Manager) notifyChatStart(agentID string) {
	m.chatWatchersMu.Lock()
	watchers := m.chatWatchers[agentID]
	for w := range watchers {
		select {
		case w.ch <- struct{}{}:
		default:
		}
	}
	m.chatWatchersMu.Unlock()
}

// Messages returns recent messages for an agent.
func (m *Manager) Messages(agentID string, limit int) ([]*Message, error) {
	return loadMessages(agentID, limit)
}

// MessagesPaginated returns messages with cursor-based pagination.
func (m *Manager) MessagesPaginated(agentID string, limit int, before string) ([]*Message, bool, error) {
	return loadMessagesPaginated(agentID, limit, before)
}

// UpdateMessageContent replaces the content of a single message in the transcript.
// Only supported for the llama.cpp backend. Rejected with ErrAgentBusy while the
// agent has an active chat.
//
// ifMatchETag is forwarded to the store layer so the optimistic-
// concurrency check is atomic with the UPDATE. Empty value skips the
// check (used by daemon-internal callers — currently none, but keeps
// the door open for tools that mutate transcripts without a client
// etag).
//
// Returns (msg, newETag, err). The etag is the value computed inside
// the same transaction as the UPDATE, so handlers can echo it as an
// HTTP ETag without a follow-up read that would race against any
// edit landing after acquireTranscriptEdit's release.
func (m *Manager) UpdateMessageContent(agentID, msgID, content, ifMatchETag string) (*Message, string, error) {
	release, err := m.acquireTranscriptEdit(agentID)
	if err != nil {
		return nil, "", err
	}
	defer release()
	msg, etag, err := updateMessageContent(agentID, msgID, content, ifMatchETag)
	if err != nil {
		return nil, "", err
	}
	m.refreshLastMessage(agentID)
	return msg, etag, nil
}

// DeleteMessage removes a single message from the transcript.
// Only supported for the llama.cpp backend. Rejected with ErrAgentBusy while the
// agent has an active chat.
//
// ifMatchETag forwards an optimistic-lock precondition to the store.
// Empty disables the check (back-compat for daemon-internal callers and
// the legacy unconditional UI path); HTTP handlers always pass through
// the value parsed from the If-Match header.
func (m *Manager) DeleteMessage(agentID, msgID, ifMatchETag string) error {
	release, err := m.acquireTranscriptEdit(agentID)
	if err != nil {
		return err
	}
	defer release()
	if err := deleteMessage(agentID, msgID, ifMatchETag); err != nil {
		return err
	}
	m.refreshLastMessage(agentID)
	return nil
}

// Regenerate truncates the transcript at msgID and re-runs the associated
// user message through the llama.cpp backend. If msgID is an assistant
// message, msgID and all subsequent messages are removed and the preceding
// user message is re-sent. If msgID is a user message, all subsequent
// messages are removed and msgID itself is re-sent.
//
// Returns after truncation and busy-slot registration so reloading clients
// can immediately see the in-progress chat. The backend request and event
// streaming run in the background; any backend.Chat failure is surfaced as
// an error event on the broadcaster (and persisted as a system message).
// Regenerate truncates and re-runs from msgID. ifMatchETag, when non-empty,
// preconditions on the clicked message's current etag inside the editing
// mutex (so a racing edit between the click and the truncate is caught
// before any rows are tombstoned). Empty disables the check (back-compat).
func (m *Manager) Regenerate(ctx context.Context, agentID, msgID, ifMatchETag string) error {
	release, err := m.acquireTranscriptEdit(agentID)
	if err != nil {
		return err
	}
	editingReleased := false
	defer func() {
		if !editingReleased {
			release()
		}
	}()

	rt, err := findRegenerateTarget(ctx, agentID, msgID, ifMatchETag)
	if err != nil {
		return err
	}
	// findRegenerateTarget's etag check is a fast-fail UX optimisation
	// against an in-memory snapshot. The authoritative pivot etag
	// re-check happens inside truncateForRegenerate's transaction, and
	// afterSeq is derived from the pivot's *immutable* seq there too —
	// so a cross-device prefix mutation between the click and the
	// truncate cannot shift the boundary onto the wrong row.

	// Re-read the source content right before prepareChat so the chat
	// we re-run reflects the currently-committed source. Capture the
	// source row's etag too so truncateForRegenerate can re-validate
	// inside its transaction — without that, an edit / tombstone on
	// the source between this read and the truncate would silently
	// commit a regen against a stale snapshot. The pre-tx GetMessage
	// is still needed (the truncate itself doesn't return content)
	// but the etag round-trip turns the window from "stale-content
	// regen committed" into "ErrMessageETagMismatch and the UI can
	// refetch", which matches the rest of the If-Match story.
	st := getGlobalStore()
	if st == nil {
		return errStoreNotReady
	}
	// Bound the pre-busy DB read by the request ctx — if the HTTP
	// caller disconnected, there's no point completing the read. The
	// 30s ceiling defends against a stuck DB on a request stream
	// that's still alive but slow.
	srcCtx, srcCancel := boundedCtx(ctx)
	srcRec, err := st.GetMessage(srcCtx, rt.SourceID)
	srcCancel()
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrMessageNotFound
		}
		return err
	}
	srcMsg, err := recordToMessage(srcRec)
	if err != nil {
		return err
	}
	sourceETagSnapshot := srcRec.ETag

	// Prepare chat (system prompt, backend resolution) before touching the
	// transcript so a setup failure leaves history intact. Memory context
	// injection is skipped — the index still contains entries for messages
	// about to be truncated, which would otherwise leak into the prompt.
	prep, err := m.prepareChat(ctx, agentID, srcMsg.Content, true, true)
	if err != nil {
		return err
	}

	effectiveMessage := srcMsg.Content
	if len(srcMsg.Attachments) > 0 {
		effectiveMessage = formatMessageWithAttachments(srcMsg.Content, srcMsg.Attachments)
	}
	if prep.volatileContext != "" {
		effectiveMessage = prep.volatileContext + effectiveMessage
	}

	// Truncate atomically with the pivot AND source etag re-checks.
	// If a racing edit landed between findRegenerateTarget's snapshot
	// and now (on either the pivot or the source row), this returns
	// ErrMessageETagMismatch (or ErrMessageNotFound for a vanished
	// row) and prepareChat's setup work is wasted — but we never
	// tombstone, and never re-run the chat, against a stale view.
	if err := truncateForRegenerate(ctx, agentID, rt.PivotID, ifMatchETag, rt.SourceID, sourceETagSnapshot, rt.KillPivot); err != nil {
		return err
	}
	m.refreshLastMessage(agentID)

	chatCtx, cancel := context.WithCancel(context.Background())
	outCh := make(chan ChatEvent, 64)
	bc := newChatBroadcaster(outCh)

	// Register busy BEFORE backend.Chat so that a reloading client can
	// subscribe to the ongoing chat and see the "thinking" state while the
	// backend request is in flight.
	m.busyMu.Lock()
	delete(m.editing, agentID)
	editingReleased = true
	m.busy[agentID] = busyEntry{cancel: cancel, startedAt: time.Now(), broadcaster: bc}
	m.busyMu.Unlock()
	m.notifyChatStart(agentID)

	go func() {
		defer close(outCh)
		defer m.clearBusy(agentID)
		defer cancel()

		backendCh, err := prep.backend.Chat(chatCtx, &prep.agentCopy, effectiveMessage, prep.sysPrompt, ChatOptions{})
		if err != nil {
			// Abort before the stream started is not a failure —
			// exit silently so no error surfaces in the transcript.
			if errors.Is(err, context.Canceled) || chatCtx.Err() != nil {
				return
			}
			// Persist as a system message and fan out via the
			// broadcaster so subscribers see the failure.
			errMsg := newSystemMessage("⚠️ Error: " + err.Error())
			if appendErr := appendMessage(agentID, errMsg); appendErr != nil {
				m.logger.Warn("failed to persist regenerate error", "err", appendErr)
			}
			select {
			case outCh <- ChatEvent{Type: "error", ErrorMessage: err.Error()}:
			case <-chatCtx.Done():
			}
			return
		}
		m.processChatEvents(chatCtx, agentID, backendCh, outCh)
		m.updatePostChatIndex(agentID)
	}()
	return nil
}

// acquireTranscriptEdit verifies the agent exists and uses the llama.cpp
// backend, then reserves the agent's busy slot so no Chat can start during
// the edit. Returns ErrAgentBusy if a chat or reset is already in progress.
// The returned release func must always be called.
func (m *Manager) acquireTranscriptEdit(agentID string) (func(), error) {
	m.mu.Lock()
	a, ok := m.agents[agentID]
	if !ok {
		m.mu.Unlock()
		return nil, ErrAgentNotFound
	}
	if a.Tool != "llama.cpp" {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: only llama.cpp backend supports transcript editing", ErrUnsupportedTool)
	}
	// Hold m.mu while taking busyMu to close the TOCTOU window with Update()
	// (which changes Tool while holding m.mu). Chat acquires busyMu without
	// holding m.mu, so this ordering is safe.
	m.busyMu.Lock()
	if m.resetting[agentID] {
		m.busyMu.Unlock()
		m.mu.Unlock()
		return nil, ErrAgentResetting
	}
	if m.editing[agentID] {
		m.busyMu.Unlock()
		m.mu.Unlock()
		return nil, fmt.Errorf("%w, try again later", ErrAgentBusy)
	}
	if m.switching[agentID] {
		// §3.7 device switch is mid-flight: refuse transcript
		// edits (which trigger prepareChat side effects via
		// Regenerate) so the snapshot-on-source remains the
		// authoritative state target receives.
		m.busyMu.Unlock()
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: device switch in progress", ErrAgentBusy)
	}
	if _, busy := m.busy[agentID]; busy {
		m.busyMu.Unlock()
		m.mu.Unlock()
		return nil, fmt.Errorf("%w, try again later", ErrAgentBusy)
	}
	m.editing[agentID] = true
	m.busyMu.Unlock()
	m.mu.Unlock()

	release := func() {
		m.busyMu.Lock()
		delete(m.editing, agentID)
		m.busyMu.Unlock()
	}
	return release, nil
}

// refreshLastMessage recomputes the LastMessage preview from the transcript
// tail. Safe to call after an edit/delete that may have affected the last row.
func (m *Manager) refreshLastMessage(agentID string) {
	msgs, err := loadMessages(agentID, 1)
	if err != nil {
		m.logger.Warn("failed to refresh last message", "agent", agentID, "err", err)
		return
	}
	m.mu.Lock()
	a, ok := m.agents[agentID]
	if !ok {
		m.mu.Unlock()
		return
	}
	if len(msgs) == 0 {
		a.LastMessage = nil
	} else {
		last := msgs[len(msgs)-1]
		a.LastMessage = &MessagePreview{
			Content:   truncatePreview(last.Content, 100),
			Role:      last.Role,
			Timestamp: last.Timestamp,
		}
	}
	a.UpdatedAt = time.Now().Format(time.RFC3339)
	m.mu.Unlock()
	m.save()
}

// BackendAvailability returns which agent backends are available.
func (m *Manager) BackendAvailability() map[string]bool {
	result := make(map[string]bool, len(m.backends))
	for name, b := range m.backends {
		result[name] = b.Available()
	}
	return result
}

// Shutdown stops all cron jobs, notify polling, and cancels active chats.
//
// Short-circuits when StartSchedulers never ran (e.g. an early-exit
// CLI subcommand path that built a Manager but didn't reach the
// post-hydrate scheduler boot). Without this guard, notifyPoller.
// Stop would block on a never-launched poller goroutine and hang
// the shutdown sequence.
func (m *Manager) Shutdown() {
	if !m.schedulersStarted {
		return
	}
	m.cron.Stop()
	m.notifyPoller.Stop()

	m.busyMu.Lock()
	for _, entry := range m.busy {
		entry.cancel()
	}
	m.busyMu.Unlock()

	m.cancelAllOneShots()

	m.save()
}

// AgentDir returns the data directory path for the given agent ID.
// Exported for use by external subsystems (e.g. slackbot Hub) that need
// to resolve agent data directories without importing agent internals.
func AgentDir(id string) string {
	return agentDir(id)
}

func (m *Manager) save() {
	m.mu.Lock()
	agents := make([]*Agent, 0, len(m.agents))
	for _, a := range m.agents {
		agents = append(agents, copyAgent(a))
	}
	m.mu.Unlock()
	m.store.Save(agents)
}

// LockPatch returns a per-agent mutex acquired for the duration of an
// If-Match-gated mutation (currently only HTTP PATCH /api/v1/agents/{id}).
//
// The returned function MUST be called to release the lock; callers
// should use `defer release()` immediately after acquiring. Holding
// this lock across a precondition-check + Update + ETag-echo trio
// closes the TOCTOU window inherent in reading the store etag outside
// m.mu (m.mu is released between Update's in-memory mutation and
// m.save()'s store write, so two concurrent same-etag PATCHes would
// otherwise both pass the precheck).
//
// Cross-process / cross-device write coordination is the store-level
// optimistic-concurrency layer's job; LockPatch only serializes within
// one daemon process.
//
// We never delete entries from patchMus: agent IDs are bounded
// (effectively low-tens to low-thousands per daemon) and a freed
// *sync.Mutex would be just as small, so the GC saving isn't worth
// the bookkeeping complexity of "which mutex is currently unheld and
// safe to drop".
func (m *Manager) LockPatch(id string) (release func()) {
	m.patchMusMu.Lock()
	mu, ok := m.patchMus[id]
	if !ok {
		mu = &sync.Mutex{}
		m.patchMus[id] = mu
	}
	m.patchMusMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// copyAgent returns a deep copy of an Agent, including pointer fields.
func copyAgent(a *Agent) *Agent {
	cp := *a
	if a.LastMessage != nil {
		lm := *a.LastMessage
		cp.LastMessage = &lm
	}
	if a.NotifyDuringSilent != nil {
		v := *a.NotifyDuringSilent
		cp.NotifyDuringSilent = &v
	}
	if a.DeviceSwitchEnabled != nil {
		v := *a.DeviceSwitchEnabled
		cp.DeviceSwitchEnabled = &v
	}
	if a.SlackBot != nil {
		sb := *a.SlackBot
		cp.SlackBot = &sb
	}
	if a.TTS != nil {
		t := *a.TTS
		cp.TTS = &t
	}
	if len(a.NotifySources) > 0 {
		cp.NotifySources = make([]notifysource.Config, len(a.NotifySources))
		for i, ns := range a.NotifySources {
			cp.NotifySources[i] = ns
			if ns.Options != nil {
				cp.NotifySources[i].Options = make(map[string]string, len(ns.Options))
				for k, v := range ns.Options {
					cp.NotifySources[i].Options[k] = v
				}
			}
		}
	}
	return &cp
}

// resolvePublicProfile determines whether the agent's public profile needs
// regeneration based on persona/override changes, clearing the profile as needed.
// Returns true if background regeneration should be triggered.
//
// oldOverride is the agent's PublicProfileOverride value *before* the
// PATCH was applied. We need it to distinguish "the user just turned
// override OFF" (regenerate) from "override is still OFF and the form
// re-sent the same value" (don't regenerate every save).
func resolvePublicProfile(a *Agent, cfg AgentUpdateConfig, oldPersona string, oldOverride bool) bool {
	personaChanged := cfg.Persona != nil && *cfg.Persona != oldPersona
	overrideTurnedOff := cfg.PublicProfileOverride != nil &&
		!*cfg.PublicProfileOverride && oldOverride
	needsRegen := false
	if !a.PublicProfileOverride {
		if personaChanged && *cfg.Persona == "" {
			// Persona emptied → clear profile
			a.PublicProfile = ""
		} else if personaChanged && *cfg.Persona != "" {
			// Persona actually changed → will regenerate
			a.PublicProfile = ""
			needsRegen = true
		}
		if overrideTurnedOff && a.Persona != "" {
			// Override flipped from ON to OFF → will regenerate
			a.PublicProfile = ""
			needsRegen = true
		}
	}
	// Override OFF + empty persona → clear any leftover manual profile
	if !a.PublicProfileOverride && a.Persona == "" {
		a.PublicProfile = ""
	}
	return needsRegen
}

// isRateLimitMessage detects CLI rate-limit notices that should be shown
// as system messages rather than assistant chat bubbles.
// Only matches short messages with no tool uses to avoid false positives
// (e.g. an assistant explaining how rate limits work).
func isRateLimitMessage(msg *Message) bool {
	if msg == nil || msg.Content == "" || len(msg.ToolUses) > 0 {
		return false
	}
	// Rate limit notices are typically 1-2 lines
	if len([]rune(msg.Content)) > 300 {
		return false
	}
	lower := strings.ToLower(msg.Content)
	// Specific phrases from known CLIs; intentionally narrow to reduce FP.
	patterns := []string{
		"hit your limit",              // Claude CLI
		"rate limit exceeded",         // generic API error
		"resource exhausted",          // Google/Gemini
		"exceeded your current quota", // OpenAI
		"usage limit exceeded",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// formatMessageWithAttachments prepends attachment references to the user
// message so the CLI backend can access the files via its Read tool.
func formatMessageWithAttachments(message string, atts []MessageAttachment) string {
	var b strings.Builder
	b.WriteString("[Attached files — use your Read tool to view these files]\n")
	for _, a := range atts {
		b.WriteString("- ")
		b.WriteString(a.Path)
		b.WriteString(" (")
		b.WriteString(a.Name)
		b.WriteString(", ")
		b.WriteString(a.Mime)
		b.WriteString(")\n")
	}
	b.WriteString("\n")
	b.WriteString(message)
	return b.String()
}

func truncatePreview(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "…"
}
