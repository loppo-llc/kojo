package server

import "testing"

// Pure-function tests for the blob handler helpers. Kept out of the
// heavy_test gate (which the round-trip tests live behind) so the
// default `go test ./internal/server/` run still covers etag parsing
// and formatting.

func TestBlobETagHeaderFormat(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"sha256:abc", `"sha256:abc"`},
	}
	for _, c := range cases {
		if got := blobETagHeader(c.in); got != c.want {
			t.Errorf("blobETagHeader(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseStrongIfMatch(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"plain strong", `"sha256:abc"`, "sha256:abc", true},
		{"surrounding whitespace", `   "sha256:abc"  `, "sha256:abc", true},
		{"empty", "", "", false},
		// `*` is HTTP-meaningful ("any current version") but defeats the
		// content-hash gate — must be rejected.
		{"wildcard", "*", "", false},
		// Weak etags can still drift between bytewise-identical bodies
		// (RFC 9110 §8.8.3); for a precondition over a content hash
		// they're not safe.
		{"weak", `W/"sha256:abc"`, "", false},
		// A comma list would let a stale etag and a fresh one race —
		// refuse outright instead of guessing which one the caller meant.
		{"multi", `"sha256:a","sha256:b"`, "", false},
		{"unquoted", `sha256:abc`, "", false},
		{"only-open-quote", `"sha256:abc`, "", false},
		{"only-close-quote", `sha256:abc"`, "", false},
		// `""` is syntactically a strong etag but the empty inner
		// value would silently bypass the precondition (blob.Put
		// treats IfMatch == "" as "no check"). Refuse.
		{"empty quotes", `""`, "", false},
	}
	for _, c := range cases {
		got, ok := parseStrongIfMatch(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: parseStrongIfMatch(%q) = (%q, %v), want (%q, %v)",
				c.name, c.in, got, ok, c.want, c.ok)
		}
	}
}
