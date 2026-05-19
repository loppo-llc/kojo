package server

import (
	"net/http"
	"os"

	"github.com/loppo-llc/kojo/internal/auth"
)

// handleDebugWhoAmI is a transient diagnostic endpoint that echoes
// the request's Principal as stamped by the chain that ran ahead of
// the mux. Used to bisect "browser sees 403" reports without having
// to enable Debug logging or recompile with extra instrumentation.
//
// Returns a small JSON body with role, peer_id (if any), agent_id
// (if any), the resolved tailnet self-NodeKey (so the caller can
// confirm the Hub's identity capture has completed), and the
// observed RemoteAddr (so the operator can see what tsnet's WhoIs
// was actually fed for this request).
//
// Allowlisted in policy.AllowNonOwner so callers stamped as Guest
// can still reach it — otherwise the endpoint would just 403 along
// with the rest of the API and tell us nothing.
//
// Remove when the desktop-tvt 403 regression is closed out.
func (s *Server) handleDebugWhoAmI(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	hostname, _ := os.Hostname()
	resp := map[string]any{
		"role":          roleName(p.Role),
		"peer_id":       p.PeerID,
		"agent_id":      p.AgentID,
		"remote_addr":   r.RemoteAddr,
		"self_node_key": s.currentSelfNodeKey(),
		"hostname":      hostname,
	}
	writeJSONResponse(w, http.StatusOK, resp)
}

// roleName returns a human-readable name for an auth.Role. Kept
// local to the debug handler — production code only ever needs
// IsOwner/IsAgent/IsPeer, never a string label.
func roleName(r auth.Role) string {
	switch r {
	case auth.RoleGuest:
		return "guest"
	case auth.RoleAgent:
		return "agent"
	case auth.RolePrivAgent:
		return "priv_agent"
	case auth.RoleWebDAV:
		return "webdav"
	case auth.RolePeer:
		return "peer"
	case auth.RoleOwner:
		return "owner"
	}
	return "unknown"
}
