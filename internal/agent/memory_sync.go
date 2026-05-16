package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/atomicfile"
	"github.com/loppo-llc/kojo/internal/store"
)

// memorySyncMu serializes SyncAgentMemoryFromDisk on a per-agent basis.
// Without it, two concurrent syncs (e.g. fork-from-A landing in parallel
// with prepareChat-on-A) could each take an inconsistent on-disk
// snapshot, then race their upserts/tombstones — the loser overwrites
// the winner, possibly resurrecting stale state. The mutex is acquired
// for the duration of the FS walk + every store write so the
// snapshot-is-consistent-with-the-writes invariant holds for the whole
// sync.
//
// Map entries are never deleted: agent IDs are bounded (low thousands
// in the worst case) and an unheld *sync.Mutex is small. The simpler
// "leak by design" pattern matches Manager.LockPatch.
var memorySyncMu struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func memorySyncLockFor(agentID string) *sync.Mutex {
	memorySyncMu.mu.Lock()
	defer memorySyncMu.mu.Unlock()
	if memorySyncMu.m == nil {
		memorySyncMu.m = make(map[string]*sync.Mutex)
	}
	mu, ok := memorySyncMu.m[agentID]
	if !ok {
		mu = &sync.Mutex{}
		memorySyncMu.m[agentID] = mu
	}
	return mu
}

// lockMemorySync blocks until the per-agent mutex is acquired. Used by
// callers (Load, fork, Create, ResetData) where we MUST sync before
// returning so the DB reflects the freshly-copied / freshly-loaded
// state. The lock wait is bounded by contention against other syncs
// for the same agent, which in normal operation is short — concurrent
// callers serialize but each individual sync runs to completion.
func lockMemorySync(agentID string) func() {
	mu := memorySyncLockFor(agentID)
	mu.Lock()
	return mu.Unlock
}

// tryLockMemorySync is the non-blocking variant for hot paths
// (prepareChat) where blocking on an in-flight sync would stall the
// chat. Uses sync.Mutex.TryLock so a held lock returns immediately;
// caller skips this turn and the next prepareChat / scheduled hook
// retries. No ctx parameter — the contract is "if busy, skip" with no
// timeout to manage.
func tryLockMemorySync(agentID string) (func(), bool) {
	mu := memorySyncLockFor(agentID)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// canonicalMemoryKindDirs maps a top-level memory subdirectory to its DB kind.
// Mirrors internal/migrate/importers/agents.canonicalKindDirs so the round-trip
// (v0 → DB → file) and the live sync (file → DB) classify entries identically.
// Anything not listed is folded into "topic" with the directory name baked
// into the entry's stored name so the partial unique index treats sibling
// files as distinct rows.
var canonicalMemoryKindDirs = map[string]string{
	"projects": "project",
	"people":   "people",
	"topics":   "topic",
	"archive":  "archive",
}

// memoryDailyDateRe identifies top-level memory/<YYYY-MM-DD>.md files which
// land under kind=daily.
var memoryDailyDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// syncAgentMemoryToDB reconciles the on-disk MEMORY.md with the
// agent_memory row. Post-cutover the DB is canonical (design §2.3 /
// §5.5); disk is a hydrated mirror that the CLI process reads on each
// invocation.
//
// Idempotent: a stable file that already matches the DB sha256 is a no-op,
// so this is safe to call from startup, fork, ensureAgentDir, and any other
// known mutation hook without triggering etag churn.
//
// Missing-file behavior:
//   - no DB row OR row already tombstoned → no-op
//   - live non-empty DB row → HYDRATE disk from DB (atomic write).
//     This covers (a) post-v0→v1 first boot where the importer
//     populated the DB but never wrote disk, (b) operator wiping
//     agentDir manually. CLI `rm MEMORY.md` no longer auto-tombstones;
//     explicit clear must round-trip through Web UI / API
//     handleDeleteAgentMemory which calls SoftDeleteAgentMemory
//     directly.
func syncAgentMemoryToDB(ctx context.Context, st *store.Store, agentID string, logger *slog.Logger) error {
	if st == nil {
		return nil
	}
	path := filepath.Join(agentDir(agentID), "MEMORY.md")
	body, readErr := os.ReadFile(path)
	missing := errors.Is(readErr, fs.ErrNotExist)
	if readErr != nil && !missing {
		return fmt.Errorf("sync MEMORY.md: read %s: %w", path, readErr)
	}
	prev, err := st.GetAgentMemory(ctx, agentID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("sync MEMORY.md: read DB row: %w", err)
	}

	if missing {
		// File deleted out from under us. If there's no live DB row
		// nothing to do.
		if prev == nil || prev.DeletedAt != nil {
			return nil
		}
		// Disk-hydrate path. Post-cutover the DB is canonical for
		// MEMORY.md; disk is a local mirror the CLI reads. On
		// missing-disk + live DB row, hydrate disk from DB instead
		// of tombstoning. Covers (a) first boot after v0→v1
		// migration where the importer populated the DB but never
		// wrote disk, (b) operator wiping agentDir manually. CLI
		// `rm MEMORY.md` no longer auto-tombstones — explicit
		// DELETE via Web UI / API is the only way to remove the
		// row. See docs §2.3 / §5.5 (DB canonical, disk mirror).
		if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
			return fmt.Errorf("sync MEMORY.md: ensure agent dir: %w", err)
		}
		if err := atomicfile.WriteBytes(filepath.Join(agentDir(agentID), "MEMORY.md"),
			[]byte(prev.Body), 0o644); err != nil {
			return fmt.Errorf("sync MEMORY.md: hydrate disk: %w", err)
		}
		if logger != nil {
			logger.Debug("MEMORY.md sync: hydrated disk from DB row",
				"agent", agentID, "size", len(prev.Body))
		}
		return nil
	}

	bodySHA := store.SHA256Hex(body)
	if prev != nil && prev.BodySHA256 == bodySHA && prev.DeletedAt == nil {
		return nil // already in sync
	}
	if _, err := st.UpsertAgentMemory(ctx, agentID, string(body), "", store.AgentMemoryInsertOptions{
		AllowOverwrite: true,
	}); err != nil {
		// ErrNotFound here means the agent row itself is gone (race
		// against Delete). Treat as a no-op rather than failing the
		// caller — the next sync of a still-live agent will succeed.
		if errors.Is(err, store.ErrNotFound) {
			if logger != nil {
				logger.Debug("MEMORY.md sync: agent row missing", "agent", agentID)
			}
			return nil
		}
		return fmt.Errorf("sync MEMORY.md: upsert: %w", err)
	}
	return nil
}

// memoryFileEntry is the (kind, name, path) triple a single *.md file in
// the agent's memory/ subtree maps to. Used as the bridge between
// filesystem walk and DB upsert.
type memoryFileEntry struct {
	Kind string
	Name string
	Path string
}

// scanMemoryDir enumerates the agent's memory/ directory and produces the
// (kind, name) pairs each *.md file should land under in memory_entries.
// The classification mirrors the v0→v1 importer so a file that was once
// imported and then re-edited stays under the same row.
//
// Missing memory/ is reported as an empty slice (not an error) so a
// brand-new agent with no memory yet syncs cleanly.
func scanMemoryDir(agentID string, logger *slog.Logger) ([]memoryFileEntry, error) {
	root := filepath.Join(agentDir(agentID), "memory")
	rootEntries, err := os.ReadDir(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("scan memory dir: %w", err)
	}

	var (
		out      []memoryFileEntry
		firstErr error
	)
	for _, e := range rootEntries {
		full := filepath.Join(root, e.Name())
		if e.IsDir() {
			kind, ok := canonicalMemoryKindDirs[e.Name()]
			namePrefix := ""
			if !ok {
				if logger != nil {
					logger.Debug("memory: unrecognized subdirectory, treating as topic with dir-prefixed name",
						"agent", agentID, "dir", e.Name())
				}
				kind = "topic"
				namePrefix = e.Name() + "/"
			}
			entries, subErr := scanMemorySubdir(full, kind, namePrefix, logger, agentID)
			// Append whatever entries the subdir scan DID enumerate
			// even on error, so the upsert phase upstream still
			// reflects the readable subset. Capturing firstErr
			// signals the upstream tombstone phase to stay home.
			out = append(out, entries...)
			if subErr != nil && firstErr == nil {
				firstErr = subErr
			}
			continue
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".md")
		kind := "topic"
		if memoryDailyDateRe.MatchString(stem) {
			kind = "daily"
		}
		out = append(out, memoryFileEntry{Kind: kind, Name: stem, Path: full})
	}
	return out, firstErr
}

// scanMemorySubdir is the recursive walker for one canonical (or
// fall-through) subdir. Mirrors importMemoryDir's name-resolution rules.
//
// IMPORTANT: walkErr / Rel errors are propagated (returned non-nil) so
// the caller knows the scan was incomplete. Swallowing them would let
// the tombstone phase soft-delete legitimate rows whose backing files
// existed in an unreadable subtree but weren't enumerated. The caller
// (syncMemoryEntriesToDB) skips the tombstone phase when scanErr != nil.
func scanMemorySubdir(dir, kind, namePrefix string, logger *slog.Logger, agentID string) ([]memoryFileEntry, error) {
	var out []memoryFileEntry
	var firstWalkErr error
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if logger != nil {
				logger.Warn("memory walk error",
					"agent", agentID, "path", path, "err", walkErr)
			}
			if firstWalkErr == nil {
				firstWalkErr = walkErr
			}
			// Continue walking siblings so we get the most-complete
			// possible upsert set; the firstWalkErr capture above
			// disables the destructive tombstone phase upstream.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			if logger != nil {
				logger.Warn("memory walk: rel-path failed",
					"agent", agentID, "path", path, "err", relErr)
			}
			if firstWalkErr == nil {
				firstWalkErr = relErr
			}
			return nil
		}
		name := namePrefix + strings.TrimSuffix(filepath.ToSlash(rel), ".md")
		if name == "" {
			return nil
		}
		out = append(out, memoryFileEntry{Kind: kind, Name: name, Path: path})
		return nil
	})
	if walkErr != nil {
		return out, walkErr
	}
	return out, firstWalkErr
}

// syncMemoryEntriesToDB reconciles the agent's memory/ directory with the
// memory_entries table. For each on-disk *.md it upserts a row keyed by
// (agent_id, kind, name).
//
// Tombstone-vs-hydrate decision: if the on-disk memory/ tree contains at
// least one .md file we treat disk as user-managed and tombstone any DB
// row whose backing file is gone (preserves "CLI deleted a note → row
// disappears cross-device"). If the tree is missing entirely OR contains
// zero .md files we treat disk as uninitialized and HYDRATE the DB rows
// to disk instead (covers post-v0→v1 first boot where the importer
// populated the DB but never wrote files; also covers an operator who
// wiped agentDir/memory/ wholesale and expects DB to repopulate it).
// Partial wipes — `rm memory/projects/*` while memory/people/ still has
// files — fall on the tombstone side; the operator must use the explicit
// DELETE API for that subset, or wipe the whole tree to trigger
// hydrate. See docs §2.3 / §5.5 (DB canonical, disk mirror).
//
// Best-effort per entry: a single failed upsert / tombstone / hydrate
// logs and continues so one stuck file (e.g. a permissions race) doesn't
// prevent the rest from syncing. Returns the first error encountered for
// the caller to log; the side-effects up to that point still apply.
func syncMemoryEntriesToDB(ctx context.Context, st *store.Store, agentID string, logger *slog.Logger) error {
	if st == nil {
		return nil
	}
	disk, scanErr := scanMemoryDir(agentID, logger)
	if scanErr != nil {
		// Read failure on the agent's own memory dir: log and skip
		// the tombstone phase. Running tombstone against a partial
		// onDisk would silently soft-delete legitimate rows whose
		// files we just couldn't read — a transient permission
		// glitch could wipe out a year of memory entries that way.
		// The upsert phase is still safe (only acts on files we DID
		// read), so we run it against the partial set so a user-
		// visible subset of the agent's notes is at least reflected.
		if logger != nil {
			logger.Warn("memory scan failed; tombstone phase skipped, upsert phase partial",
				"agent", agentID, "err", scanErr)
		}
	}

	// Build lookup of (kind, name) → path so the tombstone phase below
	// can ask "is this DB row still backed by a file?" in O(1).
	type natKey struct{ kind, name string }
	onDisk := make(map[natKey]memoryFileEntry, len(disk))
	for _, e := range disk {
		onDisk[natKey{e.Kind, e.Name}] = e
	}

	// Hydrate-mode trigger. Two reasons we may need to hydrate disk
	// from DB instead of tombstoning DB rows:
	//   (a) disk is fresh / wiped: no .md files anywhere under
	//       memory/ yet a live DB row population exists (post-
	//       migration first boot, operator-wiped tree).
	//   (b) a previous hydrate run was interrupted partway: a
	//       .hydrate_pending marker file remains. Without this the
	//       next sync would observe "some .md files now exist" and
	//       fall into tombstone-mode, soft-deleting every un-
	//       hydrated row.
	// Marker is written before any hydrate writes and removed only
	// after all hydrates succeed, so a crash mid-hydrate keeps us
	// in hydrate-mode on the next sync.
	hydratePendingPath := filepath.Join(agentDir(agentID), "memory", ".hydrate_pending")
	_, hydratePendingErr := os.Stat(hydratePendingPath)
	hydratePending := hydratePendingErr == nil
	diskUninitialized := scanErr == nil && (len(disk) == 0 || hydratePending)

	// Upsert phase: each disk file → DB row (insert if missing,
	// update if body changed, no-op if sha matches).
	//
	// Hydrate-mode skip: when diskUninitialized is true (or the
	// .hydrate_pending marker is present from a prior partial hydrate)
	// the disk files we DO see are themselves the hydrated mirror of
	// DB rows from a previous run — re-upserting them now would
	// clobber any DB-side write that landed during the hydrate
	// window (e.g., a Web UI edit while hydrate was finishing the
	// other half of the tree). DB is canonical in this state; skip
	// the upsert phase entirely and let the hydrate loop below
	// catch up the still-missing rows.
	var firstErr error
	if scanErr != nil {
		firstErr = scanErr
	}
	if diskUninitialized {
		goto hydratePhase
	}
	for _, e := range disk {
		body, err := os.ReadFile(e.Path)
		if err != nil {
			if logger != nil {
				logger.Warn("memory entry read failed", "agent", agentID,
					"kind", e.Kind, "name", e.Name, "err", err)
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := upsertMemoryEntry(ctx, st, agentID, e.Kind, e.Name, body, e.Path); err != nil {
			if logger != nil {
				logger.Warn("memory entry upsert failed", "agent", agentID,
					"kind", e.Kind, "name", e.Name, "err", err)
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}

hydratePhase:
	// Tombstone phase: any live DB entry whose (kind, name) isn't
	// on disk anymore must be soft-deleted so cross-device readers
	// stop seeing it. Use a generous limit because memory_entries can
	// grow large (one entry per day for years); paginate via Cursor.
	//
	// SKIP this phase if the disk scan reported an error — onDisk is
	// incomplete, and tombstoning against an incomplete set would
	// soft-delete legitimate rows whose files exist but couldn't be
	// listed.
	if scanErr != nil {
		return firstErr
	}

	// Hydrate-mode bookkeeping: write the .hydrate_pending marker
	// before any hydrate writes so a crash mid-loop keeps the next
	// sync in hydrate-mode (otherwise a partially-populated tree
	// would be misread as user-managed and the un-hydrated rows
	// would be tombstoned). Removed at function end iff we hydrated
	// every missing row successfully.
	//
	// Marker write failure is fatal for the hydrate phase: continuing
	// without a marker means a crash mid-hydrate leaves the next sync
	// unable to detect the partial state, and tombstone-mode would
	// soft-delete the un-hydrated rows. Skip the loop in that case;
	// the next sync will retry both the marker and the hydrate.
	hydratedAll := true
	if diskUninitialized {
		if err := os.MkdirAll(filepath.Dir(hydratePendingPath), 0o755); err != nil {
			if logger != nil {
				logger.Warn("memory entry hydrate marker mkdir failed; skipping hydrate phase",
					"agent", agentID, "err", err)
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("memory hydrate marker mkdir: %w", err)
			}
			return firstErr
		}
		if err := atomicfile.WriteBytes(hydratePendingPath, []byte("hydrate-pending\n"), 0o644); err != nil {
			if logger != nil {
				logger.Warn("memory entry hydrate marker write failed; skipping hydrate phase",
					"agent", agentID, "err", err)
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("memory hydrate marker write: %w", err)
			}
			return firstErr
		}
	}

	var cursor int64
	for {
		recs, err := st.ListMemoryEntries(ctx, agentID, store.MemoryEntryListOptions{
			Limit:  500,
			Cursor: cursor,
		})
		if err != nil {
			if logger != nil {
				logger.Warn("memory entry list failed", "agent", agentID, "err", err)
			}
			if firstErr == nil {
				firstErr = err
			}
			break
		}
		if len(recs) == 0 {
			break
		}
		for _, rec := range recs {
			cursor = rec.Seq
			_, onDiskNow := onDisk[natKey{rec.Kind, rec.Name}]
			if onDiskNow && !diskUninitialized {
				continue
			}
			// In hydrate-mode (diskUninitialized), re-hydrate every
			// row regardless of disk presence. The on-disk file was
			// itself written by a prior partial-hydrate run; if the
			// DB body has since diverged (Web UI edit during the
			// hydrate window), the file is stale and a non-overwrite
			// would freeze that staleness — next sync would treat
			// disk as canonical and roll the DB back.
			if diskUninitialized {
				// Disk tree empty or missing; hydrate from DB
				// instead of tombstoning.
				if err := hydrateMemoryEntryToDisk(agentID, rec); err != nil {
					if logger != nil {
						logger.Warn("memory entry hydrate failed",
							"agent", agentID, "id", rec.ID,
							"kind", rec.Kind, "name", rec.Name, "err", err)
					}
					if firstErr == nil {
						firstErr = err
					}
					hydratedAll = false
				} else if logger != nil {
					logger.Debug("memory entry: hydrated disk from DB row",
						"agent", agentID, "kind", rec.Kind,
						"name", rec.Name, "size", len(rec.Body))
				}
				continue
			}
			// File is gone, but disk has other files (operator-
			// managed tree) → tombstone the DB row so cross-device
			// readers don't see a phantom entry.
			if err := st.SoftDeleteMemoryEntry(ctx, rec.ID, ""); err != nil {
				if logger != nil {
					logger.Warn("memory entry tombstone failed", "agent", agentID,
						"id", rec.ID, "kind", rec.Kind, "name", rec.Name, "err", err)
				}
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		if len(recs) < 500 {
			break
		}
	}

	// Marker cleanup: only remove if hydrate-mode and every row
	// either was already on disk or hydrated cleanly. Mixed
	// success keeps the marker so the next sync re-enters hydrate-
	// mode and finishes the job.
	if diskUninitialized && hydratedAll && firstErr == nil {
		if err := os.Remove(hydratePendingPath); err != nil && !os.IsNotExist(err) {
			if logger != nil {
				logger.Warn("memory entry hydrate marker remove failed",
					"agent", agentID, "err", err)
			}
		}
	}
	return firstErr
}

// upsertMemoryEntry inserts or updates a single memory_entries row keyed
// by (agent_id, kind, name). Used by syncMemoryEntriesToDB.
func upsertMemoryEntry(ctx context.Context, st *store.Store, agentID, kind, name string, body []byte, path string) error {
	cur, err := st.FindMemoryEntryByName(ctx, agentID, kind, name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if cur != nil {
		if cur.BodySHA256 == store.SHA256Hex(body) {
			return nil // already in sync
		}
		bodyStr := string(body)
		_, err := st.UpdateMemoryEntry(ctx, cur.ID, "", store.MemoryEntryPatch{Body: &bodyStr})
		return err
	}
	updated := fileMTimeMillis(path)
	if updated == 0 {
		updated = store.NowMillis()
	}
	_, err = st.InsertMemoryEntry(ctx, &store.MemoryEntryRecord{
		ID:      newMemoryEntryID(),
		AgentID: agentID,
		Kind:    kind,
		Name:    name,
		Body:    string(body),
	}, store.MemoryEntryInsertOptions{
		UpdatedAt: updated,
		CreatedAt: updated,
	})
	if errors.Is(err, store.ErrNotFound) {
		// Agent row vanished between scan and upsert — best-effort
		// no-op. The next sync of a still-live agent picks up the
		// state.
		return nil
	}
	return err
}

// fileMTimeMillis returns the file's modification time as Unix
// milliseconds, or 0 if the stat fails. Best-effort — callers fall
// back to NowMillis() when the result is 0.
func fileMTimeMillis(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().UTC().UnixNano() / int64(time.Millisecond)
}

// newMemoryEntryID generates a 16-byte random "me_"-prefixed id. Mirrors
// the importer's helper so live-synced rows get the same ID shape as
// imported ones — keeps log lines and FK lookups uniform.
func newMemoryEntryID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Should be unreachable on every supported platform; fall back
		// to a timestamp-based id rather than crashing the daemon.
		return "me_fallback_" + hex.EncodeToString([]byte(fmt.Sprintf("%d", store.NowMillis())))[:8]
	}
	return "me_" + hex.EncodeToString(b[:])
}

// SyncAgentMemoryFromDisk runs both MEMORY.md and memory/ syncs for one
// agent. Used by Manager.Load (startup), Manager.fork (post-copy), and
// any other code that knows the disk state may have diverged from the DB.
//
// Per-agent serialization: holds memorySyncMu[agentID] for the entire
// run so concurrent callers (fork + prepareChat hitting the same agent,
// startup overlapping with an in-flight Web UI write, etc.) don't race
// their FS reads against each other's DB writes.
//
// Best-effort: each sub-sync logs its own errors and the function returns
// the first encountered so the caller can log a summary. Both sub-syncs
// run regardless of one's failure — a corrupted MEMORY.md should not
// prevent memory/ entries from syncing.
func SyncAgentMemoryFromDisk(ctx context.Context, st *store.Store, agentID string, logger *slog.Logger) error {
	if agentID == "" || st == nil {
		return nil
	}
	release := lockMemorySync(agentID)
	defer release()
	// Mint a fresh DB context so a long lock wait doesn't burn the
	// caller's deadline before we even start the DB ops. Caller's
	// ctx still cancels via the AfterFunc bridge.
	dbCtx, cancel := dbContextWithCancel(ctx, 30*time.Second)
	defer cancel()
	return syncAgentMemoryHeld(dbCtx, st, agentID, logger)
}

// SyncAgentMemoryFromDiskBestEffort is the non-blocking variant for hot
// paths (prepareChat). If the per-agent mutex is held by another sync
// in flight, we skip this turn rather than block — the file remains
// canonical for the CLI prompt build that runs immediately after, and
// the next prepareChat / Load / scheduled hook will retry. Returns
// (false, nil) on skip; (true, syncErr) when the sync ran.
//
// The DB context is internal: a fresh 5s timeout rooted at
// context.Background(). The caller's ctx is intentionally NOT
// propagated (see dbContextWithCancel for why) — its deadline would
// otherwise reapply to the DB ops, which is exactly the foot-gun the
// detachment is meant to avoid.
func SyncAgentMemoryFromDiskBestEffort(ctx context.Context, st *store.Store, agentID string, logger *slog.Logger) (ran bool, err error) {
	if agentID == "" || st == nil {
		return false, nil
	}
	release, ok := tryLockMemorySync(agentID)
	if !ok {
		return false, nil
	}
	defer release()
	dbCtx, cancel := dbContextWithCancel(ctx, 5*time.Second)
	defer cancel()
	return true, syncAgentMemoryHeld(dbCtx, st, agentID, logger)
}

// dbContextWithCancel mints a fresh context for the DB ops with the
// supplied timeout, rooted at context.Background() and intentionally
// detached from caller's parent ctx. Bridging via context.AfterFunc
// would re-couple the parent's deadline (Done fires on deadline OR
// cancel — there's no stdlib way to filter to "cancel only") which
// is exactly what we're trying to avoid: a caller passing a 5s ctx
// shouldn't see its DB ops cancelled at the 5s mark when the sync
// itself wants 30s.
//
// Caller-driven cancellation is therefore NOT propagated. In
// practice all current callers (Load, fork, Create, ResetData,
// prepareChat) pass a Background-rooted ctx with their own timeout
// and never cancel it explicitly — the only thing that would have
// been bridged is deadline expiry, which we don't want.
func dbContextWithCancel(_ context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

// syncAgentMemoryHeld runs both sub-syncs assuming the caller holds
// memorySyncMu[agentID]. Extracted so the blocking and best-effort
// public entry points share one body.
func syncAgentMemoryHeld(ctx context.Context, st *store.Store, agentID string, logger *slog.Logger) error {
	var firstErr error
	if err := syncAgentMemoryToDB(ctx, st, agentID, logger); err != nil {
		if logger != nil {
			logger.Warn("MEMORY.md sync failed", "agent", agentID, "err", err)
		}
		firstErr = err
	}
	if err := syncMemoryEntriesToDB(ctx, st, agentID, logger); err != nil {
		if logger != nil {
			logger.Warn("memory entries sync failed", "agent", agentID, "err", err)
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// memoryEntryDiskPath returns the canonical on-disk path for a
// memory_entries row. Mirrors the importer's (path → kind/name)
// classification so a hydrated file lands at a path the next scan
// will re-classify identically (round-trip stable).
//
// Topic ambiguity: a v0 file at memory/foo.md and one at
// memory/topics/foo.md both import as kind=topic, name="foo". The
// importer's natural-key dedup means only one row survives if both
// existed. On hydrate we land all such rows at memory/topics/<name>.md
// (the canonical v1 form). A name containing "/" is treated as the
// unknown-subdir fallthrough form (importer namePrefix="<dir>/") and
// re-expanded under memory/<dir>/<name>.md so topology survives.
func memoryEntryDiskPath(agentID, kind, name string) string {
	dir := filepath.Join(agentDir(agentID), "memory")
	switch kind {
	case "daily":
		return filepath.Join(dir, name+".md")
	case "project":
		return filepath.Join(dir, "projects", name+".md")
	case "people":
		return filepath.Join(dir, "people", name+".md")
	case "archive":
		return filepath.Join(dir, "archive", name+".md")
	case "topic":
		if strings.Contains(name, "/") {
			return filepath.Join(dir, filepath.FromSlash(name)+".md")
		}
		return filepath.Join(dir, "topics", name+".md")
	}
	// Unknown kind — best-effort under topics/ so the file is at
	// least addressable. Should not be reachable: kind is validated
	// at insert time by the schema CHECK constraint.
	return filepath.Join(dir, "topics", name+".md")
}

// hydrateMemoryEntryToDisk writes one memory_entries row's body to its
// canonical disk path, creating intermediate directories. Used by
// syncMemoryEntriesToDB's diskUninitialized branch on first boot
// after v0→v1 migration. Best-effort: a permission glitch on one
// entry surfaces as the sync's firstErr but does not abort the loop.
func hydrateMemoryEntryToDisk(agentID string, rec *store.MemoryEntryRecord) error {
	path := memoryEntryDiskPath(agentID, rec.Kind, rec.Name)

	// Containment guard: a corrupt or hostile rec.Name (containing
	// "..", absolute path, or smuggled separator that survived the
	// schema's CHECK + insert path) could otherwise resolve to a
	// path outside the agent's memory dir, allowing a write
	// arbitrary location. Re-rooting via filepath.Rel after the
	// Join + checking that the relative form has no leading ".."
	// segment is the canonical containment test the rest of kojo
	// uses (filebrowser.validatePath, agent_file_handlers
	// resolveAgentPath).
	memRoot := filepath.Join(agentDir(agentID), "memory")
	rel, err := filepath.Rel(memRoot, path)
	if err != nil {
		return fmt.Errorf("containment check: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("memory entry path escapes memory root: kind=%s name=%s",
			rec.Kind, rec.Name)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("ensure dir: %w", err)
	}
	if err := atomicfile.WriteBytes(path, []byte(rec.Body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
