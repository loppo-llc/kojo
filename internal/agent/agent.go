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
	RespondMention *bool `json:"respondMention,omitempty"` // respond to @mentions in channels
	RespondThread  *bool `json:"respondThread,omitempty"`  // auto-reply in threads with history
}

// ReactDM returns whether the bot should respond to direct messages.
func (c *SlackBotConfig) ReactDM() bool { return c.RespondDM == nil || *c.RespondDM }

// ReactMention returns whether the bot should respond to @mentions.
func (c *SlackBotConfig) ReactMention() bool { return c.RespondMention == nil || *c.RespondMention }

// ReactThread returns whether the bot should auto-reply in threads with history.
func (c *SlackBotConfig) ReactThread() bool { return c.RespondThread == nil || *c.RespondThread }

// ValidSilentHours validates the silent hours range.
// Both must be empty (no restriction) or both must be valid HH:MM format.
func ValidSilentHours(start, end string) error {
	if start == "" && end == "" {
		return nil
	}
	if (start == "") != (end == "") {
		return fmt.Errorf("both silentStart and silentEnd must be set, or both empty")
	}
	if _, err := time.Parse("15:04", start); err != nil {
		return fmt.Errorf("invalid silentStart: %s", start)
	}
	if _, err := time.Parse("15:04", end); err != nil {
		return fmt.Errorf("invalid silentEnd: %s", end)
	}
	if start == end {
		return fmt.Errorf("silentStart and silentEnd must differ")
	}
	return nil
}

// ValidActiveHours is a compatibility alias for ValidSilentHours.
func ValidActiveHours(start, end string) error { return ValidSilentHours(start, end) }

// IsInSilentHours checks if the current local time is within the silent window.
// Returns false if no restriction is set (both empty = never silent).
// Supports overnight ranges (e.g., 23:00-09:00).
func IsInSilentHours(start, end string) bool {
	if start == "" || end == "" {
		return false
	}
	now := time.Now()
	nowMinutes := now.Hour()*60 + now.Minute()

	s, _ := time.Parse("15:04", start)
	e, _ := time.Parse("15:04", end)
	startMin := s.Hour()*60 + s.Minute()
	endMin := e.Hour()*60 + e.Minute()

	if startMin <= endMin {
		// Normal range: e.g., 01:00-07:00
		return nowMinutes >= startMin && nowMinutes < endMin
	}
	// Overnight range: e.g., 23:00-09:00
	return nowMinutes >= startMin || nowMinutes < endMin
}

// allowedIntervals defines the valid intervalMinutes values.
// Sub-hourly values must divide 60; hourly values must divide 24 (in hours).
var allowedIntervals = map[int]bool{
	0: true, 5: true, 10: true, 30: true, 60: true,
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

// MaxCronMessageRunes caps the per-agent cron check-in custom message at
// 4096 Unicode code points. The limit is in runes (not bytes) so Japanese
// users can write the same number of characters as ASCII users; the resulting
// byte size can be up to ~16 KiB worst case for 4-byte runes. The message is
// re-injected on every cron run so an unbounded value would inflate every
// prompt and the on-disk agent record.
const MaxCronMessageRunes = 4096

// validateCronMessage trims surrounding whitespace and rejects values that
// exceed MaxCronMessageRunes.
func validateCronMessage(s string) (string, error) {
	trimmed := strings.TrimSpace(s)
	if n := len([]rune(trimmed)); n > MaxCronMessageRunes {
		return "", fmt.Errorf("cronMessage too long: %d runes (max %d)", n, MaxCronMessageRunes)
	}
	return trimmed, nil
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

// allowedResumeIdles defines the valid resumeIdleMinutes values.
// 0 means "use default" (5 minutes at runtime, matching Anthropic's prompt
// cache TTL). Per-agent override lets high-frequency cron agents reset more
// aggressively, and lets long-form interactive agents extend the protection
// window before kojo abandons --resume on an over-token-threshold session.
var allowedResumeIdles = map[int]bool{
	0: true, 1: true, 3: true, 5: true, 10: true, 15: true, 30: true, 60: true,
}

// ValidResumeIdle returns true if the given resume-idle window is in the
// allowed set.
func ValidResumeIdle(minutes int) bool {
	return allowedResumeIdles[minutes]
}

// defaultResumeIdleDuration is the fallback window when ResumeIdleMinutes
// is 0 (unset). Mirrors the original sessionResetMinIdleDuration constant —
// chosen to match Anthropic's prompt cache TTL so back-to-back interactive
// turns stay cache-warm.
const defaultResumeIdleDuration = 5 * time.Minute

// Agent represents a persistent AI persona (friend).
type Agent struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Persona         string `json:"persona"`           // persona description (markdown)
	Model           string `json:"model"`             // e.g. "sonnet", "opus"
	Effort          string `json:"effort,omitempty"`  // claude only: "low", "medium", "high", "xhigh", "max"
	Tool            string `json:"tool"`              // CLI tool: "claude", "codex", "gemini"
	WorkDir         string `json:"workDir,omitempty"` // file storage directory (empty = agentDir)
	IntervalMinutes int    `json:"intervalMinutes"`   // periodic execution interval in minutes (0 = disabled)
	TimeoutMinutes  int    `json:"timeoutMinutes"`    // max duration per cron run in minutes (0 = default 10)
	// ResumeIdleMinutes is the idle-window threshold (in minutes) below
	// which kojo keeps an over-token-threshold claude session via --resume
	// instead of resetting. 0 = use defaultResumeIdleDuration (5 min).
	// claude-only; ignored by other backends.
	ResumeIdleMinutes int    `json:"resumeIdleMinutes,omitempty"`
	SilentStart       string `json:"silentStart,omitempty"` // HH:MM — start of silent window (empty = no restriction)
	SilentEnd         string `json:"silentEnd,omitempty"`   // HH:MM — end of silent window (empty = no restriction)
	// NotifyDuringSilent controls whether the agent receives DM notifications
	// during silent hours. Existing agents default to true (backward compat);
	// new agents default to false.
	NotifyDuringSilent *bool `json:"notifyDuringSilent,omitempty"`
	// CronMessage overrides the trailing instruction in the periodic check-in
	// prompt. Empty = use the default ("最近の出来事や気づきがあれば memory/...md に記録し、必要なタスクを実行してください。").
	// The literal string "{date}" is replaced with today's date in YYYY-MM-DD form.
	CronMessage string `json:"cronMessage,omitempty"`
	CreatedAt   string `json:"createdAt"` // RFC3339
	UpdatedAt   string `json:"updatedAt"` // RFC3339

	// Legacy field — only used during migration from cronExpr-based configs.
	// Not included in JSON output; consumed by store.Load migration.
	LegacyCronExpr string `json:"cronExpr,omitempty"`

	// Legacy fields — consumed by store.Load for activeStart/activeEnd →
	// silentStart/silentEnd migration. Active hours are inverted to silent
	// hours: silentStart = old activeEnd, silentEnd = old activeStart.
	LegacyActiveStart string `json:"activeStart,omitempty"`
	LegacyActiveEnd   string `json:"activeEnd,omitempty"`

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

	// AllowProtectedPaths lists protected directory names (claude, git, husky)
	// for which Edit/Write/MultiEdit prompts should be suppressed. Recent
	// claude-code versions guard these dirs even under bypassPermissions —
	// explicit permissions.allow rules are the only bypass.
	AllowProtectedPaths []string `json:"allowProtectedPaths,omitempty"`

	// ThinkingMode controls reasoning/thinking for llama.cpp backend.
	// "on" = enable, "off" = disable, "" = server default.
	ThinkingMode string `json:"thinkingMode,omitempty"`

	// NotifySources holds notification source configurations for this agent.
	NotifySources []notifysource.Config `json:"notifySources,omitempty"`

	// SlackBot holds the Slack Socket Mode bot configuration for this agent.
	SlackBot *SlackBotConfig `json:"slackBot,omitempty"`

	// LastMessage is a preview of the most recent message (for list display).
	LastMessage *MessagePreview `json:"lastMessage,omitempty"`

	// Archived is true when the agent has been archived via DELETE
	// /api/v1/agents/{id}?archive=true. Archived agents retain most on-disk
	// data (agent dir, credentials, notify tokens, messages, memory) but
	// have all runtime activity stopped: cron, notify polling, one-shot
	// chats, Slack bot, and inbound chats are refused with ErrAgentArchived.
	// Group DM memberships are an exception — they are removed on archive
	// (2-person groups are dissolved) and NOT restored on Unarchive.
	// Restored via POST /api/v1/agents/{id}/unarchive.
	Archived bool `json:"archived,omitempty"`
	// ArchivedAt is the RFC3339 timestamp when Archived was last set.
	// Cleared on unarchive.
	ArchivedAt string `json:"archivedAt,omitempty"`

	// Privileged grants the agent the ability to delete/reset other agents
	// (but NOT to fork or read their full record). Owner-only mutation —
	// the API strips this field from PATCH bodies and exposes a dedicated
	// POST /api/v1/agents/{id}/privilege handler instead.
	Privileged bool `json:"privileged,omitempty"`
}

// ShouldNotifyDuringSilent returns whether the agent should receive DM
// notifications while in silent hours. Existing agents with a nil pointer
// default to true for backward compatibility; new agents default to false.
func (a *Agent) ShouldNotifyDuringSilent() bool {
	if a.NotifyDuringSilent == nil {
		return true // backward compat
	}
	return *a.NotifyDuringSilent
}

// ResumeIdleDuration returns the configured idle window for keeping an
// over-token-threshold claude session via --resume. ResumeIdleMinutes==0
// (the default for legacy agents) maps to defaultResumeIdleDuration so
// existing behavior is preserved. Values outside the validated whitelist
// (e.g. left over from a hand-edited agents.json or a future schema) also
// fall back to the default — the API layer's whitelist must not be the
// only line of defence.
func (a *Agent) ResumeIdleDuration() time.Duration {
	if a == nil || !ValidResumeIdle(a.ResumeIdleMinutes) || a.ResumeIdleMinutes <= 0 {
		return defaultResumeIdleDuration
	}
	return time.Duration(a.ResumeIdleMinutes) * time.Minute
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
	Name            string `json:"name"`
	Persona         string `json:"persona"`
	Model           string `json:"model"`
	Effort          string `json:"effort"`
	Tool            string `json:"tool"`
	CustomBaseURL   string `json:"customBaseURL"`
	ThinkingMode    string `json:"thinkingMode"`
	WorkDir         string `json:"workDir"`
	IntervalMinutes *int   `json:"intervalMinutes"` // nil = use default (30)
	TimeoutMinutes  *int   `json:"timeoutMinutes"`  // nil = use default (0 = 10 min)
	// ResumeIdleMinutes overrides the per-agent claude --resume idle window.
	// nil/0 = use defaultResumeIdleDuration (5 min).
	ResumeIdleMinutes *int    `json:"resumeIdleMinutes"`
	SilentStart        *string `json:"silentStart"`        // HH:MM or empty
	SilentEnd          *string `json:"silentEnd"`          // HH:MM or empty
	NotifyDuringSilent *bool   `json:"notifyDuringSilent"` // nil = use default (false for new)
	CronMessage        *string `json:"cronMessage"`        // nil/empty = use default trailing instruction
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
	TimeoutMinutes        *int    `json:"timeoutMinutes"`
	ResumeIdleMinutes     *int    `json:"resumeIdleMinutes"`
	SilentStart           *string `json:"silentStart"`
	SilentEnd             *string `json:"silentEnd"`
	NotifyDuringSilent    *bool   `json:"notifyDuringSilent"`
	// CronMessage follows the standard *string PATCH convention used by every
	// other field on this struct: nil/omitted = leave unchanged, "" = clear
	// back to the built-in default trailing instruction.
	CronMessage         *string   `json:"cronMessage"`
	CustomBaseURL       *string   `json:"customBaseURL"`
	ThinkingMode        *string   `json:"thinkingMode"`
	AllowedTools        []string  `json:"allowedTools"`
	AllowProtectedPaths *[]string `json:"allowProtectedPaths"`
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
	resumeIdleMin := 0 // default (= 5 min at runtime)
	if cfg.ResumeIdleMinutes != nil {
		resumeIdleMin = *cfg.ResumeIdleMinutes
	}
	if !ValidResumeIdle(resumeIdleMin) {
		return nil, fmt.Errorf("unsupported resumeIdle: %d minutes", resumeIdleMin)
	}
	var silentStart, silentEnd string
	if cfg.SilentStart != nil {
		silentStart = *cfg.SilentStart
	}
	if cfg.SilentEnd != nil {
		silentEnd = *cfg.SilentEnd
	}
	// New agents default to false (don't receive DM during silent).
	var notifyDuringSilent *bool
	if cfg.NotifyDuringSilent != nil {
		notifyDuringSilent = cfg.NotifyDuringSilent
	} else {
		f := false
		notifyDuringSilent = &f
	}
	var cronMessage string
	if cfg.CronMessage != nil {
		v, err := validateCronMessage(*cfg.CronMessage)
		if err != nil {
			return nil, err
		}
		cronMessage = v
	}
	if err := ValidSilentHours(silentStart, silentEnd); err != nil {
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
		if err := validateWorkDir(cfg.WorkDir); err != nil {
			return nil, err
		}
	}
	a := &Agent{
		ID:                generateID(),
		Name:              cfg.Name,
		Persona:           cfg.Persona,
		Model:             cfg.Model,
		Effort:            cfg.Effort,
		Tool:              cfg.Tool,
		CustomBaseURL:     cfg.CustomBaseURL,
		ThinkingMode:      NormalizeThinkingMode(cfg.ThinkingMode),
		WorkDir:           cfg.WorkDir,
		IntervalMinutes:   interval,
		TimeoutMinutes:    timeoutMin,
		ResumeIdleMinutes: resumeIdleMin,
		SilentStart:        silentStart,
		SilentEnd:          silentEnd,
		NotifyDuringSilent: notifyDuringSilent,
		CronMessage:       cronMessage,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if a.Tool == "" {
		a.Tool = "claude"
	}
	if a.Model == "" {
		a.Model = "sonnet"
	}
	return a, nil
}

// validateWorkDir checks that a user-supplied WorkDir is acceptable for
// use as an agent's file storage directory.
//
// Beyond the obvious "absolute, exists, is a directory" checks, this
// also refuses any path that resolves inside (or equal to / above) the
// kojo agents-root directory. Without that guard, an agent could
// elevate its own sandbox to cover another agent's data dir by simply
// PATCHing its WorkDir to /…/agents/{other-id} — the next backend launch
// would add the path to the Landlock allowlist and the agent would gain
// write access to the other agent's MEMORY.md, persona, transcripts,
// etc. Symlinks are resolved before comparison so an indirect path
// cannot bypass the check.
func validateWorkDir(workDir string) error {
	if !filepath.IsAbs(workDir) {
		return fmt.Errorf("workDir must be an absolute path: %s", workDir)
	}
	info, err := os.Stat(workDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("workDir does not exist or is not a directory: %s", workDir)
	}
	if isWithinAgentsRoot(workDir) {
		return fmt.Errorf("workDir must not be inside, equal to, or above the kojo agents root (%s): %s", agentsDir(), workDir)
	}
	return nil
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

	// Sub-hourly (5, 10, 30 — all divide 60 evenly)
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
