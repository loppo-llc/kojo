package importers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/loppo-llc/kojo/internal/migrate"
	"github.com/loppo-llc/kojo/internal/store"
)

// sessionsImporter walks <v0>/sessions.json and inserts the rows into
// the v1 sessions table. Domain key: "sessions".
//
// Per design doc §5.5 (line 915): "status は全て `archived` に強制。
// live import 不可、PTY は v0 停止と同時に消失". The v0 PTY is gone
// the moment v0 stopped, so any "running" status in the v0 file is
// stale and would lie to v1 callers about what's actually live.
//
// peer_id is stamped from opts.HomePeer because sessions are local-
// scoped (per-peer PTY state); a snapshot from another device must
// be able to tell which peer originally launched a given session.
type sessionsImporter struct{}

func (sessionsImporter) Domain() string { return "sessions" }

// v0SessionInfo decodes one entry from sessions.json. Only the
// fields that map onto v1 columns are decoded; everything else
// (LastOutput / LastCols / LastRows / Attachments / TmuxSessionName /
// ToolSessionID / ParentID / YoloMode / Internal / Tool-side state)
// is intentionally dropped because:
//
//   - LastOutput/LastCols/LastRows: runtime PTY state, gone the
//     moment v0 stopped.
//   - Attachments: ephemeral upload tracking; the actual files are
//     elsewhere on disk.
//   - TmuxSessionName: v0-specific UX state with no v1 column.
//   - ToolSessionID/ParentID: claude/codex/gemini CLI internal ids.
//     v1 sessions table has no column for them and the external CLI
//     migration (5.5.1) handles tool-side state separately.
//   - YoloMode/Internal: per-row policy flags; v1 surfaces them via
//     Settings on the agent row, not on the session itself.
//
// AgentID is not a v0 field — sessions in v0 are not associated with
// agents on the SessionInfo struct itself. v1 agent_id is left NULL;
// a future enrichment pass could backfill from agent transcripts if
// needed (out of scope here per design doc §5.5).
type v0SessionInfo struct {
	ID        string   `json:"id"`
	Tool      string   `json:"tool"`
	WorkDir   string   `json:"workDir"`
	Args      []string `json:"args,omitempty"`
	Status    string   `json:"status"`
	ExitCode  *int     `json:"exitCode,omitempty"`
	CreatedAt string   `json:"createdAt"`
}

func (sessionsImporter) Run(ctx context.Context, st *store.Store, opts migrate.Options) error {
	if done, err := alreadyImported(ctx, st, "sessions"); err != nil {
		return err
	} else if done {
		return nil
	}

	logger := slog.Default().With("importer", "sessions")

	srcPaths, err := collectSessionsSourcePaths(opts.V0Dir)
	if err != nil {
		return fmt.Errorf("collect source paths: %w", err)
	}
	checksum, err := domainChecksum(opts.V0Dir, srcPaths)
	if err != nil {
		return fmt.Errorf("checksum sessions sources: %w", err)
	}

	path := filepath.Join(opts.V0Dir, "sessions.json")
	data, err := readV0(opts.V0Dir, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return markImported(ctx, st, "sessions", 0, checksum)
		}
		return err
	}
	if len(data) == 0 {
		return markImported(ctx, st, "sessions", 0, checksum)
	}

	var infos []v0SessionInfo
	if err := json.Unmarshal(data, &infos); err != nil {
		// Malformed file is not fatal — log and mark imported with 0
		// rows so a subsequent run doesn't re-attempt the same parse.
		logger.Warn("sessions: skipping malformed file",
			"path", path, "err", err)
		return markImported(ctx, st, "sessions", 0, checksum)
	}
	if len(infos) == 0 {
		return markImported(ctx, st, "sessions", 0, checksum)
	}

	mtime := fileMTimeMillis(path)
	recs := make([]*store.SessionRecord, 0, len(infos))
	for i, s := range infos {
		if s.ID == "" {
			logger.Warn("sessions: skipping entry without id",
				"index", i)
			continue
		}

		created := parseRFC3339Millis(s.CreatedAt)
		if created == 0 {
			created = mtime
		}
		if created == 0 {
			created = store.NowMillis()
		}

		// Build the launch command snapshot. v0 doesn't preserve the
		// full argv vector (Args is the *post-launch* args slice the
		// session manager fed to the PTY); joining with spaces gives
		// a debug-readable cmd column without claiming this is shell-
		// safe to re-execute.
		cmd := s.Tool
		if len(s.Args) > 0 {
			cmd = s.Tool + " " + strings.Join(s.Args, " ")
		}

		// stopped_at: the v0 file only records CreatedAt; the row was
		// last written when the session became stopped/archived. mtime
		// is the closest signal we have for "when did this session
		// actually stop". Fall back to created_at when mtime is
		// unreliable (zero / older than created_at) — every imported
		// row is forced to status='archived', so a missing stopped_at
		// would let downstream consumers ("when did this end?") see
		// NULL on a definitively-stopped row, which is misleading.
		stoppedAt := created
		if mtime > 0 && mtime >= created {
			stoppedAt = mtime
		}
		// updated_at reflects the last modification of the row on
		// disk; for an archived import that's stopped_at, not the
		// session's birth time. Without this, the etag canonical
		// record would treat an old archived session as if it had
		// just been stamped.
		updated := stoppedAt

		var exit *int64
		if s.ExitCode != nil {
			v := int64(*s.ExitCode)
			exit = &v
		}

		recs = append(recs, &store.SessionRecord{
			ID:        s.ID,
			AgentID:   nil, // v0 SessionInfo does not carry agent_id
			Status:    "archived",
			PID:       nil, // PTY died with v0; PID is meaningless
			Cmd:       cmd,
			WorkDir:   s.WorkDir,
			StartedAt: &created,
			StoppedAt: &stoppedAt,
			ExitCode:  exit,
			CreatedAt: created,
			UpdatedAt: updated,
		})
	}

	if len(recs) == 0 {
		return markImported(ctx, st, "sessions", 0, checksum)
	}

	n, err := st.BulkInsertSessions(ctx, recs, store.SessionInsertOptions{PeerID: opts.HomePeer})
	if err != nil {
		return fmt.Errorf("bulk insert sessions: %w", err)
	}
	return markImported(ctx, st, "sessions", n, checksum)
}

// collectSessionsSourcePaths returns the deterministic set of v0 files
// the sessions domain hashes. Single file, but follow the same shape
// as other importers so domainChecksum can ingest it uniformly.
func collectSessionsSourcePaths(v0Dir string) ([]string, error) {
	var paths []string
	p := filepath.Join(v0Dir, "sessions.json")
	updated, err := addLeafIfRegular(v0Dir, p, paths)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return paths, nil
		}
		return nil, err
	}
	return updated, nil
}
