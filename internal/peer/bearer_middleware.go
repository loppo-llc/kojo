package peer

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// BearerPeerMiddleware resolves an `Authorization: Bearer <token>` header
// against peer_tokens and stamps the request context with
// auth.Principal{Role: RolePeer}.
//
// Replaces the Ed25519 signature handshake at the network edge while still
// running in chain WITH the legacy AuthMiddleware: a request that does
// not carry a Bearer falls through, AuthMiddleware then has its usual
// shot at the five-header signature scheme, and finally the Owner / agent
// middlewares run. Once all peer→Hub callers have moved to Bearer the
// signing middleware can be deleted (docs/peer-simplify-plan.md step 9).
//
// Order in the chain: this MUST run BEFORE OwnerOnlyMiddleware and the
// regular AuthMiddleware so its principal is not clobbered. Same slot the
// Ed25519 AuthMiddleware occupies today; both are idempotent on a request
// that already carries a non-Guest principal.
//
// Trust model carry-over: the peer_registry.trusted bit still gates the
// privileged surface (sessions / ws / info / dirs / files / git / upload)
// inside policy.go. BearerPeerMiddleware looks up the peer_registry row
// for the token's device_id and stamps PeerTrusted accordingly, so the
// downstream policy sees the same shape it did under signing.
type BearerPeerMiddleware struct {
	store *store.Store
	// selfDeviceID, when non-empty, refuses a Bearer that claims the
	// local peer's identity. Same self-loopback guard the Ed25519
	// middleware carries — even though it is largely defensive (a
	// peer that holds its OWN Bearer is by definition this very
	// process, not a remote impersonator), the explicit reject keeps
	// the test surface symmetric across both middlewares.
	selfDeviceID string

	// now is the clock the (currently unused) freshness check would
	// consume. Kept on the struct so a future enforcement of
	// peer_tokens.created_at expiry can land without re-plumbing.
	now func() time.Time
}

// NewBearerPeerMiddleware wires the dependencies. selfDeviceID may be
// empty in test fixtures; production wires it to the local peer's
// device_id so a leaked peer→Hub token can not be replayed against the
// local Hub as a self-Bearer.
func NewBearerPeerMiddleware(st *store.Store, selfDeviceID string) *BearerPeerMiddleware {
	return &BearerPeerMiddleware{
		store:        st,
		selfDeviceID: selfDeviceID,
		now:          time.Now,
	}
}

// Wrap returns a handler that runs the Bearer check before delegating to
// next. Absent or malformed Authorization headers pass through to keep
// non-peer auth paths (Owner cookie, agent X-Kojo-Token, WebDAV Basic)
// undisturbed.
func (m *BearerPeerMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := extractBearer(r.Header.Get("Authorization"))
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		tok, err := m.store.ResolvePeerToken(ctx, raw)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Either no such token, or the row exists but is
				// revoked. The store maps both to ErrNotFound; we
				// keep them indistinguishable on the wire as well
				// so a probing attacker can't enumerate revoked
				// device_ids.
				next.ServeHTTP(w, r)
				return
			}
			// A genuine store error (DB unavailable, schema drift)
			// gets logged-by-handler-not-here and the request falls
			// through to whatever downstream auth applies. The
			// callers that REQUIRE peer auth land at 403 in policy.
			next.ServeHTTP(w, r)
			return
		}
		if m.selfDeviceID != "" && tok.DeviceID == m.selfDeviceID {
			// A request claiming the local peer's identity via
			// Bearer is by construction a replay; refuse rather
			// than admit and let policy sort it out.
			next.ServeHTTP(w, r)
			return
		}
		// Pull the peer_registry row so the policy layer can see the
		// trusted bit. A missing row is fatal — the token is bound to
		// a device_id that no longer has a registry entry; treat as
		// unauthenticated.
		rec, err := m.store.GetPeer(ctx, tok.DeviceID)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		// Bearer auth IS a liveness signal: this peer just sent us a
		// valid Bearer, so its registry row is observably reachable.
		// Best-effort touch — a failed update doesn't reject the
		// request (OfflineSweeper catches stale rows on its tick).
		touchCtx, touchCancel := context.WithTimeout(r.Context(), 2*time.Second)
		_ = m.store.TouchPeer(touchCtx, rec.DeviceID, store.PeerStatusOnline, m.now().UnixMilli())
		touchCancel()

		next.ServeHTTP(w, r.WithContext(
			auth.WithPrincipal(r.Context(), auth.Principal{
				Role:        auth.RolePeer,
				PeerID:      rec.DeviceID,
				PeerTrusted: rec.Trusted,
			}),
		))
	})
}

// extractBearer pulls the raw token out of an Authorization header value
// of the form "Bearer <token>". The check is case-insensitive on the
// scheme; the token portion is taken verbatim (whitespace-trimmed) so a
// future base64 token containing `+` / `/` survives the round-trip.
// Returns ok=false for any other shape — including X-Kojo-Token bearers
// which the agent layer uses and we MUST NOT consume here.
func extractBearer(h string) (string, bool) {
	if h == "" {
		return "", false
	}
	const prefix = "bearer "
	if len(h) <= len(prefix) {
		return "", false
	}
	if !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	raw := strings.TrimSpace(h[len(prefix):])
	if raw == "" {
		return "", false
	}
	return raw, true
}
