package importers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/loppo-llc/kojo/internal/migrate"
	"github.com/loppo-llc/kojo/internal/store"
)

// tasksImporter walks every <v0>/agents/<id>/tasks.json and inserts the
// records into agent_tasks. Domain key: "tasks". Depends on the agents
// importer having run first (FK on agent_id).
//
// v0 status vocabulary is "open"/"done"; v1 uses
// 'pending'|'in_progress'|'done'|'cancelled' (CHECK constraint). The
// importer maps:
//
//	open  → pending
//	done  → done
//
// Any v0 row whose status is *already* one of the v1 vocabulary values
// passes through unchanged so a hand-edited tasks.json that anticipates
// v1 vocabulary doesn't get clobbered. Anything else is logged and
// skipped (rather than coerced) so corrupt data doesn't silently land
// in v1 with a wrong status.
type tasksImporter struct{}

func (tasksImporter) Domain() string { return "tasks" }

// v0Task decodes one entry from v0's tasks.json. Field tags match the
// JSON the file ships today; any unknown field is ignored.
type v0Task struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"` // "open" | "done" (v0 vocab)
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

func (tasksImporter) Run(ctx context.Context, st *store.Store, opts migrate.Options) error {
	if done, err := alreadyImported(ctx, st, "tasks"); err != nil {
		return err
	} else if done {
		return nil
	}

	logger := slog.Default().With("importer", "tasks")

	srcPaths, err := collectTasksSourcePaths(opts.V0Dir)
	if err != nil {
		return fmt.Errorf("collect source paths: %w", err)
	}
	checksum, err := domainChecksum(opts.V0Dir, srcPaths)
	if err != nil {
		return fmt.Errorf("checksum tasks sources: %w", err)
	}

	agents, err := st.ListAgents(ctx)
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}

	// Cross-agent task id collision: agent_tasks.id is a global PK
	// in v1, but v0 generated task ids per-agent. Same fork-with-
	// transcript collision pattern as agent_messages — see the
	// messages importer for the parallel rationale. Pre-load the
	// global id set so importAgentTasks can rewrite a colliding id
	// to a fresh one before BulkInsertAgentTasks's preload-existing
	// guard fires.
	globalExisting, err := loadAllAgentTaskIDs(ctx, st)
	if err != nil {
		return fmt.Errorf("preload global task ids: %w", err)
	}

	total := 0
	for _, a := range agents {
		n, err := importAgentTasks(ctx, st, opts.V0Dir, a.ID, globalExisting, logger)
		if err != nil {
			return fmt.Errorf("agent %s: %w", a.ID, err)
		}
		total += n
	}
	return markImported(ctx, st, "tasks", total, checksum)
}

// importAgentTasks reads tasks.json for one agent (if present) and inserts
// the rows into agent_tasks. Idempotency rests on agent_tasks.id being a
// stable PK across runs and BulkInsertAgentTasks's preload-skip path.
//
// Seq allocation: handled entirely by BulkInsertAgentTasks (MAX+1 inside
// the tx). The importer does *not* set rec.Seq — the store ignores it
// anyway. tasks.json's natural array order is preserved as the
// allocation order (rec[0] gets MAX+1, rec[1] gets MAX+2, ...), so a
// fresh import lands seq=1..N matching file order. A partial-import
// rerun continues from the prior MAX, leaving previously-imported rows
// untouched and assigning newly-seen rows monotonically increasing
// seq above the prior peak. Cross-agent id collisions surface as a
// hard error from BulkInsertAgentTasks rather than silent data loss.
func importAgentTasks(ctx context.Context, st *store.Store, v0Dir, agentID string, globalExisting map[string]bool, logger *slog.Logger) (int, error) {
	path := filepath.Join(agentDir(v0Dir, agentID), "tasks.json")
	data, err := readV0(v0Dir, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	// Empty file: tasks.json sometimes lands as a 0-byte file when the
	// CLI crashed between O_CREAT and the first write. Treat as empty
	// list, not as a JSON error.
	if len(data) == 0 {
		return 0, nil
	}

	var tasks []v0Task
	if err := json.Unmarshal(data, &tasks); err != nil {
		logger.Warn("tasks: skipping malformed file",
			"agent", agentID, "path", path, "err", err)
		return 0, nil
	}
	if len(tasks) == 0 {
		return 0, nil
	}

	mtime := fileMTimeMillis(path)
	recs := make([]*store.AgentTaskRecord, 0, len(tasks))
	for i, t := range tasks {
		if t.ID == "" {
			logger.Warn("tasks: skipping entry without id",
				"agent", agentID, "index", i)
			continue
		}
		if t.Title == "" {
			logger.Warn("tasks: skipping entry with empty title",
				"agent", agentID, "id", t.ID, "index", i)
			continue
		}
		mapped, ok := mapV0TaskStatus(t.Status)
		if !ok {
			logger.Warn("tasks: skipping entry with unknown status",
				"agent", agentID, "id", t.ID, "status", t.Status)
			continue
		}

		created := parseRFC3339Millis(t.CreatedAt)
		updated := parseRFC3339Millis(t.UpdatedAt)
		if created == 0 {
			created = mtime
		}
		if updated == 0 {
			updated = created
		}
		if created == 0 {
			created = store.NowMillis()
			updated = created
		}

		// Cross-agent collision check. globalExisting holds every
		// task id already in the DB; if t.ID is claimed (and it's
		// not THIS agent's previous import we're resuming),
		// rewrite to a fresh id. Self-owned ids stay untouched so
		// idempotent re-runs keep matching the existing rows via
		// the BulkInsertAgentTasks preload-skip path.
		insertID := t.ID
		if globalExisting[t.ID] && !taskIDOwnedBy(ctx, st, t.ID, agentID) {
			fresh, gerr := generateFreshTaskID(globalExisting)
			if gerr != nil {
				return 0, fmt.Errorf("rewrite colliding task id %q: %w", t.ID, gerr)
			}
			logger.Warn("tasks: cross-agent id collision; rewriting",
				"agent", agentID, "v0_id", t.ID, "v1_id", fresh)
			insertID = fresh
		}
		globalExisting[insertID] = true

		// Seq is allocated by BulkInsertAgentTasks; we leave it zero.
		recs = append(recs, &store.AgentTaskRecord{
			ID:        insertID,
			AgentID:   agentID,
			Title:     t.Title,
			Status:    mapped,
			CreatedAt: created,
			UpdatedAt: updated,
		})
	}

	if len(recs) == 0 {
		return 0, nil
	}

	n, err := st.BulkInsertAgentTasks(ctx, agentID, recs, store.AgentTaskInsertOptions{})
	if err != nil {
		return n, fmt.Errorf("bulk insert tasks: %w", err)
	}
	return n, nil
}

// mapV0TaskStatus translates a v0 status string into the v1 CHECK-
// constraint vocabulary. Returns (mapped, true) on success, ("", false)
// when the input is unrecognized so the caller can warn-and-skip
// instead of silently coercing corrupt data.
func mapV0TaskStatus(s string) (string, bool) {
	switch s {
	case "open":
		return "pending", true
	case "done":
		return "done", true
	// Already-v1 vocabulary passes through. A hand-edited tasks.json
	// that anticipated v1 should not get its statuses clobbered.
	case "pending", "in_progress", "cancelled":
		return s, true
	}
	return "", false
}

// collectTasksSourcePaths enumerates each agent's tasks.json. Same
// scan-vs-import caveat as collectMessagesSourcePaths: orphan agent
// dirs contribute to the checksum even when ListAgents skips them.
func collectTasksSourcePaths(v0Dir string) ([]string, error) {
	var paths []string
	base := agentsBaseDir(v0Dir)
	entries, err := readDirV0(v0Dir, base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return paths, nil
		}
		return nil, fmt.Errorf("readdir agents: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "groupdms" {
			continue
		}
		p := filepath.Join(base, e.Name(), "tasks.json")
		updated, err := addLeafIfRegular(v0Dir, p, paths)
		if err != nil {
			return nil, err
		}
		paths = updated
	}
	return paths, nil
}

// loadAllAgentTaskIDs returns every agent_tasks id currently in the
// DB across all agents. Used by the cross-agent collision detector
// in tasksImporter.Run; see the messages importer's
// loadAllMessageIDs for the parallel design.
func loadAllAgentTaskIDs(ctx context.Context, st *store.Store) (map[string]bool, error) {
	rows, err := st.DB().QueryContext(ctx, `SELECT id FROM agent_tasks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// taskIDOwnedBy returns true iff agent_tasks contains a row with
// (id, agent_id) matching the arguments. Used to distinguish "this
// id is THIS agent's already-imported row" (idempotent re-run, OK)
// from "this id is some OTHER agent's row" (cross-agent collision,
// rewrite). On any DB error we conservatively return false so the
// caller treats the row as "not ours" and rewrites — re-imports
// already tolerate skipped duplicates via the preload check.
func taskIDOwnedBy(ctx context.Context, st *store.Store, id, agentID string) bool {
	var owner string
	err := st.DB().QueryRowContext(ctx,
		`SELECT agent_id FROM agent_tasks WHERE id = ?`, id).Scan(&owner)
	if err != nil {
		return false
	}
	return owner == agentID
}

// generateFreshTaskID returns a v0-shape "task_<16hex>" identifier
// guaranteed not to collide with any id in `taken` at call time.
// Mirrors generateFreshMessageID in the messages importer.
func generateFreshTaskID(taken map[string]bool) (string, error) {
	var buf [8]byte
	for tries := 0; tries < 32; tries++ {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", fmt.Errorf("rand: %w", err)
		}
		id := "task_" + hex.EncodeToString(buf[:])
		if !taken[id] {
			return id, nil
		}
	}
	return "", errors.New("could not generate non-colliding task id after 32 tries")
}
