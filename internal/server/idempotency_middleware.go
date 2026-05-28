package server

// Idempotency-Key middleware (3.5).
//
// Wraps every write request (POST / PUT / PATCH / DELETE) under
// /api/v1/* that carries an `Idempotency-Key` header. The middleware
// claims the key against the `idempotency_keys` table (24 h dedup
// window), then either:
//
//   - Replays the saved response when the key already carries a
//     completed entry whose request_hash matches the current request
//     (so a client retry after network loss gets back the *exact*
//     prior response, not a fresh handler run that might re-execute
//     a side effect).
//   - 409 Conflicts when the same key is reused for a *different*
//     request (request_hash mismatch) — protects against client
//     bugs that recycle keys across endpoints.
//   - 409 Conflicts when another concurrent request holds the claim
//     (request_hash match, response_status==0). Cheaper than blocking
//     the second worker; clients are expected to retry on a fresh
//     network attempt rather than poll.
//   - Otherwise: runs the wrapped handler, captures status / etag /
//     body via a buffering ResponseWriter, and saves the response so
//     the next retry within 24 h sees it. Captures up to
//     idempotencyResponseBodyCap bytes; bigger responses skip the
//     save (with a Warn log) so a streaming download doesn't bloat
//     the kv table.
//
// Read methods (GET / HEAD / OPTIONS) and WebSocket upgrades pass
// through unconditionally — there is nothing to dedup, and the
// buffering wrapper would break long-lived streams.
//
// The middleware sits AFTER auth (so principal is set on ctx and the
// caller's identity is implicitly part of "who claimed this key" via
// the request hash that includes the path) and BEFORE the mux. It is
// installed on both the public listener and the auth listener.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// idempotencyTTL is how long a saved response is honoured before the
// row is reclaimable. 24h matches docs §3.5; the table sweep
// (ExpireIdempotencyKeys) deletes rows past this point.
const idempotencyTTL = 24 * time.Hour

// idempotencyRequestBodyCap caps the body size we are willing to hash
// for idempotency. Requests beyond this cap are *not* saved (claim is
// abandoned; handler runs unconditionally) so a multi-MB upload
// doesn't OOM the middleware. The cap exists separately from
// http.MaxBytesReader on individual handlers because we need the raw
// bytes BEFORE the handler installs its own limit.
const idempotencyRequestBodyCap = 1 * 1024 * 1024 // 1 MiB

// idempotencyResponseBodyCap caps the captured response body. Above
// this size we skip the save and the next retry will re-execute the
// handler (loss of dedup, but acceptable — large responses tend to
// be streaming reads, which are GET anyway and skipped earlier).
const idempotencyResponseBodyCap = 256 * 1024 // 256 KiB

// idempotencyKeyMaxLen bounds the header value. UUIDv4 is 36 chars;
// 256 leaves room for client-prefixed schemes ("svc-foo-<uuid>") and
// blocks pathological multi-KB headers from polluting the kv row.
const idempotencyKeyMaxLen = 256

// idempotencyKeyHeader is the wire name. Lowercase per RFC 7230 §3.2,
// but http.Header.Get is case-insensitive so this is just for logs.
const idempotencyKeyHeader = "Idempotency-Key"

// idempotencyMiddleware returns an http.Handler middleware that
// applies the dedup logic above. The store handle is captured at
// install time; pass nil to opt out (the route is registered as a
// pass-through, useful for tests / Server instances bootstrapped
// without a kv store).
func (s *Server) idempotencyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !shouldIdempotencyApply(r) {
			next.ServeHTTP(w, r)
			return
		}
		st := s.idempotencyStore()
		if st == nil {
			// No backing store wired — pass through. A misconfigured
			// test bench shouldn't reject every write. Production
			// startup wires the store unconditionally.
			next.ServeHTTP(w, r)
			return
		}
		key := r.Header.Get(idempotencyKeyHeader)
		if key == "" {
			// Header absent — the design currently allows this
			// (3.5: "v1 移行期は best-effort で許可"). Pass through
			// without dedup. Wiring the 428 transition is a
			// separate slice (#3 in the task list).
			next.ServeHTTP(w, r)
			return
		}
		if !validateIdempotencyKey(key) {
			writeError(w, http.StatusBadRequest, "bad_request",
				"Idempotency-Key must be a UUID")
			return
		}

		// Refuse known-large requests with an actionable 413 instead
		// of either reading the body (which would OOM the middleware
		// for blob / avatar uploads) or silently bypassing dedup
		// (which would re-execute the handler regardless of any
		// existing in-flight / completed row for the same UUID).
		// The hint tells the caller they can resend without the
		// header — that's the contract we want: Idempotency-Key
		// implies "small body" today; supporting large keyed writes
		// requires temp-file spooling that is a separate slice.
		if r.ContentLength > idempotencyRequestBodyCap {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				"request body exceeds 1 MiB idempotency cap; resend without Idempotency-Key")
			return
		}

		// Read the request body so we can hash it AND replay it to the
		// handler. http.MaxBytesReader trips when chunked uploads
		// exceed the cap (ContentLength was -1 so the gate above
		// could not catch it).
		rawBody, err := readRequestBody(r, idempotencyRequestBodyCap)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				// Chunked upload that overshot the cap (the
				// declared ContentLength gate above could not catch
				// it because Transfer-Encoding: chunked sets it to
				// -1). Refuse with 413 — by the time we know the
				// size, we've already drained part of the body and
				// cannot reconstruct it for the handler.
				writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
					"request body exceeds 1 MiB idempotency cap; resend without Idempotency-Key")
				return
			}
			writeError(w, http.StatusBadRequest, "bad_request", "could not read request body")
			return
		}
		// Re-install the body so the handler can read it. bytes.Reader
		// + io.NopCloser preserves the original semantics (single
		// read; ContentLength stays accurate).
		r.Body = io.NopCloser(bytes.NewReader(rawBody))
		r.ContentLength = int64(len(rawBody))

		// Include the precondition headers in the hash so a retry that
		// adds or changes If-Match (or If-None-Match) is treated as a
		// distinct request — otherwise the dedup would replay a stale
		// 412 / 428 response when the caller has corrected its
		// precondition and expects a fresh evaluation.
		//
		// Header.Values rather than Header.Get so duplicate or empty
		// header lines (a buggy proxy that splits an If-Match into
		// two lines) hash distinctly from the canonicalized single-
		// header form. Otherwise a "retry without the duplicate"
		// would replay the prior duplicate-header 400 response.
		hash := computeRequestHashWithPrecondition(r.Method, r.URL.RequestURI(), rawBody,
			r.Header.Values("If-Match"), r.Header.Values("If-None-Match"))
		expires := store.NowMillis() + idempotencyTTL.Milliseconds()
		ctx := r.Context()

		// Generate a per-attempt claim token. Stored in op_id so
		// FinalizeIdempotencyKey / AbandonIdempotencyKey can scope
		// their WHERE to "this exact claim" rather than "any claim
		// for this key". Without that scope, an old handler whose
		// claim expired and was overwritten by a fresh claim could
		// finalize over the new claim's pending row.
		claimToken := uuid.NewString()
		entry, err := st.ClaimIdempotencyKey(ctx, key, claimToken, hash, expires)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrIdempotencyConflict):
				writeError(w, http.StatusConflict, "idempotency_conflict",
					"Idempotency-Key was reused for a different request")
				return
			case errors.Is(err, store.ErrIdempotencyInFlight):
				writeError(w, http.StatusConflict, "idempotency_in_flight",
					"another request with this Idempotency-Key is currently in flight")
				return
			}
			s.logger.Error("idempotency: claim failed; passing through", "key", key, "err", err)
			next.ServeHTTP(w, r)
			return
		}
		if entry != nil {
			// Saved response — replay verbatim.
			replayIdempotencyResponse(w, entry)
			return
		}

		// Fresh claim. Execute the handler with a buffering writer so
		// we can capture the response, then save it.
		bw := newBufferingWriter(w, idempotencyResponseBodyCap)
		next.ServeHTTP(bw, r)
		bw.flushHeader()

		// Save (or skip if we exceeded the cap / handler never wrote
		// a status). Use a fresh background context with a short
		// timeout so a client disconnect after the handler returned
		// doesn't drop our save — the response is already on the
		// wire, we just need to persist it for the next retry.
		saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.persistIdempotencyOutcome(saveCtx, key, claimToken, bw)
	})
}

// shouldIdempotencyApply returns true iff the request is a write
// under /api/v1/* and not a streaming endpoint (WebSocket upgrade /
// SSE). The buffering wrapper would break those.
func shouldIdempotencyApply(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		// fall through
	default:
		return false
	}
	if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		return false
	}
	// WebSocket upgrade requests are method=GET so they never reach
	// the switch above, but keep this guard for the rare client that
	// PUTs an Upgrade header.
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	// SSE: explicit Accept hints. /api/v1/changes is a long-poll-ish
	// GET so it doesn't hit this path either, but a future SSE write
	// endpoint should opt out via Accept. Parse comma-separated
	// values and compare via mime.ParseMediaType so case / parameter
	// noise (e.g. "Text/Event-Stream; charset=utf-8") doesn't slip
	// past a substring check.
	for _, raw := range r.Header.Values("Accept") {
		for _, part := range strings.Split(raw, ",") {
			mt, _, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err == nil && strings.EqualFold(mt, "text/event-stream") {
				return false
			}
		}
	}
	return true
}

// idempotencyStore exposes the agent manager's store handle through
// the same nil-safe pattern peer_handlers.go uses. Returns nil when
// the manager is not wired (test setups, --no-store).
func (s *Server) idempotencyStore() *store.Store {
	if s == nil || s.agents == nil {
		return nil
	}
	return s.agents.Store()
}

// validateIdempotencyKey enforces the design-doc contract: the
// header value must be a UUID (canonical 8-4-4-4-12 lowercase form,
// per uuid.Parse + round-trip check). Other token shapes are common
// in industry practice but the docs §3.5 / §3.13.1 are explicit, and
// admitting opaque strings here would let two clients clash on a
// "happens-to-look-like" key without realizing.
func validateIdempotencyKey(k string) bool {
	if k == "" || len(k) > idempotencyKeyMaxLen {
		return false
	}
	parsed, err := uuid.Parse(k)
	if err != nil {
		return false
	}
	return parsed.String() == k
}

// readRequestBody buffers up to maxBytes of r.Body. Returns the bytes
// or an error (http.MaxBytesError on overflow).
func readRequestBody(r *http.Request, maxBytes int64) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	limited := http.MaxBytesReader(nil, r.Body, maxBytes)
	defer limited.Close()
	return io.ReadAll(limited)
}

// computeRequestHash returns the sha256 hex of method + space + URI
// + LF + body. Including method and path defends against the same
// key being reused on a different endpoint; including the body
// defends against the same key being reused with different content.
//
// This is the legacy shape — it omits the precondition headers.
// computeRequestHashWithPrecondition forwards to it when both
// If-Match and If-None-Match are absent so binaries upgraded across
// the 3.5 cut keep dedup hits on already-saved rows.
func computeRequestHash(method, uri string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte(" "))
	h.Write([]byte(uri))
	h.Write([]byte("\n"))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// computeRequestHashWithPrecondition extends computeRequestHash with
// the wire-form If-Match / If-None-Match header values. Each header's
// value list is serialized as `<count>:<v0>\x1f<v1>\x1f...` so a
// duplicate-header attack (two If-Match lines) hashes distinctly
// from a single canonicalized header — without the count prefix a
// proxy that joins two values with comma would collide with a
// client that sent the same comma-list directly. The ASCII unit-
// separator (\x1f) is used between values because it never appears
// in a well-formed HTTP header value.
//
// Backward-compatibility: when BOTH header lists are empty we fall
// back to the legacy hash shape (no precondition block) so saved
// rows from the pre-3.5 binary still dedup against retries that
// also carry no precondition. Rows for requests that DID carry
// precondition headers in the older binary used a different hash
// shape and will report ErrIdempotencyConflict — the dedup window
// is 24h so this transition self-clears.
//
// CRITICAL: do NOT advise callers to "resend without
// Idempotency-Key" on conflict — that defeats the dedup and would
// double-execute POST-class side effects. Conflicts during the
// upgrade window should be retried with a fresh Idempotency-Key
// instead.
func computeRequestHashWithPrecondition(method, uri string, body []byte, ifMatch, ifNoneMatch []string) string {
	if len(ifMatch) == 0 && len(ifNoneMatch) == 0 {
		return computeRequestHash(method, uri, body)
	}
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte(" "))
	h.Write([]byte(uri))
	writePreconditionList(h, "If-Match", ifMatch)
	writePreconditionList(h, "If-None-Match", ifNoneMatch)
	h.Write([]byte("\n"))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// writePreconditionList serializes a header's value list into the
// hash in the shape `\nName: <n>:<v0>\x1f<v1>\x1f...`. n is the
// decimal count of values present (so a one-value canonical form
// hashes distinctly from a two-value duplicate-header form even when
// the joined comma-list would coincide).
func writePreconditionList(h interface{ Write([]byte) (int, error) }, name string, vals []string) {
	h.Write([]byte("\n"))
	h.Write([]byte(name))
	h.Write([]byte(": "))
	// n: prefix
	n := len(vals)
	var nb [20]byte
	pos := len(nb)
	if n == 0 {
		pos--
		nb[pos] = '0'
	}
	for n > 0 {
		pos--
		nb[pos] = byte('0' + n%10)
		n /= 10
	}
	h.Write(nb[pos:])
	h.Write([]byte(":"))
	for i, v := range vals {
		if i > 0 {
			h.Write([]byte{0x1f})
		}
		h.Write([]byte(v))
	}
}

// savedResponseEnvelope is the JSON shape we serialize into
// idempotency_keys.response_body so a captured response with
// arbitrary headers and any content type can be replayed faithfully.
//
// The schema only has one TEXT column for the body; rather than
// adding a sibling headers column (which would require a migration
// across already-deployed databases) we wrap the body in a JSON
// envelope:
//
//	{"v":1,"headers":{"Content-Type":["application/json"], ...},
//	 "body":"<base64 or raw>", "binary":<bool>}
//
// The "binary" flag distinguishes responses we captured as raw
// (UTF-8 / JSON) bytes from ones we base64-encoded because the
// content type isn't text-class. Replay branches on it so an avatar
// PNG response replays as PNG, not as a JSON-quoted string.
type savedResponseEnvelope struct {
	V       int                 `json:"v"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
	Binary  bool                `json:"binary,omitempty"`
}

const savedResponseEnvelopeVersion = 1

// replayIdempotencyResponse writes a saved entry back to the wire.
// Falls back to the legacy "raw JSON body" interpretation when the
// stored body is not a recognizable envelope so rows written by an
// older binary still replay (after upgrade and before the 24 h TTL
// expires).
func replayIdempotencyResponse(w http.ResponseWriter, entry *store.IdempotencyEntry) {
	envelope, ok := decodeSavedEnvelope(entry.ResponseBody)
	if !ok {
		// Legacy / pre-envelope save. Restore ETag (if any) and
		// emit body as application/json because that's what the
		// pre-envelope code path always wrote.
		if entry.ResponseEtag != "" {
			w.Header().Set("ETag", entry.ResponseEtag)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Idempotent-Replay", "true")
		w.WriteHeader(entry.ResponseStatus)
		if entry.ResponseBody != "" {
			_, _ = w.Write([]byte(entry.ResponseBody))
		}
		return
	}
	hdr := w.Header()
	for k, vs := range envelope.Headers {
		// Drop hop-by-hop headers and Set-Cookie (the latter could
		// leak a server-set token from one client's session into
		// another's retry). Keep everything else verbatim.
		if isHopByHop(k) || strings.EqualFold(k, "Set-Cookie") {
			continue
		}
		hdr[k] = append([]string(nil), vs...)
	}
	hdr.Set("X-Idempotent-Replay", "true")
	w.WriteHeader(entry.ResponseStatus)
	if envelope.Body != "" {
		if envelope.Binary {
			if raw, err := decodeBase64Body(envelope.Body); err == nil {
				_, _ = w.Write(raw)
			}
		} else {
			_, _ = w.Write([]byte(envelope.Body))
		}
	}
}

func decodeSavedEnvelope(s string) (*savedResponseEnvelope, bool) {
	if s == "" || s[0] != '{' {
		return nil, false
	}
	var env savedResponseEnvelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return nil, false
	}
	if env.V != savedResponseEnvelopeVersion {
		return nil, false
	}
	return &env, true
}

func isHopByHop(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}

// persistIdempotencyOutcome writes the captured handler response to
// the kv table or abandons the claim if the handler never wrote a
// status / overflowed the cap.
func (s *Server) persistIdempotencyOutcome(ctx context.Context, key, claimToken string, bw *bufferingWriter) {
	st := s.idempotencyStore()
	if st == nil {
		return
	}
	if bw.status == 0 {
		// Handler never called WriteHeader (rare; almost always means
		// it panicked or wrote nothing). Abandon so the next retry can
		// re-execute.
		_ = st.AbandonIdempotencyKey(ctx, key, claimToken)
		return
	}
	if bw.overflowed {
		// Body exceeded the cap. Don't save — abandoning means the
		// next retry will re-execute the handler. Logged at Warn so
		// operators notice if a write endpoint regularly overshoots.
		s.logger.Warn("idempotency: response body exceeded cap; not saving",
			"key", key, "status", bw.status, "size", bw.totalBytes)
		_ = st.AbandonIdempotencyKey(ctx, key, claimToken)
		return
	}
	if !idempotencyShouldSave(bw.status) {
		// 5xx / non-final responses are not safe to replay (the
		// failure mode might be transient). Abandon so the next
		// retry can hit the live handler.
		_ = st.AbandonIdempotencyKey(ctx, key, claimToken)
		return
	}
	if bw.header.Get(auth.HeaderNoIdempotencyCache) != "" {
		// Inner middleware explicitly stamped the response as
		// transient (e.g. AgentFencing's 409 wrong_holder
		// during a §3.7 device switch — the lock can come back
		// to this peer and a retry should re-check rather than
		// replay the stale refusal). Abandon so the dedup row
		// reverts to "no answer yet" and the next retry runs
		// the handler again.
		_ = st.AbandonIdempotencyKey(ctx, key, claimToken)
		return
	}
	envelope, err := buildSavedEnvelope(bw)
	if err != nil {
		s.logger.Warn("idempotency: envelope encode failed; not saving",
			"key", key, "err", err)
		_ = st.AbandonIdempotencyKey(ctx, key, claimToken)
		return
	}
	etag := bw.header.Get("ETag")
	if err := st.FinalizeIdempotencyKey(ctx, key, claimToken, bw.status, etag, envelope); err != nil {
		s.logger.Warn("idempotency: finalize failed", "key", key, "err", err)
	}
}

// buildSavedEnvelope packages the captured headers + body into the
// JSON envelope persisted in idempotency_keys.response_body. Binary
// (non-text-class) bodies are base64-encoded so the JSON layer
// doesn't have to escape them; text/JSON bodies are stored as raw
// strings to keep the saved row human-readable in the sqlite CLI.
func buildSavedEnvelope(bw *bufferingWriter) (string, error) {
	headers := make(map[string][]string, len(bw.header))
	for k, vs := range bw.header {
		// Filter hop-by-hop and Set-Cookie at *save* time too — that
		// way an op-log replay or sqlite-CLI inspection of the row
		// doesn't carry them around uselessly.
		if isHopByHop(k) || strings.EqualFold(k, "Set-Cookie") {
			continue
		}
		// Reset/Date/Content-Length are recomputed by net/http on
		// replay write; persisting them would be redundant.
		switch http.CanonicalHeaderKey(k) {
		case "Date", "Content-Length":
			continue
		}
		headers[k] = append([]string(nil), vs...)
	}
	body := bw.body.Bytes()
	binary := !isTextClassContentType(bw.header.Get("Content-Type"))
	env := savedResponseEnvelope{
		V:       savedResponseEnvelopeVersion,
		Headers: headers,
		Binary:  binary,
	}
	if binary {
		env.Body = encodeBase64Body(body)
	} else {
		env.Body = string(body)
	}
	out, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// isTextClassContentType returns true for content types whose body
// can be safely stored as a raw UTF-8 string in the JSON envelope:
// application/json (incl. +json), text/*, application/xml,
// application/javascript. Everything else is treated as binary and
// base64-encoded.
func isTextClassContentType(ct string) bool {
	if ct == "" {
		return true // pre-WriteHeader writes are JSON-class by default
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	mt = strings.ToLower(mt)
	switch {
	case strings.HasPrefix(mt, "text/"):
		return true
	case mt == "application/json" || strings.HasSuffix(mt, "+json"):
		return true
	case mt == "application/xml" || strings.HasSuffix(mt, "+xml"):
		return true
	case mt == "application/javascript", mt == "application/ecmascript":
		return true
	}
	return false
}

// idempotencyShouldSave decides whether a captured response is
// replay-safe. 2xx and 4xx (except 408 / 425 / 429 which are
// retry-recommended) are saved; 5xx and the retry-class 4xx codes
// are not, so a transient backend failure doesn't get cached as the
// "answer" for the next retry.
func idempotencyShouldSave(status int) bool {
	if status >= 200 && status < 400 {
		return true
	}
	switch status {
	case http.StatusRequestTimeout, // 408
		http.StatusTooEarly,        // 425
		http.StatusTooManyRequests: // 429
		return false
	}
	if status >= 400 && status < 500 {
		return true
	}
	return false
}

// bufferingWriter is an http.ResponseWriter wrapper that captures the
// status, headers, and body so the middleware can persist them. The
// underlying writer receives the same bytes — the buffer is purely
// for our save path.
type bufferingWriter struct {
	w          http.ResponseWriter
	header     http.Header
	body       *bytes.Buffer
	cap        int
	status     int
	headerSent bool
	overflowed bool
	totalBytes int
}

func newBufferingWriter(w http.ResponseWriter, cap int) *bufferingWriter {
	return &bufferingWriter{
		w:      w,
		header: w.Header(),
		body:   &bytes.Buffer{},
		cap:    cap,
	}
}

func (bw *bufferingWriter) Header() http.Header {
	return bw.header
}

func (bw *bufferingWriter) WriteHeader(status int) {
	if bw.headerSent {
		return
	}
	bw.status = status
	bw.headerSent = true
	bw.w.WriteHeader(status)
}

func (bw *bufferingWriter) Write(p []byte) (int, error) {
	// Forward to the inner writer FIRST when status is implicit so
	// net/http can sniff Content-Type from the body bytes — calling
	// our WriteHeader here would commit the header before the sniff
	// runs, and an unsniffed binary body would land in the captured
	// envelope with no Content-Type, replaying as the wrong type.
	n, err := bw.w.Write(p)
	if !bw.headerSent {
		// Implicit 200 — record the status the inner writer just
		// sealed (net/http's behaviour) without re-committing
		// headers ourselves.
		bw.status = http.StatusOK
		bw.headerSent = true
	}
	bw.totalBytes += n
	if !bw.overflowed {
		remaining := bw.cap - bw.body.Len()
		if remaining > 0 {
			toCopy := n
			if toCopy > remaining {
				toCopy = remaining
				bw.overflowed = true
			}
			bw.body.Write(p[:toCopy])
		} else if n > 0 {
			bw.overflowed = true
		}
	}
	return n, err
}

// flushHeader is a no-op when WriteHeader was already called; kept
// here so the middleware can guarantee status capture even for
// handlers that never wrote a status (which we treat as "did
// nothing" and abandon the claim).
func (bw *bufferingWriter) flushHeader() {
	if !bw.headerSent {
		// Don't flush — leave bw.status == 0 so the persist step
		// abandons the claim.
	}
}

// Flush propagates to the inner writer if it supports flushing —
// preserves SSE-ish handlers that explicitly flush. The middleware
// already opts out of streaming endpoints via shouldIdempotencyApply,
// so this is mostly defensive.
func (bw *bufferingWriter) Flush() {
	if !bw.headerSent {
		bw.WriteHeader(http.StatusOK)
	}
	if f, ok := bw.w.(http.Flusher); ok {
		f.Flush()
	}
}

// idempotencySweepInterval is how often the background goroutine
// runs ExpireIdempotencyKeys. The dedup window is 24 h, so an hourly
// sweep keeps the table from growing unbounded without running so
// often that it competes with the hot write path.
const idempotencySweepInterval = 1 * time.Hour

// StartIdempotencySweep launches a goroutine that periodically deletes
// expired idempotency_keys rows. Cancelling ctx stops the goroutine.
// Safe to call from multiple sites — the second call is a no-op.
func (s *Server) StartIdempotencySweep(ctx context.Context) {
	if s == nil || s.idempotencyStore() == nil {
		return
	}
	s.idempSweepOnce.Do(func() {
		go s.idempotencySweepLoop(ctx)
	})
}

// idempotencySweepLoop runs ExpireIdempotencyKeys at
// idempotencySweepInterval until ctx is cancelled. Errors are
// logged at Warn so a transient DB lock during a sweep doesn't
// surface as alerting noise.
func (s *Server) idempotencySweepLoop(ctx context.Context) {
	t := time.NewTicker(idempotencySweepInterval)
	defer t.Stop()
	// Run once at startup so a long-stopped binary doesn't have to
	// wait an hour for its first cleanup.
	s.idempotencySweepOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.idempotencySweepOnce(ctx)
		}
	}
}

func (s *Server) idempotencySweepOnce(ctx context.Context) {
	st := s.idempotencyStore()
	if st == nil {
		return
	}
	sweepCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	n, err := st.ExpireIdempotencyKeys(sweepCtx, store.NowMillis())
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("idempotency: sweep failed", "err", err)
		}
		return
	}
	if n > 0 && s.logger != nil {
		s.logger.Info("idempotency: swept expired keys", "count", n)
	}
}

// encodeBase64Body / decodeBase64Body wrap base64.StdEncoding for
// the binary-body envelope path. Kept inline (rather than calling
// the package directly at each site) so the encoding choice can be
// tightened or swapped in one place.
func encodeBase64Body(p []byte) string {
	return base64.StdEncoding.EncodeToString(p)
}

func decodeBase64Body(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
