package main

import (
	"testing"

	"github.com/loppo-llc/kojo/internal/peer"
)

// TestNormalizeSubscriberAddress pins the wire convention for
// peer_registry.name → dial URL. The function lives in
// internal/peer now (peer.NormalizeAddress) so the switch-device
// orchestrator and the blob-pull client can share it; the test
// stays in cmd/kojo because it documents the cluster-level
// expectation the operator stamps via `--peer-add`.
func TestNormalizeSubscriberAddress(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{
			name:   "scheme-less host:port resolves to https (legacy Hub row)",
			in:     "kojo.tailnet.ts.net:8080",
			want:   "https://kojo.tailnet.ts.net:8080",
			wantOK: true,
		},
		{
			name:   "explicit http preserved (peer-mode row)",
			in:     "http://100.64.0.42:8080",
			want:   "http://100.64.0.42:8080",
			wantOK: true,
		},
		{
			name:   "explicit https preserved",
			in:     "https://alpha.tailnet.ts.net:8443",
			want:   "https://alpha.tailnet.ts.net:8443",
			wantOK: true,
		},
		{
			name:   "uppercase scheme is canonicalized",
			in:     "HTTP://100.64.0.42:8080",
			want:   "http://100.64.0.42:8080",
			wantOK: true,
		},
		{
			name:   "ws scheme rejected (would mangle into https://ws://...)",
			in:     "ws://attacker/",
			wantOK: false,
		},
		{
			name:   "file scheme rejected",
			in:     "file:///etc/passwd",
			wantOK: false,
		},
		{
			name:   "empty string rejected",
			in:     "",
			wantOK: false,
		},
		{
			name:   "whitespace trimmed",
			in:     "  http://100.64.0.42:8080  ",
			want:   "http://100.64.0.42:8080",
			wantOK: true,
		},
		{
			name:   "fragment / path stripped",
			in:     "http://100.64.0.42:8080/admin?secret=1#x",
			want:   "http://100.64.0.42:8080",
			wantOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := peer.NormalizeAddress(tc.in)
			ok := err == nil
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (in=%q, got=%q, err=%v)",
					ok, tc.wantOK, tc.in, got, err)
			}
			if ok && got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
