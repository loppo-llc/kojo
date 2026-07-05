package server

import (
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

		// NOTE: avatar GET (`/avatar`) is intentionally NOT exempted.
		// The avatar blob is path-addressed (`agents/<id>/avatar.<ext>`)
		// and MUTABLE — not content-addressed — so a non-holder's local
		// copy can be missing (never replicated) or stale (changed on
		// the holder after a past device-switch). Proxy it to the holder
		// like every other agent read so the UI shows the live avatar.

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

		// Queue-and-forward inspection / cancel: the queue table
		// lives hub-side, so these must never be proxied to the
		// holder.
		if strings.HasPrefix(sub, "/queued-messages") {
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

		// --- Hub-local fallback for settings PATCH (offline holder) ---
		//
		// Classification (full rationale in
		// internal/agent/hub_local_update.go):
		//
		//   HUB-LOCAL-SAFE — pure hub-DB row fields, editable even when
		//   the holder is offline/unknown: name, publicProfile(+Override),
		//   effort, autoEffort, disabledInjections,
		//   silentStart/silentEnd, notifyDuringSilent.
		//
		//   HOLDER-ONLY — everything that mutates holder disk or the
		//   live session and therefore keeps the proxy (and its
		//   peer_offline failure): persona, model/tool/customBaseURL/
		//   thinkingMode, workDir, cronExpr, cronMessage (persists via
		//   agent_workspace_files, not the agents row), timeout/resumeIdle,
		//   allowedTools/allowProtectedPaths, tts, deviceSwitchEnabled,
		//   plus the non-PATCH routes (avatar upload, persona/status/
		//   memory/workspace-file writes, transcript edits, credentials,
		//   tasks, sessions).
		//
		// When the holder is ONLINE the PATCH is still proxied — the
		// holder's in-memory agent is the write authority and its row
		// syncs home at the next device switch. Only when that proxy
		// is guaranteed to fail (holder offline or unknown) does a
		// hub-safe-only payload get applied to the hub's own row.
		if r.Method == http.MethodPatch && sub == "" && !s.holderPeerOnline(r.Context(), remote.HolderPeer) {
			if s.tryPatchRemoteAgentHubLocal(w, r, id) {
				return
			}
		}

		// --- Proxy to holder peer ---

		if remote.HolderPeer == "" || s.peerID == nil {
			writeError(w, http.StatusServiceUnavailable, "agent_remote",
				"agent is on a remote peer but holder is unknown; cannot proxy")
			return
		}

		// Message send gets queue-and-forward semantics: holder
		// offline (or unreachable on dial) enqueues into
		// handoff_queued_messages instead of failing with 502.
		// Every other mutation keeps the plain proxy behaviour.
		if r.Method == http.MethodPost && sub == "/messages" {
			s.proxyOrQueueAgentMessage(w, r, id, remote.HolderPeer)
			return
		}

		s.proxyToHolderPeer(w, r, id, remote.HolderPeer)
	})
}

// proxyToHolderPeer forwards the HTTP request to the peer that
// holds the agent's runtime lock. Authentication is by Tailnet
// identity — the forward dials the target peer over tsnet and its
// ServeAuthTsnet listener stamps RolePeer from the WhoIs-resolved
// peer_registry row, so no Authorization header is required on
// the forwarded request. Handler-level guards (If-Match, busy
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
	// logs. Authentication on the hop itself is by Tailnet identity
	// (the target's ServeAuthTsnet stamps RolePeer from the
	// WhoIs-resolved peer_registry row); no header needs to survive.
	targetURL := addr + r.URL.Path
	q := r.URL.Query()
	if q.Get("token") != "" {
		q.Del("token")
	}
	if encoded := q.Encode(); encoded != "" {
		targetURL += "?" + encoded
	}

	// Peer-auth no longer hashes the body, so we stream the request
	// straight through instead of buffering it. Downstream handlers
	// apply their own per-route MaxBytesReader. Content metadata is
	// preserved so the target parses the body correctly (JSON,
	// multipart, etc.).
	//
	// No HTTP client timeout: avatar uploads can be 128 MiB and raw
	// fetches stream arbitrary body sizes. The request context cancels
	// the dispatch when the caller disconnects.
	//
	// The upstream response is streamed back unbounded, preserving
	// every header a browser keys off — raw file downloads under
	// /api/v1/agents/{id}/files/raw need Content-Disposition +
	// Content-Length to land as proper saves.
	s.forwardHTTPToPeer(w, r.Context(), peerHTTPForward{
		method:         r.Method,
		url:            targetURL,
		body:           r.Body,
		contentLength:  r.ContentLength,
		srcHeader:      r.Header,
		reqHeaderKeys:  []string{"Content-Type", "If-Match", "Idempotency-Key"},
		timeout:        0,
		buildErrStatus: http.StatusBadGateway,
		buildErrCode:   "proxy_build",
		buildErrPrefix: "failed to build proxy request: ",
		dialErrStatus:  http.StatusBadGateway,
		dialErrCode:    "proxy_failed",
		dialErrPrefix:  "holder peer unreachable: ",
		onDialErr: func(err error) {
			if s.logger != nil {
				s.logger.Debug("remote agent proxy failed",
					"agent", agentID, "peer", holderDeviceID, "err", err)
			}
		},
		respHeaderKeys: []string{
			"Content-Type", "ETag",
			"X-Kojo-No-Idempotency-Cache",
			"Content-Disposition", "Content-Length",
			"Last-Modified", "Cache-Control",
		},
	})
}
