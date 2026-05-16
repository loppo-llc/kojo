// Package session is the runtime persistence layer for live PTY
// sessions (this binary's runtime state).
//
// State today (post-Phase-2c-2 slice 28):
//
//   - The runtime canonical store is a kv row at
//     (namespace="sessions", key="all", scope=local, type=json,
//     value=JSON([]SessionInfo)). scope=local because session state
//     is per-peer PTY metadata that is never replicated — same
//     semantic the v0 sessions.json file had.
//   - On first Load after upgrade from a pre-slice-28 v1 install or
//     from v0, Store falls back to the legacy file at
//     <configdir.Path()>/sessions.json (v1 dir) and, if absent, at
//     <configdir.V0Path()>/sessions.json (the v0 dir — tmux session
//     names live there, attach is still wire-compatible because
//     `kojo_<id>` naming and `tmux attach-session` semantics did not
//     change between v0 and v1). The selected file's contents are
//     mirrored into kv via an IfMatchAny PutKV (collision-safe vs
//     a same-peer concurrent writer; scope=local means the row is
//     never replicated, so the only race here is inside the same
//     kojo process tree). After a successful mirror the v1-dir
//     legacy file is best-effort unlinked; the v0-dir legacy file
//     is NEVER unlinked from here — that is `kojo --clean v0`'s
//     job, and leaving it in place lets a roll-back to v0 still
//     find its own sessions.json.
//     The pattern matches LoadCronPaused / loadOrCreateOwner from
//     internal/agent and internal/auth — a stray legacy file from
//     a v1→v0→v1 round trip is treated as non-authoritative once
//     kv has the row, but only after the row passes the row-shape
//     gate (Type=json, Scope=local, Secret=false) and JSON parses.
//     A malformed kv row leaves the legacy file in place so the
//     operator can repair (delete the row) and let the next Load
//     re-mirror cleanly.
//   - The Phase 2c-1 sessions importer
//     (internal/migrate/importers/sessions.go) writes rows into the
//     DB `sessions` table with status forced to 'archived' (per
//     design §5.5). Those archived rows live in a different storage
//     site than the runtime kv row and are NOT consulted by this
//     Store; they exist so a future cutover that promotes the DB
//     `sessions` table to the runtime canonical site can replay
//     them. Reconciling the status vocabulary mismatch (runtime
//     "exited" vs DB CHECK 'stopped'/'archived') is also deferred
//     to that future cutover; the kv row preserves the runtime
//     vocabulary as-is.
//
// Implication for legacy file cleanup: a stray
// <configdir.Path()>/sessions.json after slice 28 is a remnant of
// the legacy file → kv migration; the runtime self-heals on its
// next Load. Adding it to `kojo --clean legacy`'s sweep list is
// safe but unnecessary — see cmd/kojo/clean_legacy.go's per-kind
// list, which omits it because the runtime path is already
// idempotent.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/store"
)

const (
	sessionsFile = "sessions.json"
	maxAge       = 7 * 24 * time.Hour

	// kv coordinates. namespace="sessions", key="all" mirrors the
	// "single JSON blob holding the whole list" shape v0 used; the
	// list is small (cap maxAge=7d worth of sessions) so an O(1)-row
	// upsert keeps the transactional cost on par with the file
	// rewrite the previous implementation paid.
	sessionsKVNamespace = "sessions"
	sessionsKVKey       = "all"

	// Per-write timeout. Generous because a slow disk on the kv put
	// is the same posture as the previous atomicfile.WriteJSON.
	sessionsKVTimeout = 10 * time.Second
)

// Store persists session metadata as a single JSON-typed kv row.
//
// See the package doc comment for the post-slice-28 state: kv is
// canonical, the legacy <configdir.Path()>/sessions.json file is
// mirrored into kv on first Load and best-effort unlinked. db may
// be nil — that path keeps Save/Load as no-ops, which is the test
// posture (the session_test.go suite constructs Sessions directly
// without going through NewManager).
type Store struct {
	mu sync.Mutex
	db *store.Store
	// legacyPath points at the v1 dir's sessions.json. Loaded as a
	// fallback when the kv row is absent / unreadable; unlinked
	// once the kv row is canonical. This is the path the runtime
	// owns end-to-end.
	legacyPath string
	// v0LegacyPath points at the v0 dir's sessions.json (the
	// pre-`kojo-v1` install). Loaded only when the v1 dir has no
	// usable legacy file AND v0 fallback is enabled by the caller
	// (see allowV0Fallback). Never unlinked from here: rollback to
	// v0 has to still find its data, and `kojo --clean v0` is the
	// only sanctioned remover. Empty string disables v0 fallback
	// entirely (e.g. when the runtime opted out via --fresh).
	v0LegacyPath string
	logger       *slog.Logger
}

// newStore constructs the kv-backed session store. v0LegacyDir is the
// v0 config directory (configdir.V0Path()) or "" to disable v0
// fallback. The runtime path always supplies a non-empty value when
// the startup gate has confirmed migration completed (v1Complete);
// --fresh / pure new-install paths supply "" so v0 data is never
// reattached for a deployment that explicitly opted out of migration.
func newStore(logger *slog.Logger, db *store.Store, v0LegacyDir string) *Store {
	st := &Store{
		db:         db,
		legacyPath: filepath.Join(configdir.Path(), sessionsFile),
		logger:     logger,
	}
	if v0LegacyDir != "" {
		st.v0LegacyPath = filepath.Join(v0LegacyDir, sessionsFile)
	}
	return st
}

// Save upserts the kv row holding all sessions. Errors are logged
// (this matches the pre-slice-28 contract — Save returned no error
// either; the live runtime cannot meaningfully react to a persistence
// failure mid-tick) but never propagated.
func (st *Store) Save(infos []SessionInfo) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.db == nil {
		return
	}
	body, err := json.Marshal(infos)
	if err != nil {
		st.logger.Warn("failed to marshal sessions", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionsKVTimeout)
	defer cancel()
	rec := &store.KVRecord{
		Namespace: sessionsKVNamespace,
		Key:       sessionsKVKey,
		Value:     string(body),
		Type:      store.KVTypeJSON,
		Scope:     store.KVScopeLocal,
	}
	if _, err := st.db.PutKV(ctx, rec, store.KVPutOptions{}); err != nil {
		st.logger.Warn("failed to save sessions to kv", "err", err)
	}
}

// sessionsKVCollisionTestHook fires inside Load AFTER the kv miss
// branch has parsed the legacy file but BEFORE the gated IfMatchAny
// PutKV runs. Tests use it to land a concurrent winner row so the
// ErrETagMismatch branch is exercised (the same approach
// cronLockKVCASTestHook takes for acquireCronLockDB). Production
// builds leave it nil; calling site is no-op when nil.
var sessionsKVCollisionTestHook func()

// Load reads persisted sessions, filtering out entries older than maxAge.
// Returns (nil, nil) when the kv row is absent and no legacy file
// exists (first run on a fresh install). Returns (nil, err) on
// read/parse errors so callers can distinguish "no sessions" from
// "failed to load" (important for orphan cleanup).
//
// Branches:
//   - valid kv hit (Type=json, Scope=local, Secret=false, JSON
//     parses): return kv contents, opportunistically unlink any
//     stray legacy file from a v1→v0→v1 round trip.
//   - malformed kv hit (row-shape gate fails OR JSON parse fails):
//     leave the legacy file in place so the operator can repair
//     (delete the bad row); return legacy-parsed contents if a
//     legacy file exists, or errMalformedKVNoLegacy if it does
//     not (the latter prevents the caller's orphan cleanup from
//     wiping live sessions on a (nil, nil) misread).
//   - kv miss + legacy file present: parse legacy file, mirror
//     into kv via IfMatchAny PutKV; on success the legacy file is
//     unlinked. On collision (a same-peer concurrent writer
//     inserted the row first) re-read the winner and unlink only
//     when the winner row also passes the row-shape + parse gate.
//   - kv miss + no legacy file: fresh install, return (nil, nil).
func (st *Store) Load() ([]SessionInfo, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.db == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionsKVTimeout)
	defer cancel()

	rec, err := st.db.GetKV(ctx, sessionsKVNamespace, sessionsKVKey)
	switch {
	case err == nil:
		// kv hit — but validate row shape and parse before treating
		// it as canonical. A malformed row (wrong type/scope/secret
		// flag, unparseable JSON) means SOMEONE wrote junk we don't
		// recognize. The session-specific posture is row-shape +
		// parse-before-unlink with legacy fallback: do NOT unlink
		// the legacy file in that case so the operator can repair
		// (delete the row) and let the next Load fall through to
		// the legacy migration branch. (loadOrCreateOwner / cron
		// don't need this kind of branch because their kv values
		// are scalars and they can fail closed; sessions hold a
		// list, so we need a graceful fallback to avoid losing
		// live state on a corrupted row.)
		if !validSessionsRow(rec) {
			st.logger.Warn("sessions kv row malformed, ignoring (legacy file kept for repair)",
				"type", rec.Type, "scope", rec.Scope, "secret", rec.Secret)
			return st.loadLegacyOnly()
		}
		infos, perr := st.parseAndFilter([]byte(rec.Value))
		if perr != nil {
			// parse failed — same posture as malformed row.
			return st.loadLegacyOnly()
		}
		// kv row is good — drop a stray legacy file if one survived
		// a v1 → v0 → v1 round trip.
		st.removeLegacyIfPresent()
		return infos, nil
	case errors.Is(err, store.ErrNotFound):
		// kv miss — fall through to legacy migration.
	default:
		st.logger.Warn("failed to read sessions from kv", "err", err)
		return nil, err
	}

	// kv miss. Check legacy file.
	//
	// Search order: v1-dir sessions.json, then v0-dir sessions.json.
	// The v0 fallback exists so a freshly-upgraded host (kojo binary
	// flipped from v0 → v1 with both binaries' configdirs side by
	// side) picks up the tmux session names v0 last persisted; the
	// `kojo_<id>` tmux naming and the attach command shape are wire-
	// compatible across the v0 → v1 cutover (see internal/session/
	// tmux.go:15 and tmuxAttachCommand), so the live tmux server's
	// panes can be re-attached without further translation. v0 dir
	// reads are intentionally read-only: never unlinked, never
	// overwritten — `kojo --clean v0` owns v0-dir lifecycle and a
	// rollback to v0 has to still find its own data.
	data, sourcePath, ferr := st.readLegacy()
	if ferr != nil {
		if os.IsNotExist(ferr) {
			return nil, nil
		}
		st.logger.Warn("failed to read legacy sessions file", "err", ferr)
		return nil, ferr
	}
	infos, perr := st.parseAndFilter(data)
	if perr != nil {
		return nil, perr
	}
	if sourcePath == st.v0LegacyPath {
		st.logger.Info("session: bootstrapped from v0 legacy sessions.json",
			"path", sourcePath, "count", len(infos))
	}

	if sessionsKVCollisionTestHook != nil {
		sessionsKVCollisionTestHook()
	}

	// Mirror into kv. IfMatchAny so a same-peer concurrent writer
	// that inserted the row between our GetKV miss and this PutKV
	// loses gracefully via ErrETagMismatch; we then re-read the
	// winner. (scope=local means the row never replicates across
	// peers, so the only writer race here is inside the same kojo
	// process tree.)
	//
	// Collision sub-cases:
	//   - winner row passes shape gate + JSON parses: return the
	//     parsed winner (canonical) and unlink legacy.
	//   - winner row malformed: keep legacy on disk for repair,
	//     return our legacy-parsed view (the next Save will
	//     overwrite the malformed row with fresh in-memory state).
	//   - GetKV against the winner errors out: return that error
	//     so orphan cleanup is skipped — without knowing what the
	//     canonical winner contains we cannot safely let Save
	//     overwrite it later.
	body, merr := json.Marshal(infos)
	if merr != nil {
		st.logger.Warn("failed to marshal legacy sessions for kv mirror", "err", merr)
		return infos, nil
	}
	mig := &store.KVRecord{
		Namespace: sessionsKVNamespace,
		Key:       sessionsKVKey,
		Value:     string(body),
		Type:      store.KVTypeJSON,
		Scope:     store.KVScopeLocal,
	}
	_, putErr := st.db.PutKV(ctx, mig, store.KVPutOptions{IfMatchETag: store.IfMatchAny})
	switch {
	case putErr == nil:
		// We landed the mirror. The legacy file is no longer
		// authoritative.
		st.removeLegacyIfPresent()
	case errors.Is(putErr, store.ErrETagMismatch):
		// Concurrent writer beat us. Re-read the winner row and,
		// when it passes the row-shape + parse gate, return the
		// PARSED WINNER as the canonical view — not our legacy
		// snapshot. Otherwise the next Save() would overwrite the
		// fresher winner with our stale legacy view. If the winner
		// is malformed/unparseable, the legacy file is the best
		// available data; keep the file around for repair.
		win, gerr := st.db.GetKV(ctx, sessionsKVNamespace, sessionsKVKey)
		if gerr != nil {
			// Canonical state is unknown — return error so the
			// caller skips orphan cleanup. A clean (legacy, nil)
			// return here would let the next Save overwrite the
			// (possibly fresher) winner with our stale view.
			st.logger.Warn("post-collision GetKV failed (legacy file kept for retry)", "err", gerr)
			return nil, gerr
		}
		if !validSessionsRow(win) {
			st.logger.Warn("post-collision sessions kv row malformed (legacy file kept for repair)",
				"type", win.Type, "scope", win.Scope, "secret", win.Secret)
			return infos, nil
		}
		winnerInfos, perr := st.parseAndFilter([]byte(win.Value))
		if perr != nil {
			st.logger.Warn("post-collision sessions kv row unparseable (legacy file kept for repair)", "err", perr)
			return infos, nil
		}
		st.removeLegacyIfPresent()
		return winnerInfos, nil
	default:
		st.logger.Warn("failed to mirror legacy sessions into kv (legacy file kept for retry)",
			"err", putErr)
	}
	return infos, nil
}

// validSessionsRow gates a kv hit: only Type=json + Scope=local +
// Secret=false rows count as canonical. A row violating any of these
// is treated as malformed (operator-written or migrated junk) and
// the runtime falls back to the legacy file rather than blindly
// trusting the row.
func validSessionsRow(rec *store.KVRecord) bool {
	if rec == nil {
		return false
	}
	return rec.Type == store.KVTypeJSON && rec.Scope == store.KVScopeLocal && !rec.Secret
}

// errMalformedKVNoLegacy distinguishes "kv exists but unreadable
// and no legacy file to fall back on" from "no sessions at all".
// Caller (loadPersistedSessions) treats a non-nil error as
// "skip orphan cleanup" so we never wipe the live tmux pane set
// because the kv row got corrupted.
var errMalformedKVNoLegacy = errors.New("session: kv row malformed and no legacy file to fall back on")

// loadLegacyOnly is the malformed-kv-row fallback: parse the legacy
// file directly without writing to kv (the malformed row is still
// in the way) and without unlinking it (the operator needs the
// data to recover). Returns errMalformedKVNoLegacy if no legacy
// file exists — a clean (nil, nil) here would let the caller's
// orphan cleanup wipe live sessions on the assumption that the
// store is empty, when in reality the data is just unreachable.
func (st *Store) loadLegacyOnly() ([]SessionInfo, error) {
	data, sourcePath, err := st.readLegacy()
	if err != nil {
		if os.IsNotExist(err) {
			st.logger.Error("sessions kv row malformed and no legacy file to fall back on; skipping orphan cleanup",
				"v1_path", st.legacyPath, "v0_path", st.v0LegacyPath)
			return nil, errMalformedKVNoLegacy
		}
		st.logger.Warn("failed to read legacy sessions file (kv malformed, legacy unreadable)", "err", err)
		return nil, err
	}
	infos, perr := st.parseAndFilter(data)
	if perr != nil {
		return nil, perr
	}
	if sourcePath == st.v0LegacyPath {
		st.logger.Info("session: kv row malformed, recovered from v0 legacy sessions.json",
			"path", sourcePath, "count", len(infos))
	}
	return infos, nil
}

// readLegacy returns the contents of whichever sessions.json file we
// should treat as authoritative when the kv row is missing or
// malformed. It tries the v1 dir first; on ENOENT it falls through
// to the v0 dir. Any non-ENOENT read error from the v1 path is
// returned as-is — we do NOT silently fall through to v0 because a
// permissions error or transient I/O failure on v1 should fail
// closed rather than promote v0 data over a (possibly recoverable)
// v1 file.
//
// The second return value is the path that supplied the data, so
// the caller can log "loaded from v0 dir" and adjust its own
// post-load actions (e.g. only the v1 path is eligible for
// post-mirror unlink).
func (st *Store) readLegacy() ([]byte, string, error) {
	data, err := os.ReadFile(st.legacyPath)
	if err == nil {
		return data, st.legacyPath, nil
	}
	if !os.IsNotExist(err) {
		return nil, "", err
	}
	// v1 file does not exist — try v0.
	if st.v0LegacyPath == "" || st.v0LegacyPath == st.legacyPath {
		// Pathological config (override pointed v1 at the v0 dir,
		// or v0 path is empty). Don't loop on the same file.
		return nil, "", err
	}
	v0Data, v0Err := os.ReadFile(st.v0LegacyPath)
	if v0Err != nil {
		// Surface ENOENT (fresh install) and other errors verbatim
		// so the caller's switch (IsNotExist vs anything-else) works
		// uniformly across both paths.
		return nil, "", v0Err
	}
	return v0Data, st.v0LegacyPath, nil
}

// parseAndFilter is the post-Unmarshal filter shared by the kv-hit
// and legacy-file paths. Sessions whose CreatedAt fails to parse are
// dropped silently (matches the pre-slice-28 contract).
//
// The maxAge cutoff is an archival convenience: stop accumulating
// year-old "exited" rows in the on-disk blob. A session that is
// still RUNNING with a live TmuxSessionName is exempt — dropping it
// here would (a) hide the row from restoreSession so the pane never
// gets reattached, and (b) leave the still-live tmux pane out of
// `cleanupOrphanedTmuxSessions`'s known set, which then kills it as
// an orphan. The exemption matters most on v0 → v1 cutover where
// the v0 binary may have been running >7d before the upgrade.
func (st *Store) parseAndFilter(data []byte) ([]SessionInfo, error) {
	var infos []SessionInfo
	if err := json.Unmarshal(data, &infos); err != nil {
		st.logger.Warn("failed to parse sessions JSON", "err", err)
		return nil, err
	}
	cutoff := time.Now().Add(-maxAge)
	filtered := infos[:0]
	for _, info := range infos {
		t, err := time.Parse(time.RFC3339, info.CreatedAt)
		if err != nil {
			continue
		}
		if t.After(cutoff) {
			filtered = append(filtered, info)
			continue
		}
		// Age-cutoff escape hatch: keep rows that still look live,
		// regardless of how old CreatedAt is. The tmux server has no
		// concept of "this pane is stale" — only the pane's own
		// remain-on-exit state does, and that is what
		// tryReattachPersistedTmux re-checks. We must not pre-filter
		// the row out of the restore pipeline.
		if info.Status == StatusRunning && info.TmuxSessionName != "" {
			filtered = append(filtered, info)
		}
	}
	return filtered, nil
}

// removeLegacyIfPresent best-effort unlinks the v1-dir legacy
// sessions.json. ENOENT is the expected case; other errors log a
// warning but are non-fatal — the kv row is already canonical, a
// surviving legacy file is harmless cruft that the next Load will
// retry to remove.
//
// v0LegacyPath is intentionally NOT touched here: the v0 dir is
// owned end-to-end by `kojo --clean v0`, and a future rollback to
// the v0 binary has to still find its own sessions.json. Leaving
// the v0 file in place is the canonical "kv-mirror is opportunistic
// for v0 data" posture.
//
// Defense-in-depth: if a pathological config (e.g. an override that
// pointed v1's dir at the v0 dir) collapses legacyPath onto
// v0LegacyPath, refuse to unlink — even though the kv mirror is
// canonical, we never want this code path to be the surface that
// destroys v0 data.
func (st *Store) removeLegacyIfPresent() {
	if st.v0LegacyPath != "" && st.legacyPath == st.v0LegacyPath {
		st.logger.Warn("sessions: refusing to unlink legacy file: v1 path collapsed onto v0 path",
			"path", st.legacyPath)
		return
	}
	if err := os.Remove(st.legacyPath); err != nil && !os.IsNotExist(err) {
		st.logger.Warn("failed to unlink legacy sessions file after kv mirror",
			"path", st.legacyPath, "err", err)
	}
}
