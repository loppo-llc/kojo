package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/migrate/externalcli"
	"github.com/loppo-llc/kojo/internal/store"
)

// applyExternalCLIForward wires the v0 → v1 external-CLI continuity layer
// described in docs/multi-device-storage.md §5.5.1. It runs after a
// successful migration and is best-effort: every failure becomes a
// warning so the v1 install can still boot. The returned warnings are
// surfaced to the operator by the caller.
//
// What it does:
//
//   - lists agents from the v1 DB (the migration just populated it),
//   - computes per-agent v0 / v1 working directories,
//   - builds claude project-symlink ops via externalcli.PlanSymlinks,
//   - builds gemini projects.json mirror ops by inspecting the existing
//     map for any v0 entry the user already has (only those agents get
//     a v1 alias — we never invent project names),
//   - calls externalcli.ApplyForward, which records each successful op
//     into external_cli_migration.json so --rollback-external-cli can
//     undo them later.
//
// Codex has no per-project hash directory in its on-disk layout, so it
// is intentionally skipped — fresh chat is the natural fallback.
func applyExternalCLIForward(ctx context.Context, v0Path, v1Path string, logger *slog.Logger) []string {
	openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := store.Open(openCtx, store.Options{
		ConfigDir: v1Path,
		ReadOnly:  true,
	})
	if err != nil {
		return []string{"external-cli forward: open v1 store: " + err.Error()}
	}
	defer st.Close()

	agents, err := st.ListAgents(openCtx)
	if err != nil {
		return []string{"external-cli forward: list agents: " + err.Error()}
	}
	if len(agents) == 0 {
		return nil
	}

	plans := make([]externalcli.PlanInput, 0, len(agents))
	for _, a := range agents {
		plans = append(plans, externalcli.PlanInput{
			AgentID: a.ID,
			V0Dir:   filepath.Join(v0Path, "agents", a.ID),
			V1Dir:   filepath.Join(v1Path, "agents", a.ID),
		})
	}

	// claude — symlink v1-hash dir at v0-hash dir under
	// $CLAUDE_CONFIG_DIR/projects (or ~/.claude/projects).
	specs := []externalcli.CLISpec{
		{
			Name:         "claude",
			ProjectsRoot: filepath.Join(claudeConfigRoot(), "projects"),
			Hash:         claudePathHash,
		},
	}
	ops := externalcli.PlanSymlinks(specs, plans)

	// gemini — read ~/.gemini/projects.json (if any), append a v1-keyed
	// entry mirroring each v0 entry we recognize. Skip silently if the
	// file does not exist; gemini may simply not be installed.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		geminiProjects := filepath.Join(home, ".gemini", "projects.json")
		if _, err := os.Stat(geminiProjects); err == nil {
			for _, p := range plans {
				ops = append(ops, externalcli.Op{
					Kind:    externalcli.OpGeminiProjectAdd,
					Path:    geminiProjects,
					Target:  p.V1Dir,
					AgentID: p.AgentID,
				})
			}
		}
	}

	if len(ops) == 0 {
		logger.Info("external-cli forward: no operations planned")
		return nil
	}
	logger.Info("external-cli forward: applying ops", "count", len(ops))
	_, warnings := externalcli.ApplyForward(v1Path, ops)
	return warnings
}

// claudePathHash mirrors internal/agent.claudeEncodePath. Duplicated
// here (rather than exported) so the migrate path does not pull the
// agent backend's process-wide globals into the migration binary.
func claudePathHash(absDir string) string {
	return strings.NewReplacer(
		string(filepath.Separator), "-",
		".", "-",
		"_", "-",
	).Replace(absDir)
}

// claudeConfigRoot mirrors internal/agent.claudeConfigDir. Same
// rationale as claudePathHash — keep migration self-contained.
func claudeConfigRoot() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".claude")
}
