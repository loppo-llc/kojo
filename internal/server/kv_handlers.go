package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

var _ = strings.HasPrefix // keep strings import for path-segment validator

// kvResponse is the wire shape for one kv row. Secret rows expose
// metadata only — the encrypted blob never leaves the daemon. Type /
// scope are echoed so the editor can preserve them on PUT.
type kvResponse struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	Value     string `json:"value,omitempty"`
	Type      string `json:"type"`
	Secret    bool   `json:"secret"`
	Scope     string `json:"scope"`
	ETag      string `json:"etag"`
	UpdatedAt int64  `json:"updatedAt,omitempty"`
}

type kvListResponse struct {
	Items []kvResponse `json:"items"`
}

type kvPutRequest struct {
	Value  string `json:"value"`
	Type   string `json:"type"`            // "string" / "json" / "binary"; default "string"
	Secret bool   `json:"secret,omitempty"` // refused at this slice — daemon-internal only
	Scope  string `json:"scope"`           // "global" / "local" / "machine"; default "global"
}

// kvRequestCap caps the JSON request size. KV values are config /
// small blobs; 1 MiB is a generous wire ceiling.
const kvRequestCap = 1 << 20

// validateKVNamespace and validateKVKey enforce a conservative
// charset so a malicious caller can't smuggle path-traversal-style
// values through the URL params. The store itself accepts any
// non-empty string but the wire contract is stricter.
//
// Allow: ASCII letters, digits, `_`, `-`, `.`, `/`. Reject empty,
// leading/trailing slash, `..` segment.
func validateKVPathSegment(s, what string) error {
	if s == "" {
		return errors.New(what + " must not be empty")
	}
	if strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") {
		return errors.New(what + " must not start or end with /")
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '_' || r == '-' || r == '.' || r == '/' {
			continue
		}
		return errors.New(what + " contains an unsupported character")
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return errors.New(what + " has an invalid segment")
		}
	}
	return nil
}

func toKVResponse(rec *store.KVRecord) kvResponse {
	out := kvResponse{
		Namespace: rec.Namespace,
		Key:       rec.Key,
		Type:      string(rec.Type),
		Secret:    rec.Secret,
		Scope:     string(rec.Scope),
		ETag:      rec.ETag,
		UpdatedAt: rec.UpdatedAt,
	}
	// Secret rows: metadata only; encrypted value never leaves the
	// daemon over HTTP. Non-secret rows: plaintext value passes
	// through.
	if !rec.Secret {
		out.Value = rec.Value
	}
	return out
}

// handleListKV lists all rows in a namespace. Owner-only — kv can
// hold arbitrary data including non-VAPID secrets.
func (s *Server) handleListKV(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden", "kv API is owner-only")
		return
	}
	ns := r.PathValue("namespace")
	if err := validateKVPathSegment(ns, "namespace"); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	st := s.agents.Store()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "store not initialized")
		return
	}
	rows, err := st.ListKV(r.Context(), ns)
	if err != nil {
		s.logger.Error("kv handler: list failed", "namespace", ns, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	out := kvListResponse{Items: make([]kvResponse, 0, len(rows))}
	for _, rec := range rows {
		out.Items = append(out.Items, toKVResponse(rec))
	}
	writeJSONResponse(w, http.StatusOK, out)
}

// handleGetKV returns one row. Secret rows return metadata only
// (the encrypted blob is never serialized over HTTP).
func (s *Server) handleGetKV(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden", "kv API is owner-only")
		return
	}
	ns := r.PathValue("namespace")
	key := r.PathValue("key")
	if err := validateKVPathSegment(ns, "namespace"); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := validateKVPathSegment(key, "key"); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	st := s.agents.Store()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "store not initialized")
		return
	}
	rec, err := st.GetKV(r.Context(), ns, key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "kv row not found")
			return
		}
		s.logger.Error("kv handler: get failed", "namespace", ns, "key", key, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	w.Header().Set("ETag", quoteETag(rec.ETag))
	if cached, ok := extractDomainIfNoneMatch(r); ok && (cached == "*" || cached == rec.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSONResponse(w, http.StatusOK, toKVResponse(rec))
}

// handlePutKV writes a non-secret kv row. Secret rows are refused
// (daemon-internal only — no plaintext-secret wire surface in this
// slice; future work can add a dedicated /api/v1/secrets/... API
// that envelope-encrypts on the daemon side).
//
// Precondition handling:
//   - If-Match: <etag>  → conditional update against that etag (412 on miss)
//   - If-Match: *       → REJECTED (would conflict with the create-only
//     semantic below; HTTP says "must currently exist" but our store
//     has no such mode)
//   - If-None-Match: *  → create-only; 412 if the row already exists
//   - If-None-Match: <etag>  → REJECTED (we don't support GET-style
//     "send only if changed" semantics on PUT)
//   - no precondition   → unconditional upsert (last-writer-wins)
func (s *Server) handlePutKV(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden", "kv API is owner-only")
		return
	}
	ns := r.PathValue("namespace")
	key := r.PathValue("key")
	if err := validateKVPathSegment(ns, "namespace"); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := validateKVPathSegment(key, "key"); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Precondition handling for PUT:
	//   - If-Match: <etag>  → conditional update against that etag
	//   - If-Match: *       → REJECTED. HTTP says "must currently
	//     exist (any etag)" but our store has no such mode and
	//     overloading the wildcard would invert the create-only
	//     semantic below.
	//   - If-None-Match: *  → create-only. Forwarded to PutKV as
	//     store.IfMatchAny — store refuses if the row already
	//     exists.
	//   - no precondition   → unconditional upsert (last-writer-wins).
	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	// PUT /kv accepts the create-only wire form (If-None-Match: *)
	// alongside If-Match, so use the opt-in gate. The legacy
	// enforceIfMatchPresence would reject create-only writes under
	// strict mode.
	if !s.enforceCreateOrUpdatePrecondition(w, r, ifMatchPresent) {
		return
	}
	if ifMatchPresent && ifMatch == "*" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"If-Match wildcard is not supported; use If-None-Match: * for create-only")
		return
	}
	storeIfMatch := ifMatch
	// If-None-Match handling. We accept ONLY `*` (create-only) on
	// PUT; any other value is rejected so a client doesn't think
	// it sent a precondition that we silently ignored.
	if cached, ok := extractDomainIfNoneMatch(r); ok {
		if cached != "*" {
			writeError(w, http.StatusBadRequest, "bad_request",
				"If-None-Match on PUT must be `*` (create-only)")
			return
		}
		if ifMatchPresent {
			writeError(w, http.StatusBadRequest, "bad_request",
				"If-Match and If-None-Match: * cannot be combined")
			return
		}
		storeIfMatch = store.IfMatchAny
	}

	r.Body = http.MaxBytesReader(w, r.Body, kvRequestCap)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1 MiB cap")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	var req kvPutRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	if req.Secret {
		writeError(w, http.StatusBadRequest, "bad_request", "secret kv rows are not exposed over HTTP")
		return
	}
	if req.Type == "" {
		req.Type = "string"
	}
	if req.Scope == "" {
		req.Scope = "global"
	}
	// Pre-validate type and scope at the handler boundary so the
	// 400 vs 500 distinction doesn't depend on string-matching the
	// store's error messages. The duplication against the store's
	// own check is intentional defense-in-depth.
	switch req.Type {
	case "string", "json", "binary":
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "invalid type")
		return
	}
	switch req.Scope {
	case "global", "local", "machine":
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "invalid scope")
		return
	}

	st := s.agents.Store()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "store not initialized")
		return
	}
	rec := &store.KVRecord{
		Namespace: ns,
		Key:       key,
		Value:     req.Value,
		Type:      store.KVType(req.Type),
		Secret:    false,
		Scope:     store.KVScope(req.Scope),
	}
	out, err := st.PutKV(r.Context(), rec, store.KVPutOptions{IfMatchETag: storeIfMatch})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrETagMismatch):
			writeError(w, http.StatusPreconditionFailed, "precondition_failed", "If-Match: etag mismatch")
		default:
			// Validation errors are caught at the handler boundary
			// above; anything reaching here is operational (DB I/O,
			// constraint violation we didn't pre-check). Surface as
			// 500 with a generic message, log the underlying error.
			s.logger.Error("kv handler: put failed", "namespace", ns, "key", key, "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		}
		return
	}
	w.Header().Set("ETag", quoteETag(out.ETag))
	writeJSONResponse(w, http.StatusOK, toKVResponse(out))
}

// handleDeleteKV removes a kv row. Honours If-Match — `*` means
// "row must currently exist" (and we treat it the same as
// any-non-empty: not actually wildcard semantics; simpler to refuse
// outright since the store doesn't support wildcard deletes).
func (s *Server) handleDeleteKV(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden", "kv API is owner-only")
		return
	}
	ns := r.PathValue("namespace")
	key := r.PathValue("key")
	if err := validateKVPathSegment(ns, "namespace"); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := validateKVPathSegment(key, "key"); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
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
		writeError(w, http.StatusBadRequest, "bad_request", "If-Match wildcard is not supported for kv DELETE")
		return
	}

	st := s.agents.Store()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "store not initialized")
		return
	}
	if err := st.DeleteKV(r.Context(), ns, key, ifMatch); err != nil {
		if errors.Is(err, store.ErrETagMismatch) {
			writeError(w, http.StatusPreconditionFailed, "precondition_failed", "If-Match: etag mismatch")
			return
		}
		s.logger.Error("kv handler: delete failed", "namespace", ns, "key", key, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
