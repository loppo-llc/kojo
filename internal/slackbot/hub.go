package slackbot

import (
	"context"
	"log/slog"
	"sync"
)

// Hub manages all SlackBot instances across agents.
type Hub struct {
	mu     sync.Mutex
	bots   map[string]*Bot // agentID → bot
	mgr    ChatManager
	tokens TokenProvider
	logger *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
}

// NewHub creates a new Hub. Call Stop() on shutdown.
func NewHub(mgr ChatManager, tokens TokenProvider, logger *slog.Logger) *Hub {
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{
		bots:   make(map[string]*Bot),
		mgr:    mgr,
		tokens: tokens,
		logger: logger.With("component", "slackbot-hub"),
		ctx:    ctx,
		cancel: cancel,
	}
}

// StartBot starts a Slack bot for the given agent. If one is already running,
// it is stopped first.
func (h *Hub) StartBot(agentID string, cfg Config) {
	if h.tokens == nil {
		h.logger.Warn("no credential store, cannot start slack bot", "agent", agentID)
		return
	}

	appToken, botToken, err := LoadTokens(h.tokens, agentID)
	if err != nil {
		h.logger.Warn("failed to load slack tokens", "agent", agentID, "err", err)
		return
	}
	if appToken == "" || botToken == "" {
		h.logger.Warn("slack tokens not configured", "agent", agentID)
		return
	}

	// Remove existing bot from map, then stop it outside the lock.
	// Re-check under lock before inserting the new bot.
	h.mu.Lock()
	old, hadOld := h.bots[agentID]
	if hadOld {
		delete(h.bots, agentID)
	}
	h.mu.Unlock()

	if hadOld {
		old.Stop()
	}

	bot := NewBot(agentID, cfg, appToken, botToken, h.mgr, h.logger)

	h.mu.Lock()
	// If another goroutine raced and already registered a bot, stop it first.
	if racing, ok := h.bots[agentID]; ok {
		delete(h.bots, agentID)
		h.mu.Unlock()
		racing.Stop()
		h.mu.Lock()
	}
	h.bots[agentID] = bot
	h.mu.Unlock()

	go bot.Run(h.ctx)
	h.logger.Info("slack bot started", "agent", agentID)
}

// StopBot stops the Slack bot for the given agent.
func (h *Hub) StopBot(agentID string) {
	h.mu.Lock()
	bot, ok := h.bots[agentID]
	if ok {
		delete(h.bots, agentID)
	}
	h.mu.Unlock()

	if ok {
		bot.Stop()
		h.logger.Info("slack bot stopped", "agent", agentID)
	}
}

// Reconfigure stops and restarts the bot with new configuration.
// If the config is disabled, it only stops the bot.
func (h *Hub) Reconfigure(agentID string, cfg Config) {
	if !cfg.Enabled {
		h.StopBot(agentID)
		return
	}
	h.StartBot(agentID, cfg)
}

// Stop stops all bots and prevents new ones from starting.
func (h *Hub) Stop() {
	h.cancel()

	h.mu.Lock()
	bots := make(map[string]*Bot, len(h.bots))
	for k, v := range h.bots {
		bots[k] = v
	}
	h.bots = make(map[string]*Bot)
	h.mu.Unlock()

	for _, bot := range bots {
		bot.Stop()
	}
	h.logger.Info("all slack bots stopped")
}

// IsRunning returns true if a bot is running for the given agent.
func (h *Hub) IsRunning(agentID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.bots[agentID]
	return ok
}
