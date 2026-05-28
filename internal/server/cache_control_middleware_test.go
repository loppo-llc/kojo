package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPINoStoreDefaultMiddleware(t *testing.T) {
	cases := []struct {
		name           string
		path           string
		handlerSets    string // Cache-Control the inner handler sets, "" = no override
		wantSet        bool
		wantHeader     string
		fallthroughHit bool
	}{
		{
			name:           "seeds no-store on API path",
			path:           "/api/v1/agents",
			handlerSets:    "",
			wantSet:        true,
			wantHeader:     "no-store",
			fallthroughHit: true,
		},
		{
			name:           "handler overwrite wins",
			path:           "/api/v1/agents/abc/tts",
			handlerSets:    "private, max-age=86400, immutable",
			wantSet:        true,
			wantHeader:     "private, max-age=86400, immutable",
			fallthroughHit: true,
		},
		{
			name:           "skips WebDAV exact",
			path:           "/api/v1/webdav",
			handlerSets:    "",
			wantSet:        false,
			fallthroughHit: true,
		},
		{
			name:           "skips WebDAV subtree",
			path:           "/api/v1/webdav/share/file.txt",
			handlerSets:    "",
			wantSet:        false,
			fallthroughHit: true,
		},
		{
			name:           "does not touch non-API surface",
			path:           "/assets/index.js",
			handlerSets:    "",
			wantSet:        false,
			fallthroughHit: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hit := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hit = true
				if c.handlerSets != "" {
					w.Header().Set("Cache-Control", c.handlerSets)
				}
				w.WriteHeader(http.StatusOK)
			})
			h := apiNoStoreDefaultMiddleware(next)
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, c.path, nil)
			h.ServeHTTP(rec, r)
			if hit != c.fallthroughHit {
				t.Fatalf("fallthrough hit = %v, want %v", hit, c.fallthroughHit)
			}
			got := rec.Header().Get("Cache-Control")
			if c.wantSet && got != c.wantHeader {
				t.Fatalf("Cache-Control = %q, want %q", got, c.wantHeader)
			}
			if !c.wantSet && got != "" {
				t.Fatalf("Cache-Control = %q, want empty (skip case)", got)
			}
		})
	}
}
