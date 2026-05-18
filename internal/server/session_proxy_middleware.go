package server

import (
	"io"
	"net/http"
	"strings"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
)

// sessionPeerProxyMiddleware forwards a peer-aware request whose
// `?peer=<deviceId>` query points at a remote peer to that peer's
// local handler, signed as RolePeer. WebSocket upgrades are handled
// in handleWebSocket directly (the WS hijack can't ride this
// middleware because we'd lose the http.Hijacker contract); other
// verbs flow through here.
//
// Intercepted paths cover every endpoint the Hub UI hits when the
// user has selected a remote peer in NewSession or a session
// screen:
//
//   - /api/v1/sessions, /api/v1/sessions/{id}/...    session lifecycle
//   - /api/v1/info, /api/v1/dirs                     peer info + dir completion
//   - /api/v1/files, /api/v1/files/view,
//     /api/v1/files/raw                              file browser tab
//   - /api/v1/upload                                 file attach
//   - /api/v1/git/status, /api/v1/git/log,
//     /api/v1/git/diff, /api/v1/git/exec             git tab
//
// Loop prevention: a peer-signed inbound request never re-proxies.
// Missing `?peer=` or `?peer=self` falls through to the local
// handler so the existing "lives here" path is untouched.
func (s *Server) sessionPeerProxyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isPeerProxyPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		pid := r.URL.Query().Get("peer")
		if pid == "" || s.peerID == nil || pid == s.peerID.DeviceID {
			next.ServeHTTP(w, r)
			return
		}
		// Loop guard.
		if p := auth.FromContext(r.Context()); p.IsPeer() {
			next.ServeHTTP(w, r)
			return
		}
		s.proxySessionRequest(w, r, pid)
	})
}

// isPeerProxyPath returns true when a request's path is one the
// peer proxy middleware intercepts. Exact / prefix list mirrors
// the routes policy.go grants to trusted RolePeer signers.
func isPeerProxyPath(p string) bool {
	if strings.HasPrefix(p, "/api/v1/sessions/") || p == "/api/v1/sessions" {
		return true
	}
	switch p {
	case "/api/v1/info", "/api/v1/dirs", "/api/v1/upload",
		"/api/v1/files", "/api/v1/files/view", "/api/v1/files/raw",
		"/api/v1/git/status", "/api/v1/git/log", "/api/v1/git/diff", "/api/v1/git/exec":
		return true
	}
	return false
}

// isRawProxyPath returns true when the request reads bulk body
// data (raw file, view, download). Kept around so callers that
// want to distinguish "API ping" from "stream a body" still can,
// but every proxied request now uses the no-timeout HTTP client
// — a multi-GiB upload over a slow tailnet can't fit in any
// sensible fixed budget, and the caller's request context
// already cancels the dispatch.
func isRawProxyPath(p string) bool {
	switch p {
	case "/api/v1/files/raw", "/api/v1/files/view":
		return true
	}
	return false
}

func (s *Server) proxySessionRequest(w http.ResponseWriter, r *http.Request, peerID string) {
	if s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"peer routing not available on this host")
		return
	}
	rec, err := s.agents.Store().GetPeer(r.Context(), peerID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"target peer not in registry: "+err.Error())
		return
	}
	addr, err := peer.NormalizeAddress(rec.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"target peer has no usable dial address: "+err.Error())
		return
	}
	// Strip `peer=` from the forwarded query so the peer's local
	// handler doesn't see it (otherwise its own peer-routing
	// middleware would be a no-op — fine — but the query value
	// would leak into the session info echoed back).
	//
	// Also strip `?token=`: extractBearer in internal/auth admits
	// it as a fallback Owner-token source on GET/HEAD requests so
	// `<img src>` / `<a href>` style attachment links work. We
	// MUST NOT forward that token to the peer — the receiver's
	// PeerAuth stamps RolePeer first so the token is ignored on
	// the privileged path, but it would still land in the peer's
	// HTTP access logs (a credential leak across a trust
	// boundary). Drop it before re-encoding the query.
	q := r.URL.Query()
	q.Del("peer")
	q.Del("token")
	rawQuery := q.Encode()
	target := addr + r.URL.Path
	if rawQuery != "" {
		target += "?" + rawQuery
	}

	// Peer-auth no longer hashes the body, so there is no
	// signing-budget reason to cap the proxy stream. Forward r.Body
	// straight through; the local upload / blob handlers downstream
	// apply their own per-route MaxBytesReader. ContentLength is
	// copied so the receiver gets the same framing the client sent.
	ctx := r.Context()
	proxyReq, err := http.NewRequestWithContext(ctx, r.Method, target, r.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "proxy_build", err.Error())
		return
	}
	proxyReq.ContentLength = r.ContentLength
	for _, h := range []string{"Content-Type", "If-Match", "Idempotency-Key"} {
		if v := r.Header.Get(h); v != "" {
			proxyReq.Header.Set(h, v)
		}
	}

	if err := peer.AuthorizeOutbound(proxyReq.Context(), s.agents.Store(), proxyReq, peerID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "authorize: "+err.Error())
		return
	}
	// No HTTP client timeout: large uploads (multi-GiB blobs,
	// attachments) easily blow past any fixed budget. The
	// request context (request-scoped, cancelled when the
	// client disconnects) provides the upper bound.
	client := peer.NoKeepAliveHTTPClient(0)
	resp, err := client.Do(proxyReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway", "peer unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	// Preserve every header a browser would key off — raw file
	// downloads need Content-Disposition + Content-Length to land
	// as proper saves rather than inline string blobs, and the
	// existing Content-Type / ETag entries stay for JSON
	// responses. Stream the body instead of buffering it so big
	// downloads aren't silently truncated at an arbitrary cap.
	for _, k := range []string{
		"Content-Type", "ETag",
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
