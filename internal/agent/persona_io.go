package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// PersonaRecord is the public representation of an agent's persona
// for HTTP / Web UI consumers. Mirrors store.AgentPersonaRecord
// without leaking the store package across the API boundary.
type PersonaRecord struct {
	AgentID   string
	Body      string
	ETag      string
	UpdatedAt int64
	DeletedAt *int64
}

// personaBodyCap is the wire-side cap on a single persona write.
// Persona is short prose ("you are a snarky barista" tier) — the
// 4 MiB ceiling we use for MEMORY.md is generous and consistent
// with the rest of the multi-device blob limits.
const personaBodyCap = 4 << 20

// personaIODBTimeout bounds the DB chain for any GET / PUT call.
const personaIODBTimeout = 30 * time.Second

// personaSyncMu serializes persona.md file writes + DB upserts on a
// per-agent basis. Distinct from memorySyncMu (which guards the
// MEMORY.md / memory_entries trio) so persona ops don't block on a
// long memory sync and vice versa.
//
// Map entries leak by design — same pattern as memorySyncMu /
// Manager.LockPatch (agent IDs bounded, unheld mutex is small).
var personaSyncMu struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func personaSyncLockFor(agentID string) *sync.Mutex {
	personaSyncMu.mu.Lock()
	defer personaSyncMu.mu.Unlock()
	if personaSyncMu.m == nil {
		personaSyncMu.m = make(map[string]*sync.Mutex)
	}
	mu, ok := personaSyncMu.m[agentID]
	if !ok {
		mu = &sync.Mutex{}
		personaSyncMu.m[agentID] = mu
	}
	return mu
}

func lockPersonaSync(agentID string) func() {
	mu := personaSyncLockFor(agentID)
	mu.Lock()
	return mu.Unlock
}

// GetAgentPersona returns the v1 store's view of persona.md for
// agentID. The on-disk file remains canonical for the CLI
// (syncPersona reads it on every prepareChat); the DB row is the
// read path for cross-device consumers.
//
// Returns store.ErrNotFound when no persona row has been synced yet
// (a brand-new agent that hasn't had upsertAgent run, or one whose
// row was tombstoned). Refuses with ErrAgentResetting during
// ResetData so a Web UI poll between the wipe + post-reset sync
// can't observe pre-reset content.
func (m *Manager) GetAgentPersona(ctx context.Context, agentID string) (*PersonaRecord, error) {
	if agentID == "" {
		return nil, fmt.Errorf("GetAgentPersona: agentID required")
	}
	st := m.Store()
	if st == nil {
		return nil, errStoreNotReady
	}
	// Reset guard first so a request landing during ResetData
	// doesn't even reach the agent-existence read (which would be
	// a wasted lock acquisition during reset).
	if err := m.refuseIfResetting(agentID); err != nil {
		return nil, err
	}
	// Side-effect-free existence check: m.Get triggers
	// syncPersona which would do disk→DB sync + spawn a
	// publicProfile regen goroutine. Reading m.agents under m.mu
	// gives us the same membership answer without those side
	// effects, which a naive Web GET shouldn't trigger.
	m.mu.Lock()
	_, exists := m.agents[agentID]
	m.mu.Unlock()
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, agentID)
	}
	rec, err := st.GetAgentPersona(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return &PersonaRecord{
		AgentID:   rec.AgentID,
		Body:      rec.Body,
		ETag:      rec.ETag,
		UpdatedAt: rec.UpdatedAt,
		DeletedAt: rec.DeletedAt,
	}, nil
}

// PutAgentPersona writes body to the agent's persona.md file and
// upserts the matching agent_persona row. ifMatchETag is the
// optimistic-concurrency precondition; "" means "ungated"
// (handlers MUST pass the client-supplied If-Match value).
//
// Empty body is handled specially: writePersonaFile removes the
// file (the existing helper's contract — empty content = no
// persona). The DB row is still upserted with empty body so
// cross-device readers observe the cleared state.
//
// Locking story (mirrors PutAgentMemory):
//
//  1. Manager.LockPatch — serializes against PATCH /agents/{id} +
//     Archive / Delete / Unarchive so a concurrent lifecycle op
//     can't land between our DB precondition and the disk write.
//  2. personaSyncMu — serializes against any concurrent
//     daemon-side persona sync (Manager.save → upsertAgent) so
//     the file write + DB upsert appear atomic to a cross-device
//     reader.
//
// We do NOT take m.editing here — persona writes are CLI-agnostic
// (the CLI process re-reads persona.md on the next prepareChat;
// no in-flight chat depends on the file staying stable). Refusing
// during ResetData is sufficient.
func (m *Manager) PutAgentPersona(ctx context.Context, agentID, body, ifMatchETag string) (*PersonaRecord, error) {
	if agentID == "" {
		return nil, fmt.Errorf("PutAgentPersona: agentID required")
	}
	if len(body) > personaBodyCap {
		return nil, fmt.Errorf("%w: body exceeds %d byte cap", ErrInvalidPersona, personaBodyCap)
	}
	st := m.Store()
	if st == nil {
		return nil, errStoreNotReady
	}

	// Layer 1: per-agent patch lock.
	releasePatch := m.LockPatch(agentID)
	defer releasePatch()

	// Refuse mid-reset — keeps the post-reset state consistent
	// (ResetData doesn't wipe persona.md but Archive/Reset stop
	// CLI consumers; a write here would be observable but the
	// reset flow has its own coordination).
	if err := m.refuseIfResetting(agentID); err != nil {
		return nil, err
	}
	// §3.7 device switch gate: a persona write after Step -1's
	// snapshot would never make it to target. AcquireMutation
	// also bumps the in-flight counter so WaitChatIdle sees
	// this write as non-idle.
	releaseMut, err := m.AcquireMutation(agentID)
	if err != nil {
		return nil, err
	}
	defer releaseMut()

	// Verify the agent exists + is live BEFORE acquiring
	// personaSyncMu. UpsertAgentPersona refuses tombstoned
	// agents at the SQL level, but a fast-fail here means we
	// don't block on the sync mutex for an agent we're going
	// to refuse anyway.
	m.mu.Lock()
	a, ok := m.agents[agentID]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, agentID)
	}
	archived := a.Archived
	m.mu.Unlock()
	if archived {
		return nil, fmt.Errorf("%w: %s", ErrAgentArchived, agentID)
	}

	// Layer 2: persona-sync gate. Released explicitly BEFORE the
	// SaveAgentRowOnly calls below — m.save / agentStore-internal
	// paths take store.mu THEN personaSyncMu (via upsertAgent),
	// so a path that holds personaSyncMu and waits for store.mu
	// would deadlock by inverting that order. We use a sync.Once
	// guard so any error-return path that hasn't already released
	// still hits the unlock at function end.
	releaseSync := lockPersonaSync(agentID)
	var releaseSyncOnce sync.Once
	releasePersonaSync := func() { releaseSyncOnce.Do(releaseSync) }
	defer releasePersonaSync()

	dbCtx, cancel := dbContextWithCancel(ctx, personaIODBTimeout)
	defer cancel()

	// Pre-write disk→DB sync: a CLI-side persona.md edit that
	// hasn't yet been picked up by syncPersona / m.save() would
	// otherwise let a Web client clobber it. Read the disk file
	// first; if it differs from the DB row's body, upsert the
	// disk view (with AllowOverwrite) BEFORE checking If-Match.
	//
	// Snapshot disk state for rollback purposes too: a DB upsert
	// failure after the file write below would otherwise leave the
	// new body on disk, where the next syncPersona / m.save would
	// pick it up and silently turn a failed PUT into a success.
	priorBody, priorExisted, err := readPersonaForSync(agentID)
	if err != nil {
		// Real I/O error reading persona.md (not ENOENT). Bail
		// before the precondition / write — a permission glitch on
		// read could mean the same glitch on write, and we'd land
		// a half-applied PUT we can't roll back.
		return nil, fmt.Errorf("PutAgentPersona: read disk: %w", err)
	}
	// Sync disk → DB only when disk has content. Post-cutover the DB
	// is canonical for persona; a missing disk file is interpreted as
	// "not yet hydrated" (e.g. first boot after v0→v1 migration where
	// the importer populated the DB but never wrote disk), NOT as
	// "CLI cleared persona". The hydrate path in upsertAgent /
	// syncPersona will write disk from DB on the next sync; here we
	// just leave DB alone so the live row survives until the actual
	// PUT below either succeeds or its If-Match check rejects it.
	//
	// CLI clear is no longer expressible by `rm persona.md` (round-
	// trip via PutAgentPersona / Manager.Update with body="" is the
	// canonical clear path). See docs §2.3 / §5.5.
	diskBody := priorBody
	prev, err := st.GetAgentPersona(dbCtx, agentID)
	switch {
	case err == nil && prev != nil:
		// Live row exists. Sync only if disk diverges from DB AND
		// disk has content. Empty/missing disk + non-empty row =
		// hydrate scenario, leave DB intact for the upsertAgent /
		// syncPersona path to handle.
		if priorExisted && prev.Body != diskBody {
			if _, err := st.UpsertAgentPersona(dbCtx, agentID, diskBody, "", store.AgentInsertOptions{
				AllowOverwrite: true,
			}); err != nil {
				return nil, fmt.Errorf("PutAgentPersona: pre-sync: %w", err)
			}
		}
	case errors.Is(err, store.ErrNotFound):
		// No row yet. Mint one only if disk has actual content —
		// creating an empty row for an absent file would etag-churn
		// it on the next real persona write without buying us
		// anything (a Web GET on a never-synced agent already
		// returns 404 via ErrNotFound, and that's the right answer
		// for a brand-new agent that hasn't been saved yet).
		if priorExisted && diskBody != "" {
			if _, err := st.UpsertAgentPersona(dbCtx, agentID, diskBody, "", store.AgentInsertOptions{
				AllowOverwrite: true,
			}); err != nil {
				return nil, fmt.Errorf("PutAgentPersona: pre-sync insert: %w", err)
			}
		}
	default:
		return nil, fmt.Errorf("PutAgentPersona: pre-sync read: %w", err)
	}

	// Pre-write If-Match check against the freshly-synced view —
	// avoids touching the disk on a stale write.
	if ifMatchETag != "" {
		prev, err := st.GetAgentPersona(dbCtx, agentID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("PutAgentPersona: read DB row: %w", err)
		}
		if prev == nil || prev.ETag != ifMatchETag {
			return nil, store.ErrETagMismatch
		}
	}

	// Disk side: write/clear the file.
	if err := writePersonaFile(agentID, body); err != nil {
		return nil, fmt.Errorf("PutAgentPersona: write file: %w", err)
	}

	// DB side: authoritative upsert with the caller's If-Match
	// preserved inside the TX so a concurrent daemon-side sync
	// that bypasses personaSyncMu (defense in depth) gets
	// caught by the store's etag check.
	rec, err := st.UpsertAgentPersona(dbCtx, agentID, body, ifMatchETag, store.AgentInsertOptions{
		AllowOverwrite: true,
	})
	if err != nil {
		// DB upsert failed AFTER the file write landed. Roll
		// back the file so the next syncPersona / m.save doesn't
		// silently turn the failed PUT into a success by pulling
		// our half-applied body into the row.
		//
		// Strategy depends on whether persona.md existed pre-
		// write (priorExisted captured before the write):
		//   - existed: restore priorBody via writePersonaFile
		//     (which writes the file when content != "" and
		//     removes it when content == "")
		//   - didn't exist: writePersonaFile("") removes the
		//     file we just minted, restoring "no file" state
		var rbErr error
		if priorExisted {
			rbErr = writePersonaFile(agentID, priorBody)
		} else {
			rbErr = writePersonaFile(agentID, "")
		}
		if rbErr != nil {
			m.logger.Warn("PutAgentPersona: file rollback failed",
				"agent", agentID, "err", rbErr)
		}
		return nil, fmt.Errorf("PutAgentPersona: upsert: %w", err)
	}

	// Persona disk + DB write is done. Release personaSyncMu BEFORE
	// the in-memory + agents-row updates below — those cross into
	// store.mu via SaveAgentRowOnly, and m.save / friends acquire
	// store.mu THEN personaSyncMu. Holding personaSyncMu past this
	// point would invert the canonical lock order.
	releasePersonaSync()

	// Reflect the new persona in the in-memory Agent so the next
	// chat doesn't have to round-trip through syncPersona to
	// observe the change. Capture override flag + old persona to
	// drive the publicProfile regeneration below.
	m.mu.Lock()
	var (
		oldPersona string
		override   bool
	)
	if cur, ok := m.agents[agentID]; ok {
		oldPersona = cur.Persona
		override = cur.PublicProfileOverride
		cur.Persona = body
		cur.UpdatedAt = time.Now().Format(time.RFC3339)
	}
	m.mu.Unlock()

	// Regenerate or clear publicProfile when persona changed,
	// mirroring the syncPersona / PATCH /agents/{id} behavior:
	//   - body == "" + !override → clear the field synchronously
	//     (matches manager.go line 423–428's empty-persona branch).
	//   - body != "" + !override → kick off async regen.
	//   - override → skip; the user has manually pinned the profile.
	if oldPersona != body && !override {
		if body == "" {
			m.mu.Lock()
			var snap *Agent
			if cur, ok := m.agents[agentID]; ok {
				cur.PublicProfile = ""
				snap = copyAgent(cur)
			}
			m.mu.Unlock()
			// Persist the cleared PublicProfile to the agents
			// row so a daemon restart doesn't resurrect the
			// pre-clear value from the on-disk settings JSON.
			// Single-agent save (matches syncPersona's pattern)
			// avoids any iteration that could deadlock against
			// a concurrent syncPersona for another agent.
			if snap != nil {
				if err := m.store.SaveAgentRowOnly(snap); err != nil {
					m.logger.Warn("PutAgentPersona: publicProfile clear save failed",
						"agent", agentID, "err", err)
				}
			}
		} else {
			go m.regeneratePublicProfile(agentID, body)
		}
	}

	return &PersonaRecord{
		AgentID:   rec.AgentID,
		Body:      rec.Body,
		ETag:      rec.ETag,
		UpdatedAt: rec.UpdatedAt,
		DeletedAt: rec.DeletedAt,
	}, nil
}

// agentPersonaFilePath returns the full path of an agent's
// persona.md. Exported indirectly so tests don't need to know the
// layout beyond the agentDir contract.
func agentPersonaFilePath(agentID string) string {
	return filepath.Join(agentDir(agentID), "persona.md")
}

// readPersonaForSync returns the on-disk persona.md content along
// with whether the file existed. Distinct from readPersonaFile
// (which collapses ENOENT and real I/O errors into one bool):
// non-ENOENT errors propagate so callers can refuse a write rather
// than racing on stale state.
//
// Returns:
//   - (body, true,  nil)  — file present, contents read
//   - ("",   false, nil)  — file does not exist (ENOENT)
//   - ("",   false, err)  — real I/O error (permission, etc.)
func readPersonaForSync(agentID string) (string, bool, error) {
	data, err := os.ReadFile(agentPersonaFilePath(agentID))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(data), true, nil
}
