package agent

import (
	"errors"
	"time"
)

// ErrNotFound indicates the requested agent does not exist.
var ErrNotFound = errors.New("agent not found")

// Agent is a persistent configuration entity that spawns sessions.
type Agent struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Tool      string   `json:"tool"`
	WorkDir   string   `json:"workDir"`
	Args      []string `json:"args,omitempty"`
	YoloMode  bool     `json:"yoloMode"`
	Schedule  string   `json:"schedule"`           // "" | "every 6h" | "daily 09:00" | "hourly"
	Enabled   bool     `json:"enabled"`
	CreatedAt string   `json:"createdAt"`
	LastRunAt string   `json:"lastRunAt,omitempty"`
	LastRunID string   `json:"lastRunId,omitempty"`
}

// Template variable placeholders used in bootstrapTemplate.
const (
	tmplSoul             = "{SOUL}"
	tmplMemory           = "{MEMORY}"
	tmplGoals            = "{GOALS}"
	tmplRecentLogs       = "{RECENT_LOGS}"
	tmplRelevantMemories = "{RELEVANT_MEMORIES}"
)

// bootstrapTemplate is injected into a new agent session via SafePaste.
const bootstrapTemplate = `You are resuming as an autonomous agent. Here is your context:

## Identity (SOUL)
{SOUL}

## Persistent Memory
{MEMORY}

## Current Goals
{GOALS}

## Recent Session Logs
{RECENT_LOGS}

## Relevant Memories (from search)
{RELEVANT_MEMORIES}

Review the above context, then continue working on your goals. When done, summarize what you accomplished.`

// Validate checks required fields.
func (a *Agent) Validate() string {
	if a.Name == "" {
		return "name is required"
	}
	if a.Tool == "" {
		return "tool is required"
	}
	if a.WorkDir == "" {
		return "workDir is required"
	}
	return ""
}

// NewAgent creates an Agent with defaults.
func NewAgent(id, name, tool, workDir string) *Agent {
	return &Agent{
		ID:        id,
		Name:      name,
		Tool:      tool,
		WorkDir:   workDir,
		Enabled:   true,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
}
