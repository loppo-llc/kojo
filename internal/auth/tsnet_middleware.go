package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// TailnetIdentityConfig configures TailnetIdentityMiddleware.
//
// Resolver maps an incoming request's RemoteAddr to the calling
// Tailscale node's stable NodeKey (`nodekey:...`). nil disables
// tsnet identity resolution — the middleware then admits every
// caller as Guest unless `Unsafe` is true.
//
// Store is the peer_registry handle the middleware queries to
// translate a NodeKey into a Principal.PeerID.
//
// SelfNodeKey, when non-empty, marks the local kojo's own
// Tailscale node. A request arriving from that key is the operator
// hitting the daemon from the same host they run kojo on — it gets
// promoted to RoleOwner to preserve the "Tailscale reach == Owner"
// UX on the public listener.
//
// Unsafe collapses every check: the middleware stamps RolePeer
// (peer-mode) or RoleOwner (hub-mode) onto every request without
// consulting tsnet or the store. Intended for `--unsafe` LAN /
// docker / CI deployments where the operator opts into trusting
// the listener boundary.
type TailnetIdentityConfig struct {
	Resolver func(ctx context.Context, remoteAddr string) (string, error)
	Store    *store.Store
	// SelfNodeKeyFunc returns the local kojo's own Tailscale
	// NodeKey at request time. Reads through a sync.RWMutex on the
	// Server so cmd/kojo can rewire the value after tsnet binds.
	// May be nil — equivalent to a function that always returns
	// "" (no self-Owner promotion).
	SelfNodeKeyFunc func() string
	// PromoteUnknownTailnetToOwner, when true, stamps RoleOwner on
	// requests whose WhoIs resolved to a NodeKey that does NOT
	// match peer_registry. This restores the legacy "Tailscale
	// reach == Owner" UX on the Hub's public listener so the
	// operator can hit the UI from any tailnet device without
	// pairing it as a peer. The peer-mode daemon leaves this
	// FALSE — a stray tailnet node that hasn't been Approved by
	// the operator should not gain privilege.
	PromoteUnknownTailnetToOwner bool
	// Unsafe collapses the entire WhoIs path. Every caller is
	// stamped RoleOwner (UnsafeAsHub=true) or RolePeer (false).
	Unsafe       bool
	UnsafeAsHub  bool // true on Hub binary (RoleOwner), false on --peer (RolePeer)
	ResolveDelay time.Duration
	Logger       *slog.Logger
}

// TailnetIdentityMiddleware authenticates inbound HTTP requests
// against the Tailscale identity of the calling node.
//
// Chain order: this middleware runs FIRST (before any per-role
// gate) so a downstream OwnerOnly or Enforce check sees the
// Principal already stamped.
//
// Resolution:
//
//  1. Unsafe → stamp RoleOwner (Hub) or RolePeer (--peer) and call
//     next. tsnet is not consulted.
//  2. Resolver(remoteAddr) → NodeKey. If the resolver errors
//     (typical: caller is on a non-tsnet listener), the request
//     falls through with Role=Guest so a downstream middleware
//     (the existing AuthMiddleware on the auth-required listener)
//     can still resolve an Authorization: Bearer.
//  3. NodeKey == SelfNodeKey → RoleOwner.
//  4. NodeKey matches peer_registry row → RolePeer.
//  5. Otherwise → Guest (a tailnet node we have not approved is
//     not yet trusted; the policy layer 403s the privileged
//     surface).
func TailnetIdentityMiddleware(cfg TailnetIdentityConfig) func(http.Handler) http.Handler {
	resolveTimeout := cfg.ResolveDelay
	if resolveTimeout <= 0 {
		resolveTimeout = 3 * time.Second
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if existing := FromContext(r.Context()); existing.Role != RoleGuest {
				// Earlier middleware already stamped a principal —
				// don't clobber.
				next.ServeHTTP(w, r)
				return
			}
			if cfg.Unsafe {
				role := RolePeer
				if cfg.UnsafeAsHub {
					role = RoleOwner
				}
				ctx := WithPrincipal(r.Context(), Principal{Role: role})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			if cfg.Resolver == nil {
				// No tsnet bound; admit as Guest and let downstream
				// auth (if any) take over.
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), resolveTimeout)
			nodeKey, err := cfg.Resolver(ctx, r.RemoteAddr)
			cancel()
			if err != nil || nodeKey == "" {
				if cfg.Logger != nil && err != nil && !errors.Is(err, context.Canceled) {
					// Warn (not Debug): a chronic resolver error
					// indicates tailscaled is down / unreachable
					// and every inbound request will fall through
					// to Guest. The operator needs to see this.
					cfg.Logger.Warn("tsnet identity: WhoIs resolution failed; caller demoted to Guest",
						"remote", r.RemoteAddr, "err", err)
				}
				next.ServeHTTP(w, r)
				return
			}
			if cfg.SelfNodeKeyFunc != nil {
				if self := cfg.SelfNodeKeyFunc(); self != "" && nodeKey == self {
					ctx := WithPrincipal(r.Context(), Principal{Role: RoleOwner})
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
			if cfg.Store == nil {
				// No registry — can't elevate to RolePeer. Stay
				// Guest. (Shouldn't happen in production; defensive
				// for test wiring that supplies a resolver without a
				// store.)
				next.ServeHTTP(w, r)
				return
			}
			lookupCtx, lookupCancel := context.WithTimeout(r.Context(), resolveTimeout)
			rec, err := cfg.Store.GetPeerByNodeKey(lookupCtx, nodeKey)
			lookupCancel()
			if err != nil {
				// Distinguish "no row" from "DB blip". Owner
				// promotion on a tailnet caller without a
				// matching peer_registry row is the legacy
				// "Tailscale reach == Owner" UX (Hub public
				// listener only). A genuine store error MUST
				// NOT silently promote — that would let a
				// flaky DB hand Owner privileges to anyone on
				// the tailnet. Fall through as Guest in that
				// case and log Warn so the operator notices.
				if errors.Is(err, store.ErrNotFound) {
					if cfg.PromoteUnknownTailnetToOwner {
						ctx := WithPrincipal(r.Context(), Principal{Role: RoleOwner})
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
					next.ServeHTTP(w, r)
					return
				}
				if cfg.Logger != nil {
					cfg.Logger.Warn("tsnet identity: peer_registry lookup failed; caller demoted to Guest",
						"remote", r.RemoteAddr, "node_key", nodeKey, "err", err)
				}
				next.ServeHTTP(w, r)
				return
			}
			// Liveness signal: a peer that just sent us a request
			// over the tailnet is observably reachable. Best-effort
			// touch — failures don't reject the request.
			touchCtx, touchCancel := context.WithTimeout(r.Context(), 2*time.Second)
			_ = cfg.Store.TouchPeer(touchCtx, rec.DeviceID, store.PeerStatusOnline, time.Now().UnixMilli())
			touchCancel()
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{
				Role:   RolePeer,
				PeerID: rec.DeviceID,
			})))
		})
	}
}
