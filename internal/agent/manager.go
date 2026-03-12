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

	"github.com/loppo-llc/kojo/internal/notifysource"
	"github.com/loppo-llc/kojo/internal/notifysource/gmail"
)

type busyEntry struct {
	cancel      context.CancelFunc
	startedAt   time.Time
	broadcaster *chatBroadcaster // fan-out for reconnecting clients
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

	// profileGen tracks agents with in-flight publicProfile generation.
	profileGen map[string]bool

	// cronPaused globally pauses all cron jobs when true.
	cronPaused bool

	// notifyPoller polls external notification sources.
	notifyPoller *notifyPoller
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
			"claude": NewClaudeBackend(logger),
			"codex":  NewCodexBackend(logger),
			"gemini": NewGeminiBackend(logger),
		},
		store:     newStore(logger),
		creds:     creds,
		logger:    logger,
		busy:       make(map[string]busyEntry),
		resetting:  make(map[string]bool),
		profileGen: make(map[string]bool),
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
		a.HasAvatar, a.AvatarHash = avatarMeta(a.ID)
		if !a.HasAvatar {
			a.AvatarHash = a.UpdatedAt
		}
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

	// Start cron schedules
	m.cron.Start()
	for _, a := range m.agents {
		if expr := intervalToCron(a.IntervalMinutes, a.ID); expr != "" {
			if err := m.cron.Schedule(a.ID, expr); err != nil {
				logger.Warn("failed to schedule cron", "agent", a.ID, "err", err)
			}
		}
	}

	// Start notify poller and rebuild sources for all agents
	m.notifyPoller.Start()
	for _, a := range m.agents {
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

	a.HasAvatar, a.AvatarHash = avatarMeta(a.ID)
	if !a.HasAvatar {
		a.AvatarHash = a.UpdatedAt
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
	m.syncPersona(id)
	has, hash := avatarMeta(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	if !ok {
		return nil, false
	}
	a.HasAvatar = has
	a.AvatarHash = hash
	if !has {
		a.AvatarHash = a.UpdatedAt
	}
	return copyAgent(a), true
}

// List returns deep copies of all agents.
func (m *Manager) List() []*Agent {
	// Collect IDs first, then sync persona outside the main lock
	m.mu.Lock()
	ids := make([]string, 0, len(m.agents))
	for id := range m.agents {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		m.syncPersona(id)
	}

	// Compute avatar info outside lock (disk I/O)
	type avInfo struct{ has bool; hash string }
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
			a.HasAvatar = info.has
			a.AvatarHash = info.hash
			if !info.has {
				a.AvatarHash = a.UpdatedAt
			}
		}
		list = append(list, copyAgent(a))
	}
	return list
}

// Directory returns minimal public info for all agents (for agent-to-agent discovery).
func (m *Manager) Directory() []DirectoryEntry {
	// Sync persona first (may trigger publicProfile regeneration)
	m.mu.Lock()
	ids := make([]string, 0, len(m.agents))
	for id := range m.agents {
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
	has, hash := avatarMeta(id)

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
	if cfg.Tool != nil {
		a.Tool = *cfg.Tool
	}
	// Validate before mutating
	if cfg.IntervalMinutes != nil && !ValidInterval(*cfg.IntervalMinutes) {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %d minutes", ErrUnsupportedInterval, *cfg.IntervalMinutes)
	}
	{
		s, e := a.ActiveStart, a.ActiveEnd
		if cfg.ActiveStart != nil {
			s = *cfg.ActiveStart
		}
		if cfg.ActiveEnd != nil {
			e = *cfg.ActiveEnd
		}
		if err := ValidActiveHours(s, e); err != nil {
			m.mu.Unlock()
			return nil, err
		}
	}

	oldInterval := a.IntervalMinutes
	if cfg.IntervalMinutes != nil {
		a.IntervalMinutes = *cfg.IntervalMinutes
	}
	if cfg.ActiveStart != nil {
		a.ActiveStart = *cfg.ActiveStart
	}
	if cfg.ActiveEnd != nil {
		a.ActiveEnd = *cfg.ActiveEnd
	}
	newInterval := a.IntervalMinutes
	a.UpdatedAt = time.Now().Format(time.RFC3339)
	a.HasAvatar = has
	a.AvatarHash = hash
	if !has {
		a.AvatarHash = a.UpdatedAt
	}

	needsRegen := resolvePublicProfile(a, cfg, oldPersona)

	// Take a copy for return and post-lock operations
	cp := copyAgent(a)
	currentPersona := a.Persona
	m.mu.Unlock()

	if oldInterval != newInterval {
		expr := intervalToCron(newInterval, id)
		if err := m.cron.Schedule(id, expr); err != nil {
			m.logger.Warn("failed to update cron", "agent", id, "err", err)
		}
	}

	if needsRegen {
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
	m.mu.Unlock()

	m.save()
	m.notifyPoller.RebuildSources(id, sources)
	return nil
}

// ResetData removes conversation logs and memory but keeps settings, persona, avatar, and credentials.
func (m *Manager) ResetData(id string) error {
	m.mu.Lock()
	a, ok := m.agents[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}
	name := a.Name
	m.mu.Unlock()

	// Mark agent as resetting to block new chats/deletes/concurrent resets
	m.busyMu.Lock()
	if m.resetting[id] {
		m.busyMu.Unlock()
		return fmt.Errorf("%w, try again later", ErrAgentBusy)
	}
	m.resetting[id] = true
	if entry, busy := m.busy[id]; busy {
		entry.cancel()
	}
	m.busyMu.Unlock()

	defer func() {
		m.busyMu.Lock()
		delete(m.resetting, id)
		m.busyMu.Unlock()
	}()

	// Wait for the chat goroutine to finish (max 5s)
	for i := 0; i < 50; i++ {
		m.busyMu.Lock()
		_, busy := m.busy[id]
		m.busyMu.Unlock()
		if !busy {
			break
		}
		if i == 49 {
			return fmt.Errorf("%w, try again later", ErrAgentBusy)
		}
		time.Sleep(100 * time.Millisecond)
	}

	dir := agentDir(id)

	// Remove conversation log
	if err := os.Remove(filepath.Join(dir, messagesFile)); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("reset: failed to remove messages", "agent", id, "err", err)
	}

	// Remove memory files
	if err := os.RemoveAll(filepath.Join(dir, "memory")); err != nil {
		m.logger.Warn("reset: failed to remove memory dir", "agent", id, "err", err)
	}
	if err := os.Remove(filepath.Join(dir, "MEMORY.md")); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("reset: failed to remove MEMORY.md", "agent", id, "err", err)
	}

	// Remove FTS index
	if err := os.RemoveAll(filepath.Join(dir, indexDir)); err != nil {
		m.logger.Warn("reset: failed to remove index dir", "agent", id, "err", err)
	}

	// Remove persona summary cache (will be regenerated)
	if err := os.Remove(filepath.Join(dir, "persona_summary.md")); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("reset: failed to remove persona summary", "agent", id, "err", err)
	}

	// Remove cron lock file
	if err := os.Remove(filepath.Join(dir, cronLockFile)); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("reset: failed to remove cron lock", "agent", id, "err", err)
	}

	// Remove CLI local state so next chat starts fresh
	if err := os.RemoveAll(filepath.Join(dir, ".claude")); err != nil {
		m.logger.Warn("reset: failed to remove .claude dir", "agent", id, "err", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, ".gemini")); err != nil {
		m.logger.Warn("reset: failed to remove .gemini dir", "agent", id, "err", err)
	}

	// Clear global CLI session stores
	clearClaudeSession(id)
	clearGeminiSession(id)

	// Recreate empty memory directory and MEMORY.md (required for agent to function)
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
		return fmt.Errorf("recreate memory dir: %w", err)
	}
	initial := fmt.Sprintf("# %s's Memory\n\nThis file stores persistent memories. Update it as you learn new things.\n", name)
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(initial), 0o644); err != nil {
		return fmt.Errorf("recreate MEMORY.md: %w", err)
	}

	// Clear last message preview
	m.mu.Lock()
	if a, ok := m.agents[id]; ok {
		a.LastMessage = nil
		a.UpdatedAt = time.Now().Format(time.RFC3339)
	}
	m.mu.Unlock()

	m.save()
	m.logger.Info("agent data reset", "id", id)
	return nil
}

// Credentials returns the credential store. Returns nil if the store failed to initialize.
func (m *Manager) Credentials() *CredentialStore {
	return m.creds
}

// HasCredentials returns true if the credential store is available.
func (m *Manager) HasCredentials() bool {
	return m.creds != nil
}

// Delete removes an agent and its data.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	_, ok := m.agents[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}

	// Block if agent is being reset
	m.busyMu.Lock()
	if m.resetting[id] {
		m.busyMu.Unlock()
		m.mu.Unlock()
		return fmt.Errorf("%w, try again later", ErrAgentBusy)
	}
	// Abort any running chat
	if entry, busy := m.busy[id]; busy {
		entry.cancel()
	}
	m.busyMu.Unlock()
	m.mu.Unlock()

	// Remove credentials and notify tokens outside lock (DB I/O)
	if m.creds != nil {
		if err := m.creds.DeleteAllForAgent(id); err != nil {
			return fmt.Errorf("delete credentials: %w", err)
		}
		if err := m.creds.DeleteTokensByAgent(id); err != nil {
			m.logger.Warn("failed to delete notify tokens", "agent", id, "err", err)
		}
	}

	m.cron.Remove(id)
	m.notifyPoller.RemoveAgent(id)

	// Remove agent from group DMs
	if m.groupdms != nil {
		m.groupdms.RemoveAgent(id)
	}

	// Remove agent data directory (best-effort: credentials/cron/notify already cleaned up)
	dir := agentDir(id)
	if err := os.RemoveAll(dir); err != nil {
		m.logger.Warn("failed to remove agent dir", "agent", id, "err", err)
	}

	// Remove from in-memory map
	m.mu.Lock()
	delete(m.agents, id)
	m.mu.Unlock()

	m.save()
	m.logger.Info("agent deleted", "id", id)
	return nil
}

// Chat sends a message to an agent and returns a channel of streaming events.
// The role parameter controls how the input message is stored in the transcript
// ("user" for interactive chat, "system" for cron-triggered messages).
func (m *Manager) Chat(ctx context.Context, agentID string, userMessage string, role string, attachments []MessageAttachment) (<-chan ChatEvent, error) {
	// Sync persona.md → Agent.Persona before chat
	m.syncPersona(agentID)

	m.mu.Lock()
	a, ok := m.agents[agentID]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, agentID)
	}
	// Copy agent data under lock
	agentCopy := *a
	m.mu.Unlock()

	// Check if agent is busy or being reset
	m.busyMu.Lock()
	if m.resetting[agentID] {
		m.busyMu.Unlock()
		return nil, ErrAgentResetting
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
	m.busy[agentID] = busyEntry{cancel: cancel, startedAt: time.Now(), broadcaster: bc}
	m.busyMu.Unlock()

	// Get the backend
	backend, ok := m.backends[agentCopy.Tool]
	if !ok {
		err := fmt.Errorf("%w: %s", ErrUnsupportedTool, agentCopy.Tool)
		outCh <- ChatEvent{Type: "error", ErrorMessage: err.Error()}
		close(outCh)
		m.clearBusy(agentID)
		cancel()
		return nil, err
	}

	atts := attachments

	// Save input message to transcript
	var inputMsg *Message
	if role == "system" {
		inputMsg = newSystemMessage(userMessage)
	} else {
		inputMsg = newUserMessage(userMessage, atts)
	}
	if err := appendMessage(agentID, inputMsg); err != nil {
		m.logger.Warn("failed to save input message", "err", err)
	}

	// Build the effective message for the backend.
	// When attachments are present, prepend file references so the CLI
	// can access them (e.g. via Read tool for images/text).
	effectiveMessage := userMessage
	if len(atts) > 0 {
		effectiveMessage = formatMessageWithAttachments(userMessage, atts)
	}

	// Build system prompt with group DM context
	var apiBase string
	var groups []*GroupDM
	if m.groupdms != nil {
		apiBase = m.groupdms.APIBase()
		groups = m.groupdms.GroupsForAgent(agentID)
	}
	systemPrompt := buildSystemPrompt(&agentCopy, m.logger, apiBase, groups, m.creds != nil)

	// Start chat
	backendCh, err := backend.Chat(chatCtx, &agentCopy, effectiveMessage, systemPrompt)
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
	}()

	return callerCh, nil
}

// processChatEvents reads events from the backend channel, persists messages,
// and forwards events to outCh for the broadcaster.
func (m *Manager) processChatEvents(ctx context.Context, agentID string, backendCh <-chan ChatEvent, outCh chan<- ChatEvent) {
	for {
		select {
		case event, ok := <-backendCh:
			if !ok {
				return
			}
			// Convert rate-limit notices to system messages before
			// sending, so both UI and transcript see the same role.
			if event.Type == "done" && event.Message != nil && isRateLimitMessage(event.Message) {
				event.Message.Role = "system"
			}

			// Save assistant message to transcript BEFORE publishing
			// the terminal event, so synthesizeTerminal can find it.
			if event.Type == "done" && event.Message != nil {
				m.persistDoneEvent(agentID, event.Message)
			}

			// Persist process errors as system messages so they survive
			// page reloads and appear in the transcript history.
			// This covers both:
			//   - terminal "error" events (no output captured)
			//   - "done" events with ErrorMessage (partial output + error)
			if event.ErrorMessage != "" {
				errMsg := newSystemMessage("⚠️ Error: " + event.ErrorMessage)
				if err := appendMessage(agentID, errMsg); err != nil {
					m.logger.Warn("failed to save error message", "err", err)
				}
			}

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

// Abort cancels any running chat for an agent.
func (m *Manager) Abort(agentID string) {
	m.busyMu.Lock()
	if entry, ok := m.busy[agentID]; ok {
		entry.cancel()
	}
	m.busyMu.Unlock()
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

// IsBusy returns true if the agent has an active chat.
func (m *Manager) IsBusy(agentID string) bool {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	_, ok := m.busy[agentID]
	return ok
}

// BusySince returns the time when the agent started its current chat.
// Returns zero time and false if the agent is not busy.
func (m *Manager) BusySince(agentID string) (time.Time, bool) {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	entry, ok := m.busy[agentID]
	if !ok {
		return time.Time{}, false
	}
	return entry.startedAt, true
}

// Subscribe returns a snapshot of all past events and a live channel for an
// agent's ongoing chat. The caller must call unsub when done to free resources.
// If the agent is not busy, busy is false and all other values are zero.
func (m *Manager) Subscribe(agentID string) (startedAt time.Time, past []ChatEvent, live <-chan ChatEvent, unsub func(), busy bool) {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	entry, ok := m.busy[agentID]
	if !ok {
		return time.Time{}, nil, nil, func() {}, false
	}
	if entry.broadcaster == nil {
		return entry.startedAt, nil, nil, func() {}, true
	}
	past, live, unsub = entry.broadcaster.Subscribe()
	return entry.startedAt, past, live, unsub, true
}

// Messages returns recent messages for an agent.
func (m *Manager) Messages(agentID string, limit int) ([]*Message, error) {
	return loadMessages(agentID, limit)
}

// MessagesPaginated returns messages with cursor-based pagination.
func (m *Manager) MessagesPaginated(agentID string, limit int, before string) ([]*Message, bool, error) {
	return loadMessagesPaginated(agentID, limit, before)
}

// Shutdown stops all cron jobs, notify polling, and cancels active chats.
func (m *Manager) Shutdown() {
	m.cron.Stop()
	m.notifyPoller.Stop() // cancels in-flight delivery goroutines via stopCtx

	m.busyMu.Lock()
	for _, entry := range m.busy {
		entry.cancel()
	}
	m.busyMu.Unlock()

	m.save()
}

func (m *Manager) clearBusy(agentID string) {
	m.busyMu.Lock()
	delete(m.busy, agentID)
	m.busyMu.Unlock()
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
		"hit your limit",      // Claude CLI
		"rate limit exceeded",  // generic API error
		"resource exhausted",  // Google/Gemini
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
