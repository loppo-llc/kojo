package server

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/store"
)

// Hub-side ingest endpoint for agent-attachment forwarding from a
// non-hub peer. The kojo-attach flow on a peer-mode daemon writes
// the attachment bytes into its local blob store and then pushes a
// copy here so the user-facing UI (which always runs on hub) can
// download the file through the standard
// /api/v1/blob/{scope}/{path} surface.
//
// Route: PUT /api/v1/peers/blobs-ingest/{scope}/{path...}
//
// Auth: RolePeer (Ed25519-signed) or RoleOwner. EnforceMiddleware
// gates the prefix down to PUT only; the policy layer additionally
// requires peer_registry.trusted=true so a stolen-but-untrusted
// peer credential cannot scribble into the hub blob store.
//
// Body: raw blob bytes. The handler enforces an
// X-Kojo-Expected-SHA256 header so the hub never accepts bytes
// whose digest disagrees with what the peer claims it shipped —
// this is the only end-to-end integrity check we have for the
// push direction (the peer-side signature only covers the request
// envelope, not the body).
//
// The handler reuses s.blob.Put with the supplied ExpectedSHA256.
// Put hashes the body in-stream and aborts pre-rename on mismatch,
// so a corrupted upload never lands on disk. Existing rows are
// overwritten silently (the URI is fully scoped per-agent /
// per-message / per-filename so a real overwrite only happens when
// the peer is retrying the same forward — idempotent).

const peerBlobIngestPrefix = "/api/v1/peers/blobs-ingest/"

// peerBlobIngestPath gates the (scope, path) accepted by the
// ingest handler down to the kojo-attach contract:
//
//	scope = global
//	path  = agents/<agentID>/attach/<messageID>/<filename>
//
// Both id segments are alnum + underscore + hyphen + dot and ≤64
// chars; filename adds a 200-char cap matching
// sanitizeAttachBasename. Anything else 400s before the blob
// store sees it — without this gate a trusted peer could PUT
// over an unrelated `kojo://global/agents/<id>/avatar.png` row
// or a future scope's blob and the handler would silently
// overwrite it.
//
// This deliberately rejects "/" inside the filename segment so a
// rogue peer can't smuggle an `agents/x/attach/m/../../avatar.png`
// past the regex.
var peerBlobIngestPath = regexp.MustCompile(
	`^agents/([A-Za-z0-9_.\-]{1,64})/attach/([A-Za-z0-9_.\-]{1,64})/([^/\x00]{1,200})$`,
)

func (s *Server) handlePeerBlobIngest(w http.ResponseWriter, r *http.Request) {
	if s.blob == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"blob store not configured")
		return
	}
	p := auth.FromContext(r.Context())
	if !p.IsPeer() && !p.IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden",
			"peer or owner principal required")
		return
	}
	if !strings.HasPrefix(r.URL.Path, peerBlobIngestPrefix) {
		writeError(w, http.StatusBadRequest, "bad_request", "unexpected path")
		return
	}
	rest := r.URL.Path[len(peerBlobIngestPrefix):]
	if rest == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "scope/path required")
		return
	}
	// Split on the first "/" so a scope segment like "global"
	// pairs with everything after. ParseURI would also work but
	// requires the kojo:// prefix; we avoid the round-trip.
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "path required after scope")
		return
	}
	scopeStr := rest[:slash]
	blobPath := rest[slash+1:]
	scope := blob.Scope(scopeStr)
	if !scope.Valid() {
		writeError(w, http.StatusBadRequest, "invalid_scope", "invalid scope")
		return
	}
	// Lock the ingest to the kojo-attach contract. Without this a
	// peer-signed PUT could overwrite avatars, books, or any other
	// blob_refs row under /agents/<id>/. Per-contract gating is the
	// only namespace check we have — the policy layer doesn't know
	// which {scope, path} a given peer is allowed to write to.
	if scope != blob.ScopeGlobal {
		writeError(w, http.StatusBadRequest, "scope_not_allowed",
			"peer blob ingest is limited to scope=global")
		return
	}
	matches := peerBlobIngestPath.FindStringSubmatch(blobPath)
	if matches == nil {
		writeError(w, http.StatusBadRequest, "path_not_allowed",
			"peer blob ingest path must be agents/<agentID>/attach/<messageID>/<filename>")
		return
	}
	pathAgentID := matches[1]

	// Agent-ownership gate. The trust-bit gate in policy.go was
	// intentionally bypassed for this surface (paired-but-untrusted
	// peers — the common case for `--peer-add` without
	// `--peer-add-trusted` — need to push attachments for their own
	// agents). The handler-side guarantee that replaces it: the
	// RolePeer signer MUST currently hold the agent_lock for the
	// agent the URI references. That ties an arbitrary-blob-write
	// vector to "you are the running runtime for this agent", which
	// is the only legitimate caller of this surface anyway.
	//
	// Operator (RoleOwner) bypasses the lock check because a manual
	// `kojo` CLI run that pushes blobs is a recovery / migration
	// path that should not depend on agent_locks state. `p` was
	// fetched at the top of the handler for the principal-role
	// check; reuse the same value here.
	if p.IsPeer() && s.agents != nil && s.agents.Store() != nil {
		lock, lockErr := s.agents.Store().GetAgentLock(r.Context(), pathAgentID)
		switch {
		case lockErr == nil:
			if lock.HolderPeer != p.PeerID {
				writeError(w, http.StatusForbidden, "not_lock_holder",
					"signer is not the current agent_lock holder for "+pathAgentID)
				return
			}
			// NOTE: deliberately no wall-clock lease_expires_at
			// check here. Each peer refreshes its OWN local
			// agent_locks row; hub's row is only re-stamped on
			// §3.7 device-switch sync. Enforcing lease expiry
			// against the hub row would 403 every legitimate
			// remote attachment once the snapshotted lease aged
			// past hub's clock (typically ~1 minute), even
			// though the holder peer's own row is still being
			// refreshed normally. The holder_peer signature
			// match above remains the authoritative gate.
		case errors.Is(lockErr, store.ErrNotFound):
			// No lock row at all means no peer can legitimately
			// claim to be running this agent. Refuse — opening
			// the surface to a no-lock state would let any
			// paired peer publish into any agent's namespace.
			writeError(w, http.StatusForbidden, "no_agent_lock",
				"no agent_lock row exists for "+pathAgentID+"; refuse peer ingest")
			return
		default:
			writeError(w, http.StatusInternalServerError, "internal",
				"agent_locks lookup: "+lockErr.Error())
			return
		}
	}

	// Mandatory digest header. A peer push without a pre-declared
	// digest would leave us with no way to detect a body that was
	// truncated or substituted between the peer's Put and the hub
	// arrival; refuse rather than store unverified bytes. Extracted
	// here so the existing-row sha256 conflict check below can
	// compare against the declared digest.
	want := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Kojo-Expected-SHA256")))
	if want == "" {
		writeError(w, http.StatusBadRequest, "sha256_required",
			"X-Kojo-Expected-SHA256 is required for peer-pushed blobs")
		return
	}
	if !validHexSHA256(want) {
		writeError(w, http.StatusBadRequest, "invalid_expected_sha256",
			"X-Kojo-Expected-SHA256 must be 64 hex characters")
		return
	}

	// Existing-row guard. The ingest path embeds (agentID,
	// messageID, filename) so the only legitimate collision is an
	// idempotent retry of the same forward by the same signer with
	// the same body. We enforce all three conditions:
	//
	//   1. ref.HandoffPending must be false (mid-handoff rows are
	//      off-limits so a peer can't race the §3.7 state machine).
	//   2. ref.HomePeer == signer.PeerID OR ref.HomePeer == hub-self
	//      (a hub-stored row from a prior accepted ingest re-PUTs OK).
	//   3. ref.SHA256 == client-declared X-Kojo-Expected-SHA256
	//      (refuse to overwrite the same URI with different bytes —
	//      that's the smuggle vector even when the signer matches,
	//      e.g. a compromised peer trying to swap a chart for a
	//      different image after the first turn rendered it).
	if s.agents != nil && s.agents.Store() != nil {
		uri := blob.BuildURI(scope, blobPath)
		ref, refErr := s.agents.Store().GetBlobRef(r.Context(), uri)
		switch {
		case refErr == nil:
			if ref.HandoffPending {
				writeError(w, http.StatusConflict, "handoff_pending",
					"target row is mid-handoff; refuse peer ingest")
				return
			}
			// Owner can always re-PUT (operator override). For
			// RolePeer, the home_peer must be either the signer
			// (peer pushed it earlier, retrying) OR the hub's
			// own DeviceID (the standard case where a prior
			// ingest stamped home_peer = hub). `p` is the
			// Principal already fetched above for the
			// agent-ownership gate.
			if p.IsPeer() && p.PeerID != "" {
				hubOK := s.peerID != nil && ref.HomePeer == s.peerID.DeviceID
				signerOK := ref.HomePeer == p.PeerID
				if !hubOK && !signerOK {
					writeError(w, http.StatusConflict, "wrong_home",
						"blob already exists with a different home_peer; refuse cross-peer overwrite")
					return
				}
			}
			if ref.SHA256 != "" && !strings.EqualFold(ref.SHA256, want) {
				writeError(w, http.StatusConflict, "sha256_conflict",
					"blob already exists with a different sha256; refuse re-PUT with different body")
				return
			}
		case errors.Is(refErr, store.ErrNotFound):
			// First publish — no row guard to apply.
		default:
			writeError(w, http.StatusInternalServerError, "internal",
				"blob_refs lookup: "+refErr.Error())
			return
		}
	}

	cap := s.blobMaxPutBytes
	if cap <= 0 {
		cap = defaultBlobMaxPutBytes
	}
	body := http.MaxBytesReader(w, r.Body, cap)
	defer body.Close()

	obj, err := s.blob.Put(scope, blobPath, body, blob.PutOptions{
		ExpectedSHA256: want,
		// Do NOT bypass handoff_pending here: the existing-row
		// guard above already returned 409 for mid-handoff rows;
		// leaving the default lets blob.Store.Put double-check
		// the invariant against a row that flipped pending in
		// the gap between our lookup and the write.
	})
	degraded := false
	if err != nil {
		if errors.Is(err, blob.ErrDurabilityDegraded) && obj != nil {
			degraded = true
			s.logger.Warn("peer blob ingest: committed degraded",
				"scope", scope, "path", blobPath, "err", err)
		} else {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
					"body exceeds maximum")
				return
			}
			s.writeBlobErr(w, err)
			return
		}
	}
	w.Header().Set("ETag", blobETagHeader(obj.ETag))
	w.Header().Set("X-Kojo-SHA256", obj.SHA256)
	if degraded {
		w.Header().Set("X-Kojo-Durability", "degraded")
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"scope":  string(obj.Scope),
		"path":   obj.Path,
		"size":   obj.Size,
		"sha256": obj.SHA256,
		"etag":   blobETagHeader(obj.ETag),
	})
}
