package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

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
	// Shared preamble covers both the strict-mode gate
	// (KOJO_REQUIRE_IF_MATCH=1 — matches the persona / agent PATCH
	// handlers; without it the workspace-file endpoints would silently
	// bypass the strict-mode contract every other mutating endpoint
	// enforces) and the wildcard reject. The pre-refactor code checked
	// the wildcard before the gate; the orders are observably identical
	// because a wildcard request always carries If-Match, which
	// satisfies the gate.
	ifMatch, _, ok := s.parseIfMatchStrict(w, r, "If-Match wildcard is not supported for workspace files")
	if !ok {
		return
	}
	var req workspaceFilePutRequest
	if !readCappedJSON(w, r, workspaceFileBodyCap, "request body exceeds 1 MiB cap", "invalid JSON body", &req) {
		return
	}
	// Status is structured: the settings UI renders it as a key-value
	// table, so a REST write must be a flat JSON object with scalar
	// values. Whitespace-only passes through — it means "clear" and
	// tombstones the row like every other kind. Direct disk edits by
	// the agent bypass this (the prompt injection is tolerant); the
	// validation only keeps the REST surface — and therefore the UI —
	// parseable.
	if kind == store.WorkspaceFileKindStatus && strings.TrimSpace(req.Content) != "" {
		if verr := validateStatusJSON(req.Content); verr != nil {
			writeError(w, http.StatusBadRequest, "bad_request", verr.Error())
			return
		}
	}
	// Admission gate: refuses while a §3.7 device switch or a restart
	// drain is in flight, and makes this write visible to WaitChatIdle /
	// WaitAllChatsIdle via the mutation counter — without it a workspace
	// PUT could land on the source after the switch snapshot (or between
	// a restart's idle observation and the re-exec). Acquired AFTER the
	// body read + validation so a slow client can't hold the mutation
	// slot open and stall those drains.
	releaseMut, err := s.agents.AcquireMutation(id)
	if err != nil {
		writeError(w, http.StatusConflict, "agent_busy", err.Error())
		return
	}
	defer releaseMut()
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

func (s *Server) handleGetAgentStatus(w http.ResponseWriter, r *http.Request) {
	s.handleGetWorkspaceFile(w, r, store.WorkspaceFileKindStatus)
}

func (s *Server) handlePutAgentStatus(w http.ResponseWriter, r *http.Request) {
	s.handlePutWorkspaceFile(w, r, store.WorkspaceFileKindStatus)
}

// validateStatusJSON enforces the shape contract for status.json writes
// arriving over the REST surface: a single flat JSON object whose values
// are scalars (string / number / bool). Nested objects and arrays are
// rejected so the settings UI's key-value table can always round-trip
// the document; null is rejected because the table has no way to render
// or re-emit it. Keys are free — the schema belongs to the agent.
func validateStatusJSON(content string) error {
	dec := json.NewDecoder(strings.NewReader(content))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("status must be valid JSON: %v", err)
	}
	// Trailing garbage after the object (a second document, a stray
	// closing bracket) would round-trip inconsistently. dec.More()
	// alone misses non-value trailers like `]`, so assert clean EOF.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("status must be a single JSON document")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return errors.New("status must be a JSON object of key-value pairs")
	}
	for k, val := range obj {
		switch val.(type) {
		case string, json.Number, bool:
		default:
			return fmt.Errorf("status value for %q must be a string, number, or boolean (nested objects, arrays, and null are not supported)", k)
		}
	}
	// map decoding silently keeps the last of duplicate keys — count
	// the keys at token level and reject when the document carried
	// more than the map retained, so a duplicated key can't silently
	// drop data on the write path.
	tok := json.NewDecoder(strings.NewReader(content))
	if _, err := tok.Token(); err != nil { // consume '{'
		return fmt.Errorf("status must be valid JSON: %v", err)
	}
	keys := 0
	for tok.More() {
		if _, err := tok.Token(); err != nil { // key
			return fmt.Errorf("status must be valid JSON: %v", err)
		}
		var skip json.RawMessage
		if err := tok.Decode(&skip); err != nil { // value
			return fmt.Errorf("status must be valid JSON: %v", err)
		}
		keys++
	}
	if keys != len(obj) {
		return errors.New("status must not contain duplicate keys")
	}
	return nil
}
