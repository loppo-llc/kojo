package peer

import (
	"context"
	"errors"
)

// ErrNoNodeKey is returned by NodeKeyResolver implementations when
// the remote address could not be mapped to a Tailscale NodeKey
// (the typical cause: the connection arrived on a non-tsnet
// listener, or tailscaled does not know the peer).
var ErrNoNodeKey = errors.New("peer: no Tailscale NodeKey for remote address")

// NodeKeyResolver maps an incoming HTTP request's remote address to
// the Tailscale stable NodeKey of the caller. The Server-side
// identity middleware uses this to find the matching peer_registry
// row.
//
// remoteAddr is the value of *http.Request.RemoteAddr (`ip:port`).
// Implementations forward to tsnet.LocalClient.WhoIs and return the
// String() form of the resolved tailcfg.Node.Key (the canonical
// `nodekey:...` representation).
//
// A nil resolver is treated as "no tsnet bound on this listener" —
// the middleware refuses every request that did not come in via a
// trusted path (e.g. --local + --unsafe).
type NodeKeyResolver func(ctx context.Context, remoteAddr string) (string, error)

// UnsafeNodeKey is the sentinel NodeKey the unsafe-mode middleware
// stamps on every request. It is NOT a real Tailscale key — the
// `unsafe:` prefix makes it distinguishable from anything tsnet
// could ever return so a row carrying it can never collide with a
// legitimate WhoIs lookup.
const UnsafeNodeKey = "unsafe:local"
