package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestEnforceIfMatchPresence covers the §3.5 transition gate:
// pass-through (any precondition header present, OR strict-mode off)
// vs 428 (strict-mode on, no precondition headers). The handler-side
// glue is pure; constructing a Server with the flag toggled is enough
// to exercise it without standing up the full agent.Manager.
func TestEnforceIfMatchPresence(t *testing.T) {
	cases := []struct {
		name           string
		require        bool
		ifMatchPresent bool
		ifNoneMatch    string // header value, empty = absent
		wantOK         bool
		wantStatus     int // 0 = no status written
	}{
		{"absent header, off (legacy)", false, false, "", true, 0},
		{"present header, off", false, true, "", true, 0},
		{"absent header, on (strict)", true, false, "", false, 428},
		{"present header, on", true, true, "", true, 0},
		// enforceIfMatchPresence (strict If-Match-only path) does
		// NOT accept If-None-Match: *. The create-only variant
		// lives in enforceCreateOrUpdatePrecondition (covered below).
		{"If-None-Match:*, strict (blocked here)", true, false, "*", false, 428},
		{"If-None-Match: \"abc\", strict (not wildcard, blocked)", true, false, `"abc"`, false, 428},
		{"If-None-Match: *, off", false, false, "*", true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{requireIfMatch: c.require}
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPut, "/api/v1/_test", nil)
			if c.ifNoneMatch != "" {
				r.Header.Set("If-None-Match", c.ifNoneMatch)
			}
			ok := s.enforceIfMatchPresence(w, r, c.ifMatchPresent)
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v", ok, c.wantOK)
			}
			if w.Code != c.wantStatus && c.wantStatus != 0 {
				t.Errorf("status = %d, want %d (body=%s)", w.Code, c.wantStatus, w.Body.String())
			}
			if c.wantStatus == 0 && w.Code != 200 {
				// httptest.ResponseRecorder defaults to 200 when
				// nothing is written — the pass-through path must
				// not have called WriteHeader.
				t.Errorf("pass-through wrote a status: %d (body=%s)", w.Code, w.Body.String())
			}
		})
	}
}

// TestEnforceCreateOrUpdatePrecondition covers the opt-in variant
// used by handlers that DO implement create-only CAS (PUT /kv). It
// accepts If-None-Match: * alongside If-Match.
func TestEnforceCreateOrUpdatePrecondition(t *testing.T) {
	cases := []struct {
		name           string
		require        bool
		ifMatchPresent bool
		ifNoneMatch    string
		wantOK         bool
		wantStatus     int
	}{
		{"absent, off (legacy)", false, false, "", true, 0},
		{"If-Match present, strict", true, true, "", true, 0},
		{"If-None-Match: *, strict", true, false, "*", true, 0},
		{"If-None-Match: \"x\", strict (not wildcard)", true, false, `"x"`, false, 428},
		{"both absent, strict", true, false, "", false, 428},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{requireIfMatch: c.require}
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPut, "/api/v1/kv/_test", nil)
			if c.ifNoneMatch != "" {
				r.Header.Set("If-None-Match", c.ifNoneMatch)
			}
			ok := s.enforceCreateOrUpdatePrecondition(w, r, c.ifMatchPresent)
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v", ok, c.wantOK)
			}
			if c.wantStatus != 0 && w.Code != c.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, c.wantStatus)
			}
		})
	}
}
