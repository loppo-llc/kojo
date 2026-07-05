package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// Hub-local vs holder-only PATCH field classification (§3.7 device
// switch).
//
// While an agent's runtime lock is held by a remote peer, PATCH
// /api/v1/agents/{id} is normally reverse-proxied to the holder so
// the holder's in-memory Agent (the write authority) applies the
// change. That proxy fails with peer_offline when the holder is not
// reachable — which used to make even a display-name edit impossible
// until the holder came back.
//
// Fields whose application is a pure hub-DB row write (no disk
// side-effect, no live backend-session or scheduler mutation on the
// holder) are classified HUB-LOCAL-SAFE. For those the hub may fall
// back to writing its own agents row when the holder is offline (or
// unknown), and may apply them mid-switch. The holder picks the
// change up through the existing §3.7 sync flows (see
// UpdateRemoteHubRow doc below for the consistency contract).
//
// HUB-LOCAL-SAFE (pure agents-row fields):
//   - name                  (display name)
//   - publicProfile         (notes / profile text)
//   - publicProfileOverride
//   - effort                (read per-turn, no session mutation)
//   - autoEffort
//   - disabledInjections    (context-injection checklist)
//   - silentStart / silentEnd / notifyDuringSilent
//
// HOLDER-ONLY (mutate holder disk / live session / scheduler —
// keep the proxy + switching lock):
//   - persona               (persona.md on holder disk)
//   - model / tool / customBaseURL / thinkingMode (live backend session)
//   - workDir               (holder filesystem)
//   - cronExpr              (holder cron scheduler reschedule)
//   - timeoutMinutes / resumeIdleMinutes (live session watchdogs)
//   - allowedTools / allowProtectedPaths (live session permissions)
//   - tts                   (holder-side synthesis config)
//   - deviceSwitchEnabled   (installs/removes SKILL.md on holder disk)
//   - cronMessage           (persisted via agent_workspace_files
//     kind=checkin — NOT in the agents settings_json row, so a
//     hub-row-only write would silently no-op)
//
// Non-PATCH mutation routes stay holder-only for the same reasons:
// avatar upload (mutable path-addressed blob served from the holder),
// PUT /persona, PUT /status + workspace files (holder disk), memory
// writes, transcript edit/reset, credentials, tasks, sessions.
var hubLocalSafePatchKeys = map[string]bool{
	"name":                  true,
	"publicprofile":         true,
	"publicprofileoverride": true,
	"effort":                true,
	"autoeffort":            true,
	"disabledinjections":    true,
	"silentstart":           true,
	"silentend":             true,
	"notifyduringsilent":    true,
}

// IsHubLocalSafePatchKey reports whether a top-level PATCH body key
// is classified hub-local-safe. Matching is case-insensitive to
// mirror encoding/json's field matching (and the privileged /
// disabledInjections casing guards in handleUpdateAgent). Callers
// must lower-case the key.
func IsHubLocalSafePatchKey(k string) bool {
	return hubLocalSafePatchKeys[k]
}

// HubLocalOnly reports whether the update touches ONLY
// hub-local-safe fields (an empty config counts — applying it is a
// harmless no-op). Struct-based twin of IsHubLocalSafePatchKey used
// on paths that already decoded the body.
func (cfg *AgentUpdateConfig) HubLocalOnly() bool {
	if cfg == nil {
		return true
	}
	return cfg.Persona == nil &&
		cfg.CronMessage == nil &&
		cfg.Model == nil &&
		cfg.Tool == nil &&
		cfg.WorkDir == nil &&
		cfg.CronExpr == nil &&
		cfg.LegacyIntervalMinutes == nil &&
		cfg.TimeoutMinutes == nil &&
		cfg.ResumeIdleMinutes == nil &&
		cfg.CustomBaseURL == nil &&
		cfg.ThinkingMode == nil &&
		cfg.AllowedTools == nil &&
		cfg.AllowProtectedPaths == nil &&
		cfg.TTS == nil &&
		cfg.DeviceSwitchEnabled == nil
}

// UpdateRemoteHubRow applies a hub-local-safe PATCH to the hub's own
// agents row for an agent whose runtime is held by a remote peer.
// Used by the offline-holder fallback in remoteAgentProxyMiddleware;
// the online-holder path keeps proxying so the holder's in-memory
// agent stays authoritative.
//
// Consistency contract: there is no continuous agents-row sync
// between hub and holder — the row travels only inside the §3.7
// peer_agent_sync payload at device-switch time, and that ingest is
// source-wins. A value written here is immediately visible on every
// hub read (GET / list go through GetRemote → hub row). To keep an
// acknowledged 200 from being SILENTLY clobbered by that source-wins
// ingest, every successful write also records a pending override (kv
// row; recordHubLocalOverride) which the sync-ingest handler re-applies
// on top of the incoming row via ReapplyHubLocalOverrides — unless the
// incoming row is newer than the hub write, in which case the
// holder-side edit is the user's later intent and legitimately wins.
//
// No AcquireMutation here: the agent is not in the local in-memory
// map, so there is no local runtime state to fence. Per-agent HTTP
// serialization (LockPatch) is the caller's job, same as
// handleUpdateAgent.
// casIfMatch, when non-empty, is passed to store.UpdateAgent as the
// row-etag precondition so the If-Match check is atomic with the
// UPDATE (stronger than handleUpdateAgent's precheck-then-write);
// mismatch surfaces as store.ErrETagMismatch.
func (m *Manager) UpdateRemoteHubRow(id string, cfg AgentUpdateConfig, casIfMatch string) (*Agent, error) {
	return m.updateRemoteHubRowInner(id, cfg, casIfMatch, true)
}

// updateRemoteHubRowInner is UpdateRemoteHubRow with the override
// recording made optional: ReapplyHubLocalOverrides calls it with
// record=false so a re-apply never re-arms the kv row it is about to
// clear (which would make the override self-perpetuating and shadow a
// concurrent PATCH's fresher override at the conditional delete).
func (m *Manager) updateRemoteHubRowInner(id string, cfg AgentUpdateConfig, casIfMatch string, record bool) (*Agent, error) {
	if m == nil || m.store == nil {
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}
	if !cfg.HubLocalOnly() {
		return nil, fmt.Errorf("update contains holder-only fields; must be applied on the holder peer")
	}
	// Same pure-input validation barrier as Manager.Update.
	if _, _, err := validateUpdateConfigPure(&cfg); err != nil {
		return nil, err
	}

	// Guard against the agent having been (re)attached locally between
	// the middleware's Get and this call — the in-memory runtime must
	// stay the write authority when it exists.
	m.mu.Lock()
	_, inMem := m.agents[id]
	m.mu.Unlock()
	if inMem {
		return nil, fmt.Errorf("%w: agent %s re-attached locally; retry", ErrAgentBusy, id)
	}

	// Read-validate-apply runs INSIDE store.UpdateAgent's transaction
	// (updateAgentRowCAS decodes the freshly-read record), so a
	// concurrent peer_agent_sync ingest cannot be clobbered by a stale
	// snapshot — the loser of the row race simply applies on top of
	// the winner's committed state.
	// Two-phase override record (review rounds 4+5):
	//
	//   phase 1  record PENDING (merge-INELIGIBLE) with the base
	//            captured from the pre-write row
	//   phase 2  agents-row CAS
	//   phase 3  CONFIRM the pending section (merge-eligible)
	//
	// Why two-phase: a record that is merge-eligible before the row
	// commit can materialize a write the client was told failed (row
	// CAS fails + cleanup fails → ghost). A PENDING section is ignored
	// by the ingest merge, so every failure combination is safe:
	//   - phase-1 failure → 500, nothing written anywhere
	//   - phase-2 failure → pending cleared best-effort; even if the
	//     clear ALSO fails, the leftover pending is merge-ineligible
	//     and dropped lazily (resolveStalePending / ingest GC) — no
	//     ghost apply, no silent materialization
	//   - phase-3 failure → row committed but 500 returned; the
	//     client retry lazily confirms the leftover pending (its cfg
	//     matches the committed row) with the ORIGINAL base, so the
	//     3-way merge base is never corrupted by the retry
	//
	// Caller-side serialization (LockPatch on the HTTP path; the
	// ingest re-apply also takes LockPatch) keeps the pre-read base
	// coherent with the row the CAS mutates.
	var recordETag string
	if record {
		row, lerr := m.store.LoadByID(id)
		if lerr != nil {
			if errors.Is(lerr, store.ErrNotFound) {
				return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, id)
			}
			return nil, fmt.Errorf("%w: read agent row: %v", ErrHubStorageFailure, lerr)
		}
		base := captureHubLocalBase(row, cfg)
		etag, oerr := m.recordHubLocalOverridePending(id, cfg, base, row)
		if oerr != nil {
			return nil, fmt.Errorf("%w: %v", ErrOverrideRecordFailed, oerr)
		}
		recordETag = etag
	}

	a, err := m.store.updateAgentRowCAS(id, casIfMatch, func(a *Agent) error {
		return applyHubLocalCfg(a, cfg)
	})
	if err != nil {
		if record {
			m.clearHubLocalOverridePending(id, recordETag)
		}
		return nil, err
	}
	if record {
		if oerr := m.confirmHubLocalOverridePending(id); oerr != nil {
			// Row committed; the pending section survives and is
			// lazily confirmed by the client's retry (see above).
			return nil, fmt.Errorf("%w: confirm: %v", ErrOverrideRecordFailed, oerr)
		}
	}
	return a, nil
}

// ErrOverrideRecordFailed marks a PATCH whose pending-override kv
// bookkeeping could not be persisted. Surfaced as 500; safe to retry
// (phase-1 failure wrote nothing, phase-3 failure is lazily healed by
// the retry).
var ErrOverrideRecordFailed = errors.New("hub-local override record failed; retry the request")

// ErrHubStorageFailure marks a non-not-found store read/write failure
// on the hub-local settings path; mapped to 500 (not 404/400).
var ErrHubStorageFailure = errors.New("hub-local settings storage failure")

// applyHubLocalCfg validates cfg against the fresh agent state and
// applies the hub-safe fields. Shared by the remote-row CAS path and
// the ingest-time override re-apply.
func applyHubLocalCfg(a *Agent, cfg AgentUpdateConfig) error {
	// Cross-field checks against the fresh row (mirrors Update).
	prospEffort := a.Effort
	if cfg.Effort != nil {
		prospEffort = *cfg.Effort
	}
	if !ValidModelEffort(a.Model, prospEffort) {
		return fmt.Errorf("unsupported effort level %q for model %q", prospEffort, a.Model)
	}
	prospSilentS, prospSilentE := a.SilentStart, a.SilentEnd
	if cfg.SilentStart != nil {
		prospSilentS = *cfg.SilentStart
	}
	if cfg.SilentEnd != nil {
		prospSilentE = *cfg.SilentEnd
	}
	if err := ValidSilentHours(prospSilentS, prospSilentE); err != nil {
		return err
	}

	if cfg.Name != nil {
		a.Name = *cfg.Name
	}
	if cfg.PublicProfile != nil {
		a.PublicProfile = *cfg.PublicProfile
	}
	if cfg.PublicProfileOverride != nil {
		a.PublicProfileOverride = *cfg.PublicProfileOverride
	}
	if cfg.Effort != nil {
		a.Effort = *cfg.Effort
	}
	if cfg.AutoEffort != nil {
		a.AutoEffort = cfg.AutoEffort
	}
	if cfg.DisabledInjections != nil {
		a.DisabledInjections = normalizeDisabledInjections(*cfg.DisabledInjections)
	}
	if cfg.SilentStart != nil {
		a.SilentStart = *cfg.SilentStart
	}
	if cfg.SilentEnd != nil {
		a.SilentEnd = *cfg.SilentEnd
	}
	if cfg.NotifyDuringSilent != nil {
		a.NotifyDuringSilent = cfg.NotifyDuringSilent
	}
	// NOTE: no publicProfile LLM regen here (resolvePublicProfile)
	// — persona is holder-only so the persona-edit regen trigger
	// can't fire, and a hub-side LLM call against a remote agent
	// would be pure cost. Flipping publicProfileOverride back to
	// false keeps the last stored profile text until the holder's
	// own regen path runs; documented divergence, not silent loss.
	a.UpdatedAt = time.Now().Format(time.RFC3339)
	return nil
}

// --- pending hub-local overrides (survive source-wins sync) ---

// kv row: namespace "device_switch", key "hub_local_overrides/<id>",
// scope machine (hub-side bookkeeping; never replicated).
const hubLocalOverrideNamespace = "device_switch"

func hubLocalOverrideKey(agentID string) string {
	return "hub_local_overrides/" + agentID
}

// hubLocalOverrideRecord accumulates every hub-local field write made
// while the agent was remote (or mid-switch).
//
// Cfg holds the values the hub acknowledged; Base holds, per
// overridden field, the value the field had just BEFORE the first hub
// write (the same value the holder departed with). Conflict
// resolution at sync ingest is a clock-free per-field 3-way merge:
//
//	incoming == base → holder never touched the field → hub edit wins
//	incoming != base → holder changed it later → holder wins (logged)
//
// No cross-peer timestamp comparison is involved (whole-row
// updated_at advances on unrelated activity and peer clocks are not
// comparable). UpdatedAtMs is informational only.
// Two-phase shape: Cfg/Base are the CONFIRMED (merge-eligible)
// override; Pending is an in-flight PATCH's record that only becomes
// merge-eligible after its agents-row CAS commits (confirm phase). A
// leftover Pending (crash / failed PATCH / failed cleanup) is either
// lazily confirmed by resolveStalePending — when the row provably
// carries its values — or dropped; it is never merged as-is.
type hubLocalOverrideRecord struct {
	Cfg       AgentUpdateConfig        `json:"cfg"`
	Base      AgentUpdateConfig        `json:"base"`
	Pending   *hubLocalOverridePending `json:"pending,omitempty"`
	UpdatedAt int64                    `json:"updatedAtMs"`
}

type hubLocalOverridePending struct {
	Cfg  AgentUpdateConfig `json:"cfg"`
	Base AgentUpdateConfig `json:"base"`
}

// resolveStalePending settles a leftover Pending section against the
// current agents row: if every pending field already equals its
// target value on the row, the row write committed (its confirm phase
// crashed/failed) → merge it into the confirmed section with its
// ORIGINAL base. Otherwise the row write never landed → drop it.
func resolveStalePending(rec *hubLocalOverrideRecord, row *Agent) {
	if rec.Pending == nil {
		return
	}
	if pendingMatchesRow(rec.Pending.Cfg, row) {
		mergeHubLocalBase(rec, rec.Pending.Base)
		mergeHubLocalCfg(&rec.Cfg, rec.Pending.Cfg)
	}
	rec.Pending = nil
}

// pendingMatchesRow reports whether every non-nil field of cfg equals
// the row's current value — the signature of a committed row write.
func pendingMatchesRow(cfg AgentUpdateConfig, row *Agent) bool {
	boolPtr := func(p *bool) bool { return p != nil && *p }
	if cfg.Name != nil && *cfg.Name != row.Name {
		return false
	}
	if cfg.PublicProfile != nil && *cfg.PublicProfile != row.PublicProfile {
		return false
	}
	if cfg.PublicProfileOverride != nil && *cfg.PublicProfileOverride != row.PublicProfileOverride {
		return false
	}
	if cfg.Effort != nil && *cfg.Effort != row.Effort {
		return false
	}
	if cfg.AutoEffort != nil && *cfg.AutoEffort != boolPtr(row.AutoEffort) {
		return false
	}
	if cfg.DisabledInjections != nil &&
		!equalStringSets(normalizeDisabledInjections(*cfg.DisabledInjections), row.DisabledInjections) {
		return false
	}
	if cfg.SilentStart != nil && *cfg.SilentStart != row.SilentStart {
		return false
	}
	if cfg.SilentEnd != nil && *cfg.SilentEnd != row.SilentEnd {
		return false
	}
	if cfg.NotifyDuringSilent != nil && *cfg.NotifyDuringSilent != boolPtr(row.NotifyDuringSilent) {
		return false
	}
	return true
}

// captureHubLocalBase snapshots, for every non-nil hub-safe field in
// cfg, the agent's CURRENT value — the 3-way-merge base.
func captureHubLocalBase(a *Agent, cfg AgentUpdateConfig) AgentUpdateConfig {
	var base AgentUpdateConfig
	strp := func(v string) *string { return &v }
	boolp := func(v bool) *bool { return &v }
	if cfg.Name != nil {
		base.Name = strp(a.Name)
	}
	if cfg.PublicProfile != nil {
		base.PublicProfile = strp(a.PublicProfile)
	}
	if cfg.PublicProfileOverride != nil {
		base.PublicProfileOverride = boolp(a.PublicProfileOverride)
	}
	if cfg.Effort != nil {
		base.Effort = strp(a.Effort)
	}
	if cfg.AutoEffort != nil {
		base.AutoEffort = boolp(a.AutoEffort != nil && *a.AutoEffort)
	}
	if cfg.DisabledInjections != nil {
		di := append([]string(nil), a.DisabledInjections...)
		base.DisabledInjections = &di
	}
	if cfg.SilentStart != nil {
		base.SilentStart = strp(a.SilentStart)
	}
	if cfg.SilentEnd != nil {
		base.SilentEnd = strp(a.SilentEnd)
	}
	if cfg.NotifyDuringSilent != nil {
		base.NotifyDuringSilent = boolp(a.NotifyDuringSilent != nil && *a.NotifyDuringSilent)
	}
	return base
}

// recordHubLocalOverride merges cfg's non-nil hub-safe fields into the
// pending override row for the agent.
// mutateHubLocalOverrideKV runs a bounded read-mutate-put CAS loop on
// the agent's override kv row. mutate receives the decoded record
// (zero value when the row is missing/corrupt) and returns false to
// abort without writing. Returns the etag of the written row.
func (m *Manager) mutateHubLocalOverrideKV(id string, mutate func(rec *hubLocalOverrideRecord) bool) (string, error) {
	st := m.Store()
	if st == nil {
		return "", errors.New("store not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		rec := hubLocalOverrideRecord{}
		prior, err := st.GetKV(ctx, hubLocalOverrideNamespace, hubLocalOverrideKey(id))
		ifMatch := ""
		switch {
		case err == nil:
			if uerr := json.Unmarshal([]byte(prior.Value), &rec); uerr != nil {
				// Corrupt row: start fresh rather than fail the caller.
				rec = hubLocalOverrideRecord{}
			}
			ifMatch = prior.ETag
		case errors.Is(err, store.ErrNotFound):
			// no row yet
		default:
			return "", err
		}
		if !mutate(&rec) {
			return ifMatch, nil
		}
		rec.UpdatedAt = store.NowMillis()

		buf, err := json.Marshal(rec)
		if err != nil {
			return "", err
		}
		put, err := st.PutKV(ctx, &store.KVRecord{
			Namespace: hubLocalOverrideNamespace,
			Key:       hubLocalOverrideKey(id),
			Value:     string(buf),
			Type:      store.KVTypeJSON,
			Scope:     store.KVScopeMachine,
		}, store.KVPutOptions{IfMatchETag: ifMatch})
		if err == nil {
			return put.ETag, nil
		}
		if !errors.Is(err, store.ErrETagMismatch) {
			return "", err
		}
		lastErr = err
	}
	return "", fmt.Errorf("override kv CAS exhausted: %w", lastErr)
}

// recordHubLocalOverridePending is phase 1 of the two-phase record:
// stash cfg+base in the merge-INELIGIBLE Pending section. Any stale
// pending from an earlier crash/failure is settled against the
// current row first (resolveStalePending), which is what preserves
// the ORIGINAL base across a phase-3-failure retry.
func (m *Manager) recordHubLocalOverridePending(id string, cfg, base AgentUpdateConfig, row *Agent) (string, error) {
	return m.mutateHubLocalOverrideKV(id, func(rec *hubLocalOverrideRecord) bool {
		resolveStalePending(rec, row)
		rec.Pending = &hubLocalOverridePending{Cfg: cfg, Base: base}
		return true
	})
}

// confirmHubLocalOverridePending is phase 3: promote the Pending
// section into the confirmed (merge-eligible) override. Base merge
// runs FIRST and only fills fields the confirmed section doesn't
// already cover, so successive PATCHes to the same field keep the
// original pre-first-write base.
func (m *Manager) confirmHubLocalOverridePending(id string) error {
	_, err := m.mutateHubLocalOverrideKV(id, func(rec *hubLocalOverrideRecord) bool {
		if rec.Pending == nil {
			return false // already confirmed (competing retry) — done
		}
		mergeHubLocalBase(rec, rec.Pending.Base)
		mergeHubLocalCfg(&rec.Cfg, rec.Pending.Cfg)
		rec.Pending = nil
		return true
	})
	return err
}

// clearHubLocalOverridePending drops the Pending section after a
// failed row CAS. Best-effort BY DESIGN: if this fails too, the
// leftover pending stays merge-ineligible — resolveStalePending (next
// PATCH) or the ingest GC drops it, and it is never applied, so a
// failed PATCH can never materialize. expectETag guards against
// clobbering a concurrent writer's newer record.
func (m *Manager) clearHubLocalOverridePending(id, expectETag string) {
	st := m.Store()
	if st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := st.GetKV(ctx, hubLocalOverrideNamespace, hubLocalOverrideKey(id))
	if err != nil || rec.ETag != expectETag {
		return // gone or rewritten by someone else — their state wins
	}
	var ov hubLocalOverrideRecord
	if uerr := json.Unmarshal([]byte(rec.Value), &ov); uerr != nil {
		return
	}
	ov.Pending = nil
	if isEmptyHubLocalCfg(ov.Cfg) {
		// Nothing confirmed either — remove the row entirely.
		if derr := st.DeleteKV(ctx, hubLocalOverrideNamespace, hubLocalOverrideKey(id), rec.ETag); derr != nil &&
			!errors.Is(derr, store.ErrETagMismatch) && !errors.Is(derr, store.ErrNotFound) && m.logger != nil {
			m.logger.Warn("hub-local override pending cleanup failed (merge-ineligible leftover; GC'd later)",
				"agent", id, "err", derr)
		}
		return
	}
	buf, merr := json.Marshal(ov)
	if merr != nil {
		return
	}
	if _, perr := st.PutKV(ctx, &store.KVRecord{
		Namespace: hubLocalOverrideNamespace,
		Key:       hubLocalOverrideKey(id),
		Value:     string(buf),
		Type:      store.KVTypeJSON,
		Scope:     store.KVScopeMachine,
	}, store.KVPutOptions{IfMatchETag: rec.ETag}); perr != nil &&
		!errors.Is(perr, store.ErrETagMismatch) && m.logger != nil {
		m.logger.Warn("hub-local override pending cleanup failed (merge-ineligible leftover; GC'd later)",
			"agent", id, "err", perr)
	}
}

// isEmptyHubLocalCfg reports whether no hub-safe field is set.
func isEmptyHubLocalCfg(cfg AgentUpdateConfig) bool {
	return cfg.Name == nil && cfg.PublicProfile == nil &&
		cfg.PublicProfileOverride == nil && cfg.Effort == nil &&
		cfg.AutoEffort == nil && cfg.DisabledInjections == nil &&
		cfg.SilentStart == nil && cfg.SilentEnd == nil &&
		cfg.NotifyDuringSilent == nil
}

// recordHubLocalOverride records a CONFIRMED override in one step —
// used by the mid-switch Manager.Update path, where the write is
// already applied in-memory and saved before this call (there is no
// later commit to gate on). Stale pendings are settled against the
// current row first.
func (m *Manager) recordHubLocalOverride(id string, cfg, base AgentUpdateConfig) error {
	row, err := m.store.LoadByID(id)
	if err != nil {
		return err
	}
	_, err = m.mutateHubLocalOverrideKV(id, func(rec *hubLocalOverrideRecord) bool {
		resolveStalePending(rec, row)
		mergeHubLocalBase(rec, base)
		mergeHubLocalCfg(&rec.Cfg, cfg)
		return true
	})
	return err
}

// mergeHubLocalCfg overlays src's non-nil hub-safe fields onto dst.
// Holder-only fields are never present here (UpdateRemoteHubRow
// rejects them before recording).
func mergeHubLocalCfg(dst *AgentUpdateConfig, src AgentUpdateConfig) {
	if src.Name != nil {
		dst.Name = src.Name
	}
	if src.PublicProfile != nil {
		dst.PublicProfile = src.PublicProfile
	}
	if src.PublicProfileOverride != nil {
		dst.PublicProfileOverride = src.PublicProfileOverride
	}
	if src.Effort != nil {
		dst.Effort = src.Effort
	}
	if src.AutoEffort != nil {
		dst.AutoEffort = src.AutoEffort
	}
	if src.DisabledInjections != nil {
		dst.DisabledInjections = src.DisabledInjections
	}
	if src.SilentStart != nil {
		dst.SilentStart = src.SilentStart
	}
	if src.SilentEnd != nil {
		dst.SilentEnd = src.SilentEnd
	}
	if src.NotifyDuringSilent != nil {
		dst.NotifyDuringSilent = src.NotifyDuringSilent
	}
}

// mergeHubLocalBase fills rec.Base for fields NEWLY overridden by
// this write; fields the prior record already covers keep their
// original base.
func mergeHubLocalBase(rec *hubLocalOverrideRecord, base AgentUpdateConfig) {
	if rec.Cfg.Name == nil {
		rec.Base.Name = base.Name
	}
	if rec.Cfg.PublicProfile == nil {
		rec.Base.PublicProfile = base.PublicProfile
	}
	if rec.Cfg.PublicProfileOverride == nil {
		rec.Base.PublicProfileOverride = base.PublicProfileOverride
	}
	if rec.Cfg.Effort == nil {
		rec.Base.Effort = base.Effort
	}
	if rec.Cfg.AutoEffort == nil {
		rec.Base.AutoEffort = base.AutoEffort
	}
	if rec.Cfg.DisabledInjections == nil {
		rec.Base.DisabledInjections = base.DisabledInjections
	}
	if rec.Cfg.SilentStart == nil {
		rec.Base.SilentStart = base.SilentStart
	}
	if rec.Cfg.SilentEnd == nil {
		rec.Base.SilentEnd = base.SilentEnd
	}
	if rec.Cfg.NotifyDuringSilent == nil {
		rec.Base.NotifyDuringSilent = base.NotifyDuringSilent
	}
}

// mergeAgainstIngestedRow performs the per-field 3-way merge: for each
// overridden field, keep the hub value only when the ingested row
// still equals the recorded base (holder never touched it). Returns
// the config to apply plus the names of applied / dropped fields.
func mergeAgainstIngestedRow(row *Agent, ov hubLocalOverrideRecord) (AgentUpdateConfig, []string, []string) {
	var out AgentUpdateConfig
	var applied, dropped []string

	strEq := func(basep *string, cur string) bool { return basep != nil && *basep == cur }
	boolEq := func(basep *bool, cur bool) bool { return basep != nil && *basep == cur }
	boolPtr := func(p *bool) bool { return p != nil && *p }

	if ov.Cfg.Name != nil {
		if strEq(ov.Base.Name, row.Name) {
			out.Name = ov.Cfg.Name
			applied = append(applied, "name")
		} else {
			dropped = append(dropped, "name")
		}
	}
	if ov.Cfg.PublicProfile != nil {
		if strEq(ov.Base.PublicProfile, row.PublicProfile) {
			out.PublicProfile = ov.Cfg.PublicProfile
			applied = append(applied, "publicProfile")
		} else {
			dropped = append(dropped, "publicProfile")
		}
	}
	if ov.Cfg.PublicProfileOverride != nil {
		if boolEq(ov.Base.PublicProfileOverride, row.PublicProfileOverride) {
			out.PublicProfileOverride = ov.Cfg.PublicProfileOverride
			applied = append(applied, "publicProfileOverride")
		} else {
			dropped = append(dropped, "publicProfileOverride")
		}
	}
	if ov.Cfg.Effort != nil {
		if strEq(ov.Base.Effort, row.Effort) {
			out.Effort = ov.Cfg.Effort
			applied = append(applied, "effort")
		} else {
			dropped = append(dropped, "effort")
		}
	}
	if ov.Cfg.AutoEffort != nil {
		if boolEq(ov.Base.AutoEffort, boolPtr(row.AutoEffort)) {
			out.AutoEffort = ov.Cfg.AutoEffort
			applied = append(applied, "autoEffort")
		} else {
			dropped = append(dropped, "autoEffort")
		}
	}
	if ov.Cfg.DisabledInjections != nil {
		baseDI := ov.Base.DisabledInjections
		if baseDI != nil && equalStringSets(*baseDI, row.DisabledInjections) {
			out.DisabledInjections = ov.Cfg.DisabledInjections
			applied = append(applied, "disabledInjections")
		} else {
			dropped = append(dropped, "disabledInjections")
		}
	}
	if ov.Cfg.SilentStart != nil {
		if strEq(ov.Base.SilentStart, row.SilentStart) {
			out.SilentStart = ov.Cfg.SilentStart
			applied = append(applied, "silentStart")
		} else {
			dropped = append(dropped, "silentStart")
		}
	}
	if ov.Cfg.SilentEnd != nil {
		if strEq(ov.Base.SilentEnd, row.SilentEnd) {
			out.SilentEnd = ov.Cfg.SilentEnd
			applied = append(applied, "silentEnd")
		} else {
			dropped = append(dropped, "silentEnd")
		}
	}
	if ov.Cfg.NotifyDuringSilent != nil {
		if boolEq(ov.Base.NotifyDuringSilent, boolPtr(row.NotifyDuringSilent)) {
			out.NotifyDuringSilent = ov.Cfg.NotifyDuringSilent
			applied = append(applied, "notifyDuringSilent")
		} else {
			dropped = append(dropped, "notifyDuringSilent")
		}
	}
	return out, applied, dropped
}

// equalStringSets compares two normalized string slices ignoring
// order (disabledInjections is normalized on every write path).
func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]int, len(a))
	for _, s := range a {
		set[s]++
	}
	for _, s := range b {
		if set[s] == 0 {
			return false
		}
		set[s]--
	}
	return true
}

// ReapplyHubLocalOverrides re-applies pending hub-local settings
// overrides on top of a just-ingested source-wins agents row (§3.7
// peer_agent_sync). Called by the sync-ingest handler right after
// store.SyncAgentFromPeer commits.
//
// Per-field 3-way merge (clock-free; see hubLocalOverrideRecord):
//   - no pending override           → no-op
//   - ingested field == base        → hub edit re-applied
//   - ingested field != base        → holder changed it later; holder
//     wins for that field (surfaced via the dropped list, never silent)
//
// The kv row is cleared with the etag read at entry, so a concurrent
// PATCH's fresher override survives the delete and is honoured at the
// next sync. On apply error the kv row is kept and the error returned
// so the (idempotent) sync request can be retried.
//
// Returns (appliedFields, droppedFields, err).
func (m *Manager) ReapplyHubLocalOverrides(ctx context.Context, agentID string) ([]string, []string, error) {
	st := m.Store()
	if st == nil {
		return nil, nil, nil
	}
	// Serialize the read-merge-delete sequence against concurrent hub
	// PATCH recordings (review round 4): the HTTP PATCH path holds the
	// same per-agent LockPatch across its record + row write, so under
	// this lock no new override can land between our kv read, the
	// merge apply, and the conditional delete. The etag-guarded delete
	// below stays as defense-in-depth for non-LockPatch writers (the
	// mid-switch Manager.Update record path).
	release := m.LockPatch(agentID)
	defer release()

	rec, err := st.GetKV(ctx, hubLocalOverrideNamespace, hubLocalOverrideKey(agentID))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var ov hubLocalOverrideRecord
	if uerr := json.Unmarshal([]byte(rec.Value), &ov); uerr != nil {
		_ = st.DeleteKV(ctx, hubLocalOverrideNamespace, hubLocalOverrideKey(agentID), rec.ETag)
		return nil, nil, fmt.Errorf("corrupt hub-local override row dropped: %w", uerr)
	}
	// A leftover Pending section here belongs to a PATCH that never
	// got a success response (its confirm/cleanup crashed) AND whose
	// row has since been replaced by this sync ingest — it cannot be
	// settled against the row anymore. Merge-ineligible by contract:
	// drop it (GC), never apply it.
	if ov.Pending != nil {
		if m.logger != nil {
			m.logger.Info("hub-local override: dropped unconfirmed pending record at sync ingest",
				"agent", agentID)
		}
		ov.Pending = nil
	}

	// Merge against the freshly-ingested row.
	row, err := m.store.LoadByID(agentID)
	if err != nil {
		return nil, nil, fmt.Errorf("load ingested row: %w", err)
	}
	applyCfg, applied, dropped := mergeAgainstIngestedRow(row, ov)

	if len(applied) > 0 {
		m.mu.Lock()
		_, inMem := m.agents[agentID]
		m.mu.Unlock()
		if inMem {
			// The agent is already attached (idempotent re-sync after
			// finalize). Do NOT route through Manager.Update here: its
			// m.save() flushes the ENTIRE stale in-memory agent and
			// would roll back every row field the sync just ingested,
			// not only the merged ones. Instead apply the merge to the
			// fresh DB row via the CAS, then refresh the in-memory copy
			// from the store — the same row→memory direction the §3.7
			// sync hook itself uses (ReloadAgentFromStore).
			_, err = m.store.updateAgentRowCAS(agentID, "", func(a *Agent) error {
				return applyHubLocalCfg(a, applyCfg)
			})
			if err == nil {
				err = m.ReloadAgentFromStore(agentID)
			}
		} else {
			// record=false so the re-apply cannot re-arm the kv row it
			// is about to clear.
			_, err = m.updateRemoteHubRowInner(agentID, applyCfg, "", false)
		}
		if err != nil {
			// Keep the kv row so a retried sync can try again.
			return nil, nil, err
		}
	}
	// Conditional delete: a concurrent PATCH that re-wrote the kv row
	// after our read keeps its (fresher) override for the next sync —
	// only an ETag mismatch (and already-gone) is benign; any other DB
	// error is surfaced so the (idempotent) sync retries the cleanup.
	if derr := st.DeleteKV(ctx, hubLocalOverrideNamespace, hubLocalOverrideKey(agentID), rec.ETag); derr != nil &&
		!errors.Is(derr, store.ErrNotFound) {
		if !errors.Is(derr, store.ErrETagMismatch) {
			return nil, nil, fmt.Errorf("clear override kv row: %w", derr)
		}
		if m.logger != nil {
			m.logger.Debug("hub-local override delete skipped (row changed concurrently)",
				"agent", agentID, "err", derr)
		}
	}
	return applied, dropped, nil
}

// updateAgentRowCAS applies fn to the agent decoded from the row that
// store.UpdateAgent freshly reads inside its own transaction, then
// re-encodes name + settings_json. Version/etag bump and change-feed
// event come from store.UpdateAgent. Returning an error from fn
// aborts the update with zero side effects.
//
// Persona is deliberately NOT decoded (settingsToAgent leaves it
// zero) and is a reserved key on the encode side, so the persona row
// / body can never be clobbered from here.
func (st *agentStore) updateAgentRowCAS(id, ifMatchETag string, fn func(a *Agent) error) (*Agent, error) {
	if st == nil || st.db == nil {
		return nil, errors.New("agentStore: not initialized")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var out *Agent
	_, err := st.db.UpdateAgent(ctx, id, ifMatchETag, func(r *store.AgentRecord) error {
		a := &Agent{}
		if derr := settingsToAgent(r, a); derr != nil {
			return fmt.Errorf("settings decode: %w", derr)
		}
		st.normalizeAgent(a)
		if ferr := fn(a); ferr != nil {
			return ferr
		}
		next, eerr := agentToSettings(a, r.Settings)
		if eerr != nil {
			return fmt.Errorf("encode settings: %w", eerr)
		}
		r.Name = a.Name
		r.Settings = next
		out = copyAgent(a)
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, id)
		}
		return nil, err
	}
	return out, nil
}

// acquireMutationAllowSwitching is AcquireMutation minus the §3.7
// switching refusal, reserved for hub-local-safe PATCHes
// (Manager.Update with cfg.HubLocalOnly()). It still bumps the
// per-agent mutating counter so WaitChatIdle's quiesce drain
// observes the write, and still refuses during a daemon restart
// quiesce. A hub-safe write landing after the switch snapshot is
// encoded simply diverges until the next sync — same documented
// last-write-wins contract as UpdateRemoteHubRow.
func (m *Manager) acquireMutationAllowSwitching(agentID string) (func(), error) {
	if m == nil {
		return func() {}, nil
	}
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	if m.quiescing {
		return nil, fmt.Errorf("%w: server restart in progress", ErrAgentBusy)
	}
	if m.mutating == nil {
		m.mutating = make(map[string]int)
	}
	m.mutating[agentID]++
	released := false
	return func() {
		m.busyMu.Lock()
		defer m.busyMu.Unlock()
		if released {
			return
		}
		released = true
		if m.mutating == nil {
			return
		}
		if m.mutating[agentID] > 0 {
			m.mutating[agentID]--
		}
		if m.mutating[agentID] == 0 {
			delete(m.mutating, agentID)
		}
	}, nil
}
