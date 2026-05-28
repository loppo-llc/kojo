package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// ErrNodeKeyResolverNotReady is returned by a TailnetIdentityConfig.Resolver
// to signal "the tsnet/tailscaled handle is not wired yet" — distinct
// from a real WhoIs failure or a genuinely unknown caller. The
// middleware uses it to refuse Hub-mode Owner-fallback during the
// brief window between server start and resolver wiring; a real
// `lc.WhoIs` error (tailscaled blip) or empty `w.Node` (tsnet view
// stale) still falls back to Owner so the operator's Dashboard
// keeps working through transient outages.
//
// Callers that build the resolver closure should return ("",
// ErrNodeKeyResolverNotReady) when the underlying handle is nil
// instead of returning ("", nil) — the latter is reserved for
// "WhoIs returned no node" and is honored by Hub fallback.
var ErrNodeKeyResolverNotReady = errors.New("auth: tsnet node key resolver not ready")

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
	// every tailnet caller that reaches the listener — both
	// WhoIs-resolved callers and callers whose WhoIs is transiently
	// unresolved (resolver error, empty Node, tsnet view lag). The
	// listener boundary itself (tsnet.ListenTLS) is the trust gate,
	// so a transient WhoIs blip never demotes the operator. The one
	// exception is ErrNodeKeyResolverNotReady — startup before
	// SetNodeKeyResolver wires the closure — which stays Guest so
	// "no resolver by design" cannot turn into Owner-by-default.
	// peer_registry is NOT consulted on the Hub's public listener:
	// that listener is
	// exclusively for the operator's UI — paired peer devices
	// opening the Hub UI in a browser also arrive here, and there
	// is no UX-meaningful distinction between "operator on the Hub
	// host" and "operator on a paired tailnet device."
	//
	// The peer-mode daemon leaves this FALSE so it can still
	// classify a tailnet caller as RolePeer via peer_registry for
	// the §3.7 device-switch surface (Server.ServeAuthTsnet wires
	// this middleware ahead of AuthMiddleware on the peer's
	// primary tsnet listener). A stray tailnet node that hasn't
	// been Approved stays Guest and falls through to the Bearer
	// gate.
	PromoteUnknownTailnetToOwner bool
	// Unsafe collapses the entire WhoIs path. Every caller is
	// stamped RoleOwner (UnsafeAsHub=true) or RolePeer (false).
	Unsafe       bool
	UnsafeAsHub  bool // true on Hub binary (RoleOwner), false on --peer (RolePeer)
	ResolveDelay time.Duration
	Logger       *slog.Logger
	// SelfDeviceID, when non-empty, demotes a peer_registry hit
	// whose device_id matches this value back to Guest. The
	// self-row carries the local NodeKey so other peers can dial
	// us; without this check, a request looped back through tsnet
	// (e.g. operator hits the peer's tailnet IP from the same
	// host) would resolve to the self-row and be stamped RolePeer,
	// silently bypassing the Bearer gate downstream. Empty
	// disables the check (Hub public listener doesn't need it
	// since PromoteUnknownTailnetToOwner short-circuits ahead of
	// the lookup).
	SelfDeviceID string
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
//  2. Resolver(remoteAddr) → NodeKey. If the resolver errors OR
//     returns an empty NodeKey (tsnet's view of the tailnet hasn't
//     refreshed for this caller yet — typical for a just-joined
//     node or one whose key was recently rotated), the request:
//     - on Hub mode (PromoteUnknownTailnetToOwner=true) → Owner,
//     UNLESS the error wraps ErrNodeKeyResolverNotReady. The
//     tsnet listener is only reachable over Tailscale, so the
//     listener boundary itself is the trust gate; a transient
//     WhoIs blip must not 403 the operator's UI. The
//     ErrNodeKeyResolverNotReady exception covers the startup
//     window where SetNodeKeyResolver has not run yet — an
//     unidentified caller there has no claim to Owner.
//     - on Peer mode (false) → falls through as Guest so a
//     downstream middleware (the AuthMiddleware on the
//     auth-required listener) can resolve a Bearer instead.
//  3. NodeKey == SelfNodeKey → RoleOwner.
//  4. PromoteUnknownTailnetToOwner true (Hub public listener) →
//     RoleOwner. peer_registry is NOT consulted for the role; it
//     is touched async as a liveness side-effect when the NodeKey
//     matches a registered peer.
//  5. PromoteUnknownTailnetToOwner false (peer-mode public
//     listener) → consult peer_registry. Hit ⇒ RolePeer + sync
//     TouchPeer. Miss / DB error ⇒ Guest (unapproved tailnet
//     callers stay Guest; the policy layer 403s privileged
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
				notReady := errors.Is(err, ErrNodeKeyResolverNotReady)
				if cfg.Logger != nil && err != nil &&
					!errors.Is(err, context.Canceled) && !notReady {
					// Warn (not Debug): a chronic resolver error
					// indicates tailscaled is down / unreachable
					// and inbound requests fall back to whatever
					// the Hub-fallback policy dictates. The
					// operator needs to see this.
					cfg.Logger.Warn("tsnet identity: WhoIs resolution failed",
						"remote", r.RemoteAddr, "err", err,
						"hub_fallback", cfg.PromoteUnknownTailnetToOwner)
				} else if cfg.Logger != nil && err == nil && nodeKey == "" {
					// err==nil but WhoIs returned no node — typically
					// tsnet's view of the tailnet has not refreshed
					// for this caller yet (just-joined node, recent
					// rekey, exit-node-routed source, …). Log at Debug
					// so a transient blip doesn't spam, but the
					// operator can still surface it with --dev.
					cfg.Logger.Debug("tsnet identity: WhoIs returned empty node",
						"remote", r.RemoteAddr,
						"hub_fallback", cfg.PromoteUnknownTailnetToOwner)
				} else if cfg.Logger != nil && notReady {
					// Resolver not wired yet — startup race between
					// http listener and SetNodeKeyResolver. Stay
					// Guest regardless of Hub fallback so an
					// unidentified caller in this window never
					// gains Owner by default. The condition
					// resolves the moment cmd/kojo finishes wiring.
					cfg.Logger.Debug("tsnet identity: resolver not ready",
						"remote", r.RemoteAddr)
				}
				// Hub public listener: the listener itself is only
				// reachable over Tailscale (tsnet.ListenTLS), so a
				// caller arriving here has already passed the tailnet
				// boundary regardless of whether WhoIs could attach a
				// NodeKey to them. Trust the listener and promote to
				// Owner so a transient WhoIs blip (just-joined node,
				// recent rekey, tsnet view lag) doesn't 403 the
				// operator's Dashboard.
				//
				// Two exceptions stay Guest:
				//   - notReady (resolver closure's underlying handle
				//     is nil): startup race, refuse Owner-by-default.
				//   - Peer mode (PromoteUnknownTailnetToOwner=false):
				//     an unidentified tailnet caller has no claim to
				//     RolePeer.
				if cfg.PromoteUnknownTailnetToOwner && !notReady {
					ctx := WithPrincipal(r.Context(), Principal{Role: RoleOwner})
					next.ServeHTTP(w, r.WithContext(ctx))
					return
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
			// Hub public listener: every WhoIs-resolved tailnet
			// caller is trusted as Owner. peer_registry is NOT
			// consulted for the principal decision — paired peer
			// devices opening the Hub UI in a browser are the
			// operator's other devices and must receive the same
			// view as the on-host Owner. Inter-peer agent-sync
			// requests that land on the Hub's tsnet listener are
			// likewise trusted as Owner here (operator-controlled
			// tailnet only); on the peer-mode daemon the same
			// inter-peer traffic lands on ServeAuthTsnet, which
			// runs THIS middleware with PromoteUnknownTailnetToOwner=
			// false so peer_registry hit ⇒ RolePeer for the §3.7
			// surface.
			//
			// peer_registry is also touched and the matched DeviceID
			// stamped onto the Principal so downstream handlers (e.g.
			// /api/v1/peers/events) can identify which paired peer is
			// on the wire and refresh last_seen periodically without
			// waiting for the next inbound request. Lookup is sync
			// (same cadence as the peer-mode branch below); a miss
			// or DB blip leaves PeerID empty but keeps the Owner
			// stamp so operator UX is unaffected.
			if cfg.PromoteUnknownTailnetToOwner {
				// The Hub public listener runs this resolution on
				// EVERY tailnet-routed API call (Dashboard polling,
				// agent chat, file list, …). Use a tight 500ms cap
				// so a DB-lock spike on the registry never stretches
				// an operator UI request — the lookup is purely
				// best-effort liveness/identity. A miss or timeout
				// leaves PeerID empty + last_seen unchanged; the
				// operator UI still renders (Owner stamp unaffected)
				// and the next request retries the lookup. Liveness
				// downstream falls back to /api/v1/peers/events's
				// own periodic touch + the sweeper's presence path.
				const hubLookupTimeout = 500 * time.Millisecond
				const hubTouchTimeout = 500 * time.Millisecond
				var peerID string
				if cfg.Store != nil {
					lookupCtx, lookupCancel := context.WithTimeout(r.Context(), hubLookupTimeout)
					rec, err := cfg.Store.GetPeerByNodeKey(lookupCtx, nodeKey)
					lookupCancel()
					switch {
					case err == nil && rec != nil:
						peerID = rec.DeviceID
						touchCtx, touchCancel := context.WithTimeout(r.Context(), hubTouchTimeout)
						_ = cfg.Store.TouchPeer(touchCtx, rec.DeviceID, store.PeerStatusOnline, time.Now().UnixMilli())
						touchCancel()
					case err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled):
						if cfg.Logger != nil {
							cfg.Logger.Warn("tsnet identity: hub liveness lookup failed",
								"node_key", nodeKey, "err", err)
						}
					}
				}
				ctx := WithPrincipal(r.Context(), Principal{Role: RoleOwner, PeerID: peerID})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
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
				// Reached only when PromoteUnknownTailnetToOwner
				// is false (peer-mode public listener). No row →
				// stray tailnet caller, stay Guest. DB blip → log
				// Warn and also fall through as Guest so a flaky
				// store can never silently promote.
				if errors.Is(err, store.ErrNotFound) {
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
			// Self-loop demotion: a request looped back through
			// tsnet onto our own listener resolves to the self-row
			// (the peer_registry self-row carries the local
			// NodeKey so others can dial us). Stamping that as
			// RolePeer would silently bypass the Bearer gate
			// downstream. The Resolver-level self check above
			// catches this when currentSelfNodeKey is already
			// populated; this DeviceID-level guard closes the
			// startup race where tsnet has bound but the self
			// NodeKey capture goroutine hasn't published yet.
			if cfg.SelfDeviceID != "" && rec.DeviceID == cfg.SelfDeviceID {
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
