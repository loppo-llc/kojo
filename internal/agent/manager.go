package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// Manager manages agent CRUD, chat orchestration, and lifecycle.
type Manager struct {
	mu       sync.Mutex
	agents   map[string]*Agent
	backends map[string]ChatBackend
	store    *store
	cron     *cronScheduler
	logger   *slog.Logger

	// busy tracks which agents have an active chat.
	busy   map[string]context.CancelFunc
	busyMu sync.Mutex
}

// NewManager creates a new agent manager.
func NewManager(logger *slog.Logger) *Manager {
	m := &Manager{
		agents: make(map[string]*Agent),
		backends: map[string]ChatBackend{
			"claude": NewClaudeBackend(logger),
			"codex":  NewCodexBackend(logger),
			"gemini": NewGeminiBackend(logger),
		},
		store:  newStore(logger),
		logger: logger,
		busy:   make(map[string]context.CancelFunc),
	}

	m.cron = newCronScheduler(m, logger)

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
		if a.CronExpr != "" {
			if err := m.cron.Schedule(a.ID, a.CronExpr); err != nil {
				logger.Warn("failed to schedule cron", "agent", a.ID, "err", err)
			}
		}
	}

	return m
}

// Create creates a new agent.
func (m *Manager) Create(cfg AgentConfig) (*Agent, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("name is required")
	}

	a := newAgent(cfg)

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

	if a.CronExpr != "" {
		if err := m.cron.Schedule(a.ID, a.CronExpr); err != nil {
			m.logger.Warn("failed to schedule cron", "agent", a.ID, "err", err)
		}
	}

	m.logger.Info("agent created", "id", a.ID, "name", a.Name)
	return a, nil
}

// syncPersona reads persona.md and updates Agent.Persona if it has changed.
// This makes persona.md the single source of truth — the agent can edit it
// to evolve its personality, and the change is reflected in settings.
func (m *Manager) syncPersona(agentID string) {
	m.mu.Lock()
	a, exists := m.agents[agentID]
	if !exists {
		m.mu.Unlock()
		return
	}
	// Read file under lock to prevent race with Update's writePersonaFile
	content, ok := readPersonaFile(agentID)
	if !ok || a.Persona == content {
		m.mu.Unlock()
		return
	}
	a.Persona = content
	tool := a.Tool
	a.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	m.mu.Unlock()
	m.save()

	// Pre-generate persona summary in background so it's cached for next chat
	if len([]rune(content)) > maxPersonaSummaryRunes {
		go getPersonaSummary(agentID, content, tool, m.logger)
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

// Update updates an agent's configuration. Only non-nil fields are applied.
func (m *Manager) Update(id string, cfg AgentUpdateConfig) (*Agent, error) {
	has, hash := avatarMeta(id)
	m.mu.Lock()
	a, ok := m.agents[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("agent not found: %s", id)
	}

	// Write persona.md first — if it fails, no in-memory state is modified
	if cfg.Persona != nil {
		if err := writePersonaFile(a.ID, *cfg.Persona); err != nil {
			m.mu.Unlock()
			return nil, fmt.Errorf("write persona.md: %w", err)
		}
		a.Persona = *cfg.Persona
	}
	if cfg.Name != nil {
		a.Name = *cfg.Name
	}
	if cfg.Model != nil {
		a.Model = *cfg.Model
	}
	if cfg.Tool != nil {
		a.Tool = *cfg.Tool
	}
	oldCron := a.CronExpr
	if cfg.CronExpr != nil {
		a.CronExpr = *cfg.CronExpr
	}
	newCron := a.CronExpr
	a.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	a.HasAvatar = has
	a.AvatarHash = hash
	if !has {
		a.AvatarHash = a.UpdatedAt
	}

	// Take a copy for return and post-lock operations
	cp := copyAgent(a)
	m.mu.Unlock()

	if oldCron != newCron {
		if err := m.cron.Schedule(id, newCron); err != nil {
			m.logger.Warn("failed to update cron", "agent", id, "err", err)
		}
	}

	m.save()
	return cp, nil
}

// Delete removes an agent and its data.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	_, ok := m.agents[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("agent not found: %s", id)
	}

	// Abort any running chat
	m.busyMu.Lock()
	if cancel, busy := m.busy[id]; busy {
		cancel()
	}
	m.busyMu.Unlock()

	delete(m.agents, id)
	m.mu.Unlock()

	m.cron.Remove(id)

	// Remove agent data directory
	dir := agentDir(id)
	os.RemoveAll(dir)

	m.save()
	m.logger.Info("agent deleted", "id", id)
	return nil
}

// Chat sends a message to an agent and returns a channel of streaming events.
// The role parameter controls how the input message is stored in the transcript
// ("user" for interactive chat, "system" for cron-triggered messages).
func (m *Manager) Chat(ctx context.Context, agentID string, userMessage string, role string) (<-chan ChatEvent, error) {
	// Sync persona.md → Agent.Persona before chat
	m.syncPersona(agentID)

	m.mu.Lock()
	a, ok := m.agents[agentID]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("agent not found: %s", agentID)
	}
	// Copy agent data under lock
	agentCopy := *a
	m.mu.Unlock()

	// Check if agent is busy
	m.busyMu.Lock()
	if _, busy := m.busy[agentID]; busy {
		m.busyMu.Unlock()
		return nil, fmt.Errorf("agent is busy")
	}
	chatCtx, cancel := context.WithCancel(ctx)
	m.busy[agentID] = cancel
	m.busyMu.Unlock()

	// Get the backend
	backend, ok := m.backends[agentCopy.Tool]
	if !ok {
		m.clearBusy(agentID)
		cancel()
		return nil, fmt.Errorf("unsupported tool: %s", agentCopy.Tool)
	}

	// Save input message to transcript
	var inputMsg *Message
	if role == "system" {
		inputMsg = newSystemMessage(userMessage)
	} else {
		inputMsg = newUserMessage(userMessage)
	}
	if err := appendMessage(agentID, inputMsg); err != nil {
		m.logger.Warn("failed to save input message", "err", err)
	}

	// Build system prompt
	systemPrompt := buildSystemPrompt(&agentCopy, m.logger)

	// Start chat
	backendCh, err := backend.Chat(chatCtx, &agentCopy, userMessage, systemPrompt)
	if err != nil {
		m.clearBusy(agentID)
		cancel()
		return nil, err
	}

	// Wrap the backend channel to handle completion
	outCh := make(chan ChatEvent, 64)
	go func() {
		defer close(outCh)
		defer m.clearBusy(agentID)
		defer cancel()

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

				// Terminal events (done/error) use blocking send so the
			// client always receives them. Streaming events use
			// non-blocking send — if no reader (WS disconnected),
			// they are dropped but processing continues.
				if event.Type == "done" || event.Type == "error" {
					select {
					case outCh <- event:
					case <-chatCtx.Done():
						return
					}
				} else {
					select {
					case outCh <- event:
					default:
					}
				}

				// Save assistant message to transcript and update last message
				if event.Type == "done" && event.Message != nil {
					if err := appendMessage(agentID, event.Message); err != nil {
						m.logger.Warn("failed to save assistant message", "err", err)
					}

					// Sync persona.md → Agent.Persona (agent may have edited it)
					m.syncPersona(agentID)

					// Update last message preview
					m.mu.Lock()
					if ag, ok := m.agents[agentID]; ok {
						ag.LastMessage = &MessagePreview{
							Content:   truncatePreview(event.Message.Content, 100),
							Role:      event.Message.Role,
							Timestamp: event.Message.Timestamp,
						}
						ag.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
					}
					m.mu.Unlock()
					m.save()
				}
			case <-chatCtx.Done():
				return
			}
		}
	}()

	return outCh, nil
}

// Abort cancels any running chat for an agent.
func (m *Manager) Abort(agentID string) {
	m.busyMu.Lock()
	if cancel, ok := m.busy[agentID]; ok {
		cancel()
	}
	m.busyMu.Unlock()
}

// IsBusy returns true if the agent has an active chat.
func (m *Manager) IsBusy(agentID string) bool {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	_, ok := m.busy[agentID]
	return ok
}

// Messages returns recent messages for an agent.
func (m *Manager) Messages(agentID string, limit int) ([]*Message, error) {
	return loadMessages(agentID, limit)
}

// MessagesPaginated returns messages with cursor-based pagination.
func (m *Manager) MessagesPaginated(agentID string, limit int, before string) ([]*Message, bool, error) {
	return loadMessagesPaginated(agentID, limit, before)
}

// Shutdown stops all cron jobs and cancels active chats.
func (m *Manager) Shutdown() {
	m.cron.Stop()

	m.busyMu.Lock()
	for _, cancel := range m.busy {
		cancel()
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
	return &cp
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

func truncatePreview(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "…"
}
