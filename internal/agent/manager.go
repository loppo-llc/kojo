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

	"github.com/loppo-llc/kojo/internal/notifysource"
	"github.com/loppo-llc/kojo/internal/notifysource/gmail"
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
	store    *store
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

	// profileGen tracks agents with in-flight publicProfile generation.
	profileGen map[string]bool

	// cronPaused globally pauses all cron jobs when true.
	cronPaused bool

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
func NewManager(logger *slog.Logger) *Manager {
	creds, err := NewCredentialStore()
	if err != nil {
		logger.Warn("failed to open credential store", "err", err)
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
		store:          newStore(logger),
		creds:          creds,
		logger:         logger,
		busy:           make(map[string]busyEntry),
		resetting:      make(map[string]bool),
		editing:        make(map[string]bool),
		profileGen:     make(map[string]bool),
		memIndexes:     make(map[string]*MemoryIndex),
		chatWatchers:   make(map[string]map[*chatWatcher]struct{}),
		oneShotCancels: make(map[string]map[int64]context.CancelFunc),
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
		has, hash := avatarMeta(a.ID)
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

	// Start cron schedules. Skip archived agents — their cron stays detached
	// until Unarchive re-schedules.
	m.cron.Start()
	for _, a := range m.agents {
		if a.Archived {
			continue
		}
		if expr := intervalToCron(a.IntervalMinutes, a.ID); expr != "" {
			if err := m.cron.Schedule(a.ID, expr); err != nil {
				logger.Warn("failed to schedule cron", "agent", a.ID, "err", err)
			}
		}
	}

	// Start notify poller and rebuild sources for all agents (skip archived).
	m.notifyPoller.Start()
	for _, a := range m.agents {
		if a.Archived {
			continue
		}
		if len(a.NotifySources) > 0 {
			m.notifyPoller.RebuildSources(a.ID, a.NotifySources)
		}
	}

	return m
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

	has, hash := avatarMeta(a.ID)
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

	if expr := intervalToCron(a.IntervalMinutes, a.ID); expr != "" {
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

// syncPersona reads persona.md and updates Agent.Persona if it has changed.
// This makes persona.md the single source of truth — the agent can edit it
// to evolve its personality, and the change is reflected in settings.
func (m *Manager) syncPersona(agentID string) {
	// Check existence under lock, then release for file I/O
	m.mu.Lock()
	_, exists := m.agents[agentID]
	m.mu.Unlock()
	if !exists {
		return
	}

	// Read file outside lock to avoid blocking other operations
	content, ok := readPersonaFile(agentID)
	if !ok {
		return
	}

	// Re-acquire lock to compare and update
	m.mu.Lock()
	a, exists := m.agents[agentID]
	if !exists {
		m.mu.Unlock()
		return
	}
	if a.Persona == content {
		// Persona unchanged — but backfill publicProfile if missing
		if content != "" && a.PublicProfile == "" && !a.PublicProfileOverride {
			persona := content
			m.mu.Unlock()
			go m.regeneratePublicProfile(agentID, persona)
		} else {
			m.mu.Unlock()
		}
		return
	}
	a.Persona = content
	tool := a.Tool
	override := a.PublicProfileOverride
	a.UpdatedAt = time.Now().Format(time.RFC3339)
	m.mu.Unlock()
	m.save()

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
			m.mu.Unlock()
			m.save()
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
	has, hash := avatarMeta(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok = m.agents[id]
	if !ok {
		return nil, false
	}
	applyAvatarMeta(a, has, hash)
	return copyAgent(a), true
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
		has, hash := avatarMeta(id)
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
func (m *Manager) regeneratePublicProfile(agentID, persona string) {
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
	m.mu.Lock()
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
func (m *Manager) Update(id string, cfg AgentUpdateConfig) (*Agent, error) {
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
	// Reject invalid resume-idle presets up front so a malformed PATCH cannot
	// land partial mutations (Persona / Name / Model are applied before the
	// later validation block, so deferring this check would let a bad value
	// still corrupt sibling fields on the in-memory Agent).
	if cfg.ResumeIdleMinutes != nil && !ValidResumeIdle(*cfg.ResumeIdleMinutes) {
		return nil, fmt.Errorf("unsupported resumeIdle: %d minutes", *cfg.ResumeIdleMinutes)
	}

	// Check agent exists before any file I/O
	m.mu.Lock()
	a, ok := m.agents[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}
	m.mu.Unlock()

	// Write persona.md outside lock — if it fails, no in-memory state is modified
	if cfg.Persona != nil {
		if err := writePersonaFile(id, *cfg.Persona); err != nil {
			return nil, fmt.Errorf("write persona.md: %w", err)
		}
	}

	// Pre-fetch avatar info outside lock (disk I/O)
	avHas, avHash := avatarMeta(id)

	m.mu.Lock()
	// Re-check: agent may have been deleted concurrently
	a, ok = m.agents[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}

	oldPersona := a.Persona
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
		if !ValidEffort(*cfg.Effort) {
			m.mu.Unlock()
			return nil, fmt.Errorf("unsupported effort level: %q", *cfg.Effort)
		}
		a.Effort = *cfg.Effort
	}
	if !ValidModelEffort(a.Model, a.Effort) {
		m.mu.Unlock()
		return nil, fmt.Errorf("unsupported effort level %q for model %q", a.Effort, a.Model)
	}
	if cfg.Tool != nil {
		a.Tool = *cfg.Tool
	}
	if cfg.WorkDir != nil {
		if *cfg.WorkDir != "" {
			if !filepath.IsAbs(*cfg.WorkDir) {
				m.mu.Unlock()
				return nil, fmt.Errorf("workDir must be an absolute path: %s", *cfg.WorkDir)
			}
			if info, err := os.Stat(*cfg.WorkDir); err != nil || !info.IsDir() {
				m.mu.Unlock()
				return nil, fmt.Errorf("workDir does not exist or is not a directory: %s", *cfg.WorkDir)
			}
		}
		a.WorkDir = *cfg.WorkDir
	}
	// Validate before mutating
	if cfg.IntervalMinutes != nil && !ValidInterval(*cfg.IntervalMinutes) {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %d minutes", ErrUnsupportedInterval, *cfg.IntervalMinutes)
	}
	if cfg.TimeoutMinutes != nil && !ValidTimeout(*cfg.TimeoutMinutes) {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %d minutes", ErrUnsupportedTimeout, *cfg.TimeoutMinutes)
	}
	// ResumeIdleMinutes is validated up-front (before any I/O / mutation).
	{
		s, e := a.SilentStart, a.SilentEnd
		if cfg.SilentStart != nil {
			s = *cfg.SilentStart
		}
		if cfg.SilentEnd != nil {
			e = *cfg.SilentEnd
		}
		if err := ValidSilentHours(s, e); err != nil {
			m.mu.Unlock()
			return nil, err
		}
	}
	oldInterval := a.IntervalMinutes
	if cfg.IntervalMinutes != nil {
		a.IntervalMinutes = *cfg.IntervalMinutes
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
	{
		finalBaseURL := a.CustomBaseURL
		if cfg.CustomBaseURL != nil {
			finalBaseURL = *cfg.CustomBaseURL
		}
		if (a.Tool == "custom" || a.Tool == "llama.cpp") && finalBaseURL == "" {
			m.mu.Unlock()
			return nil, fmt.Errorf("customBaseURL is required for %s tool", a.Tool)
		}
		a.CustomBaseURL = finalBaseURL
	}
	if cfg.AllowedTools != nil {
		a.AllowedTools = cfg.AllowedTools
	}
	if cfg.AllowProtectedPaths != nil {
		a.AllowProtectedPaths = normalizeAllowProtectedPaths(*cfg.AllowProtectedPaths)
	}
	if cfg.ThinkingMode != nil {
		if !ValidThinkingMode(*cfg.ThinkingMode) {
			m.mu.Unlock()
			return nil, fmt.Errorf("unsupported thinkingMode: %q", *cfg.ThinkingMode)
		}
		a.ThinkingMode = NormalizeThinkingMode(*cfg.ThinkingMode)
	}
	if cronMessageDirty {
		a.CronMessage = nextCronMessage
	}
	newInterval := a.IntervalMinutes
	a.UpdatedAt = time.Now().Format(time.RFC3339)
	applyAvatarMeta(a, avHas, avHash)

	needsRegen := resolvePublicProfile(a, cfg, oldPersona)

	// Take a copy for return and post-lock operations
	cp := copyAgent(a)
	currentPersona := a.Persona
	m.mu.Unlock()

	if oldInterval != newInterval {
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
			expr := intervalToCron(newInterval, id)
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
	return cp, nil
}

// UpdateNotifySources updates the notification source configs for an agent
// and rebuilds the poller's source instances.
func (m *Manager) UpdateNotifySources(id string, sources []notifysource.Config) error {
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
func (m *Manager) prepareChat(agentID, query string, indexNewMessages bool, skipMemoryContext bool) (*chatPrep, error) {
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

	m.syncPersona(agentID)

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
	}

	// Build MCP server list (backend-agnostic, URL-based).
	hasSlackBot := m.loadSlackBotToken(agentID, &agentCopy) != ""
	mcpServers := BuildMCPServers(agentID, apiBase, hasSlackBot)

	// MCP servers are injected per-backend:
	// - Claude: --mcp-config CLI arg (in backend_claude.go)
	// - Codex: -c flag override (in backend_codex.go)
	// - Gemini: .gemini/settings.json mcpServers (in backend_gemini.go)

	// Look up the agent's auth token for direct injection into the system
	// prompt. The token is NOT passed via environment variable to avoid
	// /proc/{pid}/environ exposure to other agents on the same host.
	var agentToken string
	if agentTokenLookup != nil {
		if tok, ok := agentTokenLookup(agentID); ok {
			agentToken = tok
		}
	}
	sysPrompt := buildSystemPrompt(&agentCopy, m.logger, apiBase, agentToken, groups, m.creds != nil)

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
	volatileContext := BuildVolatileContext(agentID, queryContext)

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
	prep, err := m.prepareChat(agentID, userMessage, true, false)
	if err != nil {
		return nil, err
	}

	// Check if agent is busy, editing, or being reset
	m.busyMu.Lock()
	if m.resetting[agentID] {
		m.busyMu.Unlock()
		return nil, ErrAgentResetting
	}
	if m.editing[agentID] {
		m.busyMu.Unlock()
		return nil, ErrAgentBusy
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

// ChatOneShot runs a one-shot chat that does not save to transcript
// (messages.jsonl) and does not resume the CLI session. Used for external
// platform conversations (Slack, Discord) that carry their own context.
// Memory (MEMORY.md, diary) access is still available via system prompt.
func (m *Manager) ChatOneShot(ctx context.Context, agentID string, userMessage string) (<-chan ChatEvent, error) {
	prep, err := m.prepareChat(agentID, userMessage, false, false)
	if err != nil {
		return nil, err
	}

	m.busyMu.Lock()
	if m.resetting[agentID] {
		m.busyMu.Unlock()
		return nil, ErrAgentResetting
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
			m.persistDoneEvent(agentID, event.Message)

			if m.OnChatDone != nil && event.ErrorMessage == "" {
				m.mu.Lock()
				agCopy := *m.agents[agentID]
				m.mu.Unlock()
				msgCopy := *event.Message
				go m.OnChatDone(&agCopy, &msgCopy)
			}
		}
		if event.ErrorMessage != "" {
			errMsg := newSystemMessage("⚠️ Error: " + event.ErrorMessage)
			if err := appendMessage(agentID, errMsg); err != nil {
				m.logger.Warn("failed to save error message", "err", err)
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

// NextCronRun returns the next scheduled run time for an agent, adjusted
// for silent hours. Returns the zero Time when there is no run to predict
// because the agent has no schedule, is archived, doesn't exist, or all
// cron is globally paused — anything that would make the displayed time
// misleading.
func (m *Manager) NextCronRun(agentID string) time.Time {
	m.mu.Lock()
	if m.cronPaused {
		m.mu.Unlock()
		return time.Time{}
	}
	a, ok := m.agents[agentID]
	if !ok || a.Archived || a.IntervalMinutes <= 0 {
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

// SetCronPaused sets the global cron pause state and persists it.
func (m *Manager) SetCronPaused(paused bool) {
	m.mu.Lock()
	m.cronPaused = paused
	m.mu.Unlock()
	m.store.SaveCronPaused(paused)
	m.logger.Info("cron pause toggled", "paused", paused)
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
func (m *Manager) UpdateMessageContent(agentID, msgID, content string) (*Message, error) {
	release, err := m.acquireTranscriptEdit(agentID)
	if err != nil {
		return nil, err
	}
	defer release()
	msg, err := updateMessageContent(agentID, msgID, content)
	if err != nil {
		return nil, err
	}
	m.refreshLastMessage(agentID)
	return msg, nil
}

// DeleteMessage removes a single message from the transcript.
// Only supported for the llama.cpp backend. Rejected with ErrAgentBusy while the
// agent has an active chat.
func (m *Manager) DeleteMessage(agentID, msgID string) error {
	release, err := m.acquireTranscriptEdit(agentID)
	if err != nil {
		return err
	}
	defer release()
	if err := deleteMessage(agentID, msgID); err != nil {
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
func (m *Manager) Regenerate(agentID, msgID string) error {
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

	target, keepCount, err := findRegenerateTarget(agentID, msgID)
	if err != nil {
		return err
	}

	// Prepare chat (system prompt, backend resolution) before touching the
	// transcript so a setup failure leaves history intact. Memory context
	// injection is skipped — the index still contains entries for messages
	// about to be truncated, which would otherwise leak into the prompt.
	prep, err := m.prepareChat(agentID, target.Content, true, true)
	if err != nil {
		return err
	}

	effectiveMessage := target.Content
	if len(target.Attachments) > 0 {
		effectiveMessage = formatMessageWithAttachments(target.Content, target.Attachments)
	}
	if prep.volatileContext != "" {
		effectiveMessage = prep.volatileContext + effectiveMessage
	}

	// Truncate synchronously so that any client reloading during the
	// regeneration reads a consistent transcript via GET /messages.
	if err := truncateMessagesTo(agentID, keepCount); err != nil {
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
func (m *Manager) Shutdown() {
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
	if a.SlackBot != nil {
		sb := *a.SlackBot
		cp.SlackBot = &sb
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
func resolvePublicProfile(a *Agent, cfg AgentUpdateConfig, oldPersona string) bool {
	personaChanged := cfg.Persona != nil && *cfg.Persona != oldPersona
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
		if cfg.PublicProfileOverride != nil && !*cfg.PublicProfileOverride && a.Persona != "" {
			// Override turned OFF → will regenerate
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
