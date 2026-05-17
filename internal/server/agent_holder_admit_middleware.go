package server

import (
	"net/http"
	"strings"

	"github.com/loppo-llc/kojo/internal/auth"
)

// agentHolderAdmitMiddleware narrowly promotes an untrusted RolePeer
// request to the agent-proxy surface when the agent_locks row says
// the SIGNER currently holds the lock on the targeted agent. The
// promotion lives ONLY in the request's context.Principal; the
// peer_registry.trusted column stays untouched, so the other
// privileged surfaces (sessions, files, git, upload, info, dirs)
// remain closed.
//
// Why this exists: §3.7 device-switch lands an agent on a peer that
// has not necessarily been marked trusted on the receiving host.
// Every post-switch Hub→peer chat / messages / persona / memory
// edit travels as a RolePeer-signed proxy. The strict policy gate
// (PeerTrusted required for /api/v1/agents/*) would 403 them all,
// breaking the device-switch UX. A blanket admit (drop the
// PeerTrusted requirement) leaks credentials/persona/memory to any
// paired peer. The lock-holder gate threads the needle: a peer
// that legitimately owns an agent's runtime (which only the
// orchestrator can transfer) gets request-scoped admit; everyone
// else falls through to the policy default-deny.
//
// Chain placement: this middleware MUST run AFTER PeerAuth (so
// p.PeerID is populated) and BEFORE EnforceMiddleware (so the
// promoted Principal participates in the policy gate). Owner /
// untrusted-non-peer principals pass through untouched.
func (s *Server) agentHolderAdmitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/agents/") {
			next.ServeHTTP(w, r)
			return
		}
		p := auth.FromContext(r.Context())
		if !p.IsPeer() || p.PeerTrusted {
			// Owners are admin; already-trusted peers pass via
			// the normal policy admit. Only the bare RolePeer
			// case needs the lock-holder lookup.
			next.ServeHTTP(w, r)
			return
		}
		id, _, ok := auth.SplitAgentIDPath(r.URL.Path)
		if !ok || id == "" {
			next.ServeHTTP(w, r)
			return
		}
		if s.agents == nil || s.agents.Store() == nil {
			next.ServeHTTP(w, r)
			return
		}
		lock, err := s.agents.Store().GetAgentLock(r.Context(), id)
		if err != nil || lock == nil {
			// No lock row or read error — fall through. The
			// policy gate will 403 this RolePeer request, which
			// is the correct default-deny posture for an agent
			// the signer can't prove it holds.
			next.ServeHTTP(w, r)
			return
		}
		if lock.HolderPeer == "" || lock.HolderPeer != p.PeerID {
			next.ServeHTTP(w, r)
			return
		}
		// Holder match: stamp a per-request promotion so the
		// policy gate admits THIS request only. The registry row
		// stays untrusted; the next request from the same signer
		// re-runs the lookup and gets admitted (or not) based on
		// the live lock state — releasing the lock revokes the
		// admit on the very next call.
		promoted := p
		promoted.PeerTrusted = true
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), promoted)))
	})
}
