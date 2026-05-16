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

// LockAgentMemorySync acquires the per-agent memorySyncMu and returns
// the release callback. Exported so the §3.7 agent-sync HTTP handler
// can hold the lock across BOTH the DB write (store.SyncAgentFromPeer)
// AND the disk materialize step — otherwise a concurrent prepareChat
// could grab the lock between the commit and our materialize, walk
// stale disk, and UPSERT yesterday's bodies back into the DB the sync
// just refreshed.
//
// Caller MUST defer the returned func. Reentrant use within the same
// goroutine deadlocks (sync.Mutex is not reentrant); the matching
// MaterializeAgentSyncToDiskHeld variant assumes the lock is already
// held precisely so this stays clean.
func LockAgentMemorySync(agentID string) func() {
	return lockMemorySync(agentID)
}

// ReconcileAgentDiskFromDBHeld rewrites the agent's MEMORY.md and
// memory/* tree to match the AUTHORITATIVE post-commit DB state.
// Caller MUST hold the per-agent memorySyncMu (via
// LockAgentMemorySync) so the read-and-write sequence here can't
// race a concurrent disk→DB sync.
//
// Reads DB state instead of the wire payload because:
//
//  1. Incremental mode ships only the delta — pre-existing stale
//     disk for non-delta rows would not be healed by a delta-only
//     materializer (the bug source for the original peer→hub
//     diary-rollback report). Reading the live DB set covers
//     every row, not just the changed ones.
//
//  2. The DB already filtered through schema CHECK + insert
//     validation, so we don't have to defensively skip "kind/name
//     looks bad" rows from the wire payload that wouldn't have
//     made it into the DB anyway. The containment check stays as
//     a paranoid last-line defense.
//
//  3. SyncAgentFromPeer is the only writer between us and the lock
//     release, and it just committed — so reading from the DB
//     reflects exactly the state we want disk to mirror.
//
// Strategy per surface:
//
//   - MEMORY.md: write if DB row is live and disk body differs;
//     remove if DB has no live row (covers nil-from-source AND
//     tombstoned-on-source).
//
//   - memory_entries: list every live row; for each, write its
//     body to the canonical disk path (skip if disk sha matches
//     to avoid unnecessary I/O). Then scan disk and remove any
//     *.md whose (kind, name) isn't in the live set (drops
//     tombstoned rows + any orphan file source no longer has).
//
// containmentCheck failures and other per-entry write errors are
// surfaced via the returned error — silently skipping a corrupt
// row would leave DB and disk inconsistent and could mask a real
// bug or attack attempt. The handler maps a non-nil return to
// HTTP 500 so the orchestrator retries.
func ReconcileAgentDiskFromDBHeld(ctx context.Context, st *store.Store, agentID string, logger *slog.Logger) error {
	if st == nil || agentID == "" {
		return nil
	}

	var firstErr error
	captureErr := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	// MEMORY.md.
	memPath := filepath.Join(agentDir(agentID), "MEMORY.md")
	mem, merr := st.GetAgentMemory(ctx, agentID)
	switch {
	case merr == nil && mem != nil && mem.DeletedAt == nil:
		// Live DB row. Only rewrite if disk content actually differs
		// (sha mismatch). This is the common case after a sync
		// where MEMORY.md didn't change; cheap to detect.
		needWrite := true
		if existing, rerr := os.ReadFile(memPath); rerr == nil {
			if store.SHA256Hex(existing) == mem.BodySHA256 {
				needWrite = false
			}
		}
		if needWrite {
			if err := os.MkdirAll(filepath.Dir(memPath), 0o755); err != nil {
				if logger != nil {
					logger.Warn("disk-reconcile: MEMORY.md mkdir failed",
						"agent", agentID, "err", err)
				}
				captureErr(err)
			} else if err := atomicfile.WriteBytes(memPath, []byte(mem.Body), 0o644); err != nil {
				if logger != nil {
					logger.Warn("disk-reconcile: MEMORY.md write failed",
						"agent", agentID, "err", err)
				}
				captureErr(err)
			}
		}
	case errors.Is(merr, store.ErrNotFound) || (merr == nil && (mem == nil || mem.DeletedAt != nil)):
		// No live DB row → mirror by removing disk file.
		if err := os.Remove(memPath); err != nil && !os.IsNotExist(err) {
			if logger != nil {
				logger.Warn("disk-reconcile: MEMORY.md remove failed",
					"agent", agentID, "err", err)
			}
			captureErr(err)
		}
	case merr != nil:
		if logger != nil {
			logger.Warn("disk-reconcile: GetAgentMemory failed",
				"agent", agentID, "err", merr)
		}
		captureErr(merr)
	}

	// memory_entries.
	//
	// expectedComplete tracks whether `expectedPaths` reliably
	// represents every disk path the DB-canonical layout demands.
	// The orphan-remove phase keys off this set, so any error that
	// leaves the set partial (DB list failure, containment escape,
	// validation failure) MUST skip the remove phase — otherwise
	// we'd delete legitimate disk files whose DB rows simply didn't
	// make it into the set.
	//
	// Keying by FULL canonical path (instead of just (kind, name))
	// lets us catch the alias-duplicate case: when both
	// `memory/zzz.md` and `memory/topics/zzz.md` exist, the scanner
	// classifies BOTH as kind=topic, name=zzz, but only
	// memory/topics/zzz.md is the v1 canonical layout. The legacy
	// flat-form file at memory/zzz.md must be removed so the next
	// prepareChat disk→DB sync doesn't oscillate between the two
	// bodies depending on scan order.
	expectedPaths := make(map[string]bool)
	expectedComplete := true

	const pageSize = 500
	var cursor int64
listLoop:
	for {
		recs, lerr := st.ListMemoryEntries(ctx, agentID, store.MemoryEntryListOptions{
			Limit:  pageSize,
			Cursor: cursor,
		})
		if lerr != nil {
			if logger != nil {
				logger.Warn("disk-reconcile: ListMemoryEntries failed; skipping orphan remove",
					"agent", agentID, "err", lerr)
			}
			captureErr(lerr)
			expectedComplete = false
			break listLoop
		}
		if len(recs) == 0 {
			break
		}
		for _, rec := range recs {
			cursor = rec.Seq
			if rec.DeletedAt != nil {
				// Tombstoned rows are filtered out of ListMemoryEntries
				// by default, but guard defensively in case the option
				// shape changes.
				continue
			}
			if rec.Kind == "" || rec.Name == "" {
				// A DB row with empty kind/name is a corruption
				// signal — surface so the operator notices. Mark
				// expectedComplete=false because we can't represent
				// this row in the orphan-key set, so the remove
				// phase has to stay home or it might wipe whatever
				// disk path the corruption pointed at.
				err := fmt.Errorf("memory_entries id=%s has empty kind/name", rec.ID)
				if logger != nil {
					logger.Warn("disk-reconcile: empty kind/name", "agent", agentID, "id", rec.ID)
				}
				captureErr(err)
				expectedComplete = false
				continue
			}
			path := memoryEntryDiskPath(agentID, rec.Kind, rec.Name)
			if err := containmentCheck(agentID, path, rec.Kind, rec.Name); err != nil {
				// Hostile / corrupt name escaped the schema CHECK.
				// Refuse to write — surface as the handler's 500 so
				// the operator sees something is wrong. Skip orphan
				// remove for the same incomplete-set reason.
				if logger != nil {
					logger.Warn("disk-reconcile: containment check failed",
						"agent", agentID, "id", rec.ID,
						"kind", rec.Kind, "name", rec.Name, "err", err)
				}
				captureErr(err)
				expectedComplete = false
				continue
			}
			if err := validateMemoryEntryRoundTrip(agentID, rec.Kind, rec.Name); err != nil {
				// Cross-kind / intra-kind alias: (kind, name)
				// resolves to a disk path that classifies back to
				// a DIFFERENT (kind, name). Writing this row would
				// shadow whatever other row owns that path. Refuse
				// + surface so the operator can dedupe in DB.
				if logger != nil {
					logger.Warn("disk-reconcile: round-trip mismatch",
						"agent", agentID, "id", rec.ID,
						"kind", rec.Kind, "name", rec.Name, "err", err)
				}
				captureErr(err)
				expectedComplete = false
				continue
			}
			expectedPaths[path] = true
			// Skip write when disk already matches DB to avoid
			// rewriting hundreds of unchanged daily entries on every
			// sync.
			if existing, rerr := os.ReadFile(path); rerr == nil {
				if store.SHA256Hex(existing) == rec.BodySHA256 {
					continue
				}
			}
			if err := hydrateMemoryEntryToDisk(agentID, rec); err != nil {
				if logger != nil {
					logger.Warn("disk-reconcile: entry hydrate failed",
						"agent", agentID, "id", rec.ID,
						"kind", rec.Kind, "name", rec.Name, "err", err)
				}
				captureErr(err)
				// A failed hydrate also leaves the entry's expected
				// disk state ambiguous; safer to skip orphan remove.
				expectedComplete = false
			}
		}
		if len(recs) < pageSize {
			break
		}
	}

	// Orphan-remove phase. Scan disk; remove any *.md whose
	// (kind, name) isn't in the expected live set.
	//
	// SKIP this phase if `expected` is known to be incomplete (DB
	// list failure, validation failure, hydrate failure) — running
	// against a partial set would clobber legitimate rows whose
	// disk files we couldn't represent in `expected`. firstErr
	// already surfaces the reason to the caller.
	if !expectedComplete {
		return firstErr
	}
	disk, scanErr := scanMemoryDir(agentID, logger)
	if scanErr != nil {
		if logger != nil {
			logger.Warn("disk-reconcile: scanMemoryDir partial; skipping orphan remove",
				"agent", agentID, "err", scanErr)
		}
		captureErr(scanErr)
		return firstErr
	}
	for _, e := range disk {
		if expectedPaths[e.Path] {
			continue
		}
		if err := os.Remove(e.Path); err != nil && !os.IsNotExist(err) {
			if logger != nil {
				logger.Warn("disk-reconcile: orphan remove failed",
					"agent", agentID, "kind", e.Kind, "name", e.Name,
					"path", e.Path, "err", err)
			}
			captureErr(err)
		}
	}

	return firstErr
}

// ReconcileAgentDiskFromDB is the convenience wrapper that acquires
// memorySyncMu internally. Suitable for callers (tests, ad-hoc
// tools) that don't already hold the lock. The §3.7 agent-sync
// handler holds the lock across BOTH SyncAgentFromPeer and the
// reconcile — use ReconcileAgentDiskFromDBHeld there.
func ReconcileAgentDiskFromDB(ctx context.Context, st *store.Store, agentID string, logger *slog.Logger) error {
	if agentID == "" {
		return nil
	}
	release := lockMemorySync(agentID)
	defer release()
	return ReconcileAgentDiskFromDBHeld(ctx, st, agentID, logger)
}

// containmentCheck verifies path resolves under the agent's memory
// dir. Defends against a corrupt or hostile rec.Kind/rec.Name that
// snuck past the schema CHECK + insert validation. Used by the
// agent-sync materializer's tombstone-remove branch where we don't
// have the body-write branch's atomicfile.WriteBytes to fall back on.
func containmentCheck(agentID, path, kind, name string) error {
	memRoot := filepath.Join(agentDir(agentID), "memory")
	rel, err := filepath.Rel(memRoot, path)
	if err != nil {
		return fmt.Errorf("containment check: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("memory entry path escapes memory root: kind=%s name=%s",
			kind, name)
	}
	return nil
}

// classifyDiskRel mirrors scanMemoryDir + scanMemorySubdir's
// path→(kind, name) classification for ONE relative path. `rel` is
// the path under memory/ WITH the .md suffix. Used by
// validateMemoryEntryRoundTrip to detect cross-row alias collisions:
// two different DB rows whose canonical paths point at the same
// file. Without this, the reconciler would write both, racing on
// scan order, and disk would only retain whichever wrote last.
//
// Returns ("", "") when rel doesn't end in .md (caller should skip).
func classifyDiskRel(rel string) (kind, name string) {
	if !strings.HasSuffix(rel, ".md") {
		return "", ""
	}
	rel = strings.TrimSuffix(filepath.ToSlash(rel), ".md")
	if rel == "" {
		return "", ""
	}
	parts := strings.SplitN(rel, "/", 2)
	if len(parts) == 1 {
		// Top-level: daily if name matches YYYY-MM-DD else topic.
		if memoryDailyDateRe.MatchString(parts[0]) {
			return "daily", parts[0]
		}
		return "topic", parts[0]
	}
	// Has a subdir. Canonical subdir → that kind, name = remainder
	// (which may itself contain "/" for v0-style nested topic
	// notes). Unknown subdir → kind=topic, name keeps the subdir
	// prefix (the v0 importer's fallthrough form).
	if canonical, ok := canonicalMemoryKindDirs[parts[0]]; ok {
		return canonical, parts[1]
	}
	return "topic", parts[0] + "/" + parts[1]
}

// validateMemoryEntryRoundTrip refuses DB rows whose (kind, name)
// would write to a disk path that classifies BACK to a different
// (kind, name). Catches the cross-kind alias collision case:
//
//   - row A: (kind=topic, name="people/bob")  → memory/people/bob.md
//   - row B: (kind=people, name="bob")        → memory/people/bob.md
//
// Both rows are valid by the schema's UNIQUE(kind, name, deleted_at)
// index — they're distinct (kind, name) pairs — but they write to
// the same file. Without this guard the reconciler writes whichever
// row processed last and the next disk→DB sync resurrects only one
// of them with the wrong (kind, name).
//
// Also catches name-with-slash redirection inside a single kind:
//
//   - (topic, "topics/x") → memory/topics/x.md, same as (topic, "x")
//
// containmentCheck (which protects against ".." escapes) already
// runs separately; this guard is the next layer that asserts the
// path-to-row mapping is BIJECTIVE.
func validateMemoryEntryRoundTrip(agentID, kind, name string) error {
	path := memoryEntryDiskPath(agentID, kind, name)
	memRoot := filepath.Join(agentDir(agentID), "memory")
	rel, err := filepath.Rel(memRoot, path)
	if err != nil {
		return fmt.Errorf("round-trip rel: %w", err)
	}
	bk, bn := classifyDiskRel(rel)
	if bk != kind || bn != name {
		return fmt.Errorf("memory entry round-trip mismatch: (%s, %q) → %s → reclassifies as (%s, %q)",
			kind, name, rel, bk, bn)
	}
	return nil
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
