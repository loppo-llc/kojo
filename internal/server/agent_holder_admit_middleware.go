package server

import (
	"net/http"
	"strings"

	"github.com/loppo-llc/kojo/internal/auth"
)

// agentHolderAdmitMiddleware narrowly promotes an untrusted
// RolePeer request to the agent-proxy surface when TWO conditions
// hold for the targeted agent:
//
//  1. THIS host is the current lock holder
//     (agent_locks.holder_peer == self.deviceID).
//  2. The request's SIGNER is the orchestrator the lock row
//     authorised (agent_locks.allowed_proxy_peer == p.PeerID).
//
// (2) is the load-bearing check: without it, any paired-but-
// untrusted peer that knows the lock points here could fish for
// credentials / persona / memory just by signing a request and
// hitting an agent route. The allowed_proxy_peer column is
// stamped by the §3.7 device-switch transfer (source) and the
// operator-driven force-reclaim (self); a fresh local Acquire
// sets it to the holder itself.
//
// The promotion lives ONLY in the request's context.Principal;
// peer_registry.trusted stays untouched, so the other privileged
// surfaces (sessions, files, git, upload, info, dirs) remain
// closed to this signer.
//
// Chain placement: this middleware MUST run AFTER PeerAuth (so
// p.PeerID is populated) and BEFORE EnforceMiddleware (so the
// promoted Principal participates in the policy gate). Owner /
// already-trusted-peer / non-peer principals pass through
// untouched.
func (s *Server) agentHolderAdmitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/agents/") {
			next.ServeHTTP(w, r)
			return
		}
		p := auth.FromContext(r.Context())
		if !p.IsPeer() || p.PeerTrusted {
			next.ServeHTTP(w, r)
			return
		}
		id, _, ok := auth.SplitAgentIDPath(r.URL.Path)
		if !ok || id == "" {
			next.ServeHTTP(w, r)
			return
		}
		if s.agents == nil || s.agents.Store() == nil || s.peerID == nil {
			next.ServeHTTP(w, r)
			return
		}
		lock, err := s.agents.Store().GetAgentLock(r.Context(), id)
		if err != nil || lock == nil {
			next.ServeHTTP(w, r)
			return
		}
		if lock.HolderPeer == "" || lock.HolderPeer != s.peerID.DeviceID {
			next.ServeHTTP(w, r)
			return
		}
		if lock.AllowedProxyPeer == "" || lock.AllowedProxyPeer != p.PeerID {
			// Signer is NOT the orchestrator this lock
			// authorised. Refuse via the policy gate.
			next.ServeHTTP(w, r)
			return
		}
		// Holder == self AND signer == allowed_proxy_peer: stamp
		// a per-request promotion. The next request re-runs the
		// lookup, so a transfer / release revokes the admit on
		// the very next call.
		promoted := p
		promoted.PeerTrusted = true
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), promoted)))
	})
}
