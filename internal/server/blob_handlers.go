package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/blob"
)

// defaultBlobMaxPutBytes caps the bytes a single PUT can stream by
// default. Blobs cover avatars (~few KB), books (~MB), and attachments
// (sometimes >20MB). 256MB is the upper bound so a misbehaving client
// can't fill the disk in a single request; legitimate larger artefacts
// move through the migration / snapshot path, not this handler.
// Tests override the per-Server cap (Config.MaxBlobPutBytes) so the
// limit-exceeded path can be exercised without allocating 256MB in
// memory just to trip MaxBytesReader.
const defaultBlobMaxPutBytes int64 = 256 << 20

// listResponseItem mirrors blob.Object in JSON form. Fields are
// lower-camel to match the rest of the kojo API surface.
type listResponseItem struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
	SHA256  string `json:"sha256,omitempty"`
	ETag    string `json:"etag,omitempty"`
}

type listResponse struct {
	Scope   string             `json:"scope"`
	Prefix  string             `json:"prefix,omitempty"`
	Objects []listResponseItem `json:"objects"`
}

// blobETagHeader formats a blob-internal etag (`sha256:abc...`) as the
// HTTP-quoted strong etag (`"sha256:abc..."`). Empty input returns ""
// so callers can omit the header rather than emit a malformed value.
func blobETagHeader(internal string) string {
	if internal == "" {
		return ""
	}
	return `"` + internal + `"`
}

// parseStrongIfMatch unwraps an HTTP `If-Match` value into the
// blob-internal form. We accept exactly one strong etag — `*`,
// weak (`W/"..."`), or comma-separated lists are rejected because
// blob.Store's IfMatch is a strict equality check and silently
// permitting `*` would defeat the precondition. Returns the inner
// value plus ok; ok=false means malformed input → 400.
func parseStrongIfMatch(h string) (string, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", false
	}
	if h == "*" {
		// `*` means "any current version" — meaningful for HTTP write
		// semantics but not for our content-hash gate. Refuse rather
		// than match-everything-silently.
		return "", false
	}
	if strings.HasPrefix(h, "W/") {
		return "", false
	}
	if strings.Contains(h, ",") {
		return "", false
	}
	if len(h) < 2 || h[0] != '"' || h[len(h)-1] != '"' {
		return "", false
	}
	inner := h[1 : len(h)-1]
	if inner == "" {
		// `""` is syntactically a strong etag but maps to an empty
		// IfMatch internally, which blob.Put treats as "no
		// precondition" — exactly the silent-skip we're trying to
		// prevent. Refuse rather than match-nothing-silently.
		return "", false
	}
	return inner, true
}

// errBadIfMatch sentinel for the three rejection paths in
// extractIfMatch: missing / empty value, multiple header lines, and
// malformed strong-etag content. Handlers convert it to 400.
var errBadIfMatch = errors.New("invalid If-Match header")

// extractIfMatch reads the (zero or one) `If-Match` request header.
// Returns:
//   - ("", false, nil) when no `If-Match` header was sent — the caller
//     proceeds without a precondition.
//   - ("v", true, nil) when exactly one well-formed strong etag was
//     sent.
//   - ("", false, errBadIfMatch) when the header is present but
//     malformed (multiple headers, empty value, weak, `*`, comma-list,
//     missing quotes). The handler maps that to 400.
//
// Using r.Header.Values rather than Header.Get is intentional: a stray
// duplicate `If-Match` line would otherwise silently lose to the first
// value, and a client that meant to gate the write but typoed an empty
// header would slip through PutOptions.IfMatch == "" and overwrite
// without a precondition check.
func extractIfMatch(r *http.Request) (string, bool, error) {
	vals := r.Header.Values("If-Match")
	if len(vals) == 0 {
		return "", false, nil
	}
	if len(vals) > 1 {
		return "", false, errBadIfMatch
	}
	v, ok := parseStrongIfMatch(vals[0])
	if !ok {
		return "", false, errBadIfMatch
	}
	return v, true, nil
}

// validHexSHA256 reports whether s is exactly 64 lowercase hex chars.
// Used to gate X-Kojo-Expected-SHA256 before handing it to the blob
// layer so a malformed value comes back as 400 rather than mid-stream
// 500. Uppercase hex is normalized by the caller via strings.ToLower.
func validHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// blobRequestParts pulls the {scope} / {path...} pattern values off
// the request, returning the typed Scope and the logical path. ok=false
// signals an invalid scope so the handler can answer 400 before
// touching the store.
func blobRequestParts(r *http.Request) (blob.Scope, string, bool) {
	scope := blob.Scope(r.PathValue("scope"))
	if !scope.Valid() {
		return "", "", false
	}
	return scope, r.PathValue("path"), true
}

// writeBlobErr maps blob package errors to HTTP statuses. Unknown
// errors collapse to 500 with a generic message — the underlying
// error is logged separately so we don't leak fs paths in the body.
func (s *Server) writeBlobErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, blob.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "blob not found")
	case errors.Is(err, blob.ErrETagMismatch):
		writeError(w, http.StatusPreconditionFailed, "etag_mismatch", "If-Match precondition failed")
	case errors.Is(err, blob.ErrExpectedSHA256Mismatch):
		// 400 (not 412): the body the client sent did not hash to the
		// X-Kojo-Expected-SHA256 it claimed. The on-disk file is
		// unchanged because atomicWrite aborts pre-rename. AWS S3 does
		// the same with Content-MD5 mismatch (BadDigest, 400).
		writeError(w, http.StatusBadRequest, "sha256_mismatch", "body did not match X-Kojo-Expected-SHA256")
	case errors.Is(err, blob.ErrHandoffPending):
		// §3.7 invariant: the row is mid-handoff and a runtime
		// write that would change body-derived columns is
		// refused. 409 is the canonical "your request can't
		// be served in the current state" signal the agent
		// runtime should retry after the orchestrator finishes
		// or aborts the switch.
		writeError(w, http.StatusConflict, "handoff_pending",
			"blob is mid-handoff (§3.7); retry after the device switch finishes")
	case errors.Is(err, blob.ErrInvalidScope):
		writeError(w, http.StatusBadRequest, "invalid_scope", "invalid scope")
	case errors.Is(err, blob.ErrInvalidPath):
		writeError(w, http.StatusBadRequest, "invalid_path", "invalid blob path")
	case errors.Is(err, blob.ErrScopeContainmentBroken):
		// Server-side disk defect (scope dir is a symlink, etc.) —
		// not the client's fault. 500 so monitoring picks it up
		// rather than dismissing it as "client sent garbage".
		if s.logger != nil {
			s.logger.Error("blob scope containment broken", "err", err)
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "blob layout corrupted on server")
	default:
		if s.logger != nil {
			s.logger.Warn("blob handler error", "err", err)
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "blob operation failed")
	}
}

// handleBlobGet serves a blob body or, when {path...} is empty, a
// directory-style listing. HEAD shares this handler — http.ServeContent
// honours r.Method internally.
func (s *Server) handleBlobGet(w http.ResponseWriter, r *http.Request) {
	if s.blob == nil {
		writeError(w, http.StatusServiceUnavailable, "blob_unavailable", "blob store not configured")
		return
	}
	scope, path, ok := blobRequestParts(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_scope", "invalid scope")
		return
	}
	if path == "" {
		s.blobList(w, r, scope)
		return
	}
	f, obj, err := s.blob.Open(scope, path)
	if err != nil {
		s.writeBlobErr(w, err)
		return
	}
	defer f.Close()
	if obj.ETag != "" {
		// ServeContent uses ETag for conditional response handling
		// (If-None-Match → 304, If-Match → 412). It must be set
		// before ServeContent runs.
		w.Header().Set("ETag", blobETagHeader(obj.ETag))
	}
	if obj.SHA256 != "" {
		// Surface the raw hex digest separately so curl-driven clients
		// can verify without parsing the etag form.
		w.Header().Set("X-Kojo-SHA256", obj.SHA256)
	}
	http.ServeContent(w, r, obj.Path, time.UnixMilli(obj.ModTime), f)
}

// blobList implements GET /api/v1/blob/{scope}/?prefix=... — JSON
// listing only. Range / ETag are not meaningful on a listing so this
// path bypasses ServeContent entirely.
func (s *Server) blobList(w http.ResponseWriter, r *http.Request, scope blob.Scope) {
	prefix := r.URL.Query().Get("prefix")
	objs, err := s.blob.List(scope, prefix)
	if err != nil {
		s.writeBlobErr(w, err)
		return
	}
	out := make([]listResponseItem, 0, len(objs))
	for _, o := range objs {
		out = append(out, listResponseItem{
			Path:    o.Path,
			Size:    o.Size,
			ModTime: o.ModTime,
			SHA256:  o.SHA256,
			ETag:    blobETagHeader(o.ETag),
		})
	}
	writeJSONResponse(w, http.StatusOK, listResponse{
		Scope:   string(scope),
		Prefix:  prefix,
		Objects: out,
	})
}

// handleBlobPut publishes a blob via blob.Store.Put — atomic write,
// sha256 verification (if X-Kojo-Expected-SHA256 set), If-Match gate,
// and blob_refs cache update. The body is capped at maxBlobPutBytes;
// callers that need larger bodies route through the migration path.
func (s *Server) handleBlobPut(w http.ResponseWriter, r *http.Request) {
	if s.blob == nil {
		writeError(w, http.StatusServiceUnavailable, "blob_unavailable", "blob store not configured")
		return
	}
	scope, path, ok := blobRequestParts(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_scope", "invalid scope")
		return
	}
	if path == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "PUT requires a non-empty path")
		return
	}
	opts := blob.PutOptions{}
	v, present, err := extractIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match must be exactly one strong etag")
		return
	}
	if !s.enforceIfMatchPresence(w, r, present) {
		return
	}
	if present {
		opts.IfMatch = v
	}
	if h := r.Header.Get("X-Kojo-Expected-SHA256"); h != "" {
		// Normalize and validate up front — letting an invalid value
		// reach atomicWrite would mean we'd already have written the
		// temp file and computed sha256 before noticing the request was
		// malformed. 400 is the right answer for a malformed precondition.
		want := strings.ToLower(strings.TrimSpace(h))
		if !validHexSHA256(want) {
			writeError(w, http.StatusBadRequest, "invalid_expected_sha256",
				"X-Kojo-Expected-SHA256 must be 64 hex characters")
			return
		}
		opts.ExpectedSHA256 = want
	}
	cap := s.blobMaxPutBytes
	if cap <= 0 {
		cap = defaultBlobMaxPutBytes
	}
	body := http.MaxBytesReader(w, r.Body, cap)
	defer body.Close()
	obj, err := s.blob.Put(scope, path, body, opts)
	durabilityDegraded := false
	if err != nil {
		// ErrDurabilityDegraded is the special "body + row are
		// both committed but parent-dir fsync failed" path.
		// blob.Store.Put still returns the Object; we surface
		// this as 200 with an X-Kojo-Durability warning header
		// so callers don't retry a transfer that already
		// landed.
		if errors.Is(err, blob.ErrDurabilityDegraded) && obj != nil {
			durabilityDegraded = true
			s.logger.Warn("blob put: committed with degraded durability",
				"scope", scope, "path", path, "err", err)
		} else {
			// MaxBytesReader surfaces an *http.MaxBytesError when the cap
			// is exceeded; map that to 413 explicitly so clients don't
			// guess from a 500.
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "blob body exceeds maximum")
				return
			}
			s.writeBlobErr(w, err)
			return
		}
	}
	w.Header().Set("ETag", blobETagHeader(obj.ETag))
	w.Header().Set("X-Kojo-SHA256", obj.SHA256)
	if durabilityDegraded {
		w.Header().Set("X-Kojo-Durability", "degraded")
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"scope":   string(obj.Scope),
		"path":    obj.Path,
		"size":    obj.Size,
		"modTime": obj.ModTime,
		"sha256":  obj.SHA256,
		"etag":    blobETagHeader(obj.ETag),
	})
}

// handleBlobDelete removes a blob. If-Match is honoured the same way
// PUT honours it — a non-matching value returns 412 without touching
// the file.
func (s *Server) handleBlobDelete(w http.ResponseWriter, r *http.Request) {
	if s.blob == nil {
		writeError(w, http.StatusServiceUnavailable, "blob_unavailable", "blob store not configured")
		return
	}
	scope, path, ok := blobRequestParts(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_scope", "invalid scope")
		return
	}
	if path == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "DELETE requires a non-empty path")
		return
	}
	opts := blob.DeleteOptions{}
	v, present, err := extractIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match must be exactly one strong etag")
		return
	}
	if !s.enforceIfMatchPresence(w, r, present) {
		return
	}
	if present {
		opts.IfMatch = v
	}
	if err := s.blob.Delete(scope, path, opts); err != nil {
		s.writeBlobErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

