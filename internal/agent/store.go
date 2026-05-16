package agent

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"

	"github.com/loppo-llc/kojo/internal/atomicfile"
	"github.com/loppo-llc/kojo/internal/configdir"
)

const agentsFile = "agents.json"
const cronPausedFile = "cron_paused"

// store persists agent metadata to disk using atomic rename.
type store struct {
	mu     sync.Mutex
	dir    string // base directory (~/.config/kojo/agents)
	logger *slog.Logger
}

func newStore(logger *slog.Logger) *store {
	return &store{
		dir:    agentsDir(),
		logger: logger,
	}
}

// Save writes all agents metadata to agents.json using atomic rename.
func (st *store) Save(agents []*Agent) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if err := os.MkdirAll(st.dir, 0o755); err != nil {
		st.logger.Warn("failed to create agents dir", "err", err)
		return
	}

	if err := atomicfile.WriteJSON(filepath.Join(st.dir, agentsFile), agents, 0o644); err != nil {
		st.logger.Warn("failed to save agents", "err", err)
	}
}

// Load reads persisted agents from agents.json.
func (st *store) Load() ([]*Agent, error) {
	st.mu.Lock()

	path := filepath.Join(st.dir, agentsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		st.mu.Unlock()
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var agents []*Agent
	if err := json.Unmarshal(data, &agents); err != nil {
		st.mu.Unlock()
		return nil, err
	}
	st.mu.Unlock()

	// Normalize timestamps to local timezone (legacy data may be UTC)
	for _, a := range agents {
		a.CreatedAt = normalizeTimestamp(a.CreatedAt)
		a.UpdatedAt = normalizeTimestamp(a.UpdatedAt)
	}

	// Migrate legacy cronExpr → intervalMinutes and validate
	needsSave := false
	for _, a := range agents {
		if a.LegacyCronExpr != "" && a.IntervalMinutes == 0 {
			a.IntervalMinutes = parseLegacyCron(a.LegacyCronExpr, st.logger)
			needsSave = true
		}
		a.LegacyCronExpr = ""

		// Validate loaded intervalMinutes — clamp invalid values to 0
		if !ValidInterval(a.IntervalMinutes) {
			st.logger.Warn("invalid intervalMinutes in stored data, disabling", "agent", a.ID, "value", a.IntervalMinutes)
			a.IntervalMinutes = 0
			needsSave = true
		}
		// Validate loaded resumeIdleMinutes — clamp invalid values to 0
		// (the default sentinel). Without this, a hand-edited or schema-
		// drifted agents.json would surface the bad value in the UI and
		// then bounce the user's PATCH because the whitelist would reject
		// the round-tripped read-modify-write.
		if !ValidResumeIdle(a.ResumeIdleMinutes) {
			st.logger.Warn("invalid resumeIdleMinutes in stored data, resetting to default", "agent", a.ID, "value", a.ResumeIdleMinutes)
			a.ResumeIdleMinutes = 0
			needsSave = true
		}
		// Migrate legacy activeStart/activeEnd → silentStart/silentEnd.
		// Active hours (when the agent runs) invert to silent hours (when
		// the agent sleeps): silentStart = old activeEnd, silentEnd = old
		// activeStart. Existing agents get NotifyDuringSilent=true for
		// backward compat.
		if a.LegacyActiveStart != "" && a.LegacyActiveEnd != "" && a.SilentStart == "" && a.SilentEnd == "" {
			a.SilentStart = a.LegacyActiveEnd
			a.SilentEnd = a.LegacyActiveStart
			// Backward compat: existing agents keep receiving DM during silent
			if a.NotifyDuringSilent == nil {
				t := true
				a.NotifyDuringSilent = &t
			}
			st.logger.Info("migrated active hours → silent hours",
				"agent", a.ID,
				"activeStart", a.LegacyActiveStart, "activeEnd", a.LegacyActiveEnd,
				"silentStart", a.SilentStart, "silentEnd", a.SilentEnd)
			needsSave = true
		}
		a.LegacyActiveStart = ""
		a.LegacyActiveEnd = ""

		// Migrate CronMessage → checkin.md file.
		// If the agent has a non-empty CronMessage and no checkin.md exists,
		// write the content to checkin.md and clear the JSON field.
		if a.CronMessage != "" {
			checkinPath := filepath.Join(agentDir(a.ID), "checkin.md")
			if _, err := os.Stat(checkinPath); os.IsNotExist(err) {
				if err := os.WriteFile(checkinPath, []byte(a.CronMessage), 0o644); err != nil {
					st.logger.Warn("failed to migrate cronMessage to checkin.md", "agent", a.ID, "err", err)
				} else {
					st.logger.Info("migrated cronMessage → checkin.md", "agent", a.ID)
				}
			}
			a.CronMessage = ""
			needsSave = true
		}

		// Validate loaded silent hours — clear invalid values
		if err := ValidSilentHours(a.SilentStart, a.SilentEnd); err != nil {
			st.logger.Warn("invalid silent hours in stored data, clearing", "agent", a.ID, "start", a.SilentStart, "end", a.SilentEnd, "err", err)
			a.SilentStart = ""
			a.SilentEnd = ""
			needsSave = true
		}
		// Validate loaded workDir — clear invalid values (not absolute or not a directory)
		if a.WorkDir != "" {
			if !filepath.IsAbs(a.WorkDir) {
				st.logger.Warn("invalid workDir in stored data (not absolute), clearing", "agent", a.ID, "workDir", a.WorkDir)
				a.WorkDir = ""
				needsSave = true
			} else if info, err := os.Stat(a.WorkDir); err != nil || !info.IsDir() {
				st.logger.Warn("workDir does not exist or is not a directory, clearing", "agent", a.ID, "workDir", a.WorkDir)
				a.WorkDir = ""
				needsSave = true
			}
		}
	}
	if needsSave {
		st.Save(agents)
	}

	return agents, nil
}

// parseLegacyCron attempts to extract an interval from a legacy cron expression.
// Recognizes "*/N * * * *". Returns 0 (disabled) for unrecognizable expressions
// to avoid accidental over-execution.
var legacyCronRe = regexp.MustCompile(`^\*/(\d+)\s+\*\s+\*\s+\*\s+\*$`)

func parseLegacyCron(expr string, logger *slog.Logger) int {
	if m := legacyCronRe.FindStringSubmatch(expr); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && ValidInterval(n) {
			return n
		}
	}
	logger.Warn("unrecognized legacy cronExpr, disabling schedule", "cronExpr", expr)
	return 0
}

// LoadCronPaused returns true if the cron_paused marker file exists.
func (st *store) LoadCronPaused() bool {
	_, err := os.Stat(filepath.Join(st.dir, cronPausedFile))
	return err == nil
}

// SaveCronPaused creates or removes the cron_paused marker file.
func (st *store) SaveCronPaused(paused bool) {
	path := filepath.Join(st.dir, cronPausedFile)
	if paused {
		os.MkdirAll(st.dir, 0o755)
		os.WriteFile(path, nil, 0o644)
	} else {
		os.Remove(path)
	}
}

// agentsDir returns the base directory for all agent data.
func agentsDir() string {
	return filepath.Join(configdir.Path(), "agents")
}

// agentDir returns the data directory for a specific agent.
func agentDir(id string) string {
	return filepath.Join(agentsDir(), id)
}
