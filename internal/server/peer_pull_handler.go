package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// looksLikeHex64 returns true if s is exactly 64 lowercase-or-
// uppercase hex characters. sha256 digests across the codebase
// stay lowercase (hex.EncodeToString output), but accepting either
// case keeps the handler tolerant of operators pasting a digest
// out of a CLI tool's mixed-case display.
func looksLikeHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// docs/multi-device-storage.md §3.7 step 4 — target-side pull
// endpoint. The Hub (acting as orchestrator) POSTs the URI list +
// source coordinates here so the target peer fetches each blob
// from the source's `GET /api/v1/peers/blobs/` and persists it
// locally before the Hub flips `complete`.
//
// Route: POST /api/v1/peers/pull
//
// Auth: RolePeer (the orchestrator's Ed25519 signature) OR
// RoleOwner. RolePeer is the production path — the orchestrator
// on the Hub signs as its own peer identity; the target verifies
// against its peer_registry row for the Hub. RoleOwner is the
// monolith / drill path where a local operator pokes the target
// directly without inter-peer auth.
//
// Body:
//
//	{
//	  "source_device_id": "<hub's device_id>",
//	  "items": [ {"uri": "kojo://global/agents/<id>/...", "expected_sha256": "<hex>"}, ... ]
//	}
//
// Note: source_address is NOT taken from the request. The target
// resolves the dial URL from its own peer_registry row for
// source_device_id (Name field). A request-supplied address would
// let a registered peer redirect the pull at a peer of its
// choosing — exactly the abuse Codex review flagged.
//
// Response (200):
//
//	{ "results": [ {uri, status, sha256, size, error}, ... ] }
//
// status values: "ok" | "error" | "sha256_mismatch" | "http_status".
//
// 500 with `{"error":{"code":"pull_fatal",...}, "results":[...]}`
// is returned when the pull batch hit a local-fatal error
// (context cancel, sign failure, build failure). The orchestrator
// uses the non-200 to decide on abort; partial results are still
// echoed so an operator can audit which URIs landed.

const peerPullMaxBody = 1 << 20 // 1 MiB — generous URI list cap

type peerPullItem struct {
	URI            string `json:"uri"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
}

type peerPullRequest struct {
	SourceDeviceID string         `json:"source_device_id"`
	Items          []peerPullItem `json:"items"`
}

type peerPullResponse struct {
	Results []peer.PullResult `json:"results"`
}

func (s *Server) handlePeerPull(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.IsPeer() && !p.IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden",
			"peer or owner principal required")
		return
	}
	if s.blob == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"local blob store not configured")
		return
	}
	if s.peerID == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"local peer identity not configured")
		return
	}
	if s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"peer_registry store not configured")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, peerPullMaxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"read body: "+err.Error())
		return
	}
	var req peerPullRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"invalid json: "+err.Error())
		return
	}
	if req.SourceDeviceID == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"source_device_id required")
		return
	}
	if req.SourceDeviceID == s.peerID.DeviceID {
		// Pulling from ourselves is a no-op at best and a
		// livelock (handler dials its own listener) at worst.
		writeError(w, http.StatusBadRequest, "bad_request",
			"source_device_id must not equal the local peer")
		return
	}
	// Caller-source identity binding: a RolePeer signature only
	// authorizes a pull whose declared source matches the
	// signer. Without this check, a registered peer A could ask
	// us to pull arbitrary URIs from a third peer B and surface
	// any of B's blobs to itself by reading our local store —
	// the §3.7 trust model assumes the orchestrator IS the
	// source.
	if p.IsPeer() && p.PeerID != req.SourceDeviceID {
		writeError(w, http.StatusForbidden, "forbidden",
			"signer peer device_id does not match request source_device_id")
		return
	}

	// Resolve the source's dial address from OUR peer_registry
	// row — never from the request body. The signer already
	// proved control of source_device_id; trusting them to also
	// name their own address widens the attack surface (a
	// malicious peer could redirect us at an arbitrary URL).
	srcRec, err := s.agents.Store().GetPeer(r.Context(), req.SourceDeviceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "bad_request",
				"source_device_id not in peer_registry: "+req.SourceDeviceID)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal",
			"peer_registry lookup: "+err.Error())
		return
	}
	srcAddr, err := peer.NormalizeAddress(srcRec.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"source peer has no usable dial name in peer_registry: "+err.Error())
		return
	}

	if len(req.Items) == 0 {
		writeJSONResponse(w, http.StatusOK,
			peerPullResponse{Results: []peer.PullResult{}})
		return
	}

	items := make([]peer.PullItem, 0, len(req.Items))
	for i, it := range req.Items {
		// RolePeer (production orchestrator path) MUST stamp
		// the orchestrator-authoritative sha256 on each item.
		// Without it the target falls back to "trust the
		// source's X-Kojo-Blob-SHA256 header" — exactly the
		// weakness Codex review flagged. Owner (drill / monolith
		// path) is allowed to leave the digest blank.
		if p.IsPeer() && !looksLikeHex64(it.ExpectedSHA256) {
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("items[%d].expected_sha256 must be 64-char hex for peer-signed pulls", i))
			return
		}
		// hex.EncodeToString downstream produces lowercase; an
		// uppercase digest from the caller would otherwise
		// false-mismatch at atomicStage's case-sensitive compare.
		items = append(items, peer.PullItem{
			URI:            it.URI,
			ExpectedSHA256: strings.ToLower(it.ExpectedSHA256),
		})
	}

	client := peer.NewPullClient(s.peerID, nil, s.logger)
	src := peer.PullSource{DeviceID: req.SourceDeviceID, Address: srcAddr}

	// Bound the batch with our own context so a request body
	// containing thousands of items can't pin the handler
	// indefinitely if the parent context is the loose
	// request-scoped one.
	ctx, cancel := context.WithTimeout(r.Context(), peerPullBatchTimeout)
	defer cancel()

	results, err := client.PullMany(ctx, src, items, s.blob)
	if err != nil {
		// Local-fatal (context cancel, sign failure, etc.).
		// Surface as 500 with the partial result list so the
		// orchestrator's HTTP-failure branch fires and the
		// switch is rolled back via abort. Don't pretend success
		// just because we got SOME bodies down.
		s.logger.Warn("peer pull batch hit fatal error",
			"source", req.SourceDeviceID, "items", len(items),
			"got", len(results), "err", err)
		writeJSONResponse(w, http.StatusInternalServerError, struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Results []peer.PullResult `json:"results"`
		}{
			Error: struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}{Code: "pull_fatal", Message: err.Error()},
			Results: results,
		})
		return
	}
	writeJSONResponse(w, http.StatusOK, peerPullResponse{Results: results})
}

// peerPullBatchTimeout bounds one batch of pull dispatches. The
// orchestrator side has its own 5-minute ceiling
// (switchDeviceOpTimeout); this is the inner bound that protects
// the handler when the orchestrator's deadline is loose.
const peerPullBatchTimeout = 4 * time.Minute
