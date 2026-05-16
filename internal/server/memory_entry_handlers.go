package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// memoryEntryResponse is the wire shape for a single memory_entries row.
type memoryEntryResponse struct {
	ID        string `json:"id"`
	AgentID   string `json:"agentId"`
	Seq       int64  `json:"seq"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Body      string `json:"body"`
	ETag      string `json:"etag"`
	CreatedAt int64  `json:"createdAt,omitempty"`
	UpdatedAt int64  `json:"updatedAt,omitempty"`
}

// memoryEntryListResponse is the wire shape for the LIST endpoint.
// NextCursor is set when the caller should request another page; empty
// (zero) means the page was the last.
type memoryEntryListResponse struct {
	Entries    []memoryEntryResponse `json:"entries"`
	NextCursor int64                 `json:"nextCursor,omitempty"`
}

// memoryEntryCreateRequest is the body of POST /memory-entries. All
// fields required.
type memoryEntryCreateRequest struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Body string `json:"body"`
}

// memoryEntryPatchRequest is the body of PATCH. Pointer fields so the
// JSON unmarshal preserves "field absent" vs "field set to empty
// string"; the agent layer interprets nil as "leave unchanged".
type memoryEntryPatchRequest struct {
	Kind *string `json:"kind,omitempty"`
	Name *string `json:"name,omitempty"`
	Body *string `json:"body,omitempty"`
}

// memoryEntryListPageDefault paginates LIST. Mirrors the daemon-side
// memory sync's 500 page so cross-device readers see the same chunk
// rhythm regardless of which path returned the rows.
const memoryEntryListPageDefault = 200
const memoryEntryListPageMax = 500

// memoryEntryRequestCap bounds the entire JSON request envelope
// (body field plus any other JSON keys plus encoding overhead). The
// agent layer caps the DECODED body field at 4 MiB; this is the
// wire-side limiter only. JSON encoding of typical markdown text
// adds ~5–10 % overhead (quotes, backslashes); pathological inputs
// (every byte a `"` or 0x00–0x1F control rune) expand 2x–6x. We
// allow up to 2x of the body cap plus a 64 KiB envelope — that
// covers ~99 % of realistic content while bounding wire bandwidth
// against DoS via "send a 24 MiB blob of NULs that decode to 4 MiB".
// Inputs that escape past 2x get rejected at the wire layer (413);
// the caller can split or compress them. The agent layer remains
// the authoritative cap on decoded size.
const memoryEntryRequestCap = (memoryEntryWireBodyMultiplier << 20) + (64 << 10)
const memoryEntryWireBodyMultiplier = 4 * 2 // 4 MiB body cap × 2 = 8 MiB

// validMemoryKindForHTTP duplicates the agent-layer set so we can
// reject a bad `kind` query param without doing a sync first. Kept
// in sync with agent.validMemoryKindForFS by code review (the set
// is small + stable enough that a runtime sync wasn't worth it).
var validMemoryKindForHTTP = map[string]bool{
	"daily":   true,
	"project": true,
	"topic":   true,
	"people":  true,
	"archive": true,
}

// memoryEntryErrorContext is a short label included in the daemon
// log when mapMemoryEntryError falls through to 500. The handler
// passes its own name so log triage can spot which endpoint hit
// the operational error.
type memoryEntryErrorContext string

// logMemoryEntryServerError logs an operational failure that was
// mapped to 500. The wire response gets a generic message via
// mapMemoryEntryError; this is the matching log emitter so the
// underlying error (with absolute paths, syscall numbers, etc.)
// still lands in the daemon's logs for triage.
func (s *Server) logMemoryEntryServerError(ctx memoryEntryErrorContext, err error) {
	if s.logger == nil {
		return
	}
	s.logger.Error("memory entry handler: operational failure",
		"context", string(ctx), "err", err)
}

// mapMemoryEntryError translates an agent-layer error to an HTTP
// status + error code triple. Validation errors → 400; collision →
// 409; agent lifecycle states → 404 / 409; everything else falls
// through to 500 so the caller can distinguish their bug from ours.
//
// Returns (status, code, message). The caller writes the response.
// On a 500 result the caller MUST also call logMemoryEntryServerError
// so the path-bearing error reaches the daemon log.
func mapMemoryEntryError(err error) (int, string, string) {
	switch {
	case errors.Is(err, store.ErrETagMismatch):
		return http.StatusPreconditionFailed, "precondition_failed", "If-Match: etag mismatch"
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "not_found", "memory entry not found"
	case errors.Is(err, agent.ErrMemoryEntryExists):
		return http.StatusConflict, "conflict", err.Error()
	case errors.Is(err, agent.ErrMemoryEntryRenameUnsupported):
		return http.StatusBadRequest, "bad_request", err.Error()
	case errors.Is(err, agent.ErrMemoryEntryNotCanonical):
		// Don't leak server-side absolute paths to the client. The
		// agent layer's err.Error() contains the legacy file path
		// for server logs; the wire response gets a generic message
		// that still tells the caller what to do.
		return http.StatusConflict, "conflict",
			"memory entry is at a non-canonical path; delete and recreate"
	case errors.Is(err, agent.ErrMemoryEntryStoredCorrupt):
		// Server-side bad data — caller can't fix this. Fall to 500
		// so monitoring catches it; surface a generic message so a
		// hostile probe doesn't get internal kind/name details.
		return http.StatusInternalServerError, "internal_error", "stored memory entry data is invalid"
	case errors.Is(err, agent.ErrInvalidMemoryEntry):
		return http.StatusBadRequest, "bad_request", err.Error()
	case errors.Is(err, agent.ErrAgentNotFound):
		return http.StatusNotFound, "not_found", err.Error()
	case errors.Is(err, agent.ErrAgentArchived):
		return http.StatusConflict, "conflict", "agent is archived"
	case errors.Is(err, agent.ErrAgentBusy), errors.Is(err, agent.ErrAgentResetting):
		return http.StatusConflict, "conflict", "agent is mid-reset or busy; try again"
	default:
		// Operational failure (DB, file I/O, sync). Don't pretend
		// it was the caller's fault — surface as 500 so logs and
		// alerts catch it. The wire response gets a generic message
		// because err.Error() can carry server-internal absolute
		// paths (scan/stat/write/remove wraps include filepath.Join
		// output); the unwrapped error is what the daemon logs.
		return http.StatusInternalServerError, "internal_error", "internal server error"
	}
}

func toMemoryEntryResponse(rec *agent.MemoryEntryRecord) memoryEntryResponse {
	return memoryEntryResponse{
		ID:        rec.ID,
		AgentID:   rec.AgentID,
		Seq:       rec.Seq,
		Kind:      rec.Kind,
		Name:      rec.Name,
		Body:      rec.Body,
		ETag:      rec.ETag,
		CreatedAt: rec.CreatedAt,
		UpdatedAt: rec.UpdatedAt,
	}
}

// handleListAgentMemoryEntries paginates an agent's memory_entries.
// Query params:
//   - kind: optional filter (must be a valid kind if non-empty)
//   - limit: optional page size (default 200, capped at 500)
//   - cursor: optional opaque seq cursor returned from a prior page
//
// Returns rows directly from the DB without a disk→DB pre-sync —
// the agent layer runs sync hooks at Manager.Load / fork /
// prepareChat boundaries, and mutation endpoints (POST / PATCH /
// DELETE here) all pre-sync, so by the time a Web UI edits + reads
// back, the row is fresh. A LIST during a brief window between a
// CLI write and the next sync hook may show slightly stale data;
// callers polling for changes use ?since cursor on /api/v1/changes
// for stronger freshness. Auth: CanReadFull on the agent.
func (s *Server) handleListAgentMemoryEntries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := auth.FromContext(r.Context())
	if !p.CanReadFull(id) {
		writeError(w, http.StatusForbidden, "forbidden", "access to this agent's memory is forbidden")
		return
	}
	if !s.agentKnown(id) {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	q := r.URL.Query()
	// Validate kind here so a bad `kind` param surfaces as a 400 at
	// the handler boundary rather than leaking the store-layer's
	// "invalid kind" string out as a 500. (LIST itself no longer
	// pre-syncs, so the historical "avoid a full disk walk" reason
	// is moot — but cheap input validation belongs at the edge
	// regardless.)
	if k := q.Get("kind"); k != "" && !validMemoryKindForHTTP[k] {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid kind")
		return
	}
	opts := agent.MemoryEntryListOptions{
		Kind:  q.Get("kind"),
		Limit: memoryEntryListPageDefault,
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid limit")
			return
		}
		if n > memoryEntryListPageMax {
			n = memoryEntryListPageMax
		}
		opts.Limit = n
	}
	if v := q.Get("cursor"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid cursor")
			return
		}
		opts.Cursor = n
	}

	rows, err := s.agents.ListAgentMemoryEntries(r.Context(), id, opts)
	if err != nil {
		status, code, msg := mapMemoryEntryError(err)
		if status == http.StatusInternalServerError {
			s.logMemoryEntryServerError("list", err)
		}
		writeError(w, status, code, msg)
		return
	}
	out := memoryEntryListResponse{Entries: make([]memoryEntryResponse, 0, len(rows))}
	for _, rec := range rows {
		out.Entries = append(out.Entries, toMemoryEntryResponse(rec))
	}
	// Only emit a cursor when the page was full — saves the client a
	// follow-up GET that would return 0 rows.
	if len(rows) == opts.Limit && len(rows) > 0 {
		out.NextCursor = rows[len(rows)-1].Seq
	}
	writeJSONResponse(w, http.StatusOK, out)
}

// handleGetAgentMemoryEntry returns one memory entry by id. Honours
// If-None-Match for the 304 fast path (the entry editor polls).
func (s *Server) handleGetAgentMemoryEntry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entryID := r.PathValue("entryId")
	p := auth.FromContext(r.Context())
	if !p.CanReadFull(id) {
		writeError(w, http.StatusForbidden, "forbidden", "access to this agent's memory is forbidden")
		return
	}
	if !s.agentKnown(id) {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	rec, err := s.agents.GetAgentMemoryEntry(r.Context(), id, entryID)
	if err != nil {
		status, code, msg := mapMemoryEntryError(err)
		if status == http.StatusInternalServerError {
			s.logMemoryEntryServerError("get", err)
		}
		writeError(w, status, code, msg)
		return
	}

	w.Header().Set("ETag", quoteETag(rec.ETag))
	if cached, ok := extractDomainIfNoneMatch(r); ok && (cached == "*" || cached == rec.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSONResponse(w, http.StatusOK, toMemoryEntryResponse(rec))
}

// handleCreateAgentMemoryEntry creates a new memory entry. The body
// is capped at 4 MiB; oversize requests get 413. (kind, name) collisions
// against an existing live row → 409. Auth: CanMutateSelf.
func (s *Server) handleCreateAgentMemoryEntry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := auth.FromContext(r.Context())
	if !p.CanMutateSelf(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agents may only edit their own memory")
		return
	}
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, memoryEntryRequestCap)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds cap")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	var req memoryEntryCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}

	rec, err := s.agents.CreateAgentMemoryEntry(r.Context(), id, req.Kind, req.Name, req.Body)
	if err != nil {
		status, code, msg := mapMemoryEntryError(err)
		if status == http.StatusInternalServerError {
			s.logMemoryEntryServerError("create", err)
		}
		writeError(w, status, code, msg)
		return
	}

	w.Header().Set("ETag", quoteETag(rec.ETag))
	writeJSONResponse(w, http.StatusCreated, toMemoryEntryResponse(rec))
}

// handleUpdateAgentMemoryEntry applies a body-only PATCH. Honours
// If-Match (rejecting `*`). A patch carrying `kind` or `name` is
// refused with 400 (ErrMemoryEntryRenameUnsupported) — rename is
// DELETE + CREATE. Body cap mirrors Create (4 MiB decoded).
func (s *Server) handleUpdateAgentMemoryEntry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entryID := r.PathValue("entryId")
	p := auth.FromContext(r.Context())
	if !p.CanMutateSelf(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agents may only edit their own memory")
		return
	}
	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	// `*` doesn't compose with the file-and-DB write trio (we'd
	// silently overwrite a value the user didn't see). Mirror the
	// MEMORY.md handler's policy.
	if ifMatchPresent && ifMatch == "*" {
		writeError(w, http.StatusBadRequest, "bad_request", "If-Match wildcard is not supported")
		return
	}
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, memoryEntryRequestCap)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds cap")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	var req memoryEntryPatchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}

	patch := agent.MemoryEntryPatch{
		Kind: req.Kind,
		Name: req.Name,
		Body: req.Body,
	}
	rec, err := s.agents.UpdateAgentMemoryEntry(r.Context(), id, entryID, ifMatch, patch)
	if err != nil {
		status, code, msg := mapMemoryEntryError(err)
		if status == http.StatusInternalServerError {
			s.logMemoryEntryServerError("update", err)
		}
		writeError(w, status, code, msg)
		return
	}

	w.Header().Set("ETag", quoteETag(rec.ETag))
	writeJSONResponse(w, http.StatusOK, toMemoryEntryResponse(rec))
}

// handleDeleteAgentMemoryEntry tombstones the entry. Honours If-Match
// (rejecting `*`). Idempotent without If-Match — repeating a delete
// against a missing/tombstoned row returns 204.
func (s *Server) handleDeleteAgentMemoryEntry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entryID := r.PathValue("entryId")
	p := auth.FromContext(r.Context())
	if !p.CanMutateSelf(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agents may only edit their own memory")
		return
	}
	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	if ifMatchPresent && ifMatch == "*" {
		writeError(w, http.StatusBadRequest, "bad_request", "If-Match wildcard is not supported")
		return
	}
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	if err := s.agents.DeleteAgentMemoryEntry(r.Context(), id, entryID, ifMatch); err != nil {
		status, code, msg := mapMemoryEntryError(err)
		if status == http.StatusInternalServerError {
			s.logMemoryEntryServerError("delete", err)
		}
		writeError(w, status, code, msg)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
