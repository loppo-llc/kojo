package server

import (
	"net/http"
	"strings"
)

// apiNoStoreDefaultMiddleware seeds Cache-Control: no-store on every
// /api/v1/* response before the handler runs. Handlers that want a
// different policy (TTS uses private+immutable, blob serves cache
// directives from the underlying store) overwrite the header via
// w.Header().Set("Cache-Control", …) — net/http hands the same
// header.Header map to the wrapped writer, so the last Set wins.
//
// Rationale: the API used to ship NO Cache-Control header. RFC 9111
// heuristic freshness then let browsers cache responses — including
// 403s — for an arbitrary duration tied to Date. A device that hit
// an early-2b8d0ad / pre-d4fdd8b 403 had it pinned in disk cache
// well past the deploy that fixed the underlying middleware,
// surfacing as "agents tab empty on this one Windows machine"
// (desktop-tvt 100.117.138.20). Clearing site data fixed the
// symptom but no-store at the source prevents the next regression
// from getting frozen into a stranger's cache.
//
// Limited to /api/v1/*: the static SPA bundle has its own immutable
// hash-named asset policy in registerStaticFiles. /api/v1/webdav is
// skipped — golang.org/x/net/webdav.Handler does not set
// Cache-Control on its own, so seeding no-store there would force
// the WebDAV client to refetch every byte on every PROPFIND/GET
// (Finder, davfs2, etc. follow whatever directive the server
// returns), wrecking the share's UX. The webdav surface is mounted
// behind webdavGate and is logged in / scoped enough that its
// default cache behaviour is acceptable.
func apiNoStoreDefaultMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/") &&
			!strings.HasPrefix(r.URL.Path, "/api/v1/webdav/") &&
			r.URL.Path != "/api/v1/webdav" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
