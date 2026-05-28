package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/loppo-llc/kojo/internal/store"
	"golang.org/x/text/unicode/norm"
)

// MemoryEntryRecord is the public representation of a single
// memory_entries row for HTTP / Web UI consumers. Mirrors the store
// shape but doesn't leak the package boundary.
type MemoryEntryRecord struct {
	ID        string
	AgentID   string
	Seq       int64
	Kind      string
	Name      string
	Body      string
	ETag      string
	CreatedAt int64
	UpdatedAt int64
	DeletedAt *int64
}

// memoryEntryBodyCap caps individual memory entry bodies. Memory
// entries are short-form notes (notes about a person, a daily log,
// a project blurb) — the same 4 MiB ceiling we use for MEMORY.md is
// generous and consistent.
const memoryEntryBodyCap = 4 << 20

// memoryEntryNameMaxLen caps the basename portion of an entry. Most
// filesystems accept up to 255 bytes per component; we cap at 200 so
// the `.md` suffix + safety headroom keeps the full name under 255
// even on filesystems that count grapheme clusters differently from
// bytes.
const memoryEntryNameMaxLen = 200

// MemoryEntryListOptions exposes the store's pagination knobs to
// callers without leaking the store package.
type MemoryEntryListOptions struct {
	Kind   string // "" = all kinds; otherwise must be a valid kind
	Limit  int    // 0 = unbounded (caller-side; server pages at 200/500)
	Cursor int64  // seq strictly greater (keyset pagination)
}

// MemoryEntryPatch is the partial-update shape for PATCH. Pass nil
// to leave a field unchanged.
//
// Kind / Name pointers exist on the type so callers can report 400
// for "rename not supported" cleanly — UpdateAgentMemoryEntry
// rejects any non-nil Kind / Name with ErrMemoryEntryRenameUnsupported
// rather than silently dropping. Body-only patches are the supported
// path; rename = DELETE + CREATE.
type MemoryEntryPatch struct {
	Kind *string
	Name *string
	Body *string
}

// validMemoryKindForFS lists the kinds we can map to a canonical
// on-disk path. Mirrors store.validMemoryKinds but kept local so this
// file can reject ill-formed kind values BEFORE the store layer (and
// before any disk side effects).
var validMemoryKindForFS = map[string]bool{
	"daily":   true,
	"project": true,
	"topic":   true,
	"people":  true,
	"archive": true,
}

// canonicalKindSubdir maps a DB kind to its memory/ subdirectory.
// Inverse of canonicalMemoryKindDirs in memory_sync.go. "daily" goes
// flat at memory/<name>.md so the DB→disk write matches what the
// scan picks back up.
var canonicalKindSubdir = map[string]string{
	"daily":   "",
	"project": "projects",
	"topic":   "topics",
	"people":  "people",
	"archive": "archive",
}

// windowsReservedDeviceNames mirrors blob.windowsReservedBases. NT
// refuses to create files whose stem (text before first `.`) matches
// these regardless of case; reusing such a name on POSIX would later
// prevent a Windows peer from syncing the entry, breaking the
// cross-device contract.
var windowsReservedDeviceNames = func() map[string]bool {
	out := map[string]bool{
		"con": true, "prn": true, "aux": true, "nul": true,
	}
	for i := 1; i <= 9; i++ {
		out[fmt.Sprintf("com%d", i)] = true
		out[fmt.Sprintf("lpt%d", i)] = true
	}
	return out
}()

// validateMemoryEntryName enforces what a name can be when used as a
// path segment AND when DB rows are read back from the store (peer
// rows could in theory carry a malformed value). Returns
// ErrInvalidMemoryEntry on any rule violation so the handler can map
// to 400 uniformly.
//
// Rules (mirrors blob/segment hygiene where appropriate):
//   - non-empty
//   - NFC normalized (mirroring blob §4.3 — refusing to silently
//     rewrite caller input keeps logs faithful)
//   - no path separator (we control the kind→subdir mapping; the
//     name is the basename only)
//   - no `.` or `..` segment
//   - must not start with `.` so we don't shadow temp files / dotfiles
//   - no NUL byte (POSIX truncates; SQLite would store the truncated form)
//   - no `\\` (Windows separator — keep one canonical separator)
//   - no `<>:"|?*` (Win32 reserved punctuation)
//   - no C0 control chars (BS, TAB, CR, LF, …)
//   - no trailing `.` or space (Win32 silently strips → cross-OS sync hazard)
//   - stem (text before first `.`) must not match Win32 reserved
//     device names (CON, PRN, AUX, NUL, COM1–9, LPT1–9), case
//     insensitively
//   - length cap memoryEntryNameMaxLen (so name + `.md` stays under 255)
//   - daily kind: must parse as YYYY-MM-DD (time.Parse rather than
//     regex, because regex would accept 2025-13-40 etc.)
func validateMemoryEntryName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%w: name must not be empty", ErrInvalidMemoryEntry)
	}
	if !norm.NFC.IsNormalString(name) {
		return fmt.Errorf("%w: name must be NFC normalized", ErrInvalidMemoryEntry)
	}
	if len(name) > memoryEntryNameMaxLen {
		return fmt.Errorf("%w: name exceeds %d byte cap", ErrInvalidMemoryEntry, memoryEntryNameMaxLen)
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("%w: name must not contain a path separator", ErrInvalidMemoryEntry)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%w: name must not be . or ..", ErrInvalidMemoryEntry)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("%w: name must not begin with .", ErrInvalidMemoryEntry)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("%w: name must not contain NUL", ErrInvalidMemoryEntry)
	}
	if strings.ContainsAny(name, `<>:"|?*`) {
		return fmt.Errorf("%w: name must not contain Win32 reserved punctuation", ErrInvalidMemoryEntry)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7F || unicode.IsControl(r) {
			return fmt.Errorf("%w: name must not contain control characters", ErrInvalidMemoryEntry)
		}
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return fmt.Errorf("%w: name must not end with . or space", ErrInvalidMemoryEntry)
	}
	// Win32 device-name check: stem (lowercased, before first `.`)
	// matches CON / PRN / AUX / NUL / COM1–9 / LPT1–9. Reject so a
	// POSIX peer's CON.md doesn't break a Windows peer that can't
	// even open the file.
	stem := strings.ToLower(name)
	if dot := strings.Index(stem, "."); dot >= 0 {
		stem = stem[:dot]
	}
	if windowsReservedDeviceNames[stem] {
		return fmt.Errorf("%w: name stem matches Win32 reserved device", ErrInvalidMemoryEntry)
	}
	if kind == "daily" {
		if _, perr := time.Parse("2006-01-02", name); perr != nil {
			return fmt.Errorf("%w: daily kind requires YYYY-MM-DD name", ErrInvalidMemoryEntry)
		}
	}
	return nil
}

// validateMemoryEntryKind rejects kinds outside the canonical set.
func validateMemoryEntryKind(kind string) error {
	if !validMemoryKindForFS[kind] {
		return fmt.Errorf("%w: invalid kind %q", ErrInvalidMemoryEntry, kind)
	}
	return nil
}

// memoryEntryCanonicalPath returns the canonical on-disk path for a
// (kind, name) pair under the agent's memory/ tree. The caller MUST
// pre-validate kind and name; an unknown kind here is a programming
// error (panic-worthy in principle, but we return an error so
// upstream code paths log instead of crash the daemon).
//
// As a defense-in-depth final check, the resolved path must remain
// under memory/ — a corrupt name that snuck past validation can't
// escape because filepath.Join on a bare basename + .md can't add
// separators, but Rel verifies the invariant explicitly.
func memoryEntryCanonicalPath(agentID, kind, name string) (string, error) {
	sub, ok := canonicalKindSubdir[kind]
	if !ok {
		return "", fmt.Errorf("%w: invalid kind %q", ErrInvalidMemoryEntry, kind)
	}
	root := filepath.Join(agentDir(agentID), "memory")
	var out string
	if sub == "" {
		out = filepath.Join(root, name+".md")
	} else {
		out = filepath.Join(root, sub, name+".md")
	}
	rel, relErr := filepath.Rel(root, out)
	if relErr != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: name resolves outside memory dir", ErrInvalidMemoryEntry)
	}
	return out, nil
}

// findExistingEntryFiles scans the agent's memory/ tree and returns
// every on-disk path backing (kind, name). Returns the slice + any
// scan error.
//
// Returning ALL matches (rather than just the first) lets:
//   - Update refuse when the row has both a canonical and a legacy
//     file (writing canonical alone would leave the legacy orphan
//     to be resurrected by the next sync as a duplicate row).
//   - Delete remove every matching file so a tombstoned row can't
//     be revived by an orphan the cleanup missed.
//
// Callers MUST check scanErr — silently swallowing a permission
// glitch would let Delete misclassify a legacy row as
// ErrMemoryEntryStoredCorrupt and trigger a 500 that's actually
// our fault. Empty slice + nil error means "no match"; that's
// distinct from "we failed to look".
func (m *Manager) findExistingEntryFiles(agentID, kind, name string) ([]string, error) {
	disk, scanErr := scanMemoryDir(agentID, m.logger)
	var paths []string
	for _, e := range disk {
		if e.Kind == kind && e.Name == name {
			paths = append(paths, e.Path)
		}
	}
	return paths, scanErr
}

// toMemoryEntryRecord converts the store row to the public shape.
func toMemoryEntryRecord(r *store.MemoryEntryRecord) *MemoryEntryRecord {
	if r == nil {
		return nil
	}
	return &MemoryEntryRecord{
		ID:        r.ID,
		AgentID:   r.AgentID,
		Seq:       r.Seq,
		Kind:      r.Kind,
		Name:      r.Name,
		Body:      r.Body,
		ETag:      r.ETag,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
		DeletedAt: r.DeletedAt,
	}
}

// refuseIfResetting returns ErrAgentResetting if the agent is mid-
// reset. Read paths (List / Get) call this so a caller observing
// between ResetData's wipe and the post-reset DB sync doesn't see
// stale memory_entries. ResetData holds memorySyncMu through the
// whole reset → sync window, but read paths can't cheaply take that
// lock; the resetting flag is a faster gate that produces the same
// "ask again later" semantic the Web UI already handles for chat.
func (m *Manager) refuseIfResetting(agentID string) error {
	m.busyMu.Lock()
	resetting := m.resetting[agentID]
	m.busyMu.Unlock()
	if resetting {
		return ErrAgentResetting
	}
	return nil
}

// ListAgentMemoryEntries returns the live entries for agentID.
//
// Read-only path: does NOT pre-sync (file→DB). The daemon-side sync
// runs on Manager.Load + prepareChat + fork; mutation paths
// (Create/Update/Delete here) all pre-sync, so by the time a Web UI
// edits + reads back, the row is fresh. A caller observing a brief
// window between a CLI-side disk write and the next sync hook will
// see slightly stale data — acceptable for a read endpoint.
//
// Refuses with ErrAgentResetting during ResetData so the caller
// doesn't observe pre-reset DB rows after the disk wipe but before
// the post-reset sync lands.
func (m *Manager) ListAgentMemoryEntries(ctx context.Context, agentID string, opts MemoryEntryListOptions) ([]*MemoryEntryRecord, error) {
	if agentID == "" {
		return nil, fmt.Errorf("ListAgentMemoryEntries: agentID required")
	}
	if opts.Kind != "" {
		if err := validateMemoryEntryKind(opts.Kind); err != nil {
			return nil, err
		}
	}
	st := m.Store()
	if st == nil {
		return nil, errStoreNotReady
	}
	if _, ok := m.Get(agentID); !ok {
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, agentID)
	}
	if err := m.refuseIfResetting(agentID); err != nil {
		return nil, err
	}

	dbCtx, cancel := dbContextWithCancel(ctx, memoryIODBTimeout)
	defer cancel()
	storeOpts := store.MemoryEntryListOptions{
		Kind:   opts.Kind,
		Limit:  opts.Limit,
		Cursor: opts.Cursor,
	}
	rows, err := st.ListMemoryEntries(dbCtx, agentID, storeOpts)
	if err != nil {
		return nil, err
	}
	out := make([]*MemoryEntryRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, toMemoryEntryRecord(r))
	}
	return out, nil
}

// GetAgentMemoryEntry returns a single entry by id. No pre-sync —
// see ListAgentMemoryEntries for the rationale. Refuses during
// ResetData so a caller doesn't see pre-reset rows from a tombstone-
// pending DB.
func (m *Manager) GetAgentMemoryEntry(ctx context.Context, agentID, entryID string) (*MemoryEntryRecord, error) {
	if agentID == "" || entryID == "" {
		return nil, fmt.Errorf("GetAgentMemoryEntry: agentID and entryID required")
	}
	st := m.Store()
	if st == nil {
		return nil, errStoreNotReady
	}
	if _, ok := m.Get(agentID); !ok {
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, agentID)
	}
	if err := m.refuseIfResetting(agentID); err != nil {
		return nil, err
	}

	dbCtx, cancel := dbContextWithCancel(ctx, memoryIODBTimeout)
	defer cancel()
	rec, err := st.GetMemoryEntry(dbCtx, entryID)
	if err != nil {
		return nil, err
	}
	if rec.AgentID != agentID {
		// Cross-agent leak guard: an attacker who guessed an entry
		// id for another agent must not be able to read it via
		// /api/v1/agents/{their-id}/memory-entries/{stolen-id}.
		return nil, store.ErrNotFound
	}
	return toMemoryEntryRecord(rec), nil
}

// CreateAgentMemoryEntry writes body to the canonical on-disk path
// for (kind, name) and inserts the matching memory_entries row. Same
// three-layer locking as PutAgentMemory: LockPatch → editing flag →
// memory-sync gate. Pre-syncs so a duplicate (kind, name) on disk
// surfaces as a conflict rather than a silent overwrite.
func (m *Manager) CreateAgentMemoryEntry(ctx context.Context, agentID, kind, name, body string) (*MemoryEntryRecord, error) {
	if agentID == "" {
		return nil, fmt.Errorf("CreateAgentMemoryEntry: agentID required")
	}
	if err := validateMemoryEntryKind(kind); err != nil {
		return nil, err
	}
	if err := validateMemoryEntryName(kind, name); err != nil {
		return nil, err
	}
	if len(body) > memoryEntryBodyCap {
		return nil, fmt.Errorf("%w: body exceeds %d byte cap", ErrInvalidMemoryEntry, memoryEntryBodyCap)
	}
	st := m.Store()
	if st == nil {
		return nil, errStoreNotReady
	}

	releasePatch := m.LockPatch(agentID)
	defer releasePatch()
	releaseEdit, err := m.acquireMemoryEdit(agentID)
	if err != nil {
		return nil, err
	}
	defer releaseEdit()
	releaseSync := lockMemorySync(agentID)
	defer releaseSync()

	dbCtx, cancel := dbContextWithCancel(ctx, memoryIODBTimeout)
	defer cancel()

	// Pre-sync so a CLI-side file already at this (kind, name)
	// surfaces as a row before our INSERT — otherwise we'd ENOENT
	// on stat, INSERT, and then sync would notice both files (ours +
	// the pre-existing one) and resurrect a duplicate.
	if err := syncMemoryEntriesToDB(dbCtx, st, agentID, m.logger); err != nil {
		return nil, fmt.Errorf("CreateAgentMemoryEntry: pre-sync: %w", err)
	}

	// Refuse if a live row already exists under (kind, name).
	if existing, err := st.FindMemoryEntryByName(dbCtx, agentID, kind, name); err == nil && existing != nil {
		return nil, fmt.Errorf("%w: %s/%s", ErrMemoryEntryExists, kind, name)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("CreateAgentMemoryEntry: dedup check: %w", err)
	}

	path, err := memoryEntryCanonicalPath(agentID, kind, name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("CreateAgentMemoryEntry: ensure dir: %w", err)
	}
	// Refuse if the canonical file already exists on disk — would
	// mean the DB lookup above missed something the next sync will
	// pick up. Better to surface a conflict than silently overwrite.
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%w: %s/%s", ErrMemoryEntryExists, kind, name)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("CreateAgentMemoryEntry: stat existing: %w", err)
	}
	if err := writeFileAtomic(path, []byte(body)); err != nil {
		return nil, fmt.Errorf("CreateAgentMemoryEntry: write file: %w", err)
	}

	rec, err := st.InsertMemoryEntry(dbCtx, &store.MemoryEntryRecord{
		ID:      newMemoryEntryID(),
		AgentID: agentID,
		Kind:    kind,
		Name:    name,
		Body:    body,
	}, store.MemoryEntryInsertOptions{})
	if err != nil {
		// Rollback the file write so disk and DB stay in sync.
		_ = os.Remove(path)
		return nil, fmt.Errorf("CreateAgentMemoryEntry: insert: %w", err)
	}
	return toMemoryEntryRecord(rec), nil
}

// UpdateAgentMemoryEntry applies a body-only patch to the entry. If
// the patch attempts to change kind or name, we refuse with
// ErrMemoryEntryRenameUnsupported — making rename crash-safe against
// disk + DB without an intent-file protocol is genuinely hard, and
// callers that need rename can DELETE + CREATE.
//
// If-Match is enforced against the freshly-synced view, and again
// inside the store TX (defense in depth).
func (m *Manager) UpdateAgentMemoryEntry(ctx context.Context, agentID, entryID, ifMatchETag string, patch MemoryEntryPatch) (*MemoryEntryRecord, error) {
	if agentID == "" || entryID == "" {
		return nil, fmt.Errorf("UpdateAgentMemoryEntry: agentID and entryID required")
	}
	if patch.Kind != nil || patch.Name != nil {
		return nil, ErrMemoryEntryRenameUnsupported
	}
	if patch.Body == nil {
		// Body-only patch with nil body is a no-op; rather than
		// silently churn the row's etag, surface as 400. The
		// store's all-nil patch returns the row unchanged but we
		// don't want callers to rely on that semantic via this
		// endpoint.
		return nil, fmt.Errorf("%w: PATCH body must include `body` field", ErrInvalidMemoryEntry)
	}
	if len(*patch.Body) > memoryEntryBodyCap {
		return nil, fmt.Errorf("%w: body exceeds %d byte cap", ErrInvalidMemoryEntry, memoryEntryBodyCap)
	}
	st := m.Store()
	if st == nil {
		return nil, errStoreNotReady
	}

	releasePatch := m.LockPatch(agentID)
	defer releasePatch()
	releaseEdit, err := m.acquireMemoryEdit(agentID)
	if err != nil {
		return nil, err
	}
	defer releaseEdit()
	releaseSync := lockMemorySync(agentID)
	defer releaseSync()

	dbCtx, cancel := dbContextWithCancel(ctx, memoryIODBTimeout)
	defer cancel()

	if err := syncMemoryEntriesToDB(dbCtx, st, agentID, m.logger); err != nil {
		return nil, fmt.Errorf("UpdateAgentMemoryEntry: pre-sync: %w", err)
	}

	cur, err := st.GetMemoryEntry(dbCtx, entryID)
	if err != nil {
		// Conditional update on missing row → 412 to force refetch.
		// Unconditional missing → ErrNotFound (the caller is doing
		// a non-idempotent body update, not idempotent delete; 404
		// is the right answer).
		if errors.Is(err, store.ErrNotFound) && ifMatchETag != "" {
			return nil, store.ErrETagMismatch
		}
		return nil, err
	}
	if cur.AgentID != agentID {
		// Cross-agent: same probe-asymmetry concern as DELETE. Map
		// to ErrNotFound for unconditional callers (entry doesn't
		// exist for THIS agent) and ErrETagMismatch for conditional
		// (caller can't reconcile from a phantom etag).
		if ifMatchETag != "" {
			return nil, store.ErrETagMismatch
		}
		return nil, store.ErrNotFound
	}
	if ifMatchETag != "" && cur.ETag != ifMatchETag {
		return nil, store.ErrETagMismatch
	}

	// Validate cur.Kind/Name explicitly before any path math. A
	// stored name with `/`, control chars, Win32 reserved punctuation
	// or device-name stem (CON / NUL / COM1) is corrupt or legacy —
	// memoryEntryCanonicalPath wouldn't always catch these (the
	// resolver's escape check passes for `CON.md`).
	storedKindOK := validateMemoryEntryKind(cur.Kind) == nil
	storedNameOK := storedKindOK && validateMemoryEntryName(cur.Kind, cur.Name) == nil

	// Compute canonical path when name is valid. canonicalErr non-nil
	// means the row's stored name is corrupt enough that we can't
	// even mint a path for it; the legacy-detection branch below
	// still catches those if the row is reachable via scan.
	var canonicalPath string
	var canonicalErr error
	if storedNameOK {
		canonicalPath, canonicalErr = memoryEntryCanonicalPath(agentID, cur.Kind, cur.Name)
	} else {
		canonicalErr = fmt.Errorf("stored name failed validation")
	}

	// ALWAYS scan for backing files. The earlier "Stat canonical →
	// skip scan" optimization could miss a canonical+legacy
	// duplicate pair that pre-sync's upsert phase converges by
	// arbitrary walk order: the row body would land matching
	// whichever file sync read last, but the orphan file remains
	// on disk and the next sync's upsert would overwrite the row
	// body again, silently reverting any PATCH we just made. The
	// only reliable signal is "every file backing this (kind, name)
	// is at the canonical path". scanMemoryDir runs in O(N) on the
	// memory/ tree — pre-sync already paid that cost; one extra
	// walk in Update is the price of correctness.
	existingPaths, scanErr := m.findExistingEntryFiles(agentID, cur.Kind, cur.Name)
	if scanErr != nil {
		return nil, fmt.Errorf("UpdateAgentMemoryEntry: scan disk: %w", scanErr)
	}

	switch {
	case len(existingPaths) == 0 && canonicalErr != nil:
		// No backing file AND no canonical mapping → row is
		// genuinely corrupt. Server-side bad data (500).
		return nil, fmt.Errorf("%w: kind=%q name=%q has no resolvable file",
			ErrMemoryEntryStoredCorrupt, cur.Kind, cur.Name)
	case len(existingPaths) == 0 && canonicalErr == nil:
		// No backing file, valid name → freshly-created row whose
		// CLI-side file vanished. Write canonicalPath; rollback on
		// DB failure must REMOVE the file (not restore content)
		// since none existed prior.
		return m.finishUpdateMemoryEntry(dbCtx, st, cur, entryID, ifMatchETag,
			*patch.Body, canonicalPath, "" /* no prior body */, false /* hadFile */, agentID)
	case len(existingPaths) > 0 && canonicalErr != nil:
		// Legacy row reachable on disk but name is corrupt → force
		// DELETE + CREATE to migrate to the canonical layout.
		return nil, fmt.Errorf("%w: legacy entry layout requires DELETE + CREATE", ErrMemoryEntryNotCanonical)
	case len(existingPaths) > 0 && canonicalErr == nil:
		// Refuse the PATCH if any of the matching files lives at a
		// non-canonical path. Writing canonical alone would leave
		// orphans; rewriting all of them would be silent legacy
		// preservation. Force DELETE + CREATE.
		for _, p := range existingPaths {
			if p != canonicalPath {
				return nil, fmt.Errorf("%w: legacy entry layout requires DELETE + CREATE", ErrMemoryEntryNotCanonical)
			}
		}
		// All matches are at canonicalPath. Capture the file's
		// pre-write body so a DB-update failure can restore it
		// rather than overwriting with cur.Body (the SQL row's
		// view, which could differ from the CLI-edited disk
		// content).
		//
		// Branch on read result:
		//   - success: hadFile=true, priorBody=disk content
		//   - ENOENT: file vanished between scan and ReadFile
		//     (concurrent CLI delete). Treat as hadFile=false so
		//     rollback removes our write rather than restoring
		//     stale content.
		//   - other error: abort BEFORE the disk write — we'd
		//     otherwise commit to a state we can't roll back to
		//     (a permission glitch on read could mean the same
		//     glitch on write, or could mean fs corruption; either
		//     way the caller should retry rather than land a
		//     half-applied PATCH).
		existing, readErr := os.ReadFile(canonicalPath)
		switch {
		case readErr == nil:
			return m.finishUpdateMemoryEntry(dbCtx, st, cur, entryID, ifMatchETag,
				*patch.Body, canonicalPath, string(existing), true /* hadFile */, agentID)
		case errors.Is(readErr, fs.ErrNotExist):
			return m.finishUpdateMemoryEntry(dbCtx, st, cur, entryID, ifMatchETag,
				*patch.Body, canonicalPath, "" /* no prior body */, false /* hadFile */, agentID)
		default:
			return nil, fmt.Errorf("UpdateAgentMemoryEntry: pre-write read: %w", readErr)
		}
	}
	// Unreachable. Belt-and-suspenders to satisfy the compiler.
	return nil, fmt.Errorf("UpdateAgentMemoryEntry: unreachable")
}

// finishUpdateMemoryEntry performs the disk write + DB update +
// rollback-on-failure for UpdateAgentMemoryEntry. priorBody +
// hadFile drive the rollback strategy:
//   - hadFile == true: rollback restores the file with priorBody.
//   - hadFile == false: rollback REMOVES the file we just wrote
//     (the row had no backing file before the request, so a
//     restore-via-write would resurrect content the failed PATCH
//     was supposed to leave alone).
func (m *Manager) finishUpdateMemoryEntry(dbCtx context.Context, st *store.Store, cur *store.MemoryEntryRecord, entryID, ifMatchETag, body, path, priorBody string, hadFile bool, agentID string) (*MemoryEntryRecord, error) {

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("UpdateAgentMemoryEntry: ensure dir: %w", err)
	}
	if err := writeFileAtomic(path, []byte(body)); err != nil {
		return nil, fmt.Errorf("UpdateAgentMemoryEntry: write file: %w", err)
	}

	storePatch := store.MemoryEntryPatch{Body: &body}
	rec, err := st.UpdateMemoryEntry(dbCtx, entryID, ifMatchETag, storePatch)
	if err != nil {
		// DB update failed AFTER the file write landed. Best-effort
		// rollback so the next disk→DB sync doesn't quietly absorb
		// the half-applied change. Strategy depends on hadFile:
		//   - hadFile: restore priorBody (the disk content we
		//     captured before the write). cur.Body is the SQL row
		//     view which could differ from what was on disk if a
		//     CLI edit landed between sync and our write.
		//   - !hadFile: remove the file we just wrote — the prior
		//     state was "no file", restoring would resurrect
		//     content that wasn't there.
		var rbErr error
		if hadFile {
			rbErr = writeFileAtomic(path, []byte(priorBody))
		} else {
			if remErr := os.Remove(path); remErr != nil && !errors.Is(remErr, fs.ErrNotExist) {
				rbErr = remErr
			}
		}
		if rbErr != nil && m.logger != nil {
			m.logger.Warn("UpdateAgentMemoryEntry: file rollback failed",
				"agent", agentID, "entry", entryID, "path", path,
				"hadFile", hadFile, "err", rbErr)
		}
		return nil, fmt.Errorf("UpdateAgentMemoryEntry: update: %w", err)
	}
	return toMemoryEntryRecord(rec), nil
}

// DeleteAgentMemoryEntry removes the on-disk file and tombstones
// the row. Idempotent when ifMatchETag is empty (a missing row
// returns nil); conditional with non-empty ifMatchETag surfaces
// ErrETagMismatch on missing/tombstoned rows so the caller can
// refetch.
func (m *Manager) DeleteAgentMemoryEntry(ctx context.Context, agentID, entryID, ifMatchETag string) error {
	if agentID == "" || entryID == "" {
		return fmt.Errorf("DeleteAgentMemoryEntry: agentID and entryID required")
	}
	st := m.Store()
	if st == nil {
		return errStoreNotReady
	}

	releasePatch := m.LockPatch(agentID)
	defer releasePatch()
	releaseEdit, err := m.acquireMemoryEdit(agentID)
	if err != nil {
		return err
	}
	defer releaseEdit()
	releaseSync := lockMemorySync(agentID)
	defer releaseSync()

	dbCtx, cancel := dbContextWithCancel(ctx, memoryIODBTimeout)
	defer cancel()

	if err := syncMemoryEntriesToDB(dbCtx, st, agentID, m.logger); err != nil {
		return fmt.Errorf("DeleteAgentMemoryEntry: pre-sync: %w", err)
	}

	cur, err := st.GetMemoryEntry(dbCtx, entryID)
	if err != nil {
		// Conditional delete on missing row → 412 to force refetch.
		// Unconditional delete on missing row is idempotent.
		if errors.Is(err, store.ErrNotFound) {
			if ifMatchETag != "" {
				return store.ErrETagMismatch
			}
			return nil
		}
		return err
	}
	if cur.AgentID != agentID {
		// Cross-agent: NEVER reveal the row exists. The probe
		// asymmetry (missing-id → 204, other-agent-id → 404)
		// would otherwise let an attacker enumerate entry ids
		// across agents. Return the same outcome as missing-id:
		// idempotent nil for unconditional, etag-mismatch for
		// conditional.
		if ifMatchETag != "" {
			return store.ErrETagMismatch
		}
		return nil
	}
	if ifMatchETag != "" && cur.ETag != ifMatchETag {
		return store.ErrETagMismatch
	}

	// Resolve all on-disk files backing this row. scanMemoryDir's
	// returned paths are filesystem-derived (walks memory/) so
	// they're inherently confined to the agent's tree even when
	// cur.Name is malformed. Returning ALL matches lets us delete
	// canonical + legacy duplicates so the next sync's upsert phase
	// can't resurrect a missed orphan.
	paths, scanErr := m.findExistingEntryFiles(agentID, cur.Kind, cur.Name)
	if scanErr != nil {
		return fmt.Errorf("DeleteAgentMemoryEntry: scan disk: %w", scanErr)
	}

	// If scan didn't find a file but the row's kind/name validates,
	// the canonical path is the safe fallback (the file may have
	// been deleted out from under us by a CLI process — Remove
	// below will be a no-op, but the row still tombstones cleanly).
	if len(paths) == 0 && validateMemoryEntryKind(cur.Kind) == nil && validateMemoryEntryName(cur.Kind, cur.Name) == nil {
		if p, perr := memoryEntryCanonicalPath(agentID, cur.Kind, cur.Name); perr == nil {
			paths = append(paths, p)
		}
	}
	// If we STILL don't have a path AND the row's name is corrupt,
	// the row is a stored-corruption case — refuse to tombstone
	// rather than leave an unreachable orphan that the next sync
	// would resurrect. Operator must clean up the row manually.
	if len(paths) == 0 && (validateMemoryEntryKind(cur.Kind) != nil || validateMemoryEntryName(cur.Kind, cur.Name) != nil) {
		return fmt.Errorf("%w: kind=%q name=%q has no resolvable file", ErrMemoryEntryStoredCorrupt, cur.Kind, cur.Name)
	}

	// Track which files were successfully removed so we can roll
	// them back on a DB tombstone failure. Capture each file's
	// PRE-REMOVE content per path (rather than reusing cur.Body
	// for all of them) — canonical + legacy duplicates may carry
	// divergent bodies during migration, and rolling back with
	// cur.Body would silently rewrite the legacy file with the
	// canonical row's content. Read-failure falls back to cur.Body
	// for that path with a logged warning so the rollback still
	// has SOMETHING to write.
	type removed struct {
		path string
		body []byte
	}
	var removedFiles []removed
	rollback := func() {
		for _, r := range removedFiles {
			if rbErr := writeFileAtomic(r.path, r.body); rbErr != nil && m.logger != nil {
				m.logger.Warn("DeleteAgentMemoryEntry: file rollback failed",
					"agent", agentID, "entry", entryID, "path", r.path, "err", rbErr)
			}
		}
	}
	for _, p := range paths {
		// Snapshot before remove. ReadFile failure on a path that
		// Stat() reported during the scan is rare (concurrent CLI
		// edit?); fall back to cur.Body so rollback at least
		// produces something.
		body, readErr := os.ReadFile(p)
		if readErr != nil {
			if errors.Is(readErr, fs.ErrNotExist) {
				// File vanished between scan and read; nothing to
				// remove or restore. Skip.
				continue
			}
			if m.logger != nil {
				m.logger.Warn("DeleteAgentMemoryEntry: pre-remove read failed; using cur.Body for rollback",
					"agent", agentID, "entry", entryID, "path", p, "err", readErr)
			}
			body = []byte(cur.Body)
		}
		if err := os.Remove(p); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				rollback()
				return fmt.Errorf("DeleteAgentMemoryEntry: remove file: %w", err)
			}
			// Vanished between snapshot and remove. Skip rollback
			// for this path (the prior state was "doesn't exist").
			continue
		}
		removedFiles = append(removedFiles, removed{path: p, body: body})
	}

	if err := st.SoftDeleteMemoryEntry(dbCtx, entryID, ifMatchETag); err != nil {
		// Tombstone failed AFTER we removed the file(s). Best-effort
		// restore so disk + DB stay aligned with the caller-visible
		// error. The next sync would otherwise see "row live, files
		// gone" and tombstone the row, silently turning the failed
		// DELETE into a successful one.
		rollback()
		return fmt.Errorf("DeleteAgentMemoryEntry: tombstone: %w", err)
	}
	return nil
}
