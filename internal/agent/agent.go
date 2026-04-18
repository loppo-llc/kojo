package agent

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/notifysource"
)

// SlackBotConfig holds Slack bot configuration for an agent.
type SlackBotConfig struct {
	Enabled       bool `json:"enabled"`
	ThreadReplies bool `json:"threadReplies"` // always reply in-thread (default true)

	// Reaction patterns — which message types the bot responds to.
	// All default to true for backwards compatibility.
	RespondDM      *bool `json:"respondDM,omitempty"`      // respond to direct messages
	RespondMention *bool `json:"respondMention,omitempty"`  // respond to @mentions in channels
	RespondThread  *bool `json:"respondThread,omitempty"`   // auto-reply in threads with history
}

// ReactDM returns whether the bot should respond to direct messages.
func (c *SlackBotConfig) ReactDM() bool { return c.RespondDM == nil || *c.RespondDM }

// ReactMention returns whether the bot should respond to @mentions.
func (c *SlackBotConfig) ReactMention() bool { return c.RespondMention == nil || *c.RespondMention }

// ReactThread returns whether the bot should auto-reply in threads with history.
func (c *SlackBotConfig) ReactThread() bool { return c.RespondThread == nil || *c.RespondThread }

// ValidActiveHours validates the active hours range.
// Both must be empty (no restriction) or both must be valid HH:MM format.
func ValidActiveHours(start, end string) error {
	if start == "" && end == "" {
		return nil
	}
	if (start == "") != (end == "") {
		return fmt.Errorf("both activeStart and activeEnd must be set, or both empty")
	}
	if _, err := time.Parse("15:04", start); err != nil {
		return fmt.Errorf("invalid activeStart: %s", start)
	}
	if _, err := time.Parse("15:04", end); err != nil {
		return fmt.Errorf("invalid activeEnd: %s", end)
	}
	if start == end {
		return fmt.Errorf("activeStart and activeEnd must differ")
	}
	return nil
}

// IsWithinActiveHours checks if the current local time is within the active window.
// Returns true if no restriction is set (both empty).
// Supports overnight ranges (e.g., 22:00-06:00).
func IsWithinActiveHours(start, end string) bool {
	if start == "" || end == "" {
		return true
	}
	now := time.Now()
	nowMinutes := now.Hour()*60 + now.Minute()

	s, _ := time.Parse("15:04", start)
	e, _ := time.Parse("15:04", end)
	startMin := s.Hour()*60 + s.Minute()
	endMin := e.Hour()*60 + e.Minute()

	if startMin <= endMin {
		// Normal range: e.g., 09:00-23:00
		return nowMinutes >= startMin && nowMinutes < endMin
	}
	// Overnight range: e.g., 22:00-06:00
	return nowMinutes >= startMin || nowMinutes < endMin
}

// allowedIntervals defines the valid intervalMinutes values.
// Sub-hourly values must divide 60; hourly values must divide 24 (in hours).
var allowedIntervals = map[int]bool{
	0: true, 10: true, 30: true, 60: true,
	180: true, 360: true, 720: true, 1440: true,
}

// ValidInterval returns true if the given interval is in the allowed set.
func ValidInterval(minutes int) bool {
	return allowedIntervals[minutes]
}

// allowedEfforts defines valid effort levels (claude only). Empty string is allowed (= default).
var allowedEfforts = map[string]bool{
	"": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

// ValidEffort returns true if the given effort level is valid.
func ValidEffort(effort string) bool {
	return allowedEfforts[effort]
}

var allowedThinkingModes = map[string]bool{
	"": true, "on": true, "off": true, "auto": true,
}

// ValidThinkingMode returns true if the given thinking mode is valid.
func ValidThinkingMode(mode string) bool {
	return allowedThinkingModes[mode]
}

// NormalizeThinkingMode normalizes "auto" to "" for storage.
func NormalizeThinkingMode(mode string) string {
	if mode == "auto" {
		return ""
	}
	return mode
}

// xhighModels lists models that support the "xhigh" effort level.
var xhighModels = map[string]bool{
	"opus": true, "claude-opus-4-7": true,
}

// ValidModelEffort returns true if the model+effort combination is valid.
// xhigh is only allowed for specific models.
func ValidModelEffort(model, effort string) bool {
	if !ValidEffort(effort) {
		return false
	}
	if effort == "xhigh" && !xhighModels[model] {
		return false
	}
	return true
}

// allowedTimeouts defines the valid timeoutMinutes values.
// 0 means "use default" (10 minutes at runtime) for backward compatibility.
var allowedTimeouts = map[int]bool{
	0: true, 5: true, 10: true, 15: true, 20: true, 30: true, 45: true, 60: true,
}

// ValidTimeout returns true if the given timeout is in the allowed set.
func ValidTimeout(minutes int) bool {
	return allowedTimeouts[minutes]
}

// Agent represents a persistent AI persona (friend).
type Agent struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Persona         string `json:"persona"`         // persona description (markdown)
	Model           string `json:"model"`           // e.g. "sonnet", "opus"
	Effort          string `json:"effort,omitempty"` // claude only: "low", "medium", "high", "xhigh", "max"
	Tool            string `json:"tool"`            // CLI tool: "claude", "codex", "gemini"
	WorkDir         string `json:"workDir,omitempty"` // file storage directory (empty = agentDir)
	IntervalMinutes int    `json:"intervalMinutes"` // periodic execution interval in minutes (0 = disabled)
	TimeoutMinutes  int    `json:"timeoutMinutes"`  // max duration per cron run in minutes (0 = default 10)
	ActiveStart     string `json:"activeStart,omitempty"` // HH:MM — start of active window (empty = no restriction)
	ActiveEnd       string `json:"activeEnd,omitempty"`   // HH:MM — end of active window (empty = no restriction)
	CreatedAt       string `json:"createdAt"`       // RFC3339
	UpdatedAt       string `json:"updatedAt"`       // RFC3339

	// Legacy field — only used during migration from cronExpr-based configs.
	// Not included in JSON output; consumed by store.Load migration.
	LegacyCronExpr string `json:"cronExpr,omitempty"`

	// HasAvatar indicates whether a custom avatar file exists.
	HasAvatar bool `json:"hasAvatar"`
	// AvatarHash is derived from the avatar file's modtime for cache busting.
	AvatarHash string `json:"avatarHash,omitempty"`

	// PublicProfile is a short outward-facing description generated from persona.
	// Shared with other agents via the directory endpoint. Does not expose internal persona details.
	PublicProfile         string `json:"publicProfile,omitempty"`
	PublicProfileOverride bool   `json:"publicProfileOverride,omitempty"`

	// CustomBaseURL is the base URL for a custom Anthropic Messages API endpoint
	// (e.g., llama-server). Only used when Tool is "custom".
	CustomBaseURL string `json:"customBaseURL,omitempty"`

	// AllowedTools is a whitelist of tool names forwarded to a custom endpoint.
	// If non-empty, only listed tools are forwarded.
	// If empty, all tools are forwarded.
	AllowedTools []string `json:"allowedTools,omitempty"`

	// ThinkingMode controls reasoning/thinking for llama.cpp backend.
	// "on" = enable, "off" = disable, "" = server default.
	ThinkingMode string `json:"thinkingMode,omitempty"`

	// NotifySources holds notification source configurations for this agent.
	NotifySources []notifysource.Config `json:"notifySources,omitempty"`

	// SlackBot holds the Slack Socket Mode bot configuration for this agent.
	SlackBot *SlackBotConfig `json:"slackBot,omitempty"`

	// LastMessage is a preview of the most recent message (for list display).
	LastMessage *MessagePreview `json:"lastMessage,omitempty"`
}

// MessagePreview is a short summary for agent list display.
type MessagePreview struct {
	Content   string `json:"content"`
	Role      string `json:"role"`
	Timestamp string `json:"timestamp"`
}

// DirectoryEntry is the minimal public info shared with other agents.
type DirectoryEntry struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	PublicProfile string `json:"publicProfile"`
}

// AgentConfig is the request body for creating an agent.
type AgentConfig struct {
	Name            string  `json:"name"`
	Persona         string  `json:"persona"`
	Model           string  `json:"model"`
	Effort          string  `json:"effort"`
	Tool            string  `json:"tool"`
	CustomBaseURL   string  `json:"customBaseURL"`
	ThinkingMode    string  `json:"thinkingMode"`
	WorkDir         string  `json:"workDir"`
	IntervalMinutes *int    `json:"intervalMinutes"` // nil = use default (30)
	TimeoutMinutes  *int    `json:"timeoutMinutes"`  // nil = use default (0 = 10 min)
	ActiveStart     *string `json:"activeStart"`     // HH:MM or empty
	ActiveEnd       *string `json:"activeEnd"`       // HH:MM or empty
}

// AgentUpdateConfig is the request body for PATCH updates.
// Pointer fields distinguish "not provided" (nil) from "set to zero".
type AgentUpdateConfig struct {
	Name                  *string `json:"name"`
	Persona               *string `json:"persona"`
	PublicProfile         *string `json:"publicProfile"`
	PublicProfileOverride *bool   `json:"publicProfileOverride"`
	Model                 *string `json:"model"`
	Effort                *string `json:"effort"`
	Tool                  *string `json:"tool"`
	WorkDir               *string `json:"workDir"`
	IntervalMinutes       *int    `json:"intervalMinutes"`
	TimeoutMinutes        *int      `json:"timeoutMinutes"`
	ActiveStart           *string   `json:"activeStart"`
	ActiveEnd             *string   `json:"activeEnd"`
	CustomBaseURL         *string   `json:"customBaseURL"`
	ThinkingMode          *string   `json:"thinkingMode"`
	AllowedTools          []string  `json:"allowedTools"`
}

func generateID() string {
	return generatePrefixedID("ag_")
}

func newAgent(cfg AgentConfig) (*Agent, error) {
	now := time.Now().Format(time.RFC3339)
	interval := 30 // default
	if cfg.IntervalMinutes != nil {
		interval = *cfg.IntervalMinutes
	}
	if !ValidInterval(interval) {
		return nil, fmt.Errorf("unsupported interval: %d minutes", interval)
	}
	timeoutMin := 0 // default (= 10 min at runtime)
	if cfg.TimeoutMinutes != nil {
		timeoutMin = *cfg.TimeoutMinutes
	}
	if !ValidTimeout(timeoutMin) {
		return nil, fmt.Errorf("unsupported timeout: %d minutes", timeoutMin)
	}
	var activeStart, activeEnd string
	if cfg.ActiveStart != nil {
		activeStart = *cfg.ActiveStart
	}
	if cfg.ActiveEnd != nil {
		activeEnd = *cfg.ActiveEnd
	}
	if err := ValidActiveHours(activeStart, activeEnd); err != nil {
		return nil, err
	}
	if !ValidModelEffort(cfg.Model, cfg.Effort) {
		return nil, fmt.Errorf("unsupported effort level %q for model %q", cfg.Effort, cfg.Model)
	}
	if (cfg.Tool == "custom" || cfg.Tool == "llama.cpp") && cfg.CustomBaseURL == "" {
		return nil, fmt.Errorf("customBaseURL is required for %s tool", cfg.Tool)
	}
	if !ValidThinkingMode(cfg.ThinkingMode) {
		return nil, fmt.Errorf("unsupported thinkingMode: %q", cfg.ThinkingMode)
	}
	if cfg.WorkDir != "" {
		if !filepath.IsAbs(cfg.WorkDir) {
			return nil, fmt.Errorf("workDir must be an absolute path: %s", cfg.WorkDir)
		}
		if info, err := os.Stat(cfg.WorkDir); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("workDir does not exist or is not a directory: %s", cfg.WorkDir)
		}
	}
	a := &Agent{
		ID:              generateID(),
		Name:            cfg.Name,
		Persona:         cfg.Persona,
		Model:           cfg.Model,
		Effort:          cfg.Effort,
		Tool:            cfg.Tool,
		CustomBaseURL:   cfg.CustomBaseURL,
		ThinkingMode:    NormalizeThinkingMode(cfg.ThinkingMode),
		WorkDir:         cfg.WorkDir,
		IntervalMinutes: interval,
		TimeoutMinutes:  timeoutMin,
		ActiveStart:     activeStart,
		ActiveEnd:       activeEnd,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if a.Tool == "" {
		a.Tool = "claude"
	}
	if a.Model == "" {
		a.Model = "sonnet"
	}
	return a, nil
}

// intervalToCron converts an interval in minutes and an agent ID to a cron
// expression with a deterministic per-agent offset derived from the ID hash.
// This spreads agents across time so they don't all fire simultaneously.
// Only supports values in allowedIntervals. Returns "" if interval <= 0.
func intervalToCron(intervalMinutes int, agentID string) string {
	if intervalMinutes <= 0 {
		return ""
	}

	// Deterministic offset from agent ID (spread across full day = 1440 min)
	h := fnv.New32a()
	h.Write([]byte(agentID))
	hash := int(h.Sum32())

	if intervalMinutes >= 60 {
		hours := intervalMinutes / 60
		minuteOfDay := hash % 1440 // 0..1439
		minuteOffset := minuteOfDay % 60
		hourOffset := (minuteOfDay / 60) % hours

		if hours >= 24 {
			// Once a day
			return fmt.Sprintf("%d %d * * *", minuteOffset, minuteOfDay/60%24)
		}
		// Every N hours at a fixed minute
		hourList := make([]string, 0, 24/hours)
		for hr := hourOffset; hr < 24; hr += hours {
			hourList = append(hourList, fmt.Sprintf("%d", hr))
		}
		return fmt.Sprintf("%d %s * * *", minuteOffset, strings.Join(hourList, ","))
	}

	// Sub-hourly (10 or 30 — both divide 60 evenly)
	offset := hash % intervalMinutes
	mins := make([]string, 0, 60/intervalMinutes)
	for m := offset; m < 60; m += intervalMinutes {
		mins = append(mins, fmt.Sprintf("%d", m))
	}
	return fmt.Sprintf("%s * * * *", strings.Join(mins, ","))
}

// normalizeTimestamp converts any RFC3339 timestamp to local time.
// This ensures that older UTC timestamps (e.g. "...Z") are displayed
// in the server's local timezone, consistent with newly generated ones.
func normalizeTimestamp(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format(time.RFC3339)
}
