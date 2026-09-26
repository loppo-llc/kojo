package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPeerProxyIncludesRemoteAttachmentThumbnail(t *testing.T) {
	if !isPeerProxyPath("/api/v1/files/thumb") {
		t.Fatal("remote attachment thumbnail route is not proxied to its holder")
	}
}

func TestPeerSessionListRequestIsBounded(t *testing.T) {
	cases := []struct {
		method, target string
		want           bool
	}{
		{http.MethodGet, "/api/v1/sessions?peer=x", true},
		{http.MethodPost, "/api/v1/sessions?peer=x", false},
		{http.MethodGet, "/api/v1/sessions/s1?peer=x", false},
		{http.MethodGet, "/api/v1/files/raw?peer=x", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.target, nil)
		if got := isPeerSessionListRequest(r); got != c.want {
			t.Errorf("%s %s: got %v want %v", c.method, c.target, got, c.want)
		}
	}
}
