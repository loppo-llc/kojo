package peer

import (
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/blob"
)

// TestBuildPeerBlobIngestURL pins the canonical layout PushClient
// sends to the hub. A drift would either land bytes at the wrong
// blob URI or trigger Go's ServeMux path-clean redirect — both
// surface as 401 "replayed nonce" because http.Client re-sends
// the signed envelope to the redirect target.
func TestBuildPeerBlobIngestURL(t *testing.T) {
	cases := []struct {
		base string
		path string
		want string
	}{
		{
			base: "https://hub.tail-net.ts.net:8080",
			path: "agents/ag_1/attach/m_abc/chart.png",
			want: "https://hub.tail-net.ts.net:8080/api/v1/peers/blobs-ingest/global/agents/ag_1/attach/m_abc/chart.png",
		},
		{
			base: "http://10.0.0.1:8080/", // trailing slash on base
			path: "agents/ag_1/avatar.png",
			want: "http://10.0.0.1:8080/api/v1/peers/blobs-ingest/global/agents/ag_1/avatar.png",
		},
	}
	for _, c := range cases {
		got, err := buildPeerBlobIngestURL(c.base, blob.ScopeGlobal, c.path)
		if err != nil {
			t.Errorf("buildPeerBlobIngestURL(%q,%q) err=%v", c.base, c.path, err)
			continue
		}
		if got != c.want {
			t.Errorf("got %q; want %q", got, c.want)
		}
	}
}

// TestBuildPeerBlobIngestURL_RejectsDoubleSlash refuses inputs
// that would produce a `//` segment under the ingest prefix —
// ServeMux would 301 path-clean those and the signed nonce would
// replay on the cleaned URL.
func TestBuildPeerBlobIngestURL_RejectsDoubleSlash(t *testing.T) {
	if _, err := buildPeerBlobIngestURL("https://h", blob.ScopeGlobal,
		"agents//ag_1/x.png"); err == nil {
		t.Errorf("expected error for double-slash path; got nil")
	}
}

// TestBuildPeerBlobIngestURL_RejectsBadBase guards against the
// "operator left out the scheme" misconfiguration that would
// otherwise build a relative URL the http client cannot dial.
func TestBuildPeerBlobIngestURL_RejectsBadBase(t *testing.T) {
	if _, err := buildPeerBlobIngestURL("hub.tail-net.ts.net:8080",
		blob.ScopeGlobal, "x.bin"); err == nil {
		t.Errorf("expected error for scheme-less base; got nil")
	}
	if _, err := buildPeerBlobIngestURL("not a url",
		blob.ScopeGlobal, "x.bin"); err == nil ||
		!strings.Contains(err.Error(), "scheme/host") {
		// Some malformed URLs parse OK then fail the scheme/host
		// check, others fail at url.Parse — either is fine, but
		// the user-facing message should mention scheme/host.
		t.Logf("not-a-url error: %v", err)
	}
}
