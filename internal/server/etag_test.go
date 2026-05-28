package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQuoteETag(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"3-deadbeef", `"3-deadbeef"`},
		{"sha256:abc", `"sha256:abc"`}, // not blob's job here, but format must be transparent
	}
	for _, c := range cases {
		if got := quoteETag(c.in); got != c.want {
			t.Errorf("quoteETag(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExtractDomainIfMatch(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string][]string
		wantVal string
		wantOK  bool
		wantErr bool
	}{
		{name: "absent", headers: nil, wantOK: false},
		{name: "single strong", headers: map[string][]string{"If-Match": {`"3-deadbeef"`}}, wantVal: "3-deadbeef", wantOK: true},
		{name: "wildcard accepted", headers: map[string][]string{"If-Match": {`*`}}, wantVal: "*", wantOK: true},
		{name: "weak rejected", headers: map[string][]string{"If-Match": {`W/"3-deadbeef"`}}, wantErr: true},
		{name: "comma-list rejected", headers: map[string][]string{"If-Match": {`"a","b"`}}, wantErr: true},
		{name: "empty quotes rejected", headers: map[string][]string{"If-Match": {`""`}}, wantErr: true},
		{name: "missing quotes rejected", headers: map[string][]string{"If-Match": {`3-deadbeef`}}, wantErr: true},
		{name: "two header lines rejected", headers: map[string][]string{"If-Match": {`"a"`, `"b"`}}, wantErr: true},
		{name: "whitespace-only rejected", headers: map[string][]string{"If-Match": {` `}}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			for k, vs := range c.headers {
				for _, v := range vs {
					req.Header.Add(k, v)
				}
			}
			val, ok, err := extractDomainIfMatch(req)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got val=%q ok=%v", val, ok)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != c.wantOK || val != c.wantVal {
				t.Errorf("got (%q, %v), want (%q, %v)", val, ok, c.wantVal, c.wantOK)
			}
		})
	}
}

func TestExtractDomainIfNoneMatch(t *testing.T) {
	cases := []struct {
		name    string
		header  string
		wantVal string
		wantOK  bool
	}{
		{name: "absent", header: "", wantOK: false},
		{name: "strong etag", header: `"3-deadbeef"`, wantVal: "3-deadbeef", wantOK: true},
		{name: "wildcard", header: "*", wantVal: "*", wantOK: true},
		// RFC 7232: malformed precondition -> ignore (not 400 like If-Match).
		{name: "weak ignored", header: `W/"3-deadbeef"`, wantOK: false},
		{name: "comma list ignored", header: `"a","b"`, wantOK: false},
		{name: "missing quotes ignored", header: `3-deadbeef`, wantOK: false},
		{name: "empty quotes ignored", header: `""`, wantOK: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if c.header != "" {
				req.Header.Set("If-None-Match", c.header)
			}
			val, ok := extractDomainIfNoneMatch(req)
			if ok != c.wantOK || val != c.wantVal {
				t.Errorf("got (%q, %v), want (%q, %v)", val, ok, c.wantVal, c.wantOK)
			}
		})
	}
}
