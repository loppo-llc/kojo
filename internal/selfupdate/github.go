package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultRepo is the GitHub repository that publishes kojo release
// assets. Tests and alternate mirrors override Client.Repo rather than
// hard-coding a different path in callers.
const DefaultRepo = "loppo-llc/kojo"

// defaultBaseURL is the public GitHub REST API root. Kept unexported so
// production always hits api.github.com unless a test injects BaseURL.
const defaultBaseURL = "https://api.github.com"

// ChecksumsAssetName is the conventional release asset that lists
// SHA-256 digests for every platform archive in the same release.
const ChecksumsAssetName = "checksums.txt"

// maxReleaseBody caps the releases/latest JSON response. GitHub's
// payload is small, but a misbehaving proxy or hostile response must
// not be buffered unbounded into memory (same LimitReader guard used
// elsewhere in the tree for outbound HTTP bodies).
const maxReleaseBody = 1 << 20 // 1 MiB

// Release is the subset of a GitHub release object needed to pick a
// download URL. Field names match the GitHub REST JSON keys so the
// decoder can stay tag-driven.
type Release struct {
	TagName string  `json:"tag_name"`
	HTMLURL string  `json:"html_url"`
	Assets  []Asset `json:"assets"`
}

// Asset is one downloadable file attached to a Release.
type Asset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// Client talks to the GitHub Releases API. Zero-valued BaseURL and Repo
// fall back to defaultBaseURL / DefaultRepo so tests can construct a
// Client with only HTTPClient set (or override BaseURL to an httptest
// server) without re-stating production defaults.
type Client struct {
	HTTPClient *http.Client
	BaseURL    string
	Repo       string
	UserAgent  string
}

// NewClient returns a Client stamped with the running binary's version
// in the User-Agent. GitHub rejects requests without a UA; no other
// outbound call in this repo sets one today, so this is the first
// deliberate header. Timeout is 15s so a hung API cannot block a
// startup or cron check forever.
func NewClient(currentVersion string) *Client {
	return &Client{
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
		BaseURL:    defaultBaseURL,
		Repo:       DefaultRepo,
		UserAgent:  "kojo/" + currentVersion,
	}
}

// get issues a GET with the client's User-Agent. Shared by the Releases
// API call and asset downloads so every outbound request carries the
// same identity (GitHub rejects empty UAs; asset mirrors inherit it).
// headerKV is an optional flat list of extra header key/value pairs
// (e.g. Accept for the REST media type).
func (c *Client) get(ctx context.Context, url string, headerKV ...string) (*http.Response, error) {
	if c == nil {
		return nil, fmt.Errorf("nil selfupdate client")
	}
	if len(headerKV)%2 != 0 {
		return nil, fmt.Errorf("get: odd headerKV length")
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	ua := c.UserAgent
	if ua == "" {
		ua = "kojo/unknown"
	}
	req.Header.Set("User-Agent", ua)
	for i := 0; i+1 < len(headerKV); i += 2 {
		req.Header.Set(headerKV[i], headerKV[i+1])
	}
	return client.Do(req)
}

// LatestRelease GETs /repos/{repo}/releases/latest and decodes the
// body. Non-2xx responses become errors that include the status code
// so operators can tell rate-limit (403/429) from missing releases
// (404) without scraping the body.
func (c *Client) LatestRelease(ctx context.Context) (*Release, error) {
	if c == nil {
		return nil, fmt.Errorf("nil selfupdate client")
	}
	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	repo := c.Repo
	if repo == "" {
		repo = DefaultRepo
	}

	url := strings.TrimRight(base, "/") + "/repos/" + repo + "/releases/latest"
	// application/vnd.github+json is the documented Accept for the
	// REST API; plain application/json still works today but pins us
	// to the media type GitHub recommends for versioned responses.
	resp, err := c.get(ctx, url, "Accept", "application/vnd.github+json")
	if err != nil {
		return nil, fmt.Errorf("fetch latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain a small snippet so the connection can be reused, but
		// surface only the status: error bodies are HTML/JSON noise
		// for most operators.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("github releases/latest: status %d", resp.StatusCode)
	}

	var rel Release
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseBody))
	if err := dec.Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode latest release: %w", err)
	}
	return &rel, nil
}

// AssetName builds the platform archive name published on each
// release. Windows ships a .zip (no tar on stock installs); every
// other GOOS uses .tar.gz.
func AssetName(goos, goarch string) string {
	base := "kojo_" + goos + "_" + goarch
	if goos == "windows" {
		return base + ".zip"
	}
	return base + ".tar.gz"
}

// FindAsset returns the first asset whose Name matches, or false when
// the release has no such file (wrong platform, incomplete publish).
func (r *Release) FindAsset(name string) (Asset, bool) {
	if r == nil {
		return Asset{}, false
	}
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}
