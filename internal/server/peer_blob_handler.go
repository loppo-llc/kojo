package server

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/blob"
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
	if peerBlobReadHandoffPendingOnly && !ref.HandoffPending {
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
	_, _ = io.Copy(w, f)
}
