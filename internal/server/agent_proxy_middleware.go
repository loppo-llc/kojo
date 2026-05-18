package server

import (
	"io"
	"net/http"
	"strings"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// remoteAgentProxyMiddleware transparently proxies requests for
// remote agents (§3.7 device-switch) to the holder peer. Runs
// after auth (Principal is in ctx) and before the mux handlers.
//
// Decision tree per request:
//
//  1. Not an /api/v1/agents/{id}/* path → pass through.
//  2. Agent is in the local in-memory map → pass through (local).
//  3. Agent is not in the store at all → pass through (handler 404s).
//  4. Agent is remote but no HolderPeer / no peer identity → 503.
//  5. GET /avatar → pass through (handler serves from local blob).
//  6. Routes that already have their own proxy (ws, GET messages,
//     bare GET /agents/{id}) → pass through.
//  7. Everything else → HTTP reverse-proxy to the holder peer.
//
// Loop prevention: if the caller is RolePeer (i.e. this is already
// a proxied request from another peer), we pass through to let the
// local handler run — never re-proxy.
func (s *Server) remoteAgentProxyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, sub, ok := auth.SplitAgentIDPath(r.URL.Path)
		if !ok || id == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Local agent → normal handler.
		if _, local := s.agents.Get(id); local {
			next.ServeHTTP(w, r)
			return
		}

		// Loop prevention: never re-proxy a peer-signed request.
		if p := auth.FromContext(r.Context()); p.IsPeer() {
			next.ServeHTTP(w, r)
			return
		}

		remote := s.agents.GetRemote(id)
		if remote == nil {
			// Not in store either → handler will 404.
			next.ServeHTTP(w, r)
			return
		}

		// --- Routes that don't need proxying ---

		// Avatar GET: blob is content-addressed & immutable; local
		// copy is identical to target's.
		if r.Method == http.MethodGet && sub == "/avatar" {
			next.ServeHTTP(w, r)
			return
		}

		// Bare GET /agents/{id}: handler already has GetRemote fallback.
		if sub == "" && r.Method == http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}

		// WebSocket: agent_ws_proxy.go handles its own routing.
		if sub == "/ws" {
			next.ServeHTTP(w, r)
			return
		}

		// GET /messages: proxyPeerGetMessages already exists.
		if sub == "/messages" && r.Method == http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}

		// Handoff orchestration must run locally on the Hub.
		if strings.HasPrefix(sub, "/handoff/") {
			next.ServeHTTP(w, r)
			return
		}

		// Hub-only management ops: the target peer's handler would
		// 403 anyway (CanForkOrCreate / CanSetPrivileged don't
		// admit RolePeer). Short-circuit here to avoid a wasted
		// round-trip.
		if sub == "/fork" || sub == "/privilege" {
			next.ServeHTTP(w, r)
			return
		}

		// --- Proxy to holder peer ---

		if remote.HolderPeer == "" || s.peerID == nil {
			writeError(w, http.StatusServiceUnavailable, "agent_remote",
				"agent is on a remote peer but holder is unknown; cannot proxy")
			return
		}

		s.proxyToHolderPeer(w, r, id, remote.HolderPeer)
	})
}

// isRawAgentSubpath reports whether the agent sub-path streams
// bulk body data (file raw / view) and therefore needs the
// long-proxy timeout. Mirrors session_proxy_middleware's
// isRawProxyPath; kept local so the agent-routing module owns
// its own decision.
func isRawAgentSubpath(sub string) bool {
	switch sub {
	case "/files/raw", "/files/view":
		return true
	}
	return false
}

// proxyToHolderPeer forwards the HTTP request to the peer that
// holds the agent's runtime lock, signing it with this peer's
// Ed25519 identity. The target peer's policy layer admits the
// request as RolePeer; handler-level guards (If-Match, busy
// checks, etc.) still run on the target.
//
// On proxy failure the caller receives 502; on success the
// upstream response (status + headers + body) is streamed back.
func (s *Server) proxyToHolderPeer(w http.ResponseWriter, r *http.Request, agentID, holderDeviceID string) {
	st := s.agents.Store()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "store not available")
		return
	}

	peerRec, err := st.GetPeer(r.Context(), holderDeviceID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "peer_lookup_failed",
			"cannot resolve holder peer: "+err.Error())
		return
	}
	if peerRec.Status != store.PeerStatusOnline {
		writeError(w, http.StatusBadGateway, "peer_offline",
			"holder peer is not online: "+holderDeviceID)
		return
	}

	addr, err := peer.NormalizeAddress(peerRec.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "peer_address",
			"holder peer has no usable address: "+err.Error())
		return
	}

	// Reconstruct the target URL preserving path + query. Strip
	// `?token=`: extractBearer admits it as an Owner-token fallback
	// on GET/HEAD so attachment / image-src URLs work, but it must
	// NOT cross the trust boundary into the holder peer's access
	// logs. The Ed25519 signature is the only auth that needs to
	// survive this hop.
	targetURL := addr + r.URL.Path
	q := r.URL.Query()
	if q.Get("token") != "" {
		q.Del("token")
	}
	if encoded := q.Encode(); encoded != "" {
		targetURL += "?" + encoded
	}

	// Peer-auth no longer hashes the body, so we stream the
	// request straight through instead of buffering it. Downstream
	// handlers apply their own per-route MaxBytesReader.
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "proxy_build",
			"failed to build proxy request: "+err.Error())
		return
	}
	proxyReq.ContentLength = r.ContentLength

	// Preserve content metadata so the target handler parses
	// the body correctly (JSON, multipart, etc.).
	for _, h := range []string{"Content-Type", "If-Match", "Idempotency-Key"} {
		if v := r.Header.Get(h); v != "" {
			proxyReq.Header.Set(h, v)
		}
	}

	// Attach the Bearer that pairs this Hub with the holder peer.
	if err := peer.AuthorizeOutbound(proxyReq.Context(), s.agents.Store(), proxyReq, holderDeviceID); err != nil {
		writeError(w, http.StatusInternalServerError, "authorize", err.Error())
		return
	}

	// No HTTP client timeout: avatar uploads can be 128 MiB and
	// raw fetches stream arbitrary body sizes. The request
	// context cancels the dispatch when the caller disconnects.
	client := peer.NoKeepAliveHTTPClient(0)
	resp, err := client.Do(proxyReq)
	if err != nil {
		if s.logger != nil {
			s.logger.Debug("remote agent proxy failed",
				"agent", agentID, "peer", holderDeviceID, "err", err)
		}
		writeError(w, http.StatusBadGateway, "proxy_failed",
			"holder peer unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// Stream the upstream response back to the caller. Preserve
	// every header a browser keys off — raw file downloads under
	// /api/v1/agents/{id}/files/raw need Content-Disposition +
	// Content-Length to land as proper saves, and the existing
	// JSON / WS surfaces use Content-Type / ETag. Body is copied
	// unbounded; a 32 MiB silent truncate would mangle big
	// agent-side downloads.
	for _, k := range []string{
		"Content-Type", "ETag",
		"X-Kojo-No-Idempotency-Cache",
		"Content-Disposition", "Content-Length",
		"Last-Modified", "Cache-Control",
	} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
