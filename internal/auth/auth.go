// Package auth provides a lightweight, role-based access control layer for
// kojo's HTTP API. Its purpose is "spoiler prevention" — keeping an agent
// from incidentally reading other agents' Persona / configuration when it
// curls the API on its own. It is NOT a security boundary against a
// malicious agent: an agent runs as the same OS user as kojo itself and
// can read other agents' files directly. See README for the threat model.
package auth

import (
	"context"
	"crypto/subtle"
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
	// RoleOwner is the kojo user. It has full access to everything.
	RoleOwner
)

// Principal identifies the actor behind a request.
type Principal struct {
	Role    Role
	AgentID string // populated for RoleAgent / RolePrivAgent
}

// IsOwner returns true if the principal is the kojo user.
func (p Principal) IsOwner() bool { return p.Role == RoleOwner }

// IsAgent returns true if the principal is bound to a specific agent
// (regular or privileged).
func (p Principal) IsAgent() bool {
	return p.Role == RoleAgent || p.Role == RolePrivAgent
}

// CanReadFull returns true if the principal can read the full record
// (Persona, Token-bearing fields, etc.) for the given target agent ID.
// Owners can read any. Agents can only read their own.
func (p Principal) CanReadFull(targetID string) bool {
	if p.IsOwner() {
		return true
	}
	return p.IsAgent() && p.AgentID == targetID
}

// CanMutateSelf returns true if the principal may issue self-scoped
// mutations (PATCH, reset, etc.) against targetID.
func (p Principal) CanMutateSelf(targetID string) bool {
	if p.IsOwner() {
		return true
	}
	return p.IsAgent() && p.AgentID == targetID
}

// CanDeleteOrReset returns true for delete/reset/unarchive/checkin/
// reset-session ops. Owner: any. PrivAgent: any. Agent: self only.
func (p Principal) CanDeleteOrReset(targetID string) bool {
	if p.IsOwner() || p.Role == RolePrivAgent {
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
	tokens         *TokenStore
	isPrivileged   func(agentID string) bool
}

// NewResolver builds a Resolver from a TokenStore and a privilege
// predicate (agent.Manager.IsPrivileged).
func NewResolver(tokens *TokenStore, isPrivileged func(string) bool) *Resolver {
	if isPrivileged == nil {
		isPrivileged = func(string) bool { return false }
	}
	return &Resolver{tokens: tokens, isPrivileged: isPrivileged}
}

// Resolve maps a Bearer token to a Principal. Empty/unknown tokens
// resolve to RoleGuest.
func (r *Resolver) Resolve(token string) Principal {
	if r == nil || r.tokens == nil || token == "" {
		return Principal{Role: RoleGuest}
	}
	if owner := r.tokens.OwnerToken(); owner != "" {
		if subtle.ConstantTimeCompare([]byte(token), []byte(owner)) == 1 {
			return Principal{Role: RoleOwner}
		}
	}
	if id, ok := r.tokens.LookupAgent(token); ok {
		if r.isPrivileged(id) {
			return Principal{Role: RolePrivAgent, AgentID: id}
		}
		return Principal{Role: RoleAgent, AgentID: id}
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

// OwnerOnlyMiddleware unconditionally tags every request as the Owner.
// Used on the public (Tailscale) listener that the kojo user accesses
// from their phone — the user's UX is preserved (no token required).
func OwnerOnlyMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithPrincipal(r.Context(), Principal{Role: RoleOwner})
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AuthMiddleware resolves Authorization: Bearer / X-Kojo-Token to a
// Principal and passes it through ctx. It does NOT enforce per-route
// policy — that is the handler's responsibility (or a separate gate).
func AuthMiddleware(resolver *Resolver) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := extractBearer(r)
			p := resolver.Resolve(tok)
			ctx := WithPrincipal(r.Context(), p)
			h.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractBearer reads the bearer token from `Authorization: Bearer X`
// or, as a fallback, from the `X-Kojo-Token` header. The fallback
// makes it easier for shells/curl invocations inside an agent to send
// the token via a single header without quoting "Bearer ".
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
	return ""
}
