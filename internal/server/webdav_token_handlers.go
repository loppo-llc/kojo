package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// webdavTokenIssueMaxBody bounds the POST request body for a token
// issue. The payload is two small fields (label + ttlSeconds); 4 KiB
// is a generous ceiling that still kills a malicious "stuff every
// byte you can" client before it allocates anything dangerous.
const webdavTokenIssueMaxBody = 4 << 10

// webdavTokenIssueRequest mirrors the POST /api/v1/auth/webdav-tokens
// body. ttlSeconds is the operator-supplied lifetime; label is a free-
// form note used for the list view.
type webdavTokenIssueRequest struct {
	Label      string `json:"label"`
	TTLSeconds int64  `json:"ttlSeconds"`
}

// webdavTokenIssueResponse is the single-shot view of a freshly issued
// token. The `token` field is the only chance the client has to read
// the raw value; subsequent list responses elide it.
type webdavTokenIssueResponse struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	Label     string `json:"label"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
}

// handleListWebDAVTokens returns metadata for every live (non-expired)
// short-lived WebDAV token. Owner-only.
func (s *Server) handleListWebDAVTokens(w http.ResponseWriter, r *http.Request) {
	if s.webdavTokens == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "webdav token store unavailable")
		return
	}
	if !auth.FromContext(r.Context()).IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden", "owner-only")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"items": s.webdavTokens.List(),
	})
}

// handleIssueWebDAVToken mints a fresh short-lived token. Owner-only.
// The raw value is returned exactly once; on success the operator is
// expected to hand it to the WebDAV client immediately (no second-read
// API exists).
func (s *Server) handleIssueWebDAVToken(w http.ResponseWriter, r *http.Request) {
	if s.webdavTokens == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "webdav token store unavailable")
		return
	}
	if !auth.FromContext(r.Context()).IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden", "owner-only")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, webdavTokenIssueMaxBody))
	if err != nil {
		// MaxBytesReader produces a *MaxBytesError with the cap; map
		// it to 413 so the client knows to shrink the payload.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error())
		return
	}
	var req webdavTokenIssueRequest
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "request body required")
		return
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid json: "+err.Error())
		return
	}
	if req.TTLSeconds <= 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "ttlSeconds must be positive")
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	res, err := s.webdavTokens.Issue(ctx, req.Label, ttl)
	if err != nil {
		// The store's validation errors are all caller-correctable;
		// surface them as 400. A kv write failure (server-side) would
		// already have a structured error wrap that we still want to
		// surface to the operator for diagnosis.
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusCreated, webdavTokenIssueResponse{
		ID:        res.ID,
		Token:     res.Token,
		Label:     res.Label,
		CreatedAt: res.CreatedAt,
		ExpiresAt: res.ExpiresAt,
	})
}

// handleRevokeWebDAVToken removes a token by id. Owner-only. Idempotent
// — DELETE on a missing id is 204 so the operator can safely re-issue
// a revoke after a network blip.
func (s *Server) handleRevokeWebDAVToken(w http.ResponseWriter, r *http.Request) {
	if s.webdavTokens == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "webdav token store unavailable")
		return
	}
	if !auth.FromContext(r.Context()).IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden", "owner-only")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.webdavTokens.Revoke(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Idempotent revoke: a missing id is success, not 404.
			// The operator's mental model is "make this token go
			// away"; if it's already gone, the post-condition holds.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
