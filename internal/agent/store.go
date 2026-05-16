package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/store"
)

// globalStore is the package-level handle used by the file-scoped helpers
// (transcript, memory, groupdm) that don't carry a *Manager reference.
//
// NewManager registers the agentStore's *store.Store here on success and
// agentStore.Close() clears it. Tests within this package run sequentially
// by default, so the last NewManager wins — sufficient because no test
// creates two managers in flight at once. Concurrent NewManager calls
// (e.g. t.Parallel inside the same package) are not supported and would
// produce undefined cross-test reads on the global handle.
var globalStore atomic.Pointer[store.Store]

// getGlobalStore returns the active *store.Store or nil if NewManager
// hasn't run yet. Callers MUST handle nil — for test fixtures that
// poke *Agent maps without going through NewManager, the helpers return
// a clear error rather than panic on a nil DB handle.
func getGlobalStore() *store.Store { return globalStore.Load() }

// setGlobalStore registers s as the active handle. Pass nil from
// agentStore.Close() so a follow-up helper call after shutdown gets a
// clean "store closed" error rather than reading from a freed *sql.DB.
func setGlobalStore(s *store.Store) { globalStore.Store(s) }

// cronPausedFile names the legacy v0 marker file. Read once at boot during
// migration (LoadCronPaused) and removed after the value is mirrored into
// the kv table; runtime writes go to kv only.
const cronPausedFile = "cron_paused"

// cron_paused is now backed by the kv table per design doc §2.3
//   namespace = "scheduler", key = "paused", scope = global, type = string
// Value is the literal "true" / "false". A missing row means "not paused"
// (cron defaults to running). Global scope so a deliberate pause
// propagates across peers — the v0 marker file was implicitly per-device
// only because there was no shared layer; the schedule itself is global.
const (
	cronPausedKVNamespace = "scheduler"
	cronPausedKVKey       = "paused"
)

// cronPausedKVTimeout bounds each kv read / write. The flag is touched
// at boot (one read) and on operator pause/resume (one write) — no hot
// path — so a generous timeout is fine; the bound is purely a guard
// against a deadlocked DB wedging the manager.
const cronPausedKVTimeout = 5 * time.Second

// cronPausedMigrationTestHook is a test-only injection point fired
// inside LoadCronPaused AFTER the initial GetKV miss + legacy file
// detection but BEFORE the IfMatchAny PutKV. Tests use it to seed a
// colliding row and exercise the ErrETagMismatch resolution branch
// that is otherwise unreachable via timing in a single-goroutine
// test. Production keeps it nil — the if-guard is a single
// nil-pointer compare in a non-hot path.
var cronPausedMigrationTestHook func()

// reservedAgentKeys lists the JSON fields routed to typed columns or to
// adjacent tables (agent_persona) instead of being parked in
// agents.settings_json on Save. Keep aligned with internal/migrate/importers
// agentsImporter — a key that's stripped on import must also be stripped
// here so the on-disk layout stays consistent across both write paths.
//
//   - id, name, persona       → typed columns / agent_persona
//   - createdAt, updatedAt    → typed int64 columns (rebuilt to RFC3339 on Load)
//   - lastMessage             → runtime cache (would churn etag on every msg)
//   - intervalMinutes,
//     activeStart,activeEnd   → legacy fields stripped on Save (already
//                               migrated into cronExpr / silentStart on Load)
//
// cronExpr is intentionally NOT reserved — it's the canonical persisted
// field for schedules in v1, and stripping it from settings_json would
// silently discard every schedule on the next Save/Load cycle.
var reservedAgentKeys = map[string]bool{
	"id":              true,
	"name":            true,
	"persona":         true,
	"createdAt":       true,
	"updatedAt":       true,
	"lastMessage":     true,
	"intervalMinutes": true,
	"activeStart":     true,
	"activeEnd":       true,
}

// loadStripKeys lists the keys settingsToAgent() must filter out before
// rehydrating — i.e. keys whose value is owned by a typed column or that
// would actively confuse the unmarshal. The legacy fields stay OUT of this
// set: settingsToAgent must let intervalMinutes / activeStart / activeEnd
// through so normalizeAgent's migration step can read them via the
// transient Legacy* fields and translate them into cronExpr / silentStart.
//
// Save-side stripping (reservedAgentKeys above) is what eventually retires
// the legacy keys from on-disk storage; Load-side stripping cannot, because
// agents that haven't been re-Saved since the cutover still carry them.
var loadStripKeys = map[string]bool{
	"id":          true,
	"name":        true,
	"persona":     true,
	"createdAt":   true,
	"updatedAt":   true,
	"lastMessage": true,
}

// agentStore persists agent metadata to the v1 SQLite store.
//
// store.Save / store.Load no longer touch agents.json; the file lives on
// in v0 dirs only. v1 reads and writes go through internal/store APIs
// (agents + agent_persona tables) so the change feed, etag, and version
// columns stay in sync with what the importer wrote during --migrate.
//
// cron_paused was migrated to the kv table in Phase 2c-2 slice 8 — see
// LoadCronPaused / SaveCronPaused. The legacy `<configdir>/agents/cron_paused`
// marker is read once on first LoadCronPaused after upgrade and removed
// after the value is mirrored into kv.
type agentStore struct {
	mu sync.Mutex

	// db is shared with subsystems via the manager.Store() accessor once
	// Phase 2c-2 is complete. The agentStore owns it and closes it via
	// Close().
	db *store.Store

	// dir is the v1 configdir, captured at newStore() time. Used by
	// LoadCronPaused / SaveCronPaused for the legacy marker file
	// migration + cleanup. configdir.Path() racing with shutdown can't
	// redirect those operations to a stale path.
	dir string

	logger *slog.Logger
}

// newStore opens kojo.db at the active configdir and returns an agent-side
// wrapper around it. The caller (NewManager) owns the returned *agentStore
// and is responsible for invoking Close() on shutdown.
func newStore(logger *slog.Logger) (*agentStore, error) {
	dir := configdir.Path()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("ensure configdir: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := store.Open(ctx, store.Options{ConfigDir: dir})
	if err != nil {
		return nil, fmt.Errorf("open kojo.db: %w", err)
	}
	setGlobalStore(db)
	return &agentStore{db: db, dir: dir, logger: logger}, nil
}

// Store returns the underlying *store.Store so other subsystems (in
// follow-up cutover slices: messages, memory, groupdm) can share the
// connection rather than each opening their own *sql.DB.
func (st *agentStore) Store() *store.Store {
	if st == nil {
		return nil
	}
	return st.db
}

// Close releases the underlying SQLite connection. Idempotent and
// nil-safe so the shutdown path doesn't have to special-case a manager
// whose store was never opened.
func (st *agentStore) Close() error {
	if st == nil || st.db == nil {
		return nil
	}
	// Clear the package-level handle BEFORE closing so a helper call
	// racing with shutdown sees nil rather than a closed *sql.DB. The
	// CompareAndSwap guard avoids clearing a successor manager's handle
	// in tests that overlap newStore() / Close() on the same global.
	globalStore.CompareAndSwap(st.db, nil)
	return st.db.Close()
}

// Save persists every agent in the snapshot. New agents are inserted;
// existing rows are updated. Save is upsert-only — agents removed from
// the in-memory map are tombstoned via the explicit Delete() path, NOT
// inferred here from the snapshot. That separation matters because
// m.save() is fire-and-forget and called from many goroutines: a stale
// snapshot reaching Save() last would otherwise tombstone a concurrently-
// created agent.
//
// Errors are logged but do not abort the loop — manager.save() is fire-
// and-forget in many call sites (some are even `go m.save()`), and
// surfacing a per-row failure to a void return wouldn't help any caller.
// Persistent failures will reproduce on the next save.
func (st *agentStore) Save(agents []*Agent) {
	st.mu.Lock()
	defer st.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, a := range agents {
		if err := st.upsertAgent(ctx, a); err != nil {
			st.logger.Warn("failed to upsert agent", "agent", a.ID, "err", err)
		}
	}
}

// SaveAgentRowOnly writes ONE agent's metadata (agents row only,
// not the persona row) under the store mutex. Used by
// Manager.syncPersona / PutAgentPersona AFTER they have already
// updated the persona row inline under personaSyncMu AND released
// that lock. Calling this while still holding personaSyncMu would
// invert the canonical lock order (m.save() takes store.mu THEN
// personaSyncMu via upsertAgent) and risk deadlock.
//
// Returns the underlying error rather than logging-and-continuing
// so callers can decide whether to surface the failure to the user.
//
// Crucially, this does NOT iterate all agents (unlike Save).
// Iterating while holding a per-agent personaSyncMu would
// deadlock if two syncPersonas for different agents both raced
// through the iteration: each would hold its own persona lock and
// try to acquire the other's via the upsertAgent path. Even with
// the release-before-save discipline above, single-agent semantics
// keeps the API resilient to future regressions.
func (st *agentStore) SaveAgentRowOnly(a *Agent) error {
	if st == nil || st.db == nil {
		return errors.New("agentStore: not initialized")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return st.upsertAgentSkipPersona(ctx, a)
}

// Upsert writes a single agent through upsertAgent under the store
// mutex and returns the underlying error rather than logging-and-
// continuing as Save() does. Used by paths that must observe the
// write succeed before doing further work — chiefly the fork pre-
// registration that satisfies agent_messages' FK-on-agent_id during
// transcript replay.
func (st *agentStore) Upsert(a *Agent) error {
	if st == nil || st.db == nil {
		return errors.New("agentStore: not initialized")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return st.upsertAgent(ctx, a)
}

// Delete tombstones the agent row. Called from the manager's Delete
// path so the row is invisible whether or not the next Save() runs.
// Idempotent: SoftDeleteAgent is a no-op for already-tombstoned rows.
//
// Only the agents row is tombstoned here. agent_persona / agent_memory
// / agent_messages rows stay live in the DB; they become unreachable
// via the public Get/List APIs because those join against agents and
// filter on deleted_at IS NULL. Hard cascade happens at HardDeleteAgent
// time, at which point ON DELETE CASCADE in the schema sweeps every
// dependent row in one transaction. HardDeleteAgent itself has no
// caller today — it is wired for the eventual operator-driven hard-
// delete pass (planned as a future `--clean` target alongside the
// existing `snapshots` and `legacy` targets in cmd/kojo/clean_cmd.go).
func (st *agentStore) Delete(id string) error {
	if st == nil || st.db == nil {
		return errors.New("agentStore: not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return st.db.SoftDeleteAgent(ctx, id)
}

// upsertAgent writes one agent's metadata into the agents table and its
// persona into agent_persona. Both writes use AllowOverwrite-style
// daemon-internal paths because Manager already serializes Save calls
// against per-agent state via the in-memory mu — etag round-tripping
// would only re-add the lock contention without changing observable
// behavior.
//
// Calls Manager.syncPersona / PutAgentPersona must NOT route through
// here while holding personaSyncMu(a.ID) — the lockPersonaSync below
// would self-deadlock. Use upsertAgentSkipPersona for those paths.
func (st *agentStore) upsertAgent(ctx context.Context, a *Agent) error {
	return st.upsertAgentInner(ctx, a, false)
}

// upsertAgentSkipPersona is the variant for callers that already
// updated the persona row inline (under personaSyncMu) and want to
// flush the agents-row metadata without re-acquiring that lock.
// syncPersona is the only intended caller.
func (st *agentStore) upsertAgentSkipPersona(ctx context.Context, a *Agent) error {
	return st.upsertAgentInner(ctx, a, true)
}

func (st *agentStore) upsertAgentInner(ctx context.Context, a *Agent, skipPersona bool) error {
	created := parseAgentRFC3339Millis(a.CreatedAt)
	updated := parseAgentRFC3339Millis(a.UpdatedAt)
	if updated == 0 {
		updated = store.NowMillis()
	}
	if created == 0 {
		created = updated
	}

	cur, err := st.db.GetAgent(ctx, a.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		settings, err := agentToSettings(a, nil)
		if err != nil {
			return fmt.Errorf("encode settings: %w", err)
		}
		rec := &store.AgentRecord{
			ID:       a.ID,
			Name:     a.Name,
			Settings: settings,
		}
		if _, err := st.db.InsertAgent(ctx, rec, store.AgentInsertOptions{
			CreatedAt: created,
			UpdatedAt: updated,
		}); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
	case err != nil:
		return fmt.Errorf("get: %w", err)
	default:
		// Forward-compat: keys present in cur.Settings that this binary
		// doesn't know about are preserved by merging into the new
		// settings map. Without this a v1.x daemon downgraded to v1
		// would silently drop fields its successor wrote — a regression
		// the design doc (§3.3) calls out explicitly.
		nextSettings, err := agentToSettings(a, cur.Settings)
		if err != nil {
			return fmt.Errorf("encode settings: %w", err)
		}
		// No-op skip: m.save() runs on every state transition (interval
		// changes, message-preview updates, busy clears) and many of those
		// don't change settings at all. Without this short-circuit each
		// bench-clear or shutdown bumps version/etag for every agent and
		// floods the change feed. reflect.DeepEqual works on the JSON-
		// roundtripped maps because every leaf is a JSON-native type.
		if cur.Name == a.Name && reflect.DeepEqual(cur.Settings, nextSettings) {
			break
		}
		if _, err := st.db.UpdateAgent(ctx, a.ID, "", func(r *store.AgentRecord) error {
			r.Name = a.Name
			r.Settings = nextSettings
			return nil
		}); err != nil {
			return fmt.Errorf("update: %w", err)
		}
	}

	// Persona row mirrors persona.md. Post-cutover (Phase 2c-2) the DB
	// is canonical and disk is a hydrated mirror — see docs §2.3 /
	// §5.5 — but the CLI process still edits persona.md directly, so
	// this sync path remains. Skip the upsert when (a) body is
	// unchanged — a fresh BodySHA256 equality is the same check the
	// store would do internally — or (b) body is empty AND no prior
	// row exists (creating an empty row would etag-churn it on the
	// next real persona write). Missing-disk + live-DB takes the
	// hydrate branch below before reaching this comparison.
	//
	// Take personaSyncMu around the read-then-upsert pair so a
	// concurrent PutAgentPersona (which holds the same mutex)
	// can't race us: without this, a Web client's PUT could land
	// between our GetAgentPersona and UpsertAgentPersona, and our
	// blind AllowOverwrite would clobber it. The lock is per-agent
	// so saves for unrelated agents don't queue.
	//
	// CRITICAL: syncPersona / PutAgentPersona MUST NOT call this
	// path while holding personaSyncMu — that would self-deadlock.
	// They handle their own persona-row update inline under the
	// lock, then call upsertAgentSkipPersona which routes here
	// with skipPersona=true.
	if skipPersona {
		return nil
	}
	releaseSync := lockPersonaSync(a.ID)
	defer releaseSync()

	// Stale-snapshot defeat: m.save() captures `a` early (under
	// m.mu) and releases the manager mutex before this call lands.
	// If a concurrent PutAgentPersona / syncPersona updated disk +
	// DB between snapshot and now, releasing our personaSyncMu
	// during their write means we'd reach this point with `a`
	// reflecting the OLD persona. Writing a.Persona blindly would
	// roll back the DB to the snapshot's view. Re-read the file
	// under personaSyncMu (canonical for the CLI process per the
	// design doc) to get the live state.
	//
	// Branch on read result:
	//   - present + readable: use disk content
	//   - ENOENT (file deleted out from under us): treat as ""
	//     so the row tombstones / no-ops correctly
	//   - real I/O error: SKIP the persona-row write entirely. A
	//     fallback to snapshot would let a stale `a.Persona` from
	//     a long-ago in-memory state revert a fresh DB row that a
	//     concurrent PUT just landed. Better to log + leave the DB
	//     untouched; the next sync retry (no longer in I/O-error
	//     state) reconciles.
	livePersona, fileExists, fileReadErr := readPersonaForSync(a.ID)
	if fileReadErr != nil {
		st.logger.Warn("upsertAgent: skipping persona row write (disk read failed)",
			"agent", a.ID, "err", fileReadErr)
		return nil
	}
	prev, perr := st.db.GetAgentPersona(ctx, a.ID)
	if perr != nil && !errors.Is(perr, store.ErrNotFound) {
		return fmt.Errorf("read persona: %w", perr)
	}

	// Disk-hydrate path. Post-cutover the DB is canonical for persona;
	// disk is a local mirror the CLI reads. On missing-disk + live DB
	// row, hydrate disk from DB instead of clobbering DB to "" — this
	// covers (a) first boot after v0→v1 migration where the importer
	// populated the DB but never wrote disk, (b) any later boot where
	// the operator wiped agentDir manually. Trade-off: `rm persona.md`
	// from the CLI no longer auto-clears; the next load re-hydrates.
	// CLI users that genuinely want to clear persona must round-trip
	// through Web UI / API PUT body="" (PutAgentPersona writes through
	// to DB) or PATCH /api/v1/agents/{id} with persona="" (Manager.
	// Update writes through to DB). There is no SoftDeleteAgentPersona
	// — the schema treats body="" as the canonical clear state. See
	// docs §2.3 / §5.5 (DB canonical, disk mirror).
	if !fileExists && prev != nil && prev.DeletedAt == nil && prev.Body != "" {
		if err := writePersonaFile(a.ID, prev.Body); err != nil {
			st.logger.Warn("upsertAgent: hydrate persona disk from DB failed",
				"agent", a.ID, "err", err)
		}
		return nil
	}

	if !fileExists {
		livePersona = ""
	}
	newSHA := sha256Hex(livePersona)
	switch {
	case prev != nil && prev.BodySHA256 == newSHA:
		// already in sync
	case prev == nil && livePersona == "":
		// no prior row, nothing to write
	default:
		if _, err := st.db.UpsertAgentPersona(ctx, a.ID, livePersona, "", store.AgentInsertOptions{
			UpdatedAt:      updated,
			AllowOverwrite: true,
		}); err != nil {
			return fmt.Errorf("upsert persona: %w", err)
		}
	}
	return nil
}

// sha256Hex returns the lowercase hex-encoded SHA256 of body. Mirrors
// store.SHA256Hex without pulling that helper into the agent package's
// public surface — the import dependency is fine, but the name
// collision with future kojo-side helpers is a foot-gun we'd rather
// avoid.
func sha256Hex(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// Load reads every live agent from the DB, joining persona, and applies
// the same legacy-data normalization the file-based loader did so
// downstream code doesn't have to know which path the row came from.
//
// A row whose settings_json fails to decode is logged and skipped; the
// rest of the agent set still loads. Aborting on one bad row would
// strand every other agent on the daemon.
func (st *agentStore) Load() ([]*Agent, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	recs, err := st.db.ListAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}

	agents := make([]*Agent, 0, len(recs))
	for _, rec := range recs {
		a := &Agent{}
		if err := settingsToAgent(rec, a); err != nil {
			st.logger.Warn("failed to decode agent settings; skipping", "agent", rec.ID, "err", err)
			continue
		}
		// Persona join — best-effort, missing persona is normal for
		// agents created before persona.md was written.
		//
		// upsertAgent skips the agents-row Update when only the persona
		// changed, so agents.updated_at can lag behind persona.updated_at.
		// Take max(agent, persona) for Agent.UpdatedAt so the API surface
		// reflects the most recent mutation regardless of which row it
		// landed in. Without this a persona-only edit looks "older" than
		// it is across a daemon restart.
		if p, err := st.db.GetAgentPersona(ctx, rec.ID); err == nil {
			a.Persona = p.Body
			if p.UpdatedAt > rec.UpdatedAt {
				a.UpdatedAt = millisToRFC3339(p.UpdatedAt)
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			st.logger.Warn("failed to load persona", "agent", rec.ID, "err", err)
		}
		st.normalizeAgent(a)
		agents = append(agents, a)
	}

	// Reconcile MEMORY.md and memory/*.md from disk into their DB
	// rows for every agent we just loaded. This catches:
	//   - legacy agents created before the cutover (no DB row yet)
	//   - CLI-direct edits that landed while the daemon was down
	//   - file deletions that need to tombstone DB rows
	// Best-effort: a single agent's sync failure logs and the rest
	// proceed, so one bad on-disk file can't strand the whole load.
	for _, a := range agents {
		if err := SyncAgentMemoryFromDisk(ctx, st.db, a.ID, st.logger); err != nil {
			st.logger.Warn("memory sync at load failed", "agent", a.ID, "err", err)
		}
	}
	return agents, nil
}

// LoadByID returns a single agent loaded freshly from the
// store. Used by the §3.7 device-switch agent-sync hook to
// reflect a peer-pushed row into the in-memory Manager cache
// without re-running the full Load() side effects (memory.md
// disk sync) that boot wants. Returns store.ErrNotFound when no
// row exists for id.
func (st *agentStore) LoadByID(id string) (*Agent, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rec, err := st.db.GetAgent(ctx, id)
	if err != nil {
		return nil, err
	}
	a := &Agent{}
	if err := settingsToAgent(rec, a); err != nil {
		return nil, fmt.Errorf("settings decode: %w", err)
	}
	if p, perr := st.db.GetAgentPersona(ctx, rec.ID); perr == nil {
		a.Persona = p.Body
		if p.UpdatedAt > rec.UpdatedAt {
			a.UpdatedAt = millisToRFC3339(p.UpdatedAt)
		}
	} else if !errors.Is(perr, store.ErrNotFound) {
		return nil, fmt.Errorf("persona: %w", perr)
	}
	st.normalizeAgent(a)
	return a, nil
}

// agentToSettings encodes Agent into the settings_json blob, dropping
// fields routed to typed columns or to adjacent tables, and merging in
// any previously-unknown keys from prior so a downgraded binary doesn't
// silently strip forward-compatible fields a successor wrote.
//
// The roundtrip via JSON is deliberate: settings_json is canonicalized
// by the same json.Marshal path the rest of the store uses, so the
// computed etag is stable across Save/Load cycles.
func agentToSettings(a *Agent, prior map[string]any) (map[string]any, error) {
	buf, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, err
	}
	for k := range reservedAgentKeys {
		delete(m, k)
	}
	// Forward-compat: copy keys from prior that this binary doesn't
	// model on Agent. Known keys are owned by the current Agent type
	// (m already has them); unknown keys come from prior so they survive
	// a write by a binary that didn't define them.
	for k, v := range prior {
		if reservedAgentKeys[k] {
			continue
		}
		if _, known := m[k]; known {
			continue
		}
		m[k] = v
	}
	return m, nil
}

// settingsToAgent rehydrates an Agent from a record's column data and
// settings_json. Persona is filled in by Load() after the join; this
// helper deliberately leaves a.Persona zero so an out-of-band caller
// can't accidentally surface stale settings_json["persona"] (the
// reserved-key guard would already have removed it on save, but a row
// written by an older binary may still carry it).
//
// CreatedAt/UpdatedAt are sourced from the typed int64 columns — those
// are authoritative after the importer ran, and the prior file-based
// store wrote RFC3339 strings into both places on every Save. The
// settings JSON copy (if present) is ignored to avoid divergence.
func settingsToAgent(rec *store.AgentRecord, out *Agent) error {
	m := make(map[string]any, len(rec.Settings)+4)
	for k, v := range rec.Settings {
		// loadStripKeys (NOT reservedAgentKeys) — Load must let legacy
		// keys through so normalizeAgent's transient Legacy* fields can
		// catch them and run the migration. The Save side filters them
		// out via the broader reservedAgentKeys set.
		if loadStripKeys[k] {
			continue
		}
		m[k] = v
	}
	m["id"] = rec.ID
	m["name"] = rec.Name
	if rec.CreatedAt != 0 {
		m["createdAt"] = millisToRFC3339(rec.CreatedAt)
	}
	if rec.UpdatedAt != 0 {
		m["updatedAt"] = millisToRFC3339(rec.UpdatedAt)
	}
	buf, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(buf, out)
}

// normalizeAgent runs the post-load fixups: timestamp normalization,
// legacy intervalMinutes → cronExpr migration, legacy activeStart/activeEnd
// → silentStart/silentEnd, and validation of clamp-able fields. Mutations
// are mirrored back into the DB only on the next Save() — load itself never
// writes (a re-save loop on every boot would churn the change feed for
// cosmetic fixups).
//
// WorkDir is intentionally NOT cleared here even when the path doesn't
// resolve on this peer: agents are global but workDir is peer-local, so
// dropping it would propagate the empty value to every other peer on
// the next Save(). The runtime check at PTY launch time still validates
// existence; an unreachable WorkDir surfaces there as a "directory does
// not exist" error rather than a silent reset. (Phase 4 introduces
// workspace_paths to lift this per-peer.)
func (st *agentStore) normalizeAgent(a *Agent) {
	a.CreatedAt = normalizeTimestamp(a.CreatedAt)
	a.UpdatedAt = normalizeTimestamp(a.UpdatedAt)

	// Migrate legacy intervalMinutes → cronExpr. The transient
	// LegacyIntervalMinutes field captures the old JSON key so a row
	// written before the cutover can be rehydrated; intervalToCronExpr
	// returns "" for values outside the legacy whitelist (5/10/30/60/240/
	// 1440), in which case we disable scheduling rather than translate to
	// a silently-wrong cadence.
	if a.CronExpr == "" && a.LegacyIntervalMinutes > 0 {
		a.CronExpr = intervalToCronExpr(a.LegacyIntervalMinutes, a.ID)
		if a.CronExpr == "" {
			st.logger.Warn("legacy intervalMinutes outside allowed whitelist; disabling schedule",
				"agent", a.ID, "intervalMinutes", a.LegacyIntervalMinutes)
		}
	}
	a.LegacyIntervalMinutes = 0

	// Validate loaded cronExpr — clear invalid values so the agent
	// loads with scheduling disabled instead of crashing the cron
	// scheduler at Start(). Also expand any "@preset:N" sentinel that
	// somehow landed in the DB (peer-replicated row, hand-edited JSON,
	// pre-resolve regression) so the runtime scheduler always sees a
	// real 5-field expression. The Save side resolves sentinels too;
	// this is a belt-and-suspenders for inputs that bypassed it.
	if a.CronExpr != "" {
		if err := ValidateCronExpr(a.CronExpr); err != nil {
			st.logger.Warn("invalid cronExpr in stored data, disabling schedule",
				"agent", a.ID, "cronExpr", a.CronExpr, "err", err)
			a.CronExpr = ""
		} else {
			a.CronExpr = ResolveCronPreset(a.CronExpr, a.ID)
		}
	}

	if !ValidResumeIdle(a.ResumeIdleMinutes) {
		st.logger.Warn("invalid resumeIdleMinutes in stored data, resetting to default",
			"agent", a.ID, "value", a.ResumeIdleMinutes)
		a.ResumeIdleMinutes = 0
	}
	if a.LegacyActiveStart != "" && a.LegacyActiveEnd != "" && a.SilentStart == "" && a.SilentEnd == "" {
		a.SilentStart = a.LegacyActiveEnd
		a.SilentEnd = a.LegacyActiveStart
		if a.NotifyDuringSilent == nil {
			t := true
			a.NotifyDuringSilent = &t
		}
		st.logger.Info("migrated active hours → silent hours",
			"agent", a.ID,
			"silentStart", a.SilentStart, "silentEnd", a.SilentEnd)
	}
	a.LegacyActiveStart = ""
	a.LegacyActiveEnd = ""

	if err := ValidSilentHours(a.SilentStart, a.SilentEnd); err != nil {
		st.logger.Warn("invalid silent hours in stored data, clearing",
			"agent", a.ID, "start", a.SilentStart, "end", a.SilentEnd, "err", err)
		a.SilentStart = ""
		a.SilentEnd = ""
	}
}

// parseAgentRFC3339Millis converts an RFC3339 string to ms-since-epoch
// for the agents table's int64 created_at / updated_at columns. Returns
// 0 when the input is empty or unparseable; upsertAgent falls back to
// store.NowMillis() in that case so the DB row never carries a 0
// timestamp.
//
// The agents.go importer has a similar helper; this one is local to
// avoid pulling internal/migrate/importers (a sibling package) into
// the agent runtime path.
func parseAgentRFC3339Millis(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// millisToRFC3339 converts ms-since-epoch back to a local-tz RFC3339
// string for *Agent.CreatedAt / UpdatedAt. Returns "" for a zero input
// so callers can treat 0 as "unset".
func millisToRFC3339(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).Local().Format(time.RFC3339)
}

// parseCronPausedRow returns the boolean reading of a kv row claiming to
// hold the cron pause state. Returns ok=false if the row is malformed —
// type/scope/secret mismatch or value other than the literal "true"/"false".
// Caller fails closed on ok=false (treats the flag as paused) so a corrupt
// row produced by peer replication or manual SQL surgery doesn't silently
// run schedules on what should be a paused install.
func parseCronPausedRow(rec *store.KVRecord) (paused, ok bool) {
	if rec == nil {
		return false, false
	}
	if rec.Type != store.KVTypeString || rec.Scope != store.KVScopeGlobal || rec.Secret {
		return false, false
	}
	switch rec.Value {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

// LoadCronPaused returns true if cron is paused. The flag is canonically
// stored in the kv table (namespace="scheduler", key="paused"); a missing
// row means "not paused".
//
// Migration: on first call after upgrade from v0, if the kv row is absent
// AND the legacy marker file exists, mirror the value into kv (using
// IfMatchAny so a concurrent peer-replicated insert can't be silently
// overwritten) and remove the file. A failed kv write leaves the file in
// place so a retry on the next boot picks it up.
//
// Failure posture: kv read errors and malformed rows fail CLOSED (return
// true). An operator who deliberately paused cron does not want a transient
// DB hiccup or a corrupt-row peer-replication event to silently resume
// schedules. The legitimate "fresh install / no row / no file" case is
// distinguished by store.ErrNotFound and falls through to the file check;
// any OTHER error path biases toward staying paused. Operator sees the
// Warn log and investigates the DB rather than discovering cron silently
// ran during a paused window.
func (st *agentStore) LoadCronPaused() bool {
	ctx, cancel := context.WithTimeout(context.Background(), cronPausedKVTimeout)
	defer cancel()

	rec, err := st.db.GetKV(ctx, cronPausedKVNamespace, cronPausedKVKey)
	switch {
	case err == nil:
		// Row present — value is the source of truth. Validate
		// row-shape; treat malformed rows as "paused" (fail closed)
		// so a peer-replicated junk row doesn't silently un-pause.
		paused, ok := parseCronPausedRow(rec)
		if !ok {
			st.logger.Warn("LoadCronPaused: malformed kv row; failing closed (paused)",
				"value", rec.Value, "type", rec.Type, "scope", rec.Scope, "secret", rec.Secret)
			return true
		}
		// Tolerate a pre-existing legacy file (e.g. a v0 → v1 → v0 → v1
		// round trip) by removing it once kv is authoritative; best-
		// effort.
		_ = os.Remove(filepath.Join(st.dir, "agents", cronPausedFile))
		return paused
	case errors.Is(err, store.ErrNotFound):
		// Fall through to legacy-file migration.
	default:
		st.logger.Warn("LoadCronPaused: kv read failed; failing closed (paused)",
			"err", err)
		return true
	}

	// kv miss. Check legacy marker file.
	legacyPath := filepath.Join(st.dir, "agents", cronPausedFile)
	if _, statErr := os.Stat(legacyPath); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			// No file either → never been paused. Default false
			// (legitimate fresh-install case, not an error).
			return false
		}
		// Real I/O error reading the legacy path (EACCES on a
		// hardened filesystem, transient I/O failure, etc.). We
		// can't tell whether the file says "paused" or not, so
		// fail closed for this boot — operator's prior pause may
		// or may not still be in effect, biasing toward "paused"
		// matches the kv-error policy and the rest of the routine.
		st.logger.Warn("LoadCronPaused: legacy file stat failed; failing closed (paused)",
			"path", legacyPath, "err", statErr)
		return true
	}

	// Test-only injection point: tests use this to slide a colliding
	// kv row into the table before the migration PutKV runs, so the
	// IfMatchAny ErrETagMismatch branch is exercised end-to-end.
	if cronPausedMigrationTestHook != nil {
		cronPausedMigrationTestHook()
	}

	// File exists → "paused". Mirror into kv with IfMatchAny so a
	// concurrent insert (peer replication, two processes booting
	// simultaneously) doesn't get clobbered. On collision, re-read
	// the row and honour whatever's there.
	mig := &store.KVRecord{
		Namespace: cronPausedKVNamespace,
		Key:       cronPausedKVKey,
		Value:     "true",
		Type:      store.KVTypeString,
		Scope:     store.KVScopeGlobal,
	}
	_, err = st.db.PutKV(ctx, mig, store.KVPutOptions{IfMatchETag: store.IfMatchAny})
	switch {
	case err == nil:
		// We won the race. Drop the legacy file.
		if rmErr := os.Remove(legacyPath); rmErr != nil && !os.IsNotExist(rmErr) {
			st.logger.Warn("LoadCronPaused: legacy file unlink failed after kv mirror",
				"path", legacyPath, "err", rmErr)
		}
		return true
	case errors.Is(err, store.ErrETagMismatch):
		// Someone beat us to the row. Re-read; honour whatever
		// they wrote. The legacy file is no longer authoritative
		// regardless of what we read back, so drop it.
		rec, getErr := st.db.GetKV(ctx, cronPausedKVNamespace, cronPausedKVKey)
		if getErr != nil {
			// Re-read failed. Keep the file in place; next boot
			// retries. Fail closed for this boot.
			st.logger.Warn("LoadCronPaused: post-collision kv re-read failed; failing closed (paused)",
				"err", getErr)
			return true
		}
		paused, ok := parseCronPausedRow(rec)
		if !ok {
			st.logger.Warn("LoadCronPaused: post-collision kv row malformed; failing closed (paused)",
				"value", rec.Value, "type", rec.Type, "scope", rec.Scope, "secret", rec.Secret)
			return true
		}
		if rmErr := os.Remove(legacyPath); rmErr != nil && !os.IsNotExist(rmErr) {
			st.logger.Warn("LoadCronPaused: legacy file unlink failed after collision-resolve",
				"path", legacyPath, "err", rmErr)
		}
		return paused
	default:
		st.logger.Warn("LoadCronPaused: legacy file present but kv mirror failed; will retry next boot, failing closed for this boot",
			"err", err)
		return true // honour the file so the operator's pause sticks.
	}
}

// SaveCronPaused upserts the kv row reflecting the paused flag and best-
// effort removes the legacy marker file. Returns an error so the caller
// (Manager.SetCronPaused → HTTP handler) can refuse the toggle when the
// underlying store is unreachable, rather than acknowledging a request
// that hasn't been persisted.
//
// The legacy file is NOT written as a fallback on kv failure: a stray
// file from a prior boot would confuse the next LoadCronPaused, which
// already favours "paused" on any ambiguous read. Cleanup of the legacy
// file is best-effort; failures there are logged but do not fail the
// SaveCronPaused call (kv is authoritative once written).
func (st *agentStore) SaveCronPaused(paused bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), cronPausedKVTimeout)
	defer cancel()

	val := "false"
	if paused {
		val = "true"
	}
	rec := &store.KVRecord{
		Namespace: cronPausedKVNamespace,
		Key:       cronPausedKVKey,
		Value:     val,
		Type:      store.KVTypeString,
		Scope:     store.KVScopeGlobal,
	}
	if _, err := st.db.PutKV(ctx, rec, store.KVPutOptions{}); err != nil {
		st.logger.Warn("SaveCronPaused: kv write failed", "paused", paused, "err", err)
		return fmt.Errorf("save cron paused: %w", err)
	}

	// Remove the legacy marker if it survived a prior crash between
	// the kv write and the file unlink. Best-effort — kv is already
	// authoritative; a leftover file gets cleaned up by the next
	// LoadCronPaused or by `kojo --clean legacy` (slice 19).
	legacyPath := filepath.Join(st.dir, "agents", cronPausedFile)
	if err := os.Remove(legacyPath); err != nil && !os.IsNotExist(err) {
		st.logger.Warn("SaveCronPaused: legacy file unlink failed",
			"path", legacyPath, "err", err)
	}
	return nil
}

// agentsDir returns the base directory for all per-agent on-disk
// artifacts. After the Phase 2c-2 cutover (slices 5–13) the only
// files still hosted under each per-agent subdir are:
//
//   - the FTS index dir (index/)
//   - the persona.md / MEMORY.md / memory/*.md disk mirrors that
//     the CLI subprocess (claude / gemini / codex) reads at chat
//     time
//   - persona_summary.md (regeneratable cache)
//   - the per-agent CLI workspace (.claude/, .gemini/, and .codex/
//     if the codex CLI happens to create one — codex's primary
//     session store is global at ~/.codex/sessions/, see
//     docs/multi-device-storage.md §5.5.1.3)
//   - GEMINI.md (project-instruction file written by
//     internal/agent/backend_gemini.go's prepareGeminiDir to
//     override Gemini CLI's built-in system prompt; recreated on
//     every Chat invocation, never authoritative state)
//   - chat_history/ (external-platform — Slack etc. — local cache;
//     cursor lives in DB, see internal/chathistory/store.go)
//   - any legacy file left mid-migration that the runtime now
//     reads from kv / DB / blob: tasks.json (slice 5),
//     autosummary_marker (slice 9), .cron_last (slice 12),
//     avatar.<ext> (slice 13), and credentials.json (migrated
//     into credentials.db by internal/agent/credential.go's
//     migrateLegacy)
//
// Canonical persona, MEMORY, memory entries, transcripts, tasks,
// the autosummary marker (kv), cron pause state (kv), the cron
// throttle (kv slice 12), the avatar blob (blob.ScopeGlobal slice
// 13), credentials, etc. all live outside agentsDir: the avatar
// BODY at <configdir>/global/agents/<id>/avatar.<ext> (blob.Store's
// root is configdir.Path itself, the per-scope subdir is just the
// scope name) with its metadata in the blob_refs table;
// credentials.db sits at configdir.Path() directly; everything
// else is in kojo.db.
func agentsDir() string {
	return filepath.Join(configdir.Path(), "agents")
}

// agentDir returns the data directory for a specific agent.
func agentDir(id string) string {
	return filepath.Join(agentsDir(), id)
}
