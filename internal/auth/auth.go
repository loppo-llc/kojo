// Package auth provides a lightweight, role-based access control layer for
// kojo's HTTP API. Its purpose is "spoiler prevention" — keeping an agent
// from incidentally reading other agents' Persona / configuration when it
// curls the API on its own. It is NOT a security boundary against a
// malicious agent: an agent runs as the same OS user as kojo itself and
// can read other agents' files directly. See README for the threat model.
package auth

import (
	"context"
	"net/http"
	"strings"
)

// Role identifies the actor behind an HTTP request after middleware
// resolution.
type Role int

const (
	// RoleGuest is the default role for unauthenticated requests on the
	// auth-required listener. Guests can only read directory entries.
	RoleGuest Role = iota
	// RoleAgent is a regular agent-bound principal. It can fully read its
	// own data and is limited to DirectoryEntry on other agents.
	RoleAgent
	// RolePrivAgent extends RoleAgent with the ability to delete/reset
	// other agents (but not fork or read their full data).
	RolePrivAgent
	// RoleWebDAV is a scoped principal issued for the short-lived WebDAV
	// mount token (docs §3.4 / §5.6). It has zero rights on the normal
	// /api/v1/* API surface — every IsOwner / IsAgent gate returns false.
	// The WebDAV handler is the only consumer; see Server.webdavGate.
	// Stored in ctx so the WebDAV gate can accept the token alongside
	// the OwnerOnly principal on the public listener.
	RoleWebDAV
	// RolePeer authenticates a request signed by a registered remote
	// peer's Ed25519 identity (docs §3.10 / §3.7). The principal's
	// PeerID names the device_id from peer_registry; the scope is
	// strictly peer-to-peer routes (status events feed, blob handoff
	// fetch) — every other gate returns false. Set by
	// peer.AuthMiddleware before the OwnerOnly / Auth middleware
	// chains run, so the peer's identity wins over the default
	// "Tailscale reach == Owner" promotion.
	RolePeer
	// RoleOwner is the kojo user. It has full access to everything.
	RoleOwner
)

// Principal identifies the actor behind a request.
type Principal struct {
	Role    Role
	AgentID string // populated for RoleAgent / RolePrivAgent
	PeerID  string // populated for RolePeer (device_id from peer_registry)
}

// IsOwner returns true if the principal is the kojo user.
func (p Principal) IsOwner() bool { return p.Role == RoleOwner }

// IsAgent returns true if the principal is bound to a specific agent
// (regular or privileged).
func (p Principal) IsAgent() bool {
	return p.Role == RoleAgent || p.Role == RolePrivAgent
}

// IsWebDAV reports whether the principal was authenticated via a
// short-lived WebDAV token. RoleWebDAV is intentionally a dead-end for
// every other gate: a leaked token can only reach the WebDAV mount.
func (p Principal) IsWebDAV() bool { return p.Role == RoleWebDAV }

// IsPeer reports whether the principal was authenticated via a
// peer's Ed25519 signature. RolePeer is scoped to inter-peer
// endpoints (cross-subscribe status feed, blob handoff fetch)
// AND to proxied agent requests (§3.7 remoteAgentProxy: Hub
// forwards browser/agent requests to the holder peer, signing
// them with its own Ed25519 key). Handler-level guard methods
// (CanReadFull, CanMutateSelf, CanDeleteOrReset) admit IsPeer
// because the Hub already ran Enforce before proxying —
// re-blocking at the handler would 403 every proxied request.
func (p Principal) IsPeer() bool { return p.Role == RolePeer }

// CanReadFull returns true if the principal can read the full record
// (Persona, Token-bearing fields, etc.) for the given target agent ID.
// Owners can read any. Agents can only read their own. Peers are
// admitted because the Hub's proxy already validated the original
// caller's identity before forwarding.
func (p Principal) CanReadFull(targetID string) bool {
	if p.IsOwner() || p.IsPeer() {
		return true
	}
	return p.IsAgent() && p.AgentID == targetID
}

// CanMutateSelf returns true if the principal may issue self-scoped
// mutations (PATCH, reset, etc.) against targetID. Peers pass through
// because the Hub's Enforce layer already authorised the original
// request before the proxy signed and forwarded it.
func (p Principal) CanMutateSelf(targetID string) bool {
	if p.IsOwner() || p.IsPeer() {
		return true
	}
	return p.IsAgent() && p.AgentID == targetID
}

// CanDeleteOrReset returns true for delete/reset/unarchive/checkin/
// reset-session ops. Owner: any. PrivAgent: any. Agent: self only.
// Peer: admitted — Hub proxy validated the original caller.
func (p Principal) CanDeleteOrReset(targetID string) bool {
	if p.IsOwner() || p.Role == RolePrivAgent || p.IsPeer() {
		return true
	}
	return p.IsAgent() && p.AgentID == targetID
}

// CanForkOrCreate returns true only for the Owner. Forking copies
// persona/memory and would leak the source agent's full state, so it is
// kept Owner-only even for privileged agents. Bare creation is also
// Owner-only.
func (p Principal) CanForkOrCreate() bool {
	return p.IsOwner()
}

// CanSetPrivileged returns true only for the Owner. A privileged-agent
// must never be able to grant or revoke privilege.
func (p Principal) CanSetPrivileged() bool {
	return p.IsOwner()
}

// Resolver maps a Bearer token to a Principal.
type Resolver struct {
	tokens       *TokenStore
	webdav       *WebDAVTokenStore
	isPrivileged func(agentID string) bool
}

// NewResolver builds a Resolver from a TokenStore and a privilege
// predicate (agent.Manager.IsPrivileged). The WebDAVTokenStore is
// optional; pass nil when the WebDAV slice is not wired (tests / builds
// without kv).
func NewResolver(tokens *TokenStore, isPrivileged func(string) bool) *Resolver {
	if isPrivileged == nil {
		isPrivileged = func(string) bool { return false }
	}
	return &Resolver{tokens: tokens, isPrivileged: isPrivileged}
}

// SetWebDAVStore attaches the short-lived WebDAV token store. Safe to
// call once at boot; callers that don't issue WebDAV tokens leave it
// unset. Calling twice with different stores is a misuse and panics
// (the first store would silently lose verifier coverage).
func (r *Resolver) SetWebDAVStore(s *WebDAVTokenStore) {
	if r == nil {
		return
	}
	if r.webdav != nil && r.webdav != s {
		panic("auth.Resolver: WebDAVTokenStore already set")
	}
	r.webdav = s
}

// Resolve maps a Bearer token to a Principal. Empty/unknown tokens
// resolve to RoleGuest.
func (r *Resolver) Resolve(token string) Principal {
	if r == nil || r.tokens == nil || token == "" {
		return Principal{Role: RoleGuest}
	}
	// Owner: hash-only comparison; we don't need the raw to verify a
	// presented token. VerifyOwner is constant-time internally.
	if r.tokens.VerifyOwner(token) {
		return Principal{Role: RoleOwner}
	}
	if id, ok := r.tokens.LookupAgent(token); ok {
		if r.isPrivileged(id) {
			return Principal{Role: RolePrivAgent, AgentID: id}
		}
		return Principal{Role: RoleAgent, AgentID: id}
	}
	// WebDAV short-lived token last: its surface is intentionally
	// minimal (the gate around /api/v1/webdav/ is the only consumer),
	// so verifying it after owner/agent keeps the hot path for normal
	// API traffic unchanged.
	if r.webdav != nil {
		if r.webdav.Verify(token) {
			return Principal{Role: RoleWebDAV}
		}
	}
	return Principal{Role: RoleGuest}
}

// --- context plumbing ------------------------------------------------

type ctxKey struct{}

// WithPrincipal attaches a Principal to ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext retrieves the Principal stashed in ctx by middleware.
// Defaults to RoleGuest when no principal is set.
func FromContext(ctx context.Context) Principal {
	if v, ok := ctx.Value(ctxKey{}).(Principal); ok {
		return v
	}
	return Principal{Role: RoleGuest}
}

// --- middleware ------------------------------------------------------

// OwnerOnlyMiddleware tags every request as the Owner UNLESS an
// earlier middleware (e.g. peer.AuthMiddleware) already attached a
// non-Guest principal. The exception keeps the "Tailscale reach ==
// Owner" UX for the kojo user from clobbering a peer's Ed25519-
// authenticated identity on the same listener.
//
// Used on the public (Tailscale) listener that the kojo user
// accesses from their phone — the user's UX is preserved (no
// token required) for everything except inter-peer requests that
// arrive pre-stamped.
func OwnerOnlyMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if existing := FromContext(r.Context()); existing.Role != RoleGuest {
			h.ServeHTTP(w, r)
			return
		}
		ctx := WithPrincipal(r.Context(), Principal{Role: RoleOwner})
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AuthMiddleware resolves Authorization: Bearer / X-Kojo-Token to a
// Principal and passes it through ctx. It does NOT enforce per-route
// policy — that is the handler's responsibility (or a separate gate).
//
// Skips the Bearer resolution when an earlier middleware (e.g.
// peer.AuthMiddleware) already attached a non-Guest principal so a
// peer's Ed25519-signed request doesn't get downgraded to Guest by
// the absence of a Bearer.
func AuthMiddleware(resolver *Resolver) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if existing := FromContext(r.Context()); existing.Role != RoleGuest {
				h.ServeHTTP(w, r)
				return
			}
			tok := extractBearer(r)
			p := resolver.Resolve(tok)
			ctx := WithPrincipal(r.Context(), p)
			h.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractBearer reads the bearer token from `Authorization: Bearer X`,
// the `X-Kojo-Token` header, or — only for GET / HEAD requests — the
// `?token=` query parameter. The query-param fallback exists because
// the browser WebSocket API cannot set custom headers and `<img>` /
// `<a>` elements similarly drive their own GET requests; restricting
// it to safe verbs keeps a leaked URL from being replayed against
// state-changing endpoints (POST/PATCH/DELETE).
//
// Query-param tokens land in HTTP access logs. That is acceptable for
// the spoiler-prevention threat model, and the UI strips the param
// from window.location after consuming it on first load.
func extractBearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			return strings.TrimSpace(h[len(prefix):])
		}
	}
	if h := r.Header.Get("X-Kojo-Token"); h != "" {
		return strings.TrimSpace(h)
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, "":
		if t := r.URL.Query().Get("token"); t != "" {
			return strings.TrimSpace(t)
		}
	}
	return ""
}
