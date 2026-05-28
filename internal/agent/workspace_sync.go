package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/loppo-llc/kojo/internal/atomicfile"
	"github.com/loppo-llc/kojo/internal/store"
)

// workspaceFileKinds is the canonical list of kinds the reconcile and
// disk-sync loops iterate over. Keep aligned with the SQL CHECK
// constraint at internal/store/migrations/0013_agent_workspace_files.sql
// and the WorkspaceFileKind* constants.
var workspaceFileKinds = []store.WorkspaceFileKind{
	store.WorkspaceFileKindUser,
	store.WorkspaceFileKindCheckin,
}

// workspaceFilePath returns the canonical on-disk mirror path for one
// workspace-file kind under the agent's data dir.
func workspaceFilePath(agentID string, kind store.WorkspaceFileKind) string {
	return filepath.Join(agentDir(agentID), workspaceFileDiskName(kind))
}

// ReconcileWorkspaceFilesDiskFromDBHeld rewrites the on-disk mirror of
// every workspace-file kind to match the authoritative DB state. The
// caller MUST hold the per-agent memorySyncMu via LockAgentMemorySync so
// the read-and-write sequence here can't race the disk→DB path.
//
// Strategy per kind:
//
//   - live DB row → write the body to disk iff the existing disk file
//     differs (sha mismatch) or is absent.
//   - no DB row (ErrNotFound) → ensure disk file is absent.
//   - other DB error → log and continue with the next kind so a
//     transient failure on one kind doesn't strand the others.
//
// Errors are surfaced via the returned error (first encountered);
// per-kind failures don't abort the loop so a single permission glitch
// on user.md doesn't block checkin.md from reconciling.
func ReconcileWorkspaceFilesDiskFromDBHeld(ctx context.Context, st *store.Store, agentID string, logger *slog.Logger) error {
	if st == nil || agentID == "" {
		return nil
	}

	var firstErr error
	captureErr := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	for _, kind := range workspaceFileKinds {
		diskPath := workspaceFilePath(agentID, kind)
		rec, err := st.GetAgentWorkspaceFile(ctx, agentID, kind)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// No live DB row — mirror by removing the disk file.
			if rerr := os.Remove(diskPath); rerr != nil && !os.IsNotExist(rerr) {
				if logger != nil {
					logger.Warn("workspace reconcile: disk remove failed",
						"agent", agentID, "kind", string(kind), "path", diskPath, "err", rerr)
				}
				captureErr(rerr)
			}
		case err != nil:
			if logger != nil {
				logger.Warn("workspace reconcile: DB read failed",
					"agent", agentID, "kind", string(kind), "err", err)
			}
			captureErr(err)
			continue
		default:
			// Live DB row. Only rewrite if disk differs (cheap
			// path: skip on sha match).
			needWrite := true
			if existing, rerr := os.ReadFile(diskPath); rerr == nil {
				if store.SHA256Hex(existing) == rec.BodySHA256 {
					needWrite = false
				}
			}
			if !needWrite {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
				if logger != nil {
					logger.Warn("workspace reconcile: mkdir failed",
						"agent", agentID, "kind", string(kind), "err", err)
				}
				captureErr(err)
				continue
			}
			if err := atomicfile.WriteBytes(diskPath, []byte(rec.Body), 0o644); err != nil {
				if logger != nil {
					logger.Warn("workspace reconcile: write failed",
						"agent", agentID, "kind", string(kind), "err", err)
				}
				captureErr(err)
			}
		}
	}
	return firstErr
}

// SyncWorkspaceFilesFromDisk reconciles the on-disk workspace files into
// the agent_workspace_files table. Reverse direction of
// ReconcileWorkspaceFilesDiskFromDBHeld — used by Load and any other
// path that suspects a CLI-direct edit of user.md / checkin.md happened
// while the daemon was down.
//
// For each kind:
//   - disk file absent: SoftDeleteAgentWorkspaceFile (idempotent
//     unconditional). On Load this is harmless when the row was
//     never live; in steady state it catches "user removed checkin.md
//     in the CLI" propagation.
//   - disk file present:
//     - DB body matches → skip
//     - DB body differs / no row → Upsert with AllowOverwrite=true
//
// Best-effort: per-kind failures log and surface as the returned
// firstErr but don't short-circuit other kinds.
func SyncWorkspaceFilesFromDisk(ctx context.Context, st *store.Store, agentID string, logger *slog.Logger) error {
	if st == nil || agentID == "" {
		return nil
	}

	var firstErr error
	captureErr := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	for _, kind := range workspaceFileKinds {
		diskPath := workspaceFilePath(agentID, kind)
		data, rerr := readBoundedFile(diskPath)
		missing := rerr != nil && os.IsNotExist(rerr)
		switch {
		case missing:
			// Idempotent tombstone — no-op when no live row exists.
			if err := st.SoftDeleteAgentWorkspaceFile(ctx, agentID, kind, ""); err != nil {
				if logger != nil {
					logger.Warn("workspace disk sync: tombstone failed",
						"agent", agentID, "kind", string(kind), "err", err)
				}
				captureErr(err)
			}
		case rerr != nil:
			// Real I/O error. Skip — running an unconditional
			// SoftDelete here would clobber a live DB row on a
			// transient read failure.
			if logger != nil {
				logger.Warn("workspace disk sync: read failed",
					"agent", agentID, "kind", string(kind), "path", diskPath, "err", rerr)
			}
			captureErr(rerr)
		default:
			body := string(data)
			trimmed := strings.TrimSpace(body)
			if trimmed == "" {
				// Empty / whitespace-only disk file is treated as
				// "cleared" — tombstone the row to match.
				if err := st.SoftDeleteAgentWorkspaceFile(ctx, agentID, kind, ""); err != nil {
					if logger != nil {
						logger.Warn("workspace disk sync: tombstone (empty file) failed",
							"agent", agentID, "kind", string(kind), "err", err)
					}
					captureErr(err)
				}
				continue
			}
			cur, gerr := st.GetAgentWorkspaceFile(ctx, agentID, kind)
			if gerr == nil && cur != nil && cur.BodySHA256 == store.SHA256Hex([]byte(trimmed)) {
				continue // already in sync
			}
			if gerr != nil && !errors.Is(gerr, store.ErrNotFound) {
				if logger != nil {
					logger.Warn("workspace disk sync: probe failed",
						"agent", agentID, "kind", string(kind), "err", gerr)
				}
				captureErr(gerr)
				continue
			}
			if _, uerr := st.UpsertAgentWorkspaceFile(ctx, agentID, kind, trimmed, "",
				store.AgentWorkspaceFileInsertOptions{AllowOverwrite: true}); uerr != nil {
				if errors.Is(uerr, store.ErrNotFound) {
					// Agent row missing — best-effort no-op. The
					// next sync of a still-live agent will catch up.
					continue
				}
				if logger != nil {
					logger.Warn("workspace disk sync: upsert failed",
						"agent", agentID, "kind", string(kind), "err", uerr)
				}
				captureErr(fmt.Errorf("workspace upsert (%s): %w", string(kind), uerr))
			}
		}
	}
	return firstErr
}
