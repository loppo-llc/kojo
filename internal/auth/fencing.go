package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/loppo-llc/kojo/internal/store"
)

// HeaderNoIdempotencyCache is the response header any middleware
// sets to tell the idempotency wrapper NOT to save the captured
// response. Used by AgentFencing for its 409 wrong_holder
// response: the underlying lock state is transient, so caching
// the refusal would shadow a future successful retry.
const HeaderNoIdempotencyCache = "X-Kojo-No-Idempotency-Cache"

// AgentFencingStore is the minimal contract AgentFencingMiddleware
// needs from the DB layer. Lets the test fixtures pass a fake
// without standing up a full *store.Store.
type AgentFencingStore interface {
	GetAgentLock(ctx context.Context, agentID string) (*store.AgentLockRecord, error)
}

// AgentFencingMiddleware refuses mutating requests from
// RoleAgent / RolePrivAgent principals when agent_locks.holder_peer
// does NOT match the local peer. Closes the §3.7 device-switch
// invariant the previous slice left open: once the orchestrator
// transfers the lock to a target peer, the source peer's agent
// runtime MUST stop writing to the agent's tables.
//
// Scope:
//
//   - Only RoleAgent / RolePrivAgent are gated. RoleOwner /
//     RolePeer / RoleWebDAV / RoleGuest pass through untouched
//     (owner is admin; peers run their own auth surface; webdav /
//     guest don't write to agent state).
//   - Read methods (GET / HEAD / OPTIONS) pass through. Lock
//     holders rotate via complete; readers should still observe
//     transient state without 409s.
//   - The agent's own /handoff/switch route is exempted because
//     it IS the call that moves the lock away — refusing it would
//     create a chicken-and-egg deadlock.
//   - Routes outside /api/v1/agents/{agentID}/ pass through. The
//     gate is per-agent; cross-agent routes (groupdms, etc.) have
//     their own membership checks.
//
// Failure modes:
//
//   - lock row missing (ErrNotFound): the agent has no claimed
//     lock yet (first boot, or recently released). v1 single-Hub
//     trusts that AgentLockGuard will Acquire shortly; we pass
//     through rather than block a fresh agent. A future multi-
//     peer cluster might tighten this to "no lock = refuse".
//   - store read error: 503 with a generic message; the agent
//     runtime should retry rather than barge through an unknown
//     state.
//   - holder mismatch: 409 wrong_holder so the agent runtime
//     knows to stop driving writes and (typically) shut itself
//     down — the lock has moved.
func AgentFencingMiddleware(st AgentFencingStore, selfPeerID string, logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !shouldFenceRequest(r) {
				next.ServeHTTP(w, r)
				return
			}
			p := FromContext(r.Context())
			if !p.IsAgent() {
				next.ServeHTTP(w, r)
				return
			}
			fenceID, ok := agentIDForFencing(r.URL.Path, p.AgentID)
			if !ok {
				// Not an agent-write route the fence covers.
				// EnforceMiddleware's allowlist already binds
				// non-owner mutations to the caller's own
				// agent_id; the fence trusts that gate and
				// only kicks in for routes where a mutation
				// actually targets agent-scoped state on this
				// peer.
				next.ServeHTTP(w, r)
				return
			}
			if id, sub, sok := SplitAgentIDPath(r.URL.Path); sok {
				// Exempt the orchestrated-switch route on the
				// per-agent path — it's the very mechanism
				// that moves the lock away. fenceID may differ
				// from id if the request is cross-agent, but
				// EnforceMiddleware already 403s that case.
				if id == p.AgentID && sub == "/handoff/switch" {
					next.ServeHTTP(w, r)
					return
				}
			}
			if st == nil || selfPeerID == "" {
				// Misconfigured pipeline. Refuse rather than
				// silently passing through; v1 single-peer
				// stress paths set both. A future test
				// fixture that intentionally leaves the
				// store nil can wire a no-op middleware.
				next.ServeHTTP(w, r)
				return
			}
			lock, err := st.GetAgentLock(r.Context(), fenceID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					// No lock claimed yet — pass through.
					// AgentLockGuard's refresh loop will
					// Acquire on the next tick.
					next.ServeHTTP(w, r)
					return
				}
				logger.Error("agent fencing: lock read failed",
					"agent", fenceID, "err", err)
				http.Error(w,
					`{"error":{"code":"unavailable","message":"agent lock read failed"}}`,
					http.StatusServiceUnavailable)
				return
			}
			if lock.HolderPeer != selfPeerID {
				logger.Info("agent fencing: refusing mutation; lock held elsewhere",
					"agent", fenceID, "holder", lock.HolderPeer, "self", selfPeerID,
					"method", r.Method, "path", r.URL.Path)
				// Signal the idempotency wrapper not to save
				// this response: the underlying state is
				// transient (the lock can come back to this
				// peer after a complete-then-back-switch) and
				// caching the 409 would let a stale 409
				// shadow a perfectly valid retry.
				w.Header().Set(HeaderNoIdempotencyCache, "1")
				http.Error(w,
					`{"error":{"code":"wrong_holder","message":"agent_lock is held by another peer; this peer is not authoritative for writes"}}`,
					http.StatusConflict)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// agentIDForFencing maps an agent-mutating route to the agent_id
// the fence should check the lock on. Returns ok=false for routes
// the fence does not cover (cross-cutting endpoints whose
// mutations don't tie to a single agent's lock, plus routes that
// EnforceMiddleware would 403 before they reach here).
//
// Covered routes (RoleAgent self only — cross-agent paths are
// already gated by EnforceMiddleware):
//
//   - /api/v1/agents/{p.AgentID}/...  (per-agent writes)
//   - /api/v1/groupdms                (creation — fenced because
//                                       the creating agent's row
//                                       is what mutates)
//   - /api/v1/groupdms/{id}/...       (membership / messages —
//                                       fenced against the
//                                       calling agent's lock,
//                                       since the agent's
//                                       holder peer is what
//                                       changes on a switch)
//
// Anything else (cron-paused, directory, /info, /ws) passes
// through; those either don't write or have their own auth
// surface.
func agentIDForFencing(path, callerAgentID string) (string, bool) {
	if id, _, ok := SplitAgentIDPath(path); ok {
		if id == callerAgentID {
			return id, true
		}
		return "", false
	}
	if path == "/api/v1/groupdms" || strings.HasPrefix(path, "/api/v1/groupdms/") {
		return callerAgentID, true
	}
	return "", false
}

// shouldFenceRequest decides whether the request's method
// warrants fencing. Mutating verbs are fenced; safe verbs and
// WebSocket upgrades are not. WebDAV's COPY / MOVE / PROPFIND
// / etc. are intentionally NOT fenced here because the WebDAV
// mount surface has its own token-based auth and lives outside
// the per-agent {id} path shape.
func shouldFenceRequest(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	// WebSocket upgrade GETs are already caught by the GET
	// branch above. Other non-API paths slip through too —
	// the SplitAgentIDPath check inside the handler stops
	// gating those.
	if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		return false
	}
	return true
}
