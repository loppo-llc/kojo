package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// docs/multi-device-storage.md §3.7 step 4 — the target peer of a
// device switch fetches the blob body from the source peer. This
// is the source-side endpoint that serves the body: peer-auth
// gated, scope-limited to live blob_refs rows whose handoff_pending
// flag is set (the explicit "this blob is mid-switch" marker).
//
// Route: GET /api/v1/peers/blobs/{kojo-uri}
//   where {kojo-uri} is the URL-path-escaped form of the blob's
//   kojo:// URI. Receiver decodes per-segment with blob.ParseURI
//   so a future scope / path expansion keeps the handler stable.
//
// Auth: RolePeer (the target peer's Ed25519 signature) OR
// RoleOwner (operator running a manual `kojo` CLI that fetches
// directly; useful for v1 monolith drills where there is no
// remote peer to do the pull). EnforceMiddleware narrows the
// surface to GET only.

// peerBlobReadHandoffPendingOnly toggles whether the handler
// REQUIRES blob_refs.handoff_pending=true before serving. v1
// default is true — the source peer should only export bodies
// during an active handoff. Set to false in tests that exercise
// the handler against a non-handoff fixture.
const peerBlobReadHandoffPendingOnly = true

func (s *Server) handlePeerBlobGet(w http.ResponseWriter, r *http.Request) {
	if s.blob == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"blob store not configured")
		return
	}
	if s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"store backing peer-blob handoff not configured")
		return
	}
	p := auth.FromContext(r.Context())
	if !p.IsPeer() && !p.IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden",
			"peer or owner principal required")
		return
	}

	// Hub-relay path (docs/peer-simplify-plan.md Codex review P1-2):
	// with Ed25519 signing retired, peer↔peer blob pulls can no
	// longer authenticate against the source directly. Target peers
	// instead ask the Hub to relay the fetch — both legs are
	// Hub↔peer Bearer-authenticated. When the URL carries
	// `?relay_from=<source_device_id>` AND the caller is a trusted
	// RolePeer, this handler becomes a streaming proxy to the
	// source's own /peers/blobs/{uri} endpoint.
	if relayFrom := r.URL.Query().Get("relay_from"); relayFrom != "" {
		s.relayPeerBlob(w, r, relayFrom)
		return
	}

	// The URI is the everything-after-the-prefix slice of the
	// request path. http.ServeMux gives us r.PathValue but we
	// register on "/api/v1/peers/blobs/" so the segment can hold
	// the multi-slash kojo:// URI form verbatim. Reading
	// r.URL.Path directly preserves the escaping the signer
	// committed to.
	const prefix = "/api/v1/peers/blobs/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeError(w, http.StatusBadRequest, "bad_request", "unexpected path")
		return
	}
	rawURI := r.URL.Path[len(prefix):]
	if rawURI == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "uri required")
		return
	}
	// Allow either a percent-encoded kojo:// URI as a single
	// segment, or the kojo://scope/path form embedded directly.
	// Both decode to the same canonical (scope, path) pair via
	// blob.ParseURI. The request-form URI is NOT used for the
	// blob_refs lookup — we re-canonicalise via BuildURI so a
	// %20-vs-' ' mismatch between the signer's encoding and our
	// canonical row format doesn't surface as a 404.
	inputURI := rawURI
	if !strings.HasPrefix(inputURI, "kojo://") {
		// Treat the path tail as scope/path and reconstruct.
		inputURI = "kojo://" + inputURI
	}
	scope, blobPath, err := blob.ParseURI(inputURI)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"invalid blob uri: "+err.Error())
		return
	}
	uri := blob.BuildURI(scope, blobPath)

	// blob_refs lookup. Refuse if no row exists OR (in v1
	// production) if handoff_pending isn't set — a peer
	// shouldn't be pulling a blob outside an active switch.
	ref, err := s.agents.Store().GetBlobRef(r.Context(), uri)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found",
				"no blob_refs row for "+uri)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal",
			"blob_refs read: "+err.Error())
		return
	}
	// `?live_read=1` is the kojo-attach hub-fallback path: hub asks
	// the holder peer for an attach blob whose forwarder push never
	// landed on hub (network blip, hub offline at push time, etc.).
	// Bypasses handoff_pending because attach reads are NOT part of
	// the §3.7 switch state machine — they happen during normal
	// operation, when no handoff is in flight.
	//
	// Strictly scope-limited:
	//   - scope MUST be `global` (the only scope the kojo-attach
	//     contract publishes into; mirrors peerBlobIngestHandler)
	//   - path MUST match peerBlobIngestPath
	//     (agents/<id>/attach/<msgID>/<file>)
	//
	// Together these guarantee live_read can only ever surface a
	// row this peer published via the kojo-attach skill — never an
	// avatar / book / arbitrary other blob_refs row in the same
	// agent's tree. The downstream `wrong_home` check (the row's
	// home_peer must equal this peer's DeviceID) means a paired
	// peer cannot relay-read OTHER peers' attach blobs through us
	// either.
	liveRead := r.URL.Query().Get("live_read") == "1" &&
		scope == blob.ScopeGlobal &&
		peerBlobIngestPath.MatchString(blobPath)
	if peerBlobReadHandoffPendingOnly && !ref.HandoffPending && !liveRead {
		writeError(w, http.StatusConflict, "not_in_handoff",
			"blob_refs row is not marked handoff_pending; refusing peer fetch")
		return
	}
	// Source-of-truth check: this peer should be the current
	// home_peer; serving someone else's blob would let a stolen
	// peer credential exfiltrate bodies that don't belong here.
	if s.peerID != nil && ref.HomePeer != s.peerID.DeviceID {
		writeError(w, http.StatusConflict, "wrong_home",
			"blob_refs.home_peer is not this peer; refusing fetch")
		return
	}

	f, obj, err := s.blob.Open(scope, blobPath)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			writeError(w, http.StatusGone, "body_missing",
				"blob_refs row exists but on-disk body is missing")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal",
			"open: "+err.Error())
		return
	}
	defer f.Close()

	// Echo the canonical digest so the target can verify the
	// body against ref.SHA256 it already knows. Content-Length
	// is included so net/http doesn't switch to chunked
	// transfer-encoding for large bodies.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", "sha256:"+obj.SHA256)
	w.Header().Set("X-Kojo-Blob-SHA256", obj.SHA256)
	w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// http.ServeContent would seek; the underlying *os.File supports
	// it, but a streaming Copy is simpler and avoids reading the
	// whole body into memory. We accept that range requests aren't
	// honoured for peer-fetch (the target reads the full body).
	//
	// HEAD short-circuit: http.ResponseWriter auto-discards body
	// writes for HEAD requests, but io.Copy would still drain the
	// on-disk fd before discovering nothing landed on the wire.
	// Skip the read entirely so HEAD probes don't pay the disk-I/O
	// cost of a multi-GiB blob.
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, f)
}

// relayPeerBlob is the Hub-relay arm of handlePeerBlobGet. The
// caller is a paired peer asking the Hub to fetch a blob from a
// THIRD peer on its behalf; this exists because peer↔peer Bearers
// no longer exist (each peer has only Hub↔peer credentials). The
// Hub dials the source's own /peers/blobs/{uri} with the Hub→source
// Bearer, then streams the response back to the caller. Both legs
// authenticate as Hub-paired traffic, so the auth chain stays
// intact without re-introducing per-pair credentials.
//
// Trust gates:
//   - caller must be RolePeer + PeerTrusted (an untrusted peer
//     can't ask the Hub to read blobs out of arbitrary other
//     peers' stores).
//   - source must be in the local peer_registry (otherwise we
//     have no URL + Bearer to dial).
//   - source must not equal caller / Hub (loop prevention).
//
// The relay does NOT inspect blob_refs: that gate lives on the
// source side and the Hub is just a streaming proxy.
func (s *Server) relayPeerBlob(w http.ResponseWriter, r *http.Request, sourceDeviceID string) {
	p := auth.FromContext(r.Context())
	if !p.IsPeer() && !p.IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden",
			"blob relay requires peer or owner principal")
		return
	}
	if s.peerID != nil && sourceDeviceID == s.peerID.DeviceID {
		writeError(w, http.StatusBadRequest, "bad_request",
			"relay_from must not equal the local peer (would loop)")
		return
	}
	if p.IsPeer() && sourceDeviceID == p.PeerID {
		writeError(w, http.StatusBadRequest, "bad_request",
			"relay_from must not equal the caller (would loop)")
		return
	}
	srcRec, err := s.agents.Store().GetPeer(r.Context(), sourceDeviceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "bad_request",
				"relay_from peer not in peer_registry: "+sourceDeviceID)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal",
			"peer_registry lookup: "+err.Error())
		return
	}
	srcAddr, err := peer.NormalizeAddress(srcRec.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"source peer has no usable dial address: "+err.Error())
		return
	}

	// Strip the relay_from param before re-issuing — the upstream
	// /peers/blobs handler expects the original (no-query) URL form.
	upstreamPath := r.URL.Path
	upstreamURL := strings.TrimRight(srcAddr, "/") + upstreamPath

	// No fixed timeout: switch_device blob handoffs can be hundreds
	// of MB on slow tailnet links. The request context is the only
	// deadline (caller side enforces switchDeviceOpTimeout). Codex
	// review: fixed 5-minute cap could chop long transfers.
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal",
			"build relay request: "+err.Error())
		return
	}
	client := peer.NoKeepAliveHTTPClient(0)
	resp, err := client.Do(upReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway",
			fmt.Sprintf("source dial failed: %v", err))
		return
	}
	defer resp.Body.Close()
	// Preserve the digest + size headers so the caller's sha256
	// check still works.
	for _, h := range []string{"Content-Type", "ETag", "X-Kojo-Blob-SHA256", "Content-Length", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
