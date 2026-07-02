package peer

import "testing"

// TestCanonicalHubURL covers the address shapes resolveHubURL feeds
// canonicalHubURL (via --hub / KOJO_HUB_URL) after delegating to
// NormalizeAddress. The bare-IPv6 case is the regression the delegation
// fixed: the previous hand-rolled version prepended "https://" without
// bracketing, mangling "::1" into an unparseable host.
func TestCanonicalHubURL(t *testing.T) {
	d := &Discovery{}
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"host:port no scheme", "kojo.tailnet.ts.net:8080", "https://kojo.tailnet.ts.net:8080", false},
		{"bare host no port", "kojo.tailnet.ts.net", "https://kojo.tailnet.ts.net", false},
		{"explicit https", "https://host:8443", "https://host:8443", false},
		{"explicit http", "http://100.64.0.5:8080", "http://100.64.0.5:8080", false},
		{"strips path", "https://host:8080/api/v1", "https://host:8080", false},
		{"bare ipv6 loopback", "::1", "https://[::1]", false},
		{"bare ipv6 link-local", "fe80::1", "https://[fe80::1]", false},
		{"bracketed ipv6 with port", "[::1]:8080", "https://[::1]:8080", false},
		{"unsupported scheme", "ftp://host:21", "", true},
		{"empty", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := d.canonicalHubURL(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("canonicalHubURL(%q) = %q, want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("canonicalHubURL(%q) unexpected error: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("canonicalHubURL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
