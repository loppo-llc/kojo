package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// MemoryRecord is the public representation of an agent's MEMORY.md
// for HTTP / Web UI consumers. Mirrors the shape of store.AgentMemoryRecord
// without exposing the store package directly so handlers don't need to
// import internal/store for a single field set.
type MemoryRecord struct {
	AgentID   string
	Body      string
	ETag      string
	UpdatedAt int64
	DeletedAt *int64
}

// memoryIODBTimeout is the deadline for any single PUT/DELETE/Get tx
// chain on MEMORY.md. 30s is generous; the actual ops are a single
// SELECT + a single UPDATE/INSERT.
const memoryIODBTimeout = 30 * time.Second

// GetAgentMemory returns the v1 store's view of MEMORY.md for agentID.
// The store row is the authoritative read path for cross-device readers
// — the on-disk file is the canonical write path for the local CLI but
// callers (Web UI, HTTP API) should consume the synced row.
//
// store.ErrNotFound is returned when the row hasn't been synced yet
// (brand-new agent before ensureAgentDir, or post-DELETE tombstone).
// The handler maps that to 404.
func (m *Manager) GetAgentMemory(ctx context.Context, agentID string) (*MemoryRecord, error) {
	st := m.Store()
	if st == nil {
		return nil, errStoreNotReady
	}
	// Refuse mid-reset so a Web UI poll between ResetData's wipe
	// and the post-reset sync doesn't read the pre-reset MEMORY.md
	// row (the DB row trails the disk during the reset window;
	// memorySyncMu held by ResetData blocks WRITE callers but not
	// reads, hence this explicit gate).
	if err := m.refuseIfResetting(agentID); err != nil {
		return nil, err
	}
	rec, err := st.GetAgentMemory(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return &MemoryRecord{
		AgentID:   rec.AgentID,
		Body:      rec.Body,
		ETag:      rec.ETag,
		UpdatedAt: rec.UpdatedAt,
		DeletedAt: rec.DeletedAt,
	}, nil
}

// PutAgentMemory writes body into the agent's MEMORY.md file and
// records the result in the v1 store. ifMatchETag is an optimistic-
// concurrency precondition checked against the v1 store's current row
// — empty means "create or replace, ungated". Returns the new
// MemoryRecord (with fresh etag) on success.
//
// Locking story (in order):
//
//  1. Manager.LockPatch(agentID) — serializes against PATCH-style
//     mutations on the agent record (Archive / Delete / Unarchive
//     all take this lock too). Without it a concurrent Archive could
//     land between our archived-precheck and the file write, and we'd
//     write to the dir of a now-dormant agent.
//  2. resetting / busy guards via m.busyMu — refuses writes during a
//     ResetData (which is itself wiping the file) so we don't race
//     resurrection.
//  3. memorySyncMu[agentID] — serializes against any concurrent
//     daemon-side sync (Load / fork / prepareChat) so the file write
//     and the DB upsert appear atomic to a cross-device reader.
//
// The DB UPDATE goes through the singleton-only sync path
// (syncAgentMemoryToDB) — touching memory_entries here would conflate
// MEMORY.md updates with unrelated memory/*.md scan errors. After the
// disk write, we re-read the DB row to obtain the freshly-computed
// etag for the response.
//
// Cross-device staleness: before checking If-Match we run a
// disk→DB sync so an unsynced CLI edit lands in the row first. Without
// that, a Web client holding an etag from a previous GET could PUT and
// silently clobber a CLI write that hadn't yet been synced.
func (m *Manager) PutAgentMemory(ctx context.Context, agentID, body, ifMatchETag string) (*MemoryRecord, error) {
	if agentID == "" {
		return nil, fmt.Errorf("PutAgentMemory: agentID required")
	}
	st := m.Store()
	if st == nil {
		return nil, errStoreNotReady
	}

	// Layer 1: per-agent patch lock (covers Archive / Delete / Unarchive
	// against us). Held for the entire trio.
	releasePatch := m.LockPatch(agentID)
	defer releasePatch()

	// Layer 2: editing flag. Reserves the agent against ResetData
	// (acquireResetGuard refuses when m.editing[agentID] is set), so
	// a Reset that lands after our refuseDormantOrResetting check
	// still can't run mid-write. Held until release below.
	releaseEdit, err := m.acquireMemoryEdit(agentID)
	if err != nil {
		return nil, err
	}
	defer releaseEdit()

	// Layer 3: memory-sync gate.
	releaseSync := lockMemorySync(agentID)
	defer releaseSync()

	dbCtx, cancel := dbContextWithCancel(ctx, memoryIODBTimeout)
	defer cancel()

	// Reflect any unsynced CLI edit BEFORE the precondition check so a
	// stale Web write can't overwrite it. syncAgentMemoryToDB only
	// touches the singleton row — no memory_entries scan side effects.
	if err := syncAgentMemoryToDB(dbCtx, st, agentID, m.logger); err != nil {
		return nil, fmt.Errorf("PutAgentMemory: pre-sync: %w", err)
	}

	// Pre-write If-Match check against the freshly-synced view. Refusing
	// here avoids touching the disk on a stale write — UpsertAgentMemory
	// also enforces ifMatch inside its TX, but doing the check here lets
	// us return cleanly without a half-written file.
	if ifMatchETag != "" {
		prev, err := st.GetAgentMemory(dbCtx, agentID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("PutAgentMemory: read DB row: %w", err)
		}
		if prev == nil || prev.ETag != ifMatchETag {
			return nil, store.ErrETagMismatch
		}
	}

	// Ensure the agent dir exists (legacy / freshly-imported agents
	// may not have minted it yet).
	dir := agentDir(agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("PutAgentMemory: ensure dir: %w", err)
	}
	memPath := filepath.Join(dir, "MEMORY.md")
	if err := writeFileAtomic(memPath, []byte(body)); err != nil {
		return nil, fmt.Errorf("PutAgentMemory: write file: %w", err)
	}

	// Authoritative DB write with the caller's If-Match preserved
	// inside the TX. UpsertAgentMemory hits ErrETagMismatch if the row
	// shifted between our pre-check and now (would only happen if a
	// daemon-side sync raced through despite our lock, e.g. via a code
	// path that bypasses memorySyncMu — defense-in-depth).
	rec, err := st.UpsertAgentMemory(dbCtx, agentID, body, ifMatchETag, store.AgentMemoryInsertOptions{
		AllowOverwrite: true,
	})
	if err != nil {
		return nil, fmt.Errorf("PutAgentMemory: upsert: %w", err)
	}
	return &MemoryRecord{
		AgentID:   rec.AgentID,
		Body:      rec.Body,
		ETag:      rec.ETag,
		UpdatedAt: rec.UpdatedAt,
		DeletedAt: rec.DeletedAt,
	}, nil
}

// DeleteAgentMemory removes the on-disk MEMORY.md and tombstones the
// DB row. Idempotent: a missing file with a tombstoned (or absent)
// row returns nil. ifMatchETag is checked against the live row when
// non-empty.
//
// Same three-lock dance as PutAgentMemory (LockPatch → refuse-dormant
// → memorySyncMu) so concurrent lifecycle / sync ops can't race the
// disk-then-DB pair.
func (m *Manager) DeleteAgentMemory(ctx context.Context, agentID, ifMatchETag string) error {
	if agentID == "" {
		return fmt.Errorf("DeleteAgentMemory: agentID required")
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

	if err := syncAgentMemoryToDB(dbCtx, st, agentID, m.logger); err != nil {
		return fmt.Errorf("DeleteAgentMemory: pre-sync: %w", err)
	}

	// Pre-check the precondition against the freshly-synced view so
	// we avoid touching the disk on a stale request. SoftDeleteAgentMemory
	// also enforces ifMatch inside its TX, but doing the check here
	// keeps the file/DB pair consistent: file is removed only when
	// the precondition is satisfied at this point in time.
	if ifMatchETag != "" {
		prev, err := st.GetAgentMemory(dbCtx, agentID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("DeleteAgentMemory: read DB row: %w", err)
		}
		if prev == nil || prev.ETag != ifMatchETag {
			return store.ErrETagMismatch
		}
	}

	memPath := filepath.Join(agentDir(agentID), "MEMORY.md")
	if err := os.Remove(memPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("DeleteAgentMemory: remove file: %w", err)
	}

	// TX-internal precondition: pass the caller's ifMatch through.
	// If a daemon-side sync raced through despite our locks (defense
	// in depth), the TX-side check returns ErrETagMismatch and the
	// tombstone doesn't commit.
	if err := st.SoftDeleteAgentMemory(dbCtx, agentID, ifMatchETag); err != nil {
		return fmt.Errorf("DeleteAgentMemory: tombstone: %w", err)
	}
	return nil
}

// acquireMemoryEdit verifies the agent exists and is live, then
// reserves its m.editing slot so concurrent Chat / ResetData fail
// fast (acquireResetGuard refuses when m.editing[agentID] is set).
// The returned release MUST always be called.
//
// Mirrors acquireTranscriptEdit but without the llama.cpp constraint
// — MEMORY.md edits are CLI-agnostic. Returns ErrAgentNotFound,
// ErrAgentArchived, ErrAgentResetting, or ErrAgentBusy depending on
// which guard tripped; the HTTP handler maps these to 404 / 409.
//
// Lock order is m.mu → m.busyMu, the same as acquireTranscriptEdit,
// so a concurrent Update() (which holds m.mu) can't sneak in a
// Tool/Archived flip between our reads.
func (m *Manager) acquireMemoryEdit(agentID string) (func(), error) {
	m.mu.Lock()
	a, ok := m.agents[agentID]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, agentID)
	}
	archived := a.Archived
	m.busyMu.Lock()
	if archived {
		m.busyMu.Unlock()
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAgentArchived, agentID)
	}
	if m.resetting[agentID] {
		m.busyMu.Unlock()
		m.mu.Unlock()
		return nil, ErrAgentResetting
	}
	if m.editing[agentID] {
		m.busyMu.Unlock()
		m.mu.Unlock()
		return nil, fmt.Errorf("%w, try again later", ErrAgentBusy)
	}
	if m.switching != nil && m.switching[agentID] {
		// §3.7 device switch is mid-flight: refuse memory /
		// persona / task edits so a cron-driven autosummary
		// or scheduled memory hook doesn't write to source
		// after the snapshot. The runtime caller (autosummary,
		// notify) sees ErrAgentBusy and retries after the
		// switch finishes.
		m.busyMu.Unlock()
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: device switch in progress", ErrAgentBusy)
	}
	m.editing[agentID] = true
	m.busyMu.Unlock()
	m.mu.Unlock()
	release := func() {
		m.busyMu.Lock()
		delete(m.editing, agentID)
		m.busyMu.Unlock()
	}
	return release, nil
}

// writeFileAtomic writes data to path via a temp file in the same dir
// + rename so a crash mid-write doesn't leave a half-written file the
// CLI would then read on next session. The full §2.5 intent-file
// protocol is a future hardening; this gives us crash safety today
// without the cross-device coordination surface.
//
// The temp file uses a `.tmp-*` prefix without a `.md` suffix so the
// memory/ scan (scanMemorySubdir) skips it on a crash — otherwise a
// half-written intermediate would be picked up and resurrected as a
// phantom memory entry on the next sync.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
