package main

import "testing"

func TestIsPeerRegistryDialAddress(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// Hub (tsnet) shape: scheme-less host:port.
		{"tsnet FQDN with port", "kojo.tailnet.ts.net:8080", true},
		{"tsnet FQDN with default port", "kojo.tailnet.ts.net:443", true},
		// Peer (--peer) shape: explicit http scheme.
		{"peer http with Tailscale IPv4", "http://100.64.0.42:8080", true},
		{"peer http with hostname:port", "http://bravo:8080", true},
		// Operator-stamped https with port.
		{"https with port", "https://alpha.tailnet.ts.net:8443", true},

		// Bare hostnames (no port) — the failure mode that put
		// users in offline-loop territory.
		{"bare hostname (OS hostname)", "TVT-DEV-0000", false},
		{"bare FQDN no port", "macbook-bravo.local", false},
		{"host with trailing colon, no port", "host:", false},

		// Wrong scheme.
		{"ws scheme", "ws://host:8080", false},
		{"file scheme", "file:///etc/passwd", false},
		// Cruft past the authority.
		{"path component", "http://host:8080/admin", false},
		{"query string", "http://host:8080?x=1", false},
		{"fragment", "http://host:8080#frag", false},

		// Empty / nonsense.
		{"empty", "", false},
		{"just whitespace", "   ", false},
		{"junk", "not a url", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPeerRegistryDialAddress(tc.in); got != tc.want {
				t.Errorf("isPeerRegistryDialAddress(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
