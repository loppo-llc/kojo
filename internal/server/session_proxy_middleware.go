package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

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

const sessionProxyTimeout = 30 * time.Second

// sessionProxyRawTimeout bounds raw / download proxies. Large
// file fetches (raw, view of a big log, attachment download)
// cannot fit in the JSON-API budget — a multi-MB pull over a
// slow tailnet would otherwise hit the 30s ceiling mid-stream.
// 5 minutes mirrors the orchestrator's switch-device window.
const sessionProxyRawTimeout = 5 * time.Minute

// isRawProxyPath returns true when the request reads bulk body
// data (raw file, view, download). These paths get the
// sessionProxyRawTimeout client; everything else uses the
// short sessionProxyTimeout so a stuck JSON write doesn't pin
// a peer connection for minutes.
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

	// Body cap chosen to fit the receiver's PeerAuth budget: the
	// signing middleware buffers the body to hash it (16 MiB cap
	// in internal/peer.AuthMaxBodyBytes). We stay under that with
	// a small margin so legitimate uploads land. The local upload
	// handler accepts up to 20 MiB; cross-peer uploads are
	// effectively capped lower because we cannot raise PeerAuth's
	// hash budget without weakening the body-tamper guarantee.
	// Surface the difference explicitly in the 413 message so the
	// user sees why a file that uploads locally fails over the
	// peer route.
	const maxBody = 15 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		writeError(w, http.StatusBadGateway, "body_read",
			"failed to buffer request body: "+err.Error())
		return
	}
	if int64(len(body)) > maxBody {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large",
			"request body exceeds 15 MiB cross-peer upload cap (local upload cap is 20 MiB; the cross-peer limit is tied to the receiver's signed-body hash budget)")
		return
	}

	ctx := r.Context()
	proxyReq, err := http.NewRequestWithContext(ctx, r.Method, target, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "proxy_build", err.Error())
		return
	}
	for _, h := range []string{"Content-Type", "If-Match", "Idempotency-Key"} {
		if v := r.Header.Get(h); v != "" {
			proxyReq.Header.Set(h, v)
		}
	}
	nonce, err := peer.MakeNonce()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "nonce: "+err.Error())
		return
	}
	if err := peer.SignRequest(proxyReq, s.peerID.DeviceID, s.peerID.PrivateKey, nonce, peerID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "sign: "+err.Error())
		return
	}
	// Raw / view paths can carry multi-MB bodies; a 30s ceiling
	// would clip them mid-stream over a slow tailnet. JSON ops
	// stay on the short timeout so a stuck handler doesn't pin
	// a peer connection for minutes.
	clientTimeout := sessionProxyTimeout
	if isRawProxyPath(r.URL.Path) {
		clientTimeout = sessionProxyRawTimeout
	}
	client := peer.NoKeepAliveHTTPClient(clientTimeout)
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
