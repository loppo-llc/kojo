package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// memoryResponse is the JSON shape returned by GET / PUT /memory. Fields
// mirror agent.MemoryRecord but with explicit JSON tags so the wire
// schema is stable.
type memoryResponse struct {
	AgentID   string `json:"agentId"`
	Body      string `json:"body"`
	ETag      string `json:"etag"`
	UpdatedAt int64  `json:"updatedAt,omitempty"`
}

// memoryPutRequest is the body of PUT /memory.
type memoryPutRequest struct {
	Body string `json:"body"`
}

// handleGetAgentMemory returns the current MEMORY.md state from the v1
// store. Disk file is canonical for the local CLI but readers (Web UI,
// other devices) consume the synced row so cross-device reads are
// consistent. Returns 404 when the row hasn't been synced yet (brand-
// new agent before ensureAgentDir, or post-DELETE tombstone).
func (s *Server) handleGetAgentMemory(w http.ResponseWriter, r *http.Request) {
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
	rec, err := s.agents.GetAgentMemory(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "memory has not been synced yet")
		case errors.Is(err, agent.ErrAgentResetting):
			writeError(w, http.StatusConflict, "conflict", "agent is mid-reset; try again")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}

	// Honour If-None-Match for the cache-friendly 304 path. The Web UI
	// uses this to avoid re-downloading large MEMORY.md bodies on every
	// poll when nothing has changed.
	w.Header().Set("ETag", quoteETag(rec.ETag))
	if cached, ok := extractDomainIfNoneMatch(r); ok && (cached == "*" || cached == rec.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSONResponse(w, http.StatusOK, memoryResponse{
		AgentID:   rec.AgentID,
		Body:      rec.Body,
		ETag:      rec.ETag,
		UpdatedAt: rec.UpdatedAt,
	})
}

// handlePutAgentMemory writes body into the agent's MEMORY.md file and
// syncs the v1 store row. Honours If-Match for optimistic concurrency:
// stale precondition → 412. The on-disk file is what the CLI process
// reads on next session, so the write side-effects propagate to the
// agent's prompt without an explicit reload.
func (s *Server) handlePutAgentMemory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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
	// `*` is meaningful for create-or-replace, but for MEMORY.md
	// "any current state is acceptable" doesn't compose well with
	// the file-and-DB write trio (we'd silently overwrite a value
	// the user didn't see). Refuse rather than silently accept.
	if ifMatchPresent && ifMatch == "*" {
		writeError(w, http.StatusBadRequest, "bad_request", "If-Match wildcard is not supported for MEMORY.md")
		return
	}

	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	// MaxBytesReader (NOT LimitReader): a 4MiB+1 body returns an
	// error from ReadAll, mapped to 413. LimitReader silently
	// truncates which would let a malformed but valid-prefix JSON
	// land successfully — the wrong failure mode for an oversized
	// upload.
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "MEMORY.md body exceeds 4 MiB cap")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	var req memoryPutRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}

	rec, err := s.agents.PutAgentMemory(r.Context(), id, req.Body, ifMatch)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrETagMismatch):
			writeError(w, http.StatusPreconditionFailed, "precondition_failed", "If-Match: etag mismatch")
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrAgentArchived):
			writeError(w, http.StatusConflict, "conflict", "agent is archived")
		case errors.Is(err, agent.ErrAgentBusy), errors.Is(err, agent.ErrAgentResetting):
			writeError(w, http.StatusConflict, "conflict", "agent is mid-reset or busy; try again")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}

	w.Header().Set("ETag", quoteETag(rec.ETag))
	writeJSONResponse(w, http.StatusOK, memoryResponse{
		AgentID:   rec.AgentID,
		Body:      rec.Body,
		ETag:      rec.ETag,
		UpdatedAt: rec.UpdatedAt,
	})
}

// handleDeleteAgentMemory removes the on-disk MEMORY.md and tombstones
// the v1 store row. Honours If-Match. The next ensureAgentDir / Create
// flow won't run for an existing agent — to recreate, the caller PUTs
// a new body.
func (s *Server) handleDeleteAgentMemory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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
		writeError(w, http.StatusBadRequest, "bad_request", "If-Match wildcard is not supported for MEMORY.md")
		return
	}
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	if err := s.agents.DeleteAgentMemory(r.Context(), id, ifMatch); err != nil {
		switch {
		case errors.Is(err, store.ErrETagMismatch):
			writeError(w, http.StatusPreconditionFailed, "precondition_failed", "If-Match: etag mismatch")
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrAgentArchived):
			writeError(w, http.StatusConflict, "conflict", "agent is archived")
		case errors.Is(err, agent.ErrAgentBusy), errors.Is(err, agent.ErrAgentResetting):
			writeError(w, http.StatusConflict, "conflict", "agent is mid-reset or busy; try again")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
