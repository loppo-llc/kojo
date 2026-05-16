package peer

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/blob"
)

// docs/multi-device-storage.md §3.7 step 4 — target-side blob pull.
//
// PullClient is the client half of the device-switch handoff: it
// runs on the target peer and fetches blob bodies from the source
// peer's `GET /api/v1/peers/blobs/{uri}` endpoint, then writes
// them into the local blob.Store. The source-side handler
// (internal/server/peer_blob_handler.go) gates on
// blob_refs.handoff_pending=true, so this client must only be
// invoked between a successful `handoff/begin` and the matching
// `handoff/complete` on the Hub.
//
// Auth: every outbound request is signed with the local peer
// Identity (Ed25519) so the source's AuthMiddleware authenticates
// us as RolePeer. The audience header pins the request to the
// source's DeviceID, closing cross-peer signature replay.
//
// SHA256 verification: the body is streamed through
// blob.Store.Put with PutOptions.ExpectedSHA256 set to the digest
// echoed in the source's `X-Kojo-Blob-SHA256` response header.
// A mismatch aborts the write before the rename so a corrupt or
// substituted body never lands on disk.

// PullSource identifies the peer to fetch from.
type PullSource struct {
	// DeviceID is the source peer's identity, used as the
	// SigningInput.Audience and as a sanity check on incoming
	// blob_refs rows.
	DeviceID string
	// Address is the base URL (e.g. "http://hub.tail-net.ts.net:8080")
	// returned by NormalizeAddress. The blob URI path is appended.
	Address string
}

// PullItem is one entry in a pull batch — the URI to fetch plus
// the orchestrator-asserted sha256 the body must hash to. The
// digest comes from the Hub's blob_refs row (the orchestrator
// reads it before dispatching the pull) so target's trust is
// rooted in the signed authority that drove the switch, not in
// the unsigned response header the source peer echoes. Empty
// ExpectedSHA256 falls back to "header-only" verification, which
// is strictly weaker — orchestrator callers should always
// populate it.
type PullItem struct {
	URI            string `json:"uri"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
}

// PullResult is the per-URI outcome of a pull batch.
type PullResult struct {
	URI    string `json:"uri"`
	Status string `json:"status"` // "ok" | "error" | "sha256_mismatch" | "http_status"
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Error  string `json:"error,omitempty"`
}

// PullClient drives outbound GET /api/v1/peers/blobs/* requests
// against a single source peer. Reuse one *PullClient across
// many PullOne calls — the *http.Client is shared but its
// transport disables keep-alives, so each PullOne opens a fresh
// TCP/TLS connection. The single-use connection policy is what
// prevents Go's idempotent-GET stale-conn retry from re-sending
// the same signed nonce; see NewPullClient for the full
// rationale.
type PullClient struct {
	identity   *Identity
	httpClient *http.Client
	logger     *slog.Logger
}

// NewPullClient wires the client. Pass nil for httpClient to use a
// default with a sane timeout; tests can inject a fixture client.
//
// The default transport DISABLES connection keep-alive. Each
// signed request carries a unique nonce stamped into the
// Authorization headers; if Go's transport silently retries an
// idempotent GET on a stale-connection error (RST / EOF on a
// reused idle conn before any response bytes arrive), the SAME
// nonce reaches the source twice and the peer auth middleware
// rejects the retry with HTTP 401 "replayed nonce". Disabling
// keep-alives forces a fresh TCP/TLS handshake per request so
// the stale-conn retry path never fires. Cost: a few extra
// handshakes per switch — negligible against the blob payload
// sizes — vs the 401 that aborts the whole switch.
func NewPullClient(id *Identity, httpClient *http.Client, logger *slog.Logger) *PullClient {
	if httpClient == nil {
		// 60s per-blob ceiling. Big blobs over Tailscale should
		// still fit; the caller-supplied context provides the
		// overall batch deadline.
		httpClient = &http.Client{
			Timeout:   60 * time.Second,
			Transport: noKeepAliveTransport(),
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &PullClient{identity: id, httpClient: httpClient, logger: logger}
}

// noKeepAliveTransport returns an http.Transport with idle-
// connection reuse disabled. See NewPullClient for the rationale.
// Defined as a helper so other peer-signed dispatch sites that
// hit the same stale-conn-retry-replays-nonce trap can adopt it.
func noKeepAliveTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableKeepAlives = true
	return t
}

// NoKeepAliveHTTPClient returns an *http.Client with the same
// stale-conn-retry mitigation NewPullClient uses internally,
// configured with the caller's timeout. Every peer-signed
// outbound request (Ed25519-signed Authorization headers carry
// a single-use nonce) should use this client — Go's default
// transport will silently retry an idempotent request on a
// stale-conn EOF, resending the SAME nonce and triggering a
// 401 "replayed nonce" at the recipient.
func NoKeepAliveHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: noKeepAliveTransport(),
	}
}

// PullOne fetches a single blob body from src and writes it to
// dst via blob.Store.Put. The URI is the canonical kojo:// form
// (matches blob_refs.uri verbatim). When item.ExpectedSHA256 is
// non-empty (the orchestrator path), the helper enforces it as
// BOTH the X-Kojo-Blob-SHA256 response header value AND the
// ExpectedSHA256 passed to blob.Store.Put — a compromised source
// can therefore only succeed by returning a body whose actual
// hash matches the orchestrator's pre-existing blob_refs row.
// When ExpectedSHA256 is empty (legacy / drill path), the helper
// falls back to "trust the response header" verification, which
// is strictly weaker.
//
// Failure modes that DO NOT abort the batch:
//   - HTTP non-200 (handler returns 409 not_in_handoff / wrong_home,
//     410 body_missing, 404 not_found, ...): recorded as
//     Status="http_status".
//   - SHA256 mismatch (body hashed to something other than the
//     X-Kojo-Blob-SHA256 header) — recorded as
//     Status="sha256_mismatch"; the on-disk file is NOT updated
//     (blob.Store.Put aborts before rename).
//   - Header / orchestrator digest disagreement — recorded as
//     Status="sha256_mismatch" without touching disk.
//
// Failure modes that DO return an error to the caller:
//   - Local I/O when constructing/signing the request, dialing
//     the source, or wiring the response stream.
//   - Context cancellation.
//
// The pull is idempotent w.r.t. blob.Store: writing the same body
// twice produces the same digest and leaves blob_refs unchanged.
func (c *PullClient) PullOne(ctx context.Context, src PullSource, item PullItem, dst *blob.Store) (PullResult, error) {
	res := PullResult{URI: item.URI}
	if c == nil || c.identity == nil {
		return res, errors.New("peer.PullClient: nil client / identity")
	}
	if dst == nil {
		return res, errors.New("peer.PullClient: nil dst blob store")
	}
	if src.DeviceID == "" || src.Address == "" {
		return res, errors.New("peer.PullClient: source DeviceID and Address required")
	}
	scope, blobPath, err := blob.ParseURI(item.URI)
	if err != nil {
		res.Status = "error"
		res.Error = "parse uri: " + err.Error()
		return res, nil
	}

	reqURL, err := buildPeerBlobURL(src.Address, item.URI)
	if err != nil {
		res.Status = "error"
		res.Error = "build url: " + err.Error()
		return res, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return res, fmt.Errorf("peer.PullOne: new request: %w", err)
	}
	nonce, err := MakeNonce()
	if err != nil {
		return res, fmt.Errorf("peer.PullOne: nonce: %w", err)
	}
	if err := SignRequest(req, c.identity.DeviceID, c.identity.PrivateKey, nonce, src.DeviceID); err != nil {
		return res, fmt.Errorf("peer.PullOne: sign: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return res, fmt.Errorf("peer.PullOne: do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain a snippet for diagnostics, then surface the HTTP
		// status without rolling back the parent batch — a 409
		// not_in_handoff on one row shouldn't kill the whole
		// switch.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		res.Status = "http_status"
		res.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		return res, nil
	}

	headerSHA := strings.TrimSpace(resp.Header.Get("X-Kojo-Blob-SHA256"))
	expectedSHA := strings.TrimSpace(item.ExpectedSHA256)

	switch {
	case expectedSHA == "" && headerSHA == "":
		// Neither side gave us a digest; refusing avoids letting
		// a tampered source write arbitrary content.
		res.Status = "error"
		res.Error = "no expected sha256 (orchestrator) and no X-Kojo-Blob-SHA256 (source)"
		return res, nil
	case expectedSHA != "" && headerSHA != "" && !strings.EqualFold(headerSHA, expectedSHA):
		// Header disagrees with the orchestrator's authoritative
		// digest — the source is lying about what it's serving.
		// Refuse before any bytes touch disk.
		res.Status = "sha256_mismatch"
		res.Error = fmt.Sprintf("source returned sha256 %s but orchestrator expected %s",
			headerSHA, expectedSHA)
		return res, nil
	}

	// Prefer the orchestrator-supplied digest when present; the
	// header is only used as a fallback. blob.Store.Put hashes
	// the body in-stream and aborts pre-rename on mismatch.
	gateSHA := expectedSHA
	if gateSHA == "" {
		gateSHA = headerSHA
	}

	obj, err := dst.Put(scope, blobPath, resp.Body, blob.PutOptions{
		ExpectedSHA256: gateSHA,
		// The pull IS the §3.7 mechanism: any pre-existing
		// handoff_pending row on the target is either stale
		// (abandoned prior switch) or about to be supplanted by
		// the body we're committing now. Bypass the guard so
		// blob.Store.Put doesn't refuse our own orchestrator-
		// driven write.
		BypassHandoffPending: true,
	})
	if err != nil {
		switch {
		case errors.Is(err, blob.ErrExpectedSHA256Mismatch):
			res.Status = "sha256_mismatch"
			res.Error = err.Error()
			return res, nil
		case errors.Is(err, blob.ErrDurabilityDegraded):
			// Body + blob_refs row are committed; the only
			// thing missing is the parent-dir fsync. The
			// orchestrator's §3.7 switch can proceed —
			// rolling back here would discard a successful
			// transfer over a durability-grade nit. Surface
			// the warning via the Error field so an operator
			// dashboard can flag it without aborting.
			c.logger.Warn("peer pull: blob committed with degraded durability",
				"uri", item.URI, "err", err)
			if obj != nil {
				res.Status = "ok"
				res.SHA256 = obj.SHA256
				res.Size = obj.Size
				res.Error = err.Error()
				return res, nil
			}
			// obj nil shouldn't happen on ErrDurabilityDegraded,
			// but be defensive.
			res.Status = "error"
			res.Error = "put: " + err.Error()
			return res, nil
		default:
			res.Status = "error"
			res.Error = "put: " + err.Error()
			return res, nil
		}
	}

	res.Status = "ok"
	res.SHA256 = obj.SHA256
	res.Size = obj.Size
	return res, nil
}

// PullMany sequentially fetches every item from src into dst.
// A fatal local error (context cancel, signing failure) aborts
// the batch and returns the partial result list with the
// triggering error. Per-URI HTTP / sha256 failures are recorded
// in the result list and the batch continues; the caller decides
// whether to call handoff/abort based on the populated statuses.
//
// Ordering is preserved so a caller mapping result[i] back to
// items[i] for logging works without rebuilding a map.
func (c *PullClient) PullMany(ctx context.Context, src PullSource, items []PullItem, dst *blob.Store) ([]PullResult, error) {
	out := make([]PullResult, 0, len(items))
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		r, err := c.PullOne(ctx, src, it, dst)
		out = append(out, r)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// buildPeerBlobURL joins a peer base URL with the blob URI tail.
//
// The kojo:// prefix is STRIPPED before embedding — the "://"
// contains a double-slash that Go's ServeMux path-cleans into a
// single slash, triggering a 301 redirect. Go's http.Client
// follows the redirect, re-sending the SAME signed headers
// (including the single-use nonce) to the cleaned URL; the auth
// middleware sees the nonce a second time and returns 401
// "replayed nonce". Stripping the prefix produces a clean path
// like /api/v1/peers/blobs/global/agents/… with no double-slash.
//
// The source-side handler (peer_blob_handler.go) already accepts
// the prefix-less form: it prepends "kojo://" when the path tail
// doesn't start with it, then re-canonicalises via
// blob.ParseURI + BuildURI.
func buildPeerBlobURL(base, blobURI string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("base URL missing scheme/host: %q", base)
	}
	// Strip kojo:// so the path never contains "//" which would
	// cause a ServeMux redirect → nonce replay.
	tail := strings.TrimPrefix(blobURI, "kojo://")
	if strings.Contains(tail, "//") {
		return "", fmt.Errorf("blob URI tail contains double-slash after prefix strip: %q", tail)
	}
	// Strip any path/query the caller might have included; we own
	// the path here so a mis-set base can't redirect us.
	u.Path = "/api/v1/peers/blobs/" + tail
	u.RawPath = ""
	u.RawQuery = ""
	return u.String(), nil
}

// MakeNonce returns a fresh 32-byte base64 nonce for use in
// AuthHeaderNonce. Same shape as subscriber.newNonce; duplicated
// here so the pull client can stand alone without coupling to the
// status-subscribe machinery.
func MakeNonce() (string, error) {
	var b [AuthNonceLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b[:]), nil
}
