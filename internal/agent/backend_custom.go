package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
)

// CustomBackend implements ChatBackend for custom Anthropic Messages API
// endpoints (e.g., llama-server). It delegates to ClaudeBackend with
// ANTHROPIC_BASE_URL pointed at the custom endpoint.
type CustomBackend struct {
	logger *slog.Logger
}

func NewCustomBackend(logger *slog.Logger) *CustomBackend {
	return &CustomBackend{logger: logger}
}

func (b *CustomBackend) Name() string { return "custom" }

// Available returns true if the claude CLI is in PATH (required as the client).
func (b *CustomBackend) Available() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

func (b *CustomBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	if agent.CustomBaseURL == "" {
		return nil, fmt.Errorf("customBaseURL is required for custom backend")
	}
	cb := NewClaudeBackend(b.logger)
	cb.SetProxyURL(agent.CustomBaseURL)
	// This is a throwaway backend created per turn — it is not the Manager's
	// shared instance, so the Manager can neither reuse nor close a persistent
	// process it might spawn. Force the per-turn spawn model to avoid orphaned
	// long-lived processes that pile up until idle-reap.
	cb.ephemeral = true
	return cb.Chat(ctx, agent, userMessage, systemPrompt, opts)
}
