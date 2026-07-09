package selfupdate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClient_defaults(t *testing.T) {
	t.Parallel()
	c := NewClient("v0.116.3")
	if c.BaseURL != defaultBaseURL {
		t.Fatalf("BaseURL = %q, want %q", c.BaseURL, defaultBaseURL)
	}
	if c.Repo != DefaultRepo {
		t.Fatalf("Repo = %q, want %q", c.Repo, DefaultRepo)
	}
	if c.UserAgent != "kojo/v0.116.3" {
		t.Fatalf("UserAgent = %q, want %q", c.UserAgent, "kojo/v0.116.3")
	}
	if c.HTTPClient == nil || c.HTTPClient.Timeout != 15*time.Second {
		t.Fatalf("HTTPClient timeout = %v, want 15s", c.HTTPClient.Timeout)
	}
}

func TestClient_LatestRelease_happy(t *testing.T) {
	t.Parallel()
	want := Release{
		TagName: "v0.116.3",
		HTMLURL: "https://github.com/loppo-llc/kojo/releases/tag/v0.116.3",
		Assets: []Asset{
			{
				Name:               "kojo_darwin_arm64.tar.gz",
				Size:               1234,
				BrowserDownloadURL: "https://example.com/kojo_darwin_arm64.tar.gz",
			},
			{
				Name:               ChecksumsAssetName,
				Size:               99,
				BrowserDownloadURL: "https://example.com/checksums.txt",
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/repos/loppo-llc/kojo/releases/latest" {
			t.Errorf("path = %q, want /repos/loppo-llc/kojo/releases/latest", r.URL.Path)
		}
		if ua := r.Header.Get("User-Agent"); ua != "kojo/v0.116.3" {
			t.Errorf("User-Agent = %q, want kojo/v0.116.3", ua)
		}
		if acc := r.Header.Get("Accept"); acc != "application/vnd.github+json" {
			t.Errorf("Accept = %q, want application/vnd.github+json", acc)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(want); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient("v0.116.3")
	c.BaseURL = srv.URL
	c.HTTPClient = srv.Client()

	got, err := c.LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if got.TagName != want.TagName || got.HTMLURL != want.HTMLURL {
		t.Fatalf("release meta = %+v, want tag/url from fixture", got)
	}
	if len(got.Assets) != len(want.Assets) {
		t.Fatalf("assets len = %d, want %d", len(got.Assets), len(want.Assets))
	}
	for i := range want.Assets {
		if got.Assets[i] != want.Assets[i] {
			t.Fatalf("asset[%d] = %+v, want %+v", i, got.Assets[i], want.Assets[i])
		}
	}
}

func TestClient_LatestRelease_non200(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	c := NewClient("v0.1.0")
	c.BaseURL = srv.URL
	c.HTTPClient = srv.Client()

	_, err := c.LatestRelease(context.Background())
	if err == nil {
		t.Fatal("LatestRelease succeeded, want error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error = %q, want status 403 mentioned", err)
	}
}

func TestAssetName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		goos, goarch, want string
	}{
		{"darwin", "arm64", "kojo_darwin_arm64.tar.gz"},
		{"linux", "amd64", "kojo_linux_amd64.tar.gz"},
		{"windows", "amd64", "kojo_windows_amd64.zip"},
		{"windows", "arm64", "kojo_windows_arm64.zip"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.goos+"_"+tc.goarch, func(t *testing.T) {
			t.Parallel()
			if got := AssetName(tc.goos, tc.goarch); got != tc.want {
				t.Fatalf("AssetName(%q, %q) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
			}
		})
	}
}

func TestRelease_FindAsset(t *testing.T) {
	t.Parallel()
	r := &Release{
		Assets: []Asset{
			{Name: "kojo_linux_amd64.tar.gz", Size: 1},
			{Name: ChecksumsAssetName, Size: 2},
		},
	}
	a, ok := r.FindAsset(ChecksumsAssetName)
	if !ok || a.Name != ChecksumsAssetName || a.Size != 2 {
		t.Fatalf("FindAsset(checksums) = (%+v, %v)", a, ok)
	}
	if _, ok := r.FindAsset("missing.zip"); ok {
		t.Fatal("FindAsset(missing) returned true")
	}
	if _, ok := (*Release)(nil).FindAsset("x"); ok {
		t.Fatal("FindAsset on nil Release returned true")
	}
}
