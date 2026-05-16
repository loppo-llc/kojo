package server

import (
	"encoding/base64"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/configdir"
	kjwebdav "github.com/loppo-llc/kojo/internal/webdav"
)

// webdavMountPrefix is the URL path prefix the WebDAV handler
// strips before resolving names against the on-disk root. Must
// match the registered route prefix.
const webdavMountPrefix = "/api/v1/webdav"

// webdavRootDir returns the on-disk directory the WebDAV view is
// rooted at. <configdir>/webdav/. Created on demand by the
// internal/webdav package.
func webdavRootDir() string {
	return filepath.Join(configdir.Path(), "webdav")
}

// buildWebDAVHandler constructs the kojo-flavored WebDAV handler.
// Returns (nil, nil) when the build conditions aren't met (e.g. no
// configdir resolved); a non-nil error means the build itself
// failed and the caller should log + skip the mount.
func (s *Server) buildWebDAVHandler() (http.Handler, error) {
	root := webdavRootDir()
	return kjwebdav.Handler(kjwebdav.MountConfig{
		Root:   root,
		Prefix: webdavMountPrefix,
		Logger: s.logger,
	})
}

// webdavGate authenticates a WebDAV request and forwards it to the
// underlying handler when authorized. It accepts three credentials:
//
//   1. The Owner principal already stamped into ctx by the public
//      listener's OwnerOnlyMiddleware. Trusting it preserves the
//      "Tailscale reach == Owner" UX so a browser-driven WebDAV mount
//      keeps working.
//   2. The RoleWebDAV principal stamped by the auth listener's
//      AuthMiddleware after the resolver matched the presented
//      Bearer/X-Kojo-Token against the WebDAVTokenStore.
//   3. A short-lived WebDAV token presented via HTTP Basic auth — the
//      only form most OS WebDAV clients (Finder, Explorer, Files)
//      know how to send. The username is ignored; the password is
//      verified against the WebDAVTokenStore.
//
// On failure the gate emits 401 with a `WWW-Authenticate: Basic`
// challenge so OS mount clients re-prompt the user instead of
// silently failing. The realm is per-deploy ("kojo webdav") and
// carries no secrets.
//
// Agents NEVER reach this gate: RolePrivAgent / RoleAgent both
// resolve to "no Owner principal, no WebDAV token" and 401 falls
// through. Their access path is the native blob API, not WebDAV.
func (s *Server) webdavGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := auth.FromContext(r.Context())
		if p.IsOwner() || p.IsWebDAV() {
			next.ServeHTTP(w, r)
			return
		}
		// Fallback: try Basic auth. The OwnerOnlyMiddleware on the
		// public listener stamps every request as Owner up-front, so
		// this branch only runs on the auth listener (where the
		// AuthMiddleware resolver already had a shot at Bearer /
		// X-Kojo-Token). Treat the Basic password as a candidate
		// WebDAV token; the resolver path has the same effect for
		// Bearer-presenting clients.
		if tok, ok := extractBasicWebDAVToken(r); ok {
			if s.webdavTokens != nil && s.webdavTokens.Verify(tok) {
				ctx := auth.WithPrincipal(r.Context(), auth.Principal{Role: auth.RoleWebDAV})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		// 401 with a Basic challenge so OS mount clients re-prompt
		// instead of silently failing. The Owner gate already 403s
		// on the public listener via OwnerOnlyMiddleware, so the
		// 401 here is only reachable on the auth listener (or with
		// a stripped principal in tests).
		w.Header().Set("WWW-Authenticate", `Basic realm="kojo webdav", charset="UTF-8"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "webdav requires owner credentials or a webdav-scoped token")
	})
}

// extractBasicWebDAVToken returns the password portion of an HTTP
// Basic credential. The username is intentionally ignored — WebDAV
// clients let the user pick any username, and our token is the
// credential. Returns ("", false) when no Basic header is present
// or the header is malformed.
func extractBasicWebDAVToken(r *http.Request) (string, bool) {
	const basicPrefix = "Basic "
	h := r.Header.Get("Authorization")
	if h == "" || !strings.HasPrefix(h, basicPrefix) {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(basicPrefix):]))
	if err != nil {
		return "", false
	}
	// "user:pass" — split on first colon. An empty password is not
	// a valid token, so reject it eagerly.
	colon := strings.IndexByte(string(raw), ':')
	if colon < 0 || colon == len(raw)-1 {
		return "", false
	}
	return string(raw[colon+1:]), true
}
