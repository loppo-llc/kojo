package server

import (
	"errors"
	"net/http"
	"strings"
)

// Domain-table ETag/If-Match handling for the routes that mutate
// SQLite-backed records (agents, agent_persona, agent_memory,
// memory_entries, agent_messages, kv). These rows carry an etag of the
// form "<version>-<sha256(canonical_record)[:8]>" produced by
// internal/store; callers exchange the value as a strong HTTP ETag.
//
// This is intentionally separate from blob_handlers' parseStrongIfMatch
// helpers: blob etags are content-hashes (sha256:...) and want a
// stricter policy (no `*`, no weak), while domain etags are version-
// scoped and need the same wildcard treatment Hub-side write APIs
// expect — `*` means "create-or-replace, do not gate on a particular
// prior etag" per the design doc §5.5 sequence. The two helper sets
// share no code so a future tightening of one cannot loosen the other
// by accident.

// quoteETag returns the HTTP form (`"<inner>"`) of a domain-table etag.
// Empty in → empty out so a row that has no etag yet (race on insert)
// does not produce a malformed `""` header.
func quoteETag(inner string) string {
	if inner == "" {
		return ""
	}
	return `"` + inner + `"`
}

// errBadDomainIfMatch is returned by extractDomainIfMatch when the
// header is present but malformed. Handlers map this to 400.
var errBadDomainIfMatch = errors.New("invalid If-Match header")

// enforceIfMatchPresence implements the docs §3.5 transition. When
// the server's RequireIfMatch flag is set and the client did not
// supply If-Match on a write that supports optimistic concurrency,
// return 428 Precondition Required and signal the caller to stop.
//
// Returns true (caller proceeds) when:
//   - If-Match is present (precondition will be evaluated below), or
//   - RequireIfMatch is off (legacy best-effort).
//
// If-None-Match: * is NOT accepted as a substitute here. It is only
// meaningful for endpoints that implement create-only CAS (the kv
// PUT path); those callers use enforceCreateOrUpdatePrecondition
// below to opt in. Letting it pass through globally would defeat
// the gate on PATCH / DELETE handlers that have no plan to honour
// it (a client sending If-None-Match: * with PATCH would silently
// bypass the precondition without any per-row check).
//
// Centralized here so every handler that parses If-Match can opt in
// with a single line after extractDomainIfMatch / extractIfMatch.
// Putting the gate at the parser layer would force every read-only
// caller to pay the check too.
func (s *Server) enforceIfMatchPresence(w http.ResponseWriter, r *http.Request, ifMatchPresent bool) bool {
	if ifMatchPresent {
		return true
	}
	if !s.requireIfMatch {
		return true
	}
	writeError(w, http.StatusPreconditionRequired, "precondition_required",
		"If-Match header is required for this endpoint (KOJO_REQUIRE_IF_MATCH is enabled)")
	return false
}

// enforceCreateOrUpdatePrecondition is the opt-in variant used by
// handlers that DO implement create-only CAS (currently only PUT
// /api/v1/kv/{namespace}/{key} — the kv store's IfMatchAny path).
// Accepts either If-Match (update path) or If-None-Match: * (create-
// only path); refuses bare requests under strict mode.
//
// Handlers that do not actually evaluate If-None-Match: * (blob /
// memory / persona / etc.) MUST NOT call this — letting the wildcard
// through without a per-row create-only enforcement would silently
// turn the request into an unconditional overwrite.
func (s *Server) enforceCreateOrUpdatePrecondition(w http.ResponseWriter, r *http.Request, ifMatchPresent bool) bool {
	if ifMatchPresent {
		return true
	}
	if hasIfNoneMatchWildcard(r) {
		return true
	}
	if !s.requireIfMatch {
		return true
	}
	writeError(w, http.StatusPreconditionRequired, "precondition_required",
		"If-Match (or If-None-Match: * for create-only) header is required for this endpoint (KOJO_REQUIRE_IF_MATCH is enabled)")
	return false
}

// hasIfNoneMatchWildcard returns true when the request carries
// exactly one If-None-Match header whose value (after trimming
// whitespace) is `*`. Multi-line headers or any other shape
// (additional etags, comma-lists, weak etag, mixed `*` + value)
// MUST NOT satisfy the create-only gate — letting a request with
// `If-None-Match: *, "abc"` through here would allow the gate to
// pass while the kv handler's strict parser rejects the value,
// leaving the request in a confused state. The conservative single-
// value rule matches the KV handler's `extractDomainIfNoneMatch`
// strict-parse expectation so the gate and the handler agree on
// whether create-only is in effect.
func hasIfNoneMatchWildcard(r *http.Request) bool {
	vals := r.Header.Values("If-None-Match")
	if len(vals) != 1 {
		return false
	}
	return strings.TrimSpace(vals[0]) == "*"
}

// extractDomainIfMatch parses the (zero or one) If-Match header for a
// domain-table mutation.
//
// Returns:
//   - ("", false, nil)        — no header sent. Caller proceeds with no
//     precondition (used by daemon-internal callers and freshly-created
//     resources).
//   - ("*", true, nil)        — wildcard. Caller treats the write as
//     "any current version is acceptable"; useful for create-or-replace
//     and for rare admin overrides.
//   - ("v1-deadbeef", true, nil) — exactly one well-formed strong etag.
//   - ("", false, errBadDomainIfMatch) — multi-line, weak (`W/"..."`),
//     comma-list, missing quotes, or empty-quotes (`""`).
//
// Reads via Header.Values rather than .Get so a stray duplicate header
// line (e.g. from a buggy proxy) cannot silently drop a precondition.
func extractDomainIfMatch(r *http.Request) (string, bool, error) {
	vals := r.Header.Values("If-Match")
	if len(vals) == 0 {
		return "", false, nil
	}
	if len(vals) > 1 {
		return "", false, errBadDomainIfMatch
	}
	v := strings.TrimSpace(vals[0])
	if v == "" {
		return "", false, errBadDomainIfMatch
	}
	if v == "*" {
		return "*", true, nil
	}
	if strings.HasPrefix(v, "W/") {
		return "", false, errBadDomainIfMatch
	}
	if strings.Contains(v, ",") {
		return "", false, errBadDomainIfMatch
	}
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return "", false, errBadDomainIfMatch
	}
	inner := v[1 : len(v)-1]
	if inner == "" {
		// `""` is syntactically a valid strong etag but maps to an
		// empty precondition internally. Refuse rather than silently
		// match-nothing.
		return "", false, errBadDomainIfMatch
	}
	return inner, true, nil
}

// extractDomainIfNoneMatch parses If-None-Match for a GET request,
// supporting only the single-strong-etag and wildcard (`*`) forms used
// by the Web UI's local cache. Returns ("", false) when the header is
// absent or malformed (handlers treat malformed as "no match" rather
// than 400 — RFC 7232 says receivers MUST NOT use a malformed
// precondition, which is functionally the same as having none).
func extractDomainIfNoneMatch(r *http.Request) (string, bool) {
	v := strings.TrimSpace(r.Header.Get("If-None-Match"))
	if v == "" {
		return "", false
	}
	if v == "*" {
		return "*", true
	}
	if strings.HasPrefix(v, "W/") || strings.Contains(v, ",") {
		return "", false
	}
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return "", false
	}
	inner := v[1 : len(v)-1]
	if inner == "" {
		return "", false
	}
	return inner, true
}
