package peer

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// HostFromRegistryName extracts the case-folded hostname portion of
// a peer_registry.name value, stripping any scheme prefix and any
// :port suffix. Used by NameMatches so a single Tailscale machine
// name typed by a human ("bravo") matches the registry row
// regardless of whether the operator stamped it as "bravo",
// "bravo:8080", "http://bravo:8080", "bravo.tailnet.ts.net", or
// "https://bravo.tailnet.ts.net:8443". Returns "" when the input
// is empty or unparseable; callers treat that as "no match".
func HostFromRegistryName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if i := strings.Index(name, "://"); i >= 0 {
		name = name[i+3:]
	}
	// Drop a path tail an operator might have included by mistake.
	if i := strings.IndexByte(name, '/'); i >= 0 {
		name = name[:i]
	}
	// Use net.SplitHostPort so unbracketed IPv6 literals (which
	// contain multiple colons) aren't truncated at the wrong
	// colon. SplitHostPort fails on a plain hostname with no
	// port — fall through with the bare host in that case.
	if h, _, err := net.SplitHostPort(name); err == nil {
		name = h
	} else if strings.HasPrefix(name, "[") {
		// Bracketed literal without :port: strip the brackets
		// so the returned form is bare ("::1" rather than
		// "[::1]") — symmetric with the SplitHostPort branch
		// above.
		if end := strings.IndexByte(name, ']'); end > 0 {
			name = name[1:end]
		}
	}
	// Note: a bare unbracketed IPv6 literal ("::1") passes through
	// untouched — SplitHostPort refuses it (it'd interpret the
	// last `:1` as a port), and net.ParseIP accepts it
	// downstream as the IP literal it really is. NameMatches'
	// IP-literal branch then keeps the exact-match-only semantics.
	return strings.ToLower(strings.TrimSpace(name))
}

// NameMatches returns true when input (a user-supplied identifier
// like "bravo" or "bravo.tailnet.ts.net") matches the given
// peer_registry.name. Match rules:
//
//   - case-insensitive
//   - input must equal the registry name's hostname (port + scheme
//     stripped), OR
//   - input must equal the leftmost DNS label of the registry
//     name's hostname (so "bravo" matches a row whose host is
//     "bravo.tailnet.ts.net").
//
// IP literal rows (v4 or bracketed v6) match only on exact host
// equality — there's no "leftmost label" to peel.
func NameMatches(rowName, input string) bool {
	host := HostFromRegistryName(rowName)
	if host == "" {
		return false
	}
	// Normalise the input through the same parser so a caller
	// who passed "http://bravo:8080" or "bravo.tailnet:8443"
	// matches a registry row stored as a bare host.
	normalizedInput := HostFromRegistryName(input)
	if normalizedInput == "" {
		return false
	}
	if host == normalizedInput {
		return true
	}
	// Leftmost-label fallback only for DNS-looking hostnames —
	// skip IP literals (a v4 like "100.64.0.42" has dots but
	// its first octet isn't a hostname label; v6 has colons
	// which aren't labels at all).
	if net.ParseIP(host) != nil {
		return false
	}
	if dot := strings.IndexByte(host, '.'); dot > 0 {
		if strings.ToLower(host[:dot]) == normalizedInput {
			return true
		}
	}
	return false
}

// NormalizeAddress turns a peer_registry.name value into a base URL
// that can be dialed (Subscriber WS, blob-pull GET, switch-device
// POST, ...). Accepted forms:
//
//   - "host:port" (no scheme) — historical Hub-side row stamped by
//     refreshPublicNameFromTailscale; resolved to "https://host:port"
//     because tsnet listens on TLS.
//   - "http://host:port" / "https://host:port" — daemon-only peers
//     (`kojo --peer`) emit the http form; the Hub may also write
//     explicit https for FQDN aliases. Case-insensitive on the
//     scheme so url.Parse's RFC-3986 lowering doesn't trip us.
//
// Anything else (other scheme, missing host, unparseable) returns
// a non-nil error; callers typically log and skip the row.
//
// The returned URL has no path / query / fragment — only
// `scheme://host:port`. Callers append their own path segment.
func NormalizeAddress(rawName string) (string, error) {
	name := strings.TrimSpace(rawName)
	if name == "" {
		return "", errors.New("empty name")
	}
	probe := name
	if !strings.Contains(name, "://") {
		// Bare IPv6 literals need brackets — without them url.Parse
		// would treat the last `:NN` segment as a port and produce
		// an invalid Host (`https://::1` → Host=":", Path="1"). If
		// the row stored "::1" or "fe80::1", emit "https://[::1]"
		// so callers can dial it. Strings that look like
		// "host:port" or "[host]:port" already parse cleanly.
		if ip := net.ParseIP(name); ip != nil && ip.To4() == nil {
			probe = "https://[" + name + "]"
		} else {
			probe = "https://" + name
		}
	}
	u, err := url.Parse(probe)
	if err != nil {
		return "", fmt.Errorf("unparseable: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q (want http or https)", scheme)
	}
	if u.Host == "" {
		return "", errors.New("missing host")
	}
	return scheme + "://" + u.Host, nil
}
