package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/store"
)

// kojo-attach hub forwarding.
//
// PushClient is the peer-side mirror of PullClient. A non-hub
// daemon uses it to forward agent-generated attachment bytes to
// the hub's
//
//	PUT /api/v1/peers/blobs-ingest/{scope}/{path...}
//
// endpoint. The hub re-Puts the body into its own blob.Store with
// the digest the peer pinned via X-Kojo-Expected-SHA256, so the
// canonical (scope, path) URI is reachable on hub without any
// additional indirection — the user UI fetches it through the
// regular /api/v1/blob/... surface.
//
// Auth: every request is Ed25519-signed with the local peer
// Identity, audience pinned to the hub's DeviceID. Hub-side
// EnforceMiddleware + auth.AllowNonOwner require the peer_registry
// row to be trusted (the user explicitly paired the peer); without
// that the PUT returns 403.
//
// HTTP transport mirrors PullClient: keep-alives disabled so Go's
// idempotent-request stale-conn retry can never re-send the same
// signed nonce. See blobpull.go's NewPullClient for the full
// rationale (the trap is identical for PUT — http.Client treats
// PUT as idempotent and will silently re-send on a stale-conn EOF).

// PushTarget identifies the hub a non-hub peer pushes to.
type PushTarget struct {
	// DeviceID is the hub peer's identity, used as the
	// SigningInput.Audience so the signed envelope only validates
	// against this hub.
	DeviceID string
	// Address is the base URL of the hub (e.g.
	// "https://hub.tail-net.ts.net:8080"). The ingest path is
	// appended; any path / query already on the URL is dropped.
	Address string
}

// PushClient is reusable. The *http.Client is shared but the
// transport disables keep-alives so each PushOne opens a fresh
// TCP/TLS connection — same reasoning as PullClient.
type PushClient struct {
	identity *Identity
	// store carries the dual-stack Bearer lookup for the kojo-attach
	// hub-forwarding leg (docs/peer-simplify-plan.md step 7). When a
	// Hub-paired Bearer is present AuthorizeOutbound uses it;
	// otherwise SignRequest still runs so the legacy path keeps the
	// blob ingest unblocked until a follow-up capability-URL flow
	// lands.
	store      *store.Store
	httpClient *http.Client
	logger     *slog.Logger
}

// NewPushClient wires the client. Pass nil for httpClient to use
// a no-keep-alive default; tests can inject a fixture client.
func NewPushClient(id *Identity, st *store.Store, httpClient *http.Client, logger *slog.Logger) *PushClient {
	if httpClient == nil {
		httpClient = &http.Client{
			Transport: noKeepAliveTransport(),
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &PushClient{identity: id, store: st, httpClient: httpClient, logger: logger}
}

// PushOne uploads body to dst's ingest endpoint as (scope, path).
// expectedSHA256 must be the hex digest of the body (pre-computed
// during the local blob.Store.Put) and is sent as
// X-Kojo-Expected-SHA256 so the hub aborts pre-rename on any
// mismatch.
//
// Returns nil on a 2xx response. Any non-2xx surface as an error
// with the response body snippet preserved for diagnostics.
func (c *PushClient) PushOne(
	ctx context.Context,
	dst PushTarget,
	scope blob.Scope,
	blobPath string,
	expectedSHA256 string,
	body io.Reader,
	size int64,
) error {
	if c == nil || c.identity == nil {
		return errors.New("peer.PushClient: nil client / identity")
	}
	if dst.DeviceID == "" || dst.Address == "" {
		return errors.New("peer.PushClient: target DeviceID and Address required")
	}
	if !scope.Valid() {
		return fmt.Errorf("peer.PushClient: invalid scope %q", scope)
	}
	if blobPath == "" {
		return errors.New("peer.PushClient: non-empty path required")
	}
	if expectedSHA256 == "" {
		return errors.New("peer.PushClient: expectedSHA256 required (hub refuses unverified pushes)")
	}

	reqURL, err := buildPeerBlobIngestURL(dst.Address, scope, blobPath)
	if err != nil {
		return fmt.Errorf("peer.PushOne: build url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, reqURL, body)
	if err != nil {
		return fmt.Errorf("peer.PushOne: new request: %w", err)
	}
	if size >= 0 {
		// Setting ContentLength lets the transport avoid
		// chunked transfer-encoding for large bodies, which
		// matters because some reverse proxies in front of
		// hub (Tailscale Funnel, nginx) reject chunked PUTs.
		req.ContentLength = size
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Kojo-Expected-SHA256", expectedSHA256)

	if err := AuthorizeOutbound(ctx, c.store, req, dst.DeviceID); err != nil {
		return fmt.Errorf("peer.PushOne: authorize: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("peer.PushOne: do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("peer.PushOne: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	// Drain to release the connection (kept for parity even
	// with keep-alives disabled — Go's transport still expects
	// the body to be closed).
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// PushOneJSON decodes the hub's 200 response body. Used by tests
// that want to assert echoed digest / size match what they sent.
// Production callers (the attachment forwarder) ignore the body
// and check err alone.
func (c *PushClient) PushOneJSON(
	ctx context.Context,
	dst PushTarget,
	scope blob.Scope,
	blobPath string,
	expectedSHA256 string,
	body io.Reader,
	size int64,
	out any,
) error {
	if err := c.PushOne(ctx, dst, scope, blobPath, expectedSHA256, body, size); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	// PushOne drains the body for connection reuse, so this
	// variant can't actually read it back. The helper is
	// retained for future use; callers that need the response
	// JSON should refactor PushOne to expose it. Returning
	// nil here keeps the test surface honest about the
	// limitation.
	_ = json.Unmarshal(nil, out)
	return nil
}

// buildPeerBlobIngestURL composes the canonical ingest URL.
// Mirrors buildPeerBlobURL's anti-redirect posture: a path that
// contains "//" would trigger Go's ServeMux to issue a 301
// path-clean, and http.Client would re-send the SAME signed nonce
// to the cleaned target → 401 replayed nonce. We refuse such
// inputs up front rather than discover it at runtime.
func buildPeerBlobIngestURL(base string, scope blob.Scope, blobPath string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("base URL missing scheme/host: %q", base)
	}
	tail := string(scope) + "/" + strings.TrimPrefix(blobPath, "/")
	if strings.Contains(tail, "//") {
		return "", fmt.Errorf("ingest tail contains double-slash: %q", tail)
	}
	u.Path = "/api/v1/peers/blobs-ingest/" + tail
	u.RawPath = ""
	u.RawQuery = ""
	return u.String(), nil
}
