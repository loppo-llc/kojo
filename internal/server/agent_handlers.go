package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// agentKnown returns true when the agent exists either in the local
// in-memory manager (runtime is here) or in the store only (runtime
// released to a remote peer via §3.7 device-switch). Read-only
// endpoints use this instead of a bare s.agents.Get(id) so data
// (messages, memory, credentials) is still accessible after a switch.
func (s *Server) agentKnown(id string) bool {
	if _, ok := s.agents.Get(id); ok {
		return true
	}
	return s.agents.GetRemote(id) != nil
}

// directoryView returns a public, agent-safe view of an Agent. Used by
// list/get handlers when the caller is not the Owner and not the agent
// itself — exposes only ID/Name/PublicProfile and the avatar metadata
// already shipped via /api/v1/agents/{id}/avatar.
type directoryView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	PublicProfile string `json:"publicProfile"`
	HasAvatar     bool   `json:"hasAvatar"`
	AvatarHash    string `json:"avatarHash,omitempty"`
	Archived      bool   `json:"archived,omitempty"`
}

func toDirectoryView(a *agent.Agent) directoryView {
	if a == nil {
		return directoryView{}
	}
	return directoryView{
		ID:            a.ID,
		Name:          a.Name,
		PublicProfile: a.PublicProfile,
		HasAvatar:     a.HasAvatar,
		AvatarHash:    a.AvatarHash,
		Archived:      a.Archived,
	}
}

// agentResponse embeds *agent.Agent and tacks on runtime-only fields the UI
// consumes. These fields aren't persisted so they live outside the Agent
// struct itself.
type agentResponse struct {
	*agent.Agent
	NextCronAt string `json:"nextCronAt,omitempty"`
	// CronPausedGlobal mirrors Manager.CronPaused at response build
	// time so the UI can render "(paused)" next to nextCronAt when the
	// Dashboard's global cron toggle is off. NextCronRun deliberately
	// no longer zeroes on global pause (it would just show "—" on
	// every agent's settings page); the indicator carries the "this
	// time is configured but not currently firing" signal instead.
	CronPausedGlobal bool `json:"cronPausedGlobal,omitempty"`
	// ETag is the strong HTTP entity tag of the v1 store row backing
	// this agent. Surfaced inside the JSON (in addition to the HTTP
	// header) so a client can hold the etag in form state alongside
	// the other agent fields and pass it back via If-Match without
	// relying on a separate response-header capture path.
	//
	// Empty when the v1 store has no row yet (legacy paths, in-flight
	// create). Same shape as handleGetAgent's ETag header.
	ETag string `json:"etag,omitempty"`
}

// buildAgentResponse decorates an *agent.Agent with derived runtime
// state (next cron run, etag) for API responses.
//
// etag is passed in by the caller rather than read from the store
// here so a single response observes a consistent (header, body)
// pair: the caller does ONE GetAgent, sets the HTTP ETag header
// from it, and threads the same value through this builder. A
// second SELECT inside this function would race against any write
// landing between the two reads (most observably on GET, which
// holds no per-agent lock) and surface a body etag that disagreed
// with the header.
//
// Pass "" when the row has no etag yet (in-memory-only paths) or
// when the endpoint deliberately does not surface one (list view,
// directory view).
func (s *Server) buildAgentResponse(a *agent.Agent, etag string) agentResponse {
	resp := agentResponse{Agent: a, ETag: etag}
	if a == nil {
		return resp
	}
	if t := s.agents.NextCronRun(a.ID); !t.IsZero() {
		resp.NextCronAt = t.Format(time.RFC3339)
	}
	resp.CronPausedGlobal = s.agents.CronPaused()
	return resp
}

// readAgentETag is the canonical "fetch this agent's current etag"
// helper used by handlers that need the value for both the HTTP
// header and the JSON body. Best-effort: a missing row, a closed
// store, or a context error all yield "" so callers can degrade to
// a no-etag response rather than failing the whole request.
//
// Uses a 2-second timeout derived from r.Context() so the read
// honors request cancellation but cannot dangle forever if the
// caller's context has no deadline (e.g. a long-poll handler).
func (s *Server) readAgentETag(r *http.Request, agentID string) string {
	st := s.agents.Store()
	if st == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	rec, err := st.GetAgent(ctx, agentID)
	if err != nil {
		return ""
	}
	return rec.ETag
}

// agentIfMatchPrecheck verifies that the v1 store's agent row matches
// the caller's If-Match precondition. ifMatchPresent=false short-
// circuits to true (no precondition requested). On mismatch / missing
// row / store-not-wired-in, writes the appropriate HTTP error and
// returns false; callers MUST stop processing in that case.
//
// `*` is treated per RFC 7232: "any current resource exists" — we
// still require the row to be present so a stale `*` against a
// deleted agent doesn't silently succeed.
//
// Locking contract:
//
//   - PATCH-class callers (handleUpdateAgent, PUT /slackbot, POST
//     /privilege) MUST hold s.agents.LockPatch(agentID) across this
//     check and the subsequent mutation. Otherwise two concurrent
//     PATCHes carrying the same etag could both observe the same
//     store row, both pass the precheck, and both succeed — the
//     cardinal sin of optimistic concurrency.
//
//   - Lifecycle callers (handleDeleteAgent, handleUnarchiveAgent)
//     MUST NOT acquire LockPatch externally because Manager.Archive
//     / Delete / Unarchive take it internally and sync.Mutex is
//     non-reentrant. For these callers the precheck is best-effort
//     (a concurrent PATCH between precheck and lifecycle call is
//     accepted as a small race window), matching the docs §3.5
//     "v1 移行期は best-effort" stance.
//
// Cross-process / cross-device coordination is the store-level
// optimistic-concurrency layer's job; LockPatch only serializes
// within one daemon process.
//
// The store read uses the request's bare context (no extra timeout)
// to mirror the original inline check inside handleUpdateAgent —
// callers needing a deadline should set one on the request before
// dispatch.
func (s *Server) agentIfMatchPrecheck(w http.ResponseWriter, r *http.Request, agentID, ifMatch string, ifMatchPresent bool) bool {
	if !ifMatchPresent {
		// Honour the docs §3.5 transition flag: callers that opt out
		// of the precondition get 428 when the operator has
		// promoted strict-mode. Off by default — the legacy code
		// path returns true unchanged. The strict gate here is
		// If-Match-only; the agents endpoints don't honour
		// If-None-Match: * because their store layer has no
		// create-only CAS path. The opt-in variant
		// enforceCreateOrUpdatePrecondition is reserved for kv PUT.
		if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
			return false
		}
		return true
	}
	st := s.agents.Store()
	if st == nil {
		// No store wired in (test harness without a DB) — refuse
		// rather than silently ignore. The client asked for
		// optimistic concurrency; we cannot honor it, so this MUST
		// be an error rather than a quiet pass-through.
		writeError(w, http.StatusPreconditionFailed, "precondition_failed",
			"If-Match unsupported on this server")
		return false
	}
	rec, err := st.GetAgent(r.Context(), agentID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusPreconditionFailed, "precondition_failed",
			"If-Match: agent not in store")
		return false
	case err != nil:
		// DB hiccup, ctx canceled, etc. Don't pretend it's a
		// precondition failure — that lies to the client.
		writeError(w, http.StatusInternalServerError, "internal_error",
			"If-Match: store read failed")
		return false
	}
	// Strip the GET-only ".p" suffix (composite ETag for cron-paused
	// state, see handleGetAgent) so a client that copied the HTTP
	// ETag header into If-Match still matches the underlying row.
	// The row etag itself never contains "." in its on-the-wire
	// format (it's "<version>-<hash>"), so this trim is unambiguous.
	candidate := strings.TrimSuffix(ifMatch, ".p")
	if ifMatch != "*" && rec.ETag != candidate {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed",
			"If-Match: etag mismatch")
		return false
	}
	return true
}

// --- Cron Pause ---

func (s *Server) handleGetCronPaused(w http.ResponseWriter, r *http.Request) {
	writeJSONResponse(w, http.StatusOK, map[string]any{"paused": s.agents.CronPaused()})
}

// handleSetCronPaused flips the global cron-paused flag stored in kv
// under (namespace="scheduler", key="paused", scope=global).
// Intentionally If-Match-free per design §3.5: the resource is a
// singleton boolean owned by the Owner, the only "edit" is "flip
// on/off", and there is no useful value in a 412 — a stale client
// just sees the flag's current value on the next GET. Adding
// optimistic concurrency here would make the UI's "Pause cron"
// button surface PreconditionFailed errors that the user cannot
// meaningfully act on.
func (s *Server) handleSetCronPaused(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paused bool `json:"paused"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if err := s.agents.SetCronPaused(body.Paused); err != nil {
		// Persist failure — refuse to acknowledge. Otherwise the
		// UI would show "paused" while the kv row stays at its
		// previous value, and a daemon restart would silently
		// resurrect the old state.
		s.logger.Error("set cron paused failed", "paused", body.Paused, "err", err)
		writeError(w, http.StatusInternalServerError, "store_error", "failed to persist cron pause state")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"paused": body.Paused})
}

// --- Active Hour Check ---

func (s *Server) handleGetAgentActive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	active, ok := s.agents.IsAgentActive(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"active": active})
}

// --- Agent CRUD Handlers ---

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	// Default: hide archived agents from the main list. Pass
	// ?includeArchived=true to fetch archived alongside active (used by the
	// "Archived agents" section in global Settings) or ?archived=true to
	// fetch only archived. The two flags are mutually exclusive — combining
	// them is a caller bug, not something we want to silently resolve in
	// favor of one or the other.
	q := r.URL.Query()
	includeArchived := q.Get("includeArchived") == "true"
	onlyArchived := q.Get("archived") == "true"
	if includeArchived && onlyArchived {
		writeError(w, http.StatusBadRequest, "bad_request",
			"archived and includeArchived are mutually exclusive")
		return
	}

	p := auth.FromContext(r.Context())
	all := s.agents.List()
	// Append agents whose runtime lives on a remote peer (§3.7
	// device-switch released them from the local manager). They
	// still exist in the store and should appear in the list so
	// the UI can show them with a "remote" indicator / route chat
	// through the WS proxy. Dedup by ID: a TOCTOU race between
	// the in-memory snapshot and the store query could surface the
	// same agent in both sets if a concurrent Create/sync lands
	// between the two reads.
	if remote := s.agents.ListRemote(); len(remote) > 0 {
		seen := make(map[string]struct{}, len(all))
		for _, a := range all {
			seen[a.ID] = struct{}{}
		}
		for _, a := range remote {
			if _, dup := seen[a.ID]; !dup {
				all = append(all, a)
			}
		}
	}
	if p.IsOwner() {
		out := make([]*agent.Agent, 0, len(all))
		for _, a := range all {
			switch {
			case onlyArchived && !a.Archived:
				continue
			case !onlyArchived && !includeArchived && a.Archived:
				continue
			}
			out = append(out, a)
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"agents": out})
		return
	}

	// Non-owner: full record for self, directory view for others.
	// Force archived agents off the list — non-owners have no business
	// enumerating tombstoned personas, so the includeArchived /
	// onlyArchived flags are silently ignored at this layer (an
	// agent's own archived self is also filtered out — they should be
	// asking the owner to revive them, not poking the API).
	out := make([]any, 0, len(all))
	for _, a := range all {
		if a.Archived {
			continue
		}
		if p.IsAgent() && p.AgentID == a.ID {
			out = append(out, a)
		} else {
			out = append(out, toDirectoryView(a))
		}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"agents": out})
}

func (s *Server) handleAgentDirectory(w http.ResponseWriter, r *http.Request) {
	entries := s.agents.Directory()
	writeJSONResponse(w, http.StatusOK, map[string]any{"agents": entries})
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	if p := auth.FromContext(r.Context()); !p.CanForkOrCreate() {
		writeError(w, http.StatusForbidden, "forbidden", "agent creation is owner-only")
		return
	}
	var cfg agent.AgentConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	a, err := s.agents.Create(cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, a)
}

func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := s.agents.Get(id)
	if !ok {
		// The agent's runtime may live on a remote peer (§3.7
		// device-switch released it from the local manager).
		// Fall back to GetRemote so the UI can still inspect the
		// record and show the remote indicator.
		if ra := s.agents.GetRemote(id); ra != nil {
			p := auth.FromContext(r.Context())
			if p.CanReadFull(id) {
				writeJSONResponse(w, http.StatusOK,
					s.buildAgentResponse(ra, ""))
			} else {
				writeJSONResponse(w, http.StatusOK, toDirectoryView(ra))
			}
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	// Best-effort ETag: read the DB row's etag if the store backend is
	// wired in. A failure here (store not configured, row not yet
	// persisted, transient SQLite hiccup) drops back to a no-ETag
	// response rather than failing the GET — clients that don't care
	// about caching keep working, and clients that do will simply
	// re-request next time without a precondition.
	etag := s.readAgentETag(r, id)
	if etag != "" {
		// The HTTP ETag header is a composite of the agent row etag and
		// the global cron-paused flag, because cronPausedGlobal /
		// nextCronAt in the JSON body change without bumping the row
		// etag (toggling Dashboard's cron pause writes a kv row, not
		// the agents row). Without the suffix, a 304 fast-path would
		// silently hand the client a stale cronPausedGlobal value and
		// the Settings page would never re-render the "(paused)"
		// indicator until something else mutated the agent.
		//
		// PATCH's If-Match precondition is unaffected: clients send
		// back the body's `etag` field (the raw row etag from
		// buildAgentResponse), not the HTTP header — see agentApi.get's
		// "prefer body etag" branch. agentIfMatchPrecheck only ever
		// sees the row etag, so the composite layer here is GET-only.
		displayETag := etag
		if s.agents.CronPaused() {
			displayETag = etag + ".p"
		}
		w.Header().Set("ETag", quoteETag(displayETag))
		if cached, ok := extractDomainIfNoneMatch(r); ok && (cached == "*" || cached == displayETag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	p := auth.FromContext(r.Context())
	if p.CanReadFull(id) {
		// Pass the etag we just read into the body builder so header
		// and body agree even if a write lands later in this request.
		writeJSONResponse(w, http.StatusOK, s.buildAgentResponse(a, etag))
		return
	}
	writeJSONResponse(w, http.StatusOK, toDirectoryView(a))
}


func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := auth.FromContext(r.Context())
	if !p.CanMutateSelf(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agents may only PATCH themselves")
		return
	}
	// Optimistic concurrency: parse If-Match before any side-effects so a
	// malformed precondition (weak etag, comma-list, missing quotes) is
	// rejected before we read the body or run validations. `*` is treated
	// as "any current resource exists" per RFC 7232 §3.1; explicit etags
	// are checked against the v1 store row below.
	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	// Defensive: refuse a payload that smuggles a "privileged" field
	// regardless of role. The Owner has a dedicated POST /privilege
	// endpoint; anyone else trying to flip the bit through PATCH is
	// almost certainly attempting self-elevation. Match keys
	// case-insensitively so a casing trick can't sneak past either.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err == nil {
		for k := range raw {
			if strings.EqualFold(k, "privileged") {
				writeError(w, http.StatusForbidden, "forbidden",
					"privileged is owner-only; use POST /api/v1/agents/{id}/privilege")
				return
			}
		}
	}
	var cfg agent.AgentUpdateConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	// Per-agent serialization across precheck → Update → ETag echo.
	// Without this, two concurrent PATCHes carrying the same If-Match
	// would both observe the same store etag, both pass the precheck,
	// and both succeed — the cardinal sin of optimistic concurrency.
	// Manager.LockPatch is held only within this single daemon process;
	// cross-process coordination is the store layer's job (a future
	// slice will thread If-Match through Manager.Update →
	// store.UpdateAgent so the check is atomic with the UPDATE).
	release := s.agents.LockPatch(id)
	defer release()

	// If-Match precondition: when the client supplied a strong etag,
	// verify it against the current v1 store row before mutating.
	// Missing If-Match falls through without enforcement (the docs note
	// this slice is best-effort until v1 clients adopt If-Match
	// universally; see §5.5).
	if !s.agentIfMatchPrecheck(w, r, id, ifMatch, ifMatchPresent) {
		return
	}
	a, err := s.agents.Update(id, cfg)
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrAgentBusy):
			writeError(w, http.StatusConflict, "agent_busy", err.Error())
		default:
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}
	// Echo the new etag so the client can chain subsequent PATCHes
	// without an extra GET. The per-agent patch lock above guarantees
	// no other PATCH for this id can land between Update and this
	// read, so the etag returned is the one our Update produced.
	// We read once and use the same value for both the HTTP header
	// AND the JSON body's etag field — see buildAgentResponse for
	// why a second SELECT in the body builder would race.
	newETag := s.readAgentETag(r, id)
	if newETag != "" {
		// Mirror handleGetAgent's composite ETag so a follow-up GET's
		// If-None-Match correctly matches this PATCH response's header.
		// The body's `etag` field stays the raw row etag (used for
		// If-Match precondition); only the HTTP header includes the
		// pause-state suffix.
		displayETag := newETag
		if s.agents.CronPaused() {
			displayETag = newETag + ".p"
		}
		w.Header().Set("ETag", quoteETag(displayETag))
	}
	writeJSONResponse(w, http.StatusOK, s.buildAgentResponse(a, newETag))
}

// handleCheckin fires a manual check-in for the agent. The check-in runs
// asynchronously on the server (events drained in a goroutine), so we
// return immediately with 202 Accepted; the assistant reply will appear
// in the transcript via the normal chat flow.
func (s *Server) handleCheckin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := auth.FromContext(r.Context())
	if !p.CanDeleteOrReset(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agent is not allowed to checkin others")
		return
	}
	if err := s.agents.Checkin(id); err != nil {
		switch {
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrAgentArchived):
			writeError(w, http.StatusConflict, "archived", err.Error())
		case errors.Is(err, agent.ErrAgentBusy), errors.Is(err, agent.ErrAgentResetting):
			writeError(w, http.StatusConflict, "busy", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) handleResetAgentData(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := auth.FromContext(r.Context())
	if !p.CanDeleteOrReset(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agent is not allowed to reset others")
		return
	}

	// Stop Slack bot before resetting to avoid stale file references.
	if s.slackHub != nil {
		s.slackHub.StopBot(id)
	}

	if err := s.agents.ResetData(id); err != nil {
		if errors.Is(err, agent.ErrAgentNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else if errors.Is(err, agent.ErrAgentBusy) {
			writeError(w, http.StatusConflict, "conflict", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}

	// Restart Slack bot if it was enabled.
	if s.slackHub != nil {
		if a, ok := s.agents.Get(id); ok && a.SlackBot != nil && a.SlackBot.Enabled {
			s.slackHub.StartBot(id, *a.SlackBot)
		}
	}

	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleTruncateAgentMemory removes everything in the agent's memory
// recorded at-or-after the given instant: kojo transcript records (DB-
// tombstoned via Store.TruncateMessagesFromCreatedAt), Claude session
// JSONL records (with trailing-turn trim), and daily diary bullets in
// memory/YYYY-MM-DD.md (with the matching memory_entries row updated or
// soft-deleted in the same lock window). Settings, persona, MEMORY.md,
// project / people / topic notes, archive, credentials and tasks are
// untouched.
//
// Two ways to specify the threshold (request body):
//   - {"since": "2026-05-09T12:00:00+09:00"}     — absolute RFC3339
//   - {"fromMessageId": "m_abc..."}              — use that message's
//     timestamp; the message itself is included in the removed set.
//
// Same auth gate as reset/delete (CanDeleteOrReset), same busy / reset
// guard semantics. Returns 404 when the agent or fromMessageId can't be
// found, 409 if a chat is in flight or another reset is racing.
func (s *Server) handleTruncateAgentMemory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := auth.FromContext(r.Context())
	if !p.CanDeleteOrReset(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agent is not allowed to truncate others' memory")
		return
	}

	var body struct {
		Since         string `json:"since"`
		FromMessageID string `json:"fromMessageId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	hasSince := strings.TrimSpace(body.Since) != ""
	hasMsg := strings.TrimSpace(body.FromMessageID) != ""
	if hasSince == hasMsg {
		writeError(w, http.StatusBadRequest, "bad_request",
			"exactly one of 'since' (RFC3339) or 'fromMessageId' is required")
		return
	}

	var (
		res *agent.TruncateMemoryResult
		err error
	)
	if hasSince {
		t, perr := time.Parse(time.RFC3339, body.Since)
		if perr != nil {
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("'since' must be RFC3339 (e.g. 2026-05-09T12:00:00+09:00): %v", perr))
			return
		}
		res, err = s.agents.TruncateMemoryAt(id, t)
	} else {
		res, err = s.agents.TruncateMemoryFromMessage(id, body.FromMessageID)
	}

	if err != nil {
		switch {
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrMessageNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrMessageETagMismatch):
			// fromMessageId pivot got mutated under us by a regenerate /
			// edit while we were preparing the truncate. Surface as 409
			// (concurrent conflict) so the client retries with a refreshed
			// pivot — 500 would imply a server bug.
			writeError(w, http.StatusConflict, "conflict", err.Error())
		case errors.Is(err, agent.ErrAgentBusy), errors.Is(err, agent.ErrAgentResetting):
			writeError(w, http.StatusConflict, "conflict", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}

	writeJSONResponse(w, http.StatusOK, res)
}

func (s *Server) handleForkAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if p := auth.FromContext(r.Context()); !p.CanForkOrCreate() {
		writeError(w, http.StatusForbidden, "forbidden", "fork is owner-only")
		return
	}
	var body struct {
		Name              string `json:"name"`
		IncludeTranscript bool   `json:"includeTranscript"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	a, err := s.agents.Fork(id, agent.ForkOptions{Name: body.Name, IncludeTranscript: body.IncludeTranscript})
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrAgentBusy), errors.Is(err, agent.ErrAgentResetting):
			writeError(w, http.StatusConflict, "conflict", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, a)
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if p := auth.FromContext(r.Context()); !p.CanDeleteOrReset(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agent is not allowed to delete others")
		return
	}
	// Parse If-Match before any side effect. Both archive and full
	// delete mutate the agent record so optimistic concurrency
	// applies. `*` is rejected: DELETE on a missing resource is
	// already idempotent, and "any current version" carries no
	// useful semantics over the row-load below.
	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	if ifMatchPresent && ifMatch == "*" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"If-Match: * is not supported on DELETE /agents/{id}; send a specific etag or omit the header")
		return
	}

	// ?archive=true keeps all on-disk data but stops runtime activity.
	// Restored later via POST /api/v1/agents/{id}/unarchive.
	archive := r.URL.Query().Get("archive") == "true"

	// Manager.Archive / Manager.Delete take Manager.LockPatch
	// internally, so we MUST NOT acquire it here — sync.Mutex is
	// non-reentrant and a double-acquire would self-deadlock the
	// goroutine. The precheck therefore runs outside the lock; this
	// is the same best-effort window every Archive/Delete-class
	// mutation has historically lived with (see docs §3.5
	// "best-effort 許可" and the inline LockPatch comment in
	// internal/agent/manager.go).
	if !s.agentIfMatchPrecheck(w, r, id, ifMatch, ifMatchPresent) {
		return
	}

	// Lifecycle mutation runs first so the agent record reaches its
	// final state (archived / deleted) before we try to teardown the
	// Slack bot. Reversing the order would race a concurrent PUT
	// /slackbot: that handler also takes Manager.LockPatch, so it
	// serializes with our Archive/Delete, but if we StopBot here the
	// PUT would land between StopBot and Archive, see a.Archived=
	// false (Archive hasn't run yet), reconfigure the bot, and leave
	// it running for the about-to-be-archived agent. By archiving
	// first, any subsequent PUT inside the same lock observes
	// a.Archived=true and Reconfigure's `!a.Archived` guard skips
	// StartBot; our StopBot below then catches anything the PUT had
	// already started.
	var opErr error
	if archive {
		opErr = s.agents.Archive(id)
	} else {
		opErr = s.agents.Delete(id)
	}
	if opErr != nil {
		if errors.Is(opErr, agent.ErrAgentNotFound) {
			writeError(w, http.StatusNotFound, "not_found", opErr.Error())
		} else if errors.Is(opErr, agent.ErrAgentBusy) {
			writeError(w, http.StatusConflict, "conflict", opErr.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", opErr.Error())
		}
		return
	}
	// TODO(structural): a follow-up Archive/Unarchive race against
	// SlackHub StartBot remains because lifecycle mutation and bot
	// side-effect aren't in the same critical section. Closing it
	// requires either a "Locked" variant of Archive/Delete that
	// assumes the caller holds Manager.LockPatch, or a bot-aware
	// variant that takes the SlackHub as a parameter. Both touch
	// Manager API and are out of scope for the If-Match wiring slice;
	// for now archived agents may transiently re-acquire a bot from
	// a racing PUT /slackbot until this StopBot fires.
	if s.slackHub != nil {
		s.slackHub.StopBot(id)
	}
	// Echo the new ETag for archive=true (the row still exists with
	// a bumped etag). For full delete the row is gone — no useful
	// etag to return. readAgentETag returns "" when the row is
	// missing, so the same call is safe in both branches.
	if newETag := s.readAgentETag(r, id); newETag != "" {
		w.Header().Set("ETag", quoteETag(newETag))
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleUnarchiveAgent restores an archived agent. Re-arms cron, notify
// poller, and (if previously enabled) the Slack bot.
func (s *Server) handleUnarchiveAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if p := auth.FromContext(r.Context()); !p.CanDeleteOrReset(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agent is not allowed to unarchive others")
		return
	}

	// If-Match optional, but when present checked against the current
	// (archived) row. Unarchive mutates the agent record so the same
	// optimistic-concurrency contract as PATCH applies. `*` rejected:
	// the row must already exist (we 404 below if it doesn't).
	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	if ifMatchPresent && ifMatch == "*" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"If-Match: * is not supported on POST /agents/{id}/unarchive; send a specific etag or omit the header")
		return
	}

	// Manager.Unarchive takes LockPatch internally — see the comment
	// in handleDeleteAgent for why we cannot wrap it externally
	// without self-deadlocking sync.Mutex.
	if !s.agentIfMatchPrecheck(w, r, id, ifMatch, ifMatchPresent) {
		return
	}

	if err := s.agents.Unarchive(id); err != nil {
		switch {
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrAgentBusy), errors.Is(err, agent.ErrAgentResetting):
			// Concurrent Archive/Unarchive/reset still in progress — caller
			// can retry. Map both to 409 so the client knows to back off.
			writeError(w, http.StatusConflict, "busy", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	// Re-arm Slack bot if it was configured. Manager.Unarchive can't do
	// this — slackHub lives on the server, not the manager. The
	// `!a.Archived` re-check guards against a concurrent
	// Archive/Delete that landed between Unarchive returning and our
	// Get: without it we'd start the bot for a freshly-re-archived
	// agent. Re-Archive runs under Manager.LockPatch so the record
	// we read here is the post-Unarchive (or post-Re-archive) state,
	// not a torn snapshot.
	//
	// TODO(structural): the !a.Archived check is not atomic with the
	// StartBot call — a concurrent Archive between Get and StartBot
	// would still re-start the bot on an archived agent. Same fix
	// shape as in handleDeleteAgent: a bot-aware lifecycle helper
	// holding LockPatch around both the manager call and the bot
	// side-effect. Out of scope for the If-Match wire-up slice.
	if s.slackHub != nil {
		// §3.7 race: a device-switch could have begun after
		// Manager.Unarchive released its reset guard but before
		// this StartBot. AcquireMutation fails-closed when
		// switching is set; we ALSO re-Get under the mutation
		// hold so a stale snapshot (Get before AcquireMutation)
		// doesn't carry SlackBot config that's since been
		// cleared by a parallel release. The previous order
		// (Get → Acquire → Start) let the agent be evicted
		// between Get and Acquire on the source-release path,
		// then started a bot on a released agent.
		if rel, mutErr := s.agents.AcquireMutation(id); mutErr == nil {
			if a, ok := s.agents.Get(id); ok && !a.Archived && a.SlackBot != nil && a.SlackBot.Enabled {
				s.slackHub.StartBot(id, *a.SlackBot)
			}
			rel()
		} else {
			s.logger.Info("unarchive: skipped SlackBot StartBot — agent mutation refused",
				"agent", id, "reason", mutErr.Error())
		}
	}
	if a, ok := s.agents.Get(id); ok {
		if newETag := s.readAgentETag(r, id); newETag != "" {
			w.Header().Set("ETag", quoteETag(newETag))
		}
		writeJSONResponse(w, http.StatusOK, a)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Avatar Handlers ---

func (s *Server) handleGetAvatar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := s.agents.Get(id)
	if !ok {
		// Remote agent (§3.7 device-switch): the avatar blob is
		// content-addressed and immutable — the local copy on disk
		// is identical to the target's, so serve it without a
		// network round-trip. Falls back to the generated SVG when
		// no custom avatar exists.
		if ra := s.agents.GetRemote(id); ra != nil {
			agent.ServeAvatar(s.blob, w, r, ra)
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	agent.ServeAvatar(s.blob, w, r, a)
}

func (s *Server) handleUploadAvatar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	releaseMut, err := s.agents.AcquireMutation(id)
	if err != nil {
		writeError(w, http.StatusConflict, "agent_busy", err.Error())
		return
	}
	defer releaseMut()

	r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10MB max
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "file too large (max 10MB)")
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid multipart form")
		}
		return
	}

	file, header, err := r.FormFile("avatar")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "missing avatar file")
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	if !agent.IsAllowedImageExt(ext) {
		writeError(w, http.StatusBadRequest, "bad_request", "unsupported image format")
		return
	}

	// Serialize against Manager.Delete via LockPatch + post-lock
	// re-check. Without this, a Delete that tombstones the agent
	// after the initial Get above but before SaveAvatar finishes
	// would leave an orphan blob keyed by the just-deleted agent
	// id. The CAS in agent_locks/blob_refs doesn't catch this
	// because the avatar blob is per-agent state; once the agent
	// row is gone the blob would survive as cruft until the
	// eventual operator-driven blob GC pass (planned `--clean
	// blobs` target alongside the existing `snapshots` / `legacy`
	// targets in cmd/kojo/clean_cmd.go; Phase 6 #18 in
	// docs/multi-device-storage.md).
	release := s.agents.LockPatch(id)
	defer release()
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	if err := agent.SaveAvatar(s.blob, id, file, ext); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Messages Handler ---

func (s *Server) handleGetMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Local agent — serve from this peer's store.
	if _, ok := s.agents.Get(id); ok {
		s.serveLocalMessages(w, r, id)
		return
	}

	// Remote agent (§3.7 device-switch): proxy the request to the
	// holder peer so the Web UI sees live messages even while the
	// agent is on another device. Falls back to the local store
	// snapshot when the holder is unreachable or unknown.
	remote := s.agents.GetRemote(id)
	if remote == nil {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	if remote.HolderPeer != "" && s.peerID != nil {
		if s.proxyPeerGetMessages(w, r, id, remote.HolderPeer) {
			return
		}
		// Proxy failed — fall through to local store snapshot.
	}
	s.serveLocalMessages(w, r, id)
}

// serveLocalMessages reads messages from this peer's local store.
func (s *Server) serveLocalMessages(w http.ResponseWriter, r *http.Request, id string) {
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}
	before := r.URL.Query().Get("before")

	msgs, hasMore, err := s.agents.MessagesPaginated(id, limit, before)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if msgs == nil {
		msgs = []*agent.Message{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"messages": msgs, "hasMore": hasMore})
}

// proxyPeerGetMessages forwards a GET /messages request to the
// holder peer via peer-auth signed HTTP. Returns true if the proxy
// succeeded and the response was written; false if the caller should
// fall back to the local store (peer unreachable, signing not
// configured, etc.). Errors are logged but not surfaced to the
// client — the fallback path serves stale-but-available data.
func (s *Server) proxyPeerGetMessages(w http.ResponseWriter, r *http.Request, agentID, holderDeviceID string) bool {
	if s.peerID == nil || s.agents.Store() == nil {
		return false
	}
	st := s.agents.Store()
	peerRec, err := st.GetPeer(r.Context(), holderDeviceID)
	if err != nil || peerRec == nil || peerRec.Status != store.PeerStatusOnline {
		return false
	}
	addr, err := peer.NormalizeAddress(peerRec.Name)
	if err != nil {
		return false
	}

	// Rebuild the query string for the upstream request.
	targetURL := addr + "/api/v1/agents/" + agentID + "/messages"
	if qs := r.URL.RawQuery; qs != "" {
		targetURL += "?" + qs
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return false
	}
	nonce, err := peer.MakeNonce()
	if err != nil {
		return false
	}
	if err := peer.SignRequest(req, s.peerID.DeviceID, s.peerID.PrivateKey, nonce, holderDeviceID); err != nil {
		return false
	}
	// Forward auth token so the holder's auth middleware can verify
	// read access (the original request's token may carry
	// owner/agent role).
	if tok := r.Header.Get("X-Kojo-Token"); tok != "" {
		req.Header.Set("X-Kojo-Token", tok)
	}
	if tok := r.Header.Get("Authorization"); tok != "" {
		req.Header.Set("Authorization", tok)
	}

	client := peer.NoKeepAliveHTTPClient(10 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		if s.logger != nil {
			s.logger.Debug("proxyPeerGetMessages: request failed", "agent", agentID, "peer", holderDeviceID, "err", err)
		}
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if s.logger != nil {
			s.logger.Debug("proxyPeerGetMessages: non-200 from peer", "agent", agentID, "peer", holderDeviceID, "status", resp.StatusCode)
		}
		return false
	}

	// Stream the upstream response body to the client.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	io.Copy(w, io.LimitReader(resp.Body, 32<<20))
	return true
}

func (s *Server) handleUpdateMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	msgID := r.PathValue("msgId")

	// If-Match is forwarded into the store transaction below — no
	// handler-level mutex needed (unlike PATCH /api/v1/agents/{id},
	// which still goes through the file-backed Manager.Update).
	// `*` is rejected: PATCH /messages/{msgId} is per-row and a
	// wildcard precondition makes no sense (`*` means "the resource
	// exists with any current etag", which here adds nothing over
	// the existing row-load and would invite sloppy clients to skip
	// real concurrency control entirely).
	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	if ifMatchPresent && ifMatch == "*" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"If-Match: * is not supported on PATCH /messages/{msgId}; send a specific etag")
		return
	}

	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	msg, newETag, err := s.agents.UpdateMessageContent(id, msgID, body.Content, ifMatch)
	if err != nil {
		if errors.Is(err, agent.ErrMessageETagMismatch) {
			writeError(w, http.StatusPreconditionFailed, "precondition_failed",
				"If-Match: etag mismatch")
			return
		}
		writeTranscriptEditError(w, err, msgID)
		return
	}
	// Echo the etag returned by the same store transaction that did
	// the UPDATE, NOT a fresh GetMessage — between Manager.Update's
	// editing-flag release and a follow-up read another PATCH could
	// land and surface its etag here, which would lie to the client.
	if newETag != "" {
		w.Header().Set("ETag", quoteETag(newETag))
	}
	writeJSONResponse(w, http.StatusOK, msg)
}

func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	msgID := r.PathValue("msgId")

	// If-Match here mirrors PATCH /messages/{msgId}: the store enforces
	// the precondition atomically inside SoftDeleteMessage, so the
	// handler just parses and forwards. `*` is rejected for the same
	// reason as PATCH — a per-row wildcard adds nothing over the
	// existing row-load and would invite clients to bypass CAS.
	//
	// Empty If-Match disables the precondition (legacy unconditional
	// delete), but the agent.deleteMessage layer still runs a
	// GetMessage up front (cross-agent guard), so a bare DELETE on a
	// missing or already-tombstoned row surfaces as 404, NOT 200 —
	// callers that want strict idempotence should not rely on the
	// historical store-level "missing row → ok" path through this
	// handler.
	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	if ifMatchPresent && ifMatch == "*" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"If-Match: * is not supported on DELETE /messages/{msgId}; send a specific etag or omit the header")
		return
	}

	if err := s.agents.DeleteMessage(id, msgID, ifMatch); err != nil {
		if errors.Is(err, agent.ErrMessageETagMismatch) {
			writeError(w, http.StatusPreconditionFailed, "precondition_failed",
				"If-Match: etag mismatch")
			return
		}
		writeTranscriptEditError(w, err, msgID)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRegenerateMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	msgID := r.PathValue("msgId")

	// Optional If-Match preconditions on the *clicked* message's etag —
	// catches the case where another device edited the message between
	// the user clicking Regenerate and the request landing, which would
	// otherwise truncate the transcript against a stale view. `*` is
	// rejected for the same reason as PATCH/DELETE: regenerate is per-
	// row and a wildcard precondition adds nothing over the existing
	// row-load. Empty If-Match disables the check (legacy behaviour).
	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	if ifMatchPresent && ifMatch == "*" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"If-Match: * is not supported on POST /messages/{msgId}/regenerate; send a specific etag or omit the header")
		return
	}

	if err := s.agents.Regenerate(r.Context(), id, msgID, ifMatch); err != nil {
		if errors.Is(err, agent.ErrMessageETagMismatch) {
			writeError(w, http.StatusPreconditionFailed, "precondition_failed",
				"If-Match: etag mismatch")
			return
		}
		writeTranscriptEditError(w, err, msgID)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func writeTranscriptEditError(w http.ResponseWriter, err error, msgID string) {
	switch {
	case errors.Is(err, agent.ErrAgentNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, agent.ErrMessageNotFound):
		writeError(w, http.StatusNotFound, "not_found", "message not found: "+msgID)
	case errors.Is(err, agent.ErrInvalidRegenerate):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, agent.ErrUnsupportedTool):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, agent.ErrAgentBusy), errors.Is(err, agent.ErrAgentResetting):
		writeError(w, http.StatusConflict, "busy", err.Error())
	case errors.Is(err, agent.ErrAgentArchived):
		writeError(w, http.StatusConflict, "archived", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

// --- Generate Handlers ---

func (s *Server) handleGeneratePersona(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentPersona string `json:"currentPersona"`
		Prompt         string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "prompt is required")
		return
	}

	persona, err := agent.GeneratePersona(req.CurrentPersona, req.Prompt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]string{"persona": persona})
}

func (s *Server) handleGenerateName(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Persona string `json:"persona"`
		Prompt  string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if req.Persona == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "persona is required")
		return
	}

	name, err := agent.GenerateName(req.Persona, req.Prompt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) handleGenerateAvatar(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Persona      string `json:"persona"`
		Name         string `json:"name"`
		Prompt       string `json:"prompt"`
		PreviousPath string `json:"previousPath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	// Clean up previous temp avatar if provided
	if req.PreviousPath != "" {
		cleanupTempAvatar(req.PreviousPath)
	}

	avatarPath, err := agent.GenerateAvatarWithAI(r.Context(), "", req.Persona, req.Name, req.Prompt, s.logger)
	if err != nil {
		s.logger.Warn("AI avatar generation failed, using SVG fallback", "err", err)
		svgPath, svgErr := agent.GenerateSVGAvatarFile(req.Name)
		if svgErr != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", svgErr.Error())
			return
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"avatarPath": svgPath, "fallback": true})
		return
	}

	writeJSONResponse(w, http.StatusOK, map[string]string{"avatarPath": avatarPath})
}

// cleanupTempAvatar removes a previously generated temp avatar directory.
func cleanupTempAvatar(avatarPath string) {
	absPath, err := filepath.EvalSymlinks(avatarPath)
	if err != nil {
		return
	}
	tempDir, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return
	}
	if !strings.HasPrefix(absPath, tempDir+string(filepath.Separator)) {
		return
	}
	rel, _ := filepath.Rel(tempDir, absPath)
	parts := strings.SplitN(rel, string(filepath.Separator), 2)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "kojo-avatar-") {
		return
	}
	// Remove the entire kojo-avatar-* temp directory
	os.RemoveAll(filepath.Join(tempDir, parts[0]))
}

// handlePreviewAvatar serves a generated avatar from the temp directory for preview.
func (s *Server) handlePreviewAvatar(w http.ResponseWriter, r *http.Request) {
	avatarPath := r.URL.Query().Get("path")
	if avatarPath == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "path is required")
		return
	}

	absPath, err := agent.ValidateTempAvatarPath(avatarPath)
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrAvatarInternal):
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		case errors.Is(err, agent.ErrAvatarNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		default:
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}

	http.ServeFile(w, r, absPath)
}

// handleUploadGeneratedAvatar copies a generated avatar to the agent's directory.
// --- Credential Handlers ---

func (s *Server) requireCredentialStore(w http.ResponseWriter) bool {
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store is not available")
		return false
	}
	return true
}

func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	if !s.requireCredentialStore(w) {
		return
	}
	creds, err := s.agents.Credentials().ListCredentials(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if creds == nil {
		creds = []*agent.Credential{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"credentials": creds})
}

func (s *Server) handleAddCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// AcquireMutation BEFORE Get. ReleaseAgentLocally on the
	// source-release path deletes from m.agents BEFORE clearing
	// the switching flag, so once AcquireMutation succeeds the
	// agent is guaranteed to still be in the map (or already
	// gone and we 404). The previous order (Get → Acquire) had
	// a window where Get hit, release evicted, Acquire then
	// succeeded post-switch and wrote credentials for a
	// released agent.
	releaseMut, err := s.agents.AcquireMutation(id)
	if err != nil {
		writeError(w, http.StatusConflict, "agent_busy", err.Error())
		return
	}
	defer releaseMut()
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req struct {
		Label         string `json:"label"`
		Username      string `json:"username"`
		Password      string `json:"password"`
		TOTPSecret    string `json:"totpSecret"`
		TOTPAlgorithm string `json:"totpAlgorithm"`
		TOTPDigits    int    `json:"totpDigits"`
		TOTPPeriod    int    `json:"totpPeriod"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if req.Label == "" || req.Username == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "label and username are required")
		return
	}
	if req.Password == "" && req.TOTPSecret == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "password or totpSecret is required")
		return
	}
	if !s.requireCredentialStore(w) {
		return
	}
	var totp *agent.TOTPParams
	if req.TOTPSecret != "" {
		totp = &agent.TOTPParams{
			Secret:    req.TOTPSecret,
			Algorithm: req.TOTPAlgorithm,
			Digits:    req.TOTPDigits,
			Period:    req.TOTPPeriod,
		}
	}
	cred, err := s.agents.Credentials().AddCredential(id, req.Label, req.Username, req.Password, totp)
	if err != nil {
		if errors.Is(err, agent.ErrInvalidTOTP) {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, cred)
}

func (s *Server) handleUpdateCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	credID := r.PathValue("credId")
	// AcquireMutation before Get — see handleAddCredential
	// comment for the source-release race rationale.
	releaseMut, err := s.agents.AcquireMutation(id)
	if err != nil {
		writeError(w, http.StatusConflict, "agent_busy", err.Error())
		return
	}
	defer releaseMut()
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req struct {
		Label         *string `json:"label"`
		Username      *string `json:"username"`
		Password      *string `json:"password"`
		TOTPSecret    *string `json:"totpSecret"`
		TOTPAlgorithm *string `json:"totpAlgorithm"`
		TOTPDigits    *int    `json:"totpDigits"`
		TOTPPeriod    *int    `json:"totpPeriod"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	var totp *agent.TOTPParams
	if req.TOTPSecret != nil {
		alg := ""
		if req.TOTPAlgorithm != nil {
			alg = *req.TOTPAlgorithm
		}
		digits := 0
		if req.TOTPDigits != nil {
			digits = *req.TOTPDigits
		}
		period := 0
		if req.TOTPPeriod != nil {
			period = *req.TOTPPeriod
		}
		totp = &agent.TOTPParams{
			Secret:    *req.TOTPSecret,
			Algorithm: alg,
			Digits:    digits,
			Period:    period,
		}
	}
	if !s.requireCredentialStore(w) {
		return
	}
	cred, err := s.agents.Credentials().UpdateCredential(id, credID, req.Label, req.Username, req.Password, totp)
	if err != nil {
		if errors.Is(err, agent.ErrCredentialNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else if errors.Is(err, agent.ErrInvalidTOTP) {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, cred)
}

func (s *Server) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	credID := r.PathValue("credId")
	// AcquireMutation before Get — see handleAddCredential
	// comment for the source-release race rationale.
	releaseMut, err := s.agents.AcquireMutation(id)
	if err != nil {
		writeError(w, http.StatusConflict, "agent_busy", err.Error())
		return
	}
	defer releaseMut()
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	if !s.requireCredentialStore(w) {
		return
	}
	if err := s.agents.Credentials().DeleteCredential(id, credID); err != nil {
		if errors.Is(err, agent.ErrCredentialNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRevealCredentialPassword(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	credID := r.PathValue("credId")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	if !s.requireCredentialStore(w) {
		return
	}
	password, err := s.agents.Credentials().RevealPassword(id, credID)
	if err != nil {
		if errors.Is(err, agent.ErrCredentialNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONResponse(w, http.StatusOK, map[string]string{"password": password})
}

func (s *Server) handleGetTOTPCode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	credID := r.PathValue("credId")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	if !s.requireCredentialStore(w) {
		return
	}
	code, remaining, err := s.agents.Credentials().GetTOTPCode(id, credID)
	if err != nil {
		if errors.Is(err, agent.ErrCredentialNotFound) || errors.Is(err, agent.ErrNoTOTPSecret) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONResponse(w, http.StatusOK, map[string]any{"code": code, "remaining": remaining})
}

func (s *Server) handleParseQR(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10MB max
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid multipart form")
		return
	}

	file, _, err := r.FormFile("qr")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "missing qr file")
		return
	}
	defer file.Close()

	entries, err := agent.DecodeQRImage(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleParseOTPURI(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req struct {
		URI string `json:"uri"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if req.URI == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "uri is required")
		return
	}

	entries, err := agent.ParseOTPURI(req.URI)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleUploadGeneratedAvatar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	releaseMut, err := s.agents.AcquireMutation(id)
	if err != nil {
		writeError(w, http.StatusConflict, "agent_busy", err.Error())
		return
	}
	defer releaseMut()

	var req struct {
		AvatarPath string `json:"avatarPath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	if req.AvatarPath == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "avatarPath is required")
		return
	}

	absPath, err := agent.ValidateTempAvatarPath(req.AvatarPath)
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrAvatarInternal):
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		default:
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}

	ext := strings.ToLower(filepath.Ext(absPath))

	src, err := os.Open(absPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("cannot open avatar: %v", err))
		return
	}
	defer src.Close()

	// Serialize against Manager.Delete — see handleUploadAvatar for
	// the orphan-blob race rationale. The duplicated lock-and-recheck
	// is intentional: factoring it into a helper would obscure the
	// contract that callers MUST hold LockPatch across the live-check
	// → SaveAvatar window.
	release := s.agents.LockPatch(id)
	defer release()
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	if err := agent.SaveAvatar(s.blob, id, src, ext); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Clean up the temp file
	os.Remove(absPath)

	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Task Handlers ---
//
// Tasks live in the agent_tasks DB table post-cutover. The HTTP surface
// keeps the v0 vocabulary ("open" / "done") for status; translation
// to/from the v1 store vocabulary ('pending' / 'done') happens inside
// internal/agent/task.go.
//
// PATCH and DELETE accept a strong If-Match header for optimistic
// concurrency. The store's etag-in-WHERE clause is the canonical guard;
// the handler simply forwards the parsed value.

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tasks, err := s.agents.ListTasks(r.Context(), id)
	if err != nil {
		mapTaskErr(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var params agent.TaskCreateParams
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	task, err := s.agents.CreateTask(r.Context(), id, params)
	if err != nil {
		mapTaskErr(w, err)
		return
	}
	if task.ETag != "" {
		w.Header().Set("ETag", quoteETag(task.ETag))
	}
	writeJSONResponse(w, http.StatusOK, task)
}

func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	taskID := r.PathValue("taskId")

	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	// `*` is rejected on PATCH because the row must already exist —
	// "any current version" carries no meaning when the only valid
	// shape of "current" is "live row with some etag".
	if ifMatchPresent && ifMatch == "*" {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed",
			"If-Match: * is not supported on PATCH /tasks/{taskId}; send a specific etag or omit the header")
		return
	}

	var params agent.TaskUpdateParams
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	task, err := s.agents.UpdateTask(r.Context(), id, taskID, ifMatch, params)
	if err != nil {
		mapTaskErr(w, err)
		return
	}
	if task.ETag != "" {
		w.Header().Set("ETag", quoteETag(task.ETag))
	}
	writeJSONResponse(w, http.StatusOK, task)
}

func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	taskID := r.PathValue("taskId")

	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	if ifMatchPresent && ifMatch == "*" {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed",
			"If-Match: * is not supported on DELETE /tasks/{taskId}")
		return
	}

	if err := s.agents.DeleteTask(r.Context(), id, taskID, ifMatch); err != nil {
		mapTaskErr(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// mapTaskErr translates the typed errors raised by Manager task helpers
// into the right HTTP status. Keeps the handlers free of repeated
// errors.Is ladders.
//
// The default branch returns 500 — anything that isn't a known input
// validation error (typed sentinel + the few "title is required" /
// "title cannot be empty" cases) is treated as an internal failure so
// a SQL error or transport hiccup doesn't get masquerade as a 4xx
// "your request is bad". Callers who want to distinguish should add
// a typed sentinel rather than relying on string-matching here.
func mapTaskErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agent.ErrAgentNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, agent.ErrTaskNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, agent.ErrTaskETagMismatch):
		writeError(w, http.StatusPreconditionFailed, "precondition_failed",
			"If-Match: etag mismatch")
	case errors.Is(err, agent.ErrAgentBusy):
		writeError(w, http.StatusConflict, "agent_busy", err.Error())
	case errors.Is(err, agent.ErrInvalidTaskStatus),
		errors.Is(err, agent.ErrTaskTitleRequired),
		errors.Is(err, agent.ErrTaskTitleEmpty):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		// Unknown error — almost certainly a store / SQL / transport
		// failure. The Manager has already logged details; surface as
		// 500 so a buggy / failing backend isn't silently miscoded as
		// "client sent a bad request".
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

// --- Pre-Compact Handler ---

// preCompactHookPayload mirrors the subset of Claude Code's PreCompact hook
// JSON we care about. Claude pipes the full event object to the hook
// command's stdin; with `--data-binary @-` it lands here verbatim.
// Documented at:
//
//	https://docs.anthropic.com/en/docs/claude-code/hooks#precompact
//
// We only extract `transcript_path` so PreCompactSummarize can read the
// exact session being compacted, instead of probing by mtime in the
// project directory (which races with parallel sessions).
type preCompactHookPayload struct {
	TranscriptPath string `json:"transcript_path"`
	Trigger        string `json:"trigger"`
	SessionID      string `json:"session_id"`
}

func (s *Server) handlePreCompact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// §3.7 device switch gate: PreCompact writes memory diary /
	// recent / marker. A switch mid-write would strand those
	// entries on source.
	//
	// AcquireMutation BEFORE Get so a release between the
	// initial Get and the mutation hold can't slip past — see
	// handleAddCredential for the source-release-race
	// rationale. We re-Get the agent under the mutation hold
	// to read fresh Tool / Archived flags; a stale snapshot
	// from before AcquireMutation could carry a Tool value
	// that's since been switched (target may have changed it
	// during sync).
	releaseMut, mutErr := s.agents.AcquireMutation(id)
	if mutErr != nil {
		writeError(w, http.StatusConflict, "agent_busy", mutErr.Error())
		return
	}
	defer releaseMut()
	a, ok := s.agents.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	// Archived agents must not have summaries appended to their diary —
	// nothing should be writing into their persisted state while dormant.
	if a.Archived {
		writeError(w, http.StatusConflict, "conflict", "agent is archived")
		return
	}
	// Best-effort decode. An empty / malformed body is fine: the
	// summarizer falls back to project-dir discovery when transcript_path
	// is empty, and we still want to honour the hook in either case.
	// Cap the body so a misbehaving hook can't OOM the server with a
	// gigabyte JSON object — Claude's PreCompact payload is a few hundred
	// bytes in practice, 64 KiB is plenty of headroom.
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var payload preCompactHookPayload
	_ = json.NewDecoder(r.Body).Decode(&payload)

	// Run synchronously — the PreCompact hook blocks until this returns
	if err := agent.PreCompactSummarize(id, a.Tool, payload.TranscriptPath, s.logger); err != nil {
		s.logger.Warn("pre-compact summarize failed", "agent", id, "err", err)
		// Return 200 anyway — don't block compaction on summary failure
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Session Reset Handler ---

// handlePrivilegeAgent toggles the Privileged flag on an agent. Owner-only;
// the auth middleware already denies non-Owner principals on this route, but
// the handler reasserts the check defensively because the privilege flag is
// the only authority an agent has to reach delete/reset on others.
func (s *Server) handlePrivilegeAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if p := auth.FromContext(r.Context()); !p.CanSetPrivileged() {
		writeError(w, http.StatusForbidden, "forbidden", "privilege is owner-only")
		return
	}

	// privilege flips are admin-class mutations of the agent record;
	// same If-Match contract as PATCH applies. `*` rejected — the row
	// must already exist (we 404 below if it doesn't).
	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	if ifMatchPresent && ifMatch == "*" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"If-Match: * is not supported on POST /agents/{id}/privilege; send a specific etag or omit the header")
		return
	}

	var body struct {
		Privileged bool `json:"privileged"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	release := s.agents.LockPatch(id)
	defer release()

	if !s.agentIfMatchPrecheck(w, r, id, ifMatch, ifMatchPresent) {
		return
	}

	if err := s.agents.SetPrivileged(id, body.Privileged); err != nil {
		switch {
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrAgentBusy):
			writeError(w, http.StatusConflict, "agent_busy", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	if newETag := s.readAgentETag(r, id); newETag != "" {
		w.Header().Set("ETag", quoteETag(newETag))
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"id": id, "privileged": body.Privileged})
}

func (s *Server) handleResetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if p := auth.FromContext(r.Context()); !p.CanDeleteOrReset(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agent is not allowed to reset others")
		return
	}
	if err := s.agents.ResetSession(id); err != nil {
		if errors.Is(err, agent.ErrAgentNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else if errors.Is(err, agent.ErrAgentBusy) {
			writeError(w, http.StatusConflict, "conflict", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

