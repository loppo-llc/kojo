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

// workspaceFileBodyCap caps the PUT body for user.md / checkin.md at 1 MiB.
// These files are inlined into the system prompt every chat turn so an
// unbounded write would balloon both the prompt cache prefix and the
// system-prompt build cost. 1 MiB is far above any realistic hand-edited
// workspace file (4 KiB is typical) while small enough that hitting it
// almost certainly indicates a misuse or attack.
const workspaceFileBodyCap = 1 << 20

// workspaceFileResponse is the JSON shape returned by GET / PUT for both
// /user-context and /checkin-file. `etag` is empty for the in-memory
// default-template path (isDefault=true) — the row hasn't been written
// yet, so there's nothing for a future If-Match to assert against. After
// the first save the response carries the canonical agent_workspace_files
// etag.
type workspaceFileResponse struct {
	Content   string `json:"content"`
	IsDefault bool   `json:"isDefault"`
	ETag      string `json:"etag,omitempty"`
}

// workspaceFilePutRequest is the body shape for PUT /user-context and
// PUT /checkin-file. Empty content is a valid request — the server
// treats whitespace-only as "clear" and tombstones the row (see
// agent.WriteWorkspaceFile).
type workspaceFilePutRequest struct {
	Content string `json:"content"`
}

// handleGetWorkspaceFile is the shared GET implementation. The caller-
// provided `kind` picks which workspace file to read.
//
// DB-canonical: `agent.ReadWorkspaceFile` returns the row from
// agent_workspace_files when present; for agents that never saved a
// row the DefaultUserContent / DefaultCheckinContent template is
// returned with isDefault=true and an empty etag — the UI uses the
// flag to suppress no-op PUTs on save.
//
// If-None-Match handling mirrors persona: on match the response is a
// 304 with the ETag header so the client's cached body can be reused.
// The default-template path skips ETag emission entirely (the row
// doesn't exist yet so there's no stable etag to assert).
func (s *Server) handleGetWorkspaceFile(w http.ResponseWriter, r *http.Request, kind store.WorkspaceFileKind) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	p := auth.FromContext(r.Context())
	if !p.CanReadFull(id) {
		writeError(w, http.StatusForbidden, "forbidden", "not allowed to read this agent's workspace file")
		return
	}
	body, isDefault, etag, err := agent.ReadWorkspaceFile(r.Context(), s.agents.Store(), id, kind)
	if err != nil {
		s.logger.Warn("workspace file: read failed", "agent", id, "kind", string(kind), "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to read workspace file")
		return
	}
	if etag != "" {
		w.Header().Set("ETag", quoteETag(etag))
		if cached, ok := extractDomainIfNoneMatch(r); ok && (cached == "*" || cached == etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	writeJSONResponse(w, http.StatusOK, workspaceFileResponse{
		Content:   body,
		IsDefault: isDefault,
		ETag:      etag,
	})
}

// handlePutWorkspaceFile is the shared PUT implementation. If-Match is
// honoured the same way persona's PUT does: optional but, when present,
// a `*` wildcard is rejected (the disk-and-DB mirror pair doesn't
// compose with "any current state is acceptable") and a non-matching
// etag surfaces as 412.
//
// Empty / whitespace-only `content` tombstones the row and removes the
// disk mirror — agent.WriteWorkspaceFile handles both halves under one
// store transaction. After the write the response echoes the freshly
// computed body / etag (or the default template + isDefault=true when
// the row was just cleared) so the client can update its in-memory
// snapshot without a follow-up GET.
func (s *Server) handlePutWorkspaceFile(w http.ResponseWriter, r *http.Request, kind store.WorkspaceFileKind) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	p := auth.FromContext(r.Context())
	if !p.CanMutateSelf(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agents may only PUT their own workspace file")
		return
	}
	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if ifMatchPresent && ifMatch == "*" {
		writeError(w, http.StatusBadRequest, "bad_request", "If-Match wildcard is not supported for workspace files")
		return
	}
	// Strict-mode gate (KOJO_REQUIRE_IF_MATCH=1): reject the PUT when the
	// caller didn't send an If-Match. Matches the persona / agent PATCH
	// handlers — without this gate the workspace-file endpoints would
	// silently bypass the strict-mode contract every other mutating
	// endpoint enforces.
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, workspaceFileBodyCap)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1 MiB cap")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	var req workspaceFilePutRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	rec, err := agent.WriteWorkspaceFile(r.Context(), s.agents.Store(), id, kind, req.Content, ifMatch)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrETagMismatch):
			writeError(w, http.StatusPreconditionFailed, "precondition_failed", "If-Match: etag mismatch")
		case errors.Is(err, store.ErrNotFound):
			// Parent agent tombstoned mid-write. Same 404 surface as a
			// missing-agent lookup at the top of the handler — the
			// caller can refetch the agent list to reconcile.
			writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		default:
			s.logger.Warn("workspace file: write failed", "agent", id, "kind", string(kind), "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to write workspace file")
		}
		return
	}
	// rec == nil means "cleared" (empty body, store row tombstoned, disk
	// mirror removed). Re-read so the response shows the default
	// template + isDefault=true, matching what a follow-up GET would
	// return.
	if rec == nil {
		body, isDefault, _, rerr := agent.ReadWorkspaceFile(r.Context(), s.agents.Store(), id, kind)
		if rerr != nil {
			s.logger.Warn("workspace file: post-clear re-read failed", "agent", id, "kind", string(kind), "err", rerr)
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to read workspace file after clear")
			return
		}
		writeJSONResponse(w, http.StatusOK, workspaceFileResponse{
			Content:   body,
			IsDefault: isDefault,
		})
		return
	}
	w.Header().Set("ETag", quoteETag(rec.ETag))
	writeJSONResponse(w, http.StatusOK, workspaceFileResponse{
		Content:   rec.Body,
		IsDefault: false,
		ETag:      rec.ETag,
	})
}

// handleGetCheckinFile / handlePutCheckinFile / handleGetUserContext /
// handleSetUserContext are the route-registered entry points. Each is a
// one-line dispatch to the shared implementation with the appropriate
// kind — keeping the indirection thin so the per-route auth gate (see
// internal/auth/policy.go::isSelfScopedRoute) stays directly on the
// public method.

func (s *Server) handleGetCheckinFile(w http.ResponseWriter, r *http.Request) {
	s.handleGetWorkspaceFile(w, r, store.WorkspaceFileKindCheckin)
}

func (s *Server) handlePutCheckinFile(w http.ResponseWriter, r *http.Request) {
	s.handlePutWorkspaceFile(w, r, store.WorkspaceFileKindCheckin)
}

func (s *Server) handleGetUserContext(w http.ResponseWriter, r *http.Request) {
	s.handleGetWorkspaceFile(w, r, store.WorkspaceFileKindUser)
}

func (s *Server) handleSetUserContext(w http.ResponseWriter, r *http.Request) {
	s.handlePutWorkspaceFile(w, r, store.WorkspaceFileKindUser)
}
