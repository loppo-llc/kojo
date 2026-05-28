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

// personaResponse is the JSON shape returned by GET / PUT /persona.
type personaResponse struct {
	AgentID   string `json:"agentId"`
	Body      string `json:"body"`
	ETag      string `json:"etag"`
	UpdatedAt int64  `json:"updatedAt,omitempty"`
}

// personaPutRequest is the body of PUT /persona. Empty body is the
// "clear persona" path — agent.PutAgentPersona / writePersonaFile
// handle the empty-→-remove file contract.
type personaPutRequest struct {
	Body string `json:"body"`
}

// personaRequestCap matches memoryEntryRequestCap's reasoning: 4 MiB
// decoded body cap plus envelope overhead. JSON expansion of
// markdown text is typically <1.5x; 8 MiB is the wire ceiling.
const personaRequestCap = (4 << 20 * 2) + (64 << 10)

// handleGetAgentPersona returns the current persona body from the v1
// store. The on-disk persona.md remains canonical for the CLI
// (syncPersona reads it on every prepareChat); the DB row is the
// cross-device read path. 404 when the row hasn't been synced yet
// (brand-new agent before its first save, or post-tombstone).
func (s *Server) handleGetAgentPersona(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := auth.FromContext(r.Context())
	if !p.CanReadFull(id) {
		writeError(w, http.StatusForbidden, "forbidden", "access to this agent's persona is forbidden")
		return
	}
	// No s.agents.Get(id) precheck here: m.Get triggers
	// syncPersona's disk-→DB side effects (and could spawn a
	// publicProfile regen goroutine) before we'd authenticate the
	// row. agent.GetAgentPersona returns ErrAgentNotFound for
	// unknown ids; we map it below.
	rec, err := s.agents.GetAgentPersona(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "persona has not been synced yet")
		case errors.Is(err, agent.ErrAgentResetting):
			writeError(w, http.StatusConflict, "conflict", "agent is mid-reset; try again")
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		default:
			s.logger.Error("persona handler: GET operational failure", "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		}
		return
	}

	w.Header().Set("ETag", quoteETag(rec.ETag))
	if cached, ok := extractDomainIfNoneMatch(r); ok && (cached == "*" || cached == rec.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSONResponse(w, http.StatusOK, personaResponse{
		AgentID:   rec.AgentID,
		Body:      rec.Body,
		ETag:      rec.ETag,
		UpdatedAt: rec.UpdatedAt,
	})
}

// handlePutAgentPersona writes body into the agent's persona.md and
// upserts the matching agent_persona row. If-Match enforced; `*` is
// rejected (the disk-and-DB pair doesn't compose with "any current
// state is acceptable" — same policy as MEMORY.md).
//
// Empty body clears the persona (writePersonaFile removes the file
// when content == "" and the DB row is upserted with empty body so
// cross-device readers observe the cleared state).
func (s *Server) handlePutAgentPersona(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := auth.FromContext(r.Context())
	if !p.CanMutateSelf(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agents may only edit their own persona")
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
		writeError(w, http.StatusBadRequest, "bad_request", "If-Match wildcard is not supported for persona")
		return
	}
	// No s.agents.Get(id) precheck: same syncPersona-side-effect
	// concern as GET. PutAgentPersona returns ErrAgentNotFound /
	// ErrAgentArchived / ErrAgentResetting which the switch below
	// maps to 404 / 409.

	r.Body = http.MaxBytesReader(w, r.Body, personaRequestCap)
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
	var req personaPutRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}

	rec, err := s.agents.PutAgentPersona(r.Context(), id, req.Body, ifMatch)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrETagMismatch):
			writeError(w, http.StatusPreconditionFailed, "precondition_failed", "If-Match: etag mismatch")
		case errors.Is(err, agent.ErrInvalidPersona):
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrAgentArchived):
			writeError(w, http.StatusConflict, "conflict", "agent is archived")
		case errors.Is(err, agent.ErrAgentResetting):
			writeError(w, http.StatusConflict, "conflict", "agent is mid-reset; try again")
		case errors.Is(err, agent.ErrAgentBusy):
			// §3.7 device switch (or other mutation) is mid-
			// flight; surface as 409 so the client retries
			// instead of treating it as a server bug.
			writeError(w, http.StatusConflict, "agent_busy", err.Error())
		default:
			s.logger.Error("persona handler: PUT operational failure", "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		}
		return
	}

	w.Header().Set("ETag", quoteETag(rec.ETag))
	writeJSONResponse(w, http.StatusOK, personaResponse{
		AgentID:   rec.AgentID,
		Body:      rec.Body,
		ETag:      rec.ETag,
		UpdatedAt: rec.UpdatedAt,
	})
}
