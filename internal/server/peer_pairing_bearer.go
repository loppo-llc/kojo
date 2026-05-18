package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// peerPkgOutBearerNS is the source-of-truth kv namespace for outbound
// Bearers — both Hub→peer and peer→Hub use the same row shape. The
// peer package owns the constant so the to-be-migrated SignRequest
// callers can pull values via a single helper without duplicating the
// namespace string.
const peerPkgOutBearerNS = peer.OutBearerNS

// suppress unused-import errors when only the alias above is used.
var _ = peer.OutBearerNS

// Pairing-time Bearer issuance, the docs/peer-simplify-plan.md step-4 wiring
// that links handleApprovePeerPending and the /join-request poll handlers
// to the peer_tokens store. The flow lives in its own file because it
// straddles two paths (approve + poll) and the kv stash semantics are
// easier to reason about in one place.

const (
	// pairingBearerStashNS is the kv namespace the approve handler writes
	// the (raw_A, raw_B) pair to so the next /join-request poll for the
	// same device_id can hand them over. machine-scoped — these are this
	// host's secret to deliver and must never replicate.
	pairingBearerStashNS = "peer/pairing_bearer_stash"
	// pairingHubOutNS aliases the unified peer.OutBearerNS so the
	// pairing handler keeps its single point of contact with the kv
	// namespace. Hub-side: stores the raw Bearer we present in
	// `Authorization: Bearer …` when calling each paired peer,
	// keyed by peer device_id. machine-scoped, plaintext — TLS is
	// the on-wire boundary; a KEK-wrapped row would still be
	// reconstructable on every send. Future hardening (KEK +
	// decrypt-on-use) is tracked alongside the blob-capability
	// signing key (plan step 7).
	pairingHubOutNS = peerPkgOutBearerNS
)

// stashedPairingBearers is the JSON envelope kv writes from approve
// and reads from the poll handler.
//
// CRITICAL: NEVER persist the raw peer→Hub Bearer here. That raw
// value, once minted, is the peer's permanent credential against
// the Hub — a Hub DB read-only leak between approve and the peer's
// first authenticated poll would otherwise hand an attacker a
// reusable credential. Instead we record a "mint pending" flag and
// defer the actual mint to the authenticated poll, where the raw
// value goes straight onto the response wire and only the hash
// lands in peer_tokens. The Hub→peer Bearer raw still lives here
// (Hub-side credential, needs the raw to dial peer) but that's
// scoped to Hub compromise, which is already cluster-fatal in this
// threat model.
type stashedPairingBearers struct {
	// JoinSecretHash is sha256(raw join_secret) inherited from the
	// peer_pending row at approve time. peer_pending is deleted by
	// ApprovePeerPending, so without this carry-over the join
	// secret would have nowhere to live during the first
	// authenticated poll (the peer still holds the raw and uses it
	// as its Bearer). callerHoldsJoinIdentity checks both
	// peer_pending.join_secret_hash and this field.
	JoinSecretHash string `json:"join_secret_hash"`
	// ActivePeerBearerHash tracks the most-recent peer→Hub Bearer
	// hash this stash minted, so the next attach can revoke it
	// before issuing a fresh raw value. The raw is never stored;
	// every authenticated poll mints a new one + ships it on the
	// wire, the prior hash is revoked. Result: a dropped first
	// response loses the raw forever (peer didn't see it), but a
	// retried poll mints a new one and the old hash is revoked, so
	// the peer ends up with whatever the LATEST successful response
	// shipped (Codex review critical: ACK-resilient delivery).
	ActivePeerBearerHash string `json:"active_peer_bearer_hash,omitempty"`
	// HubBearer is the raw Hub→peer Bearer (Hub-side credential,
	// needed for outbound dial path). Stays in OutBearerNS for
	// runtime use; carried in stash so re-attach can re-deliver
	// the same raw to a peer that lost its first response.
	HubBearer string `json:"hub_bearer"`
}

// mintAndStashPairingBearers is called from handleApprovePeerPending right
// after the registry promotion. It:
//
//  1. Mints Token A and inserts its hash into peer_tokens with role
//     peer_to_hub. The raw value goes into the kv stash for the peer to
//     pick up on its next poll. Hub-side BearerPeerMiddleware will hash
//     the inbound Authorization header and look it up.
//  2. Mints Token B (raw only) and stores it in the Hub-side
//     pairingHubOutNS kv keyed by peer device_id; this is the Bearer
//     this Hub presents when calling THIS peer. The peer side stamps
//     the hash via StorePeerTokenHash when it receives raw_B.
//  3. Writes both raw values to the pairing-bearer stash so the next
//     /join-request poll hands them over once and then deletes the
//     stash.
//
// Errors at any step roll back the whole sequence — partial state would
// leave the Hub thinking it had a working Bearer pair when the peer
// has not yet received them.
func (s *Server) mintAndStashPairingBearers(ctx context.Context, peerDeviceID, joinSecretHash string) error {
	if s == nil || s.agents == nil || s.agents.Store() == nil {
		return errors.New("peer-pairing: store not initialized")
	}
	if peerDeviceID == "" {
		return errors.New("peer-pairing: peer device_id required")
	}
	st := s.agents.Store()

	// Hub → peer raw. Hub keeps the raw in OutBearerNS (needs it to
	// dial peer) and a copy in the stash for re-delivery on retried
	// authenticated polls.
	rawB, err := store.MintPeerTokenRaw()
	if err != nil {
		return fmt.Errorf("mint hub→peer token: %w", err)
	}
	outRec := &store.KVRecord{
		Namespace: pairingHubOutNS,
		Key:       peerDeviceID,
		Value:     rawB,
		Type:      store.KVTypeString,
		Scope:     store.KVScopeMachine,
	}
	if _, err := st.PutKV(ctx, outRec, store.KVPutOptions{}); err != nil {
		return fmt.Errorf("persist hub→peer raw: %w", err)
	}

	// Peer→Hub Bearer is minted lazily per authenticated poll (see
	// attachPairingBearers). The stash carries the join_secret_hash
	// so callerHoldsJoinIdentity can verify the peer's join_secret
	// after peer_pending is dropped by Approve.
	stash := stashedPairingBearers{JoinSecretHash: joinSecretHash, HubBearer: rawB}
	body, _ := json.Marshal(stash)
	stashRec := &store.KVRecord{
		Namespace: pairingBearerStashNS,
		Key:       peerDeviceID,
		Value:     string(body),
		Type:      store.KVTypeJSON,
		Scope:     store.KVScopeMachine,
	}
	if _, err := st.PutKV(ctx, stashRec, store.KVPutOptions{}); err != nil {
		_ = st.DeleteKV(ctx, pairingHubOutNS, peerDeviceID, "")
		return fmt.Errorf("stash pairing bearers: %w", err)
	}
	return nil
}

// loadStash returns the parsed stash for device_id, or nil when no
// row / corrupt JSON. Single read point so attach + ACK + secret-
// hash lookup share the same parsing.
func (s *Server) loadStash(ctx context.Context, peerDeviceID string) *stashedPairingBearers {
	if s == nil || s.agents == nil || s.agents.Store() == nil {
		return nil
	}
	rec, err := s.agents.Store().GetKV(ctx, pairingBearerStashNS, peerDeviceID)
	if err != nil {
		return nil
	}
	var stash stashedPairingBearers
	if err := json.Unmarshal([]byte(rec.Value), &stash); err != nil {
		return nil
	}
	return &stash
}

// attachPairingBearers mints a FRESH peer→Hub Bearer on every call
// and ships the raw on the response wire. The prior attempt's hash
// (if any) is revoked first, so a peer that lost its earlier
// response ends up authenticated with the latest fresh raw on the
// retry (Codex review critical: ACK-resilient delivery without
// persisting any raw on Hub disk).
//
// The HubBearer raw is re-attached on every retry from the stash
// (Hub already needs to hold it in OutBearerNS for its outbound
// dial path, so this is the same secret it already keeps anyway).
//
// The stash itself is NOT deleted on attach. consumePairingStashOnAck
// removes it once the peer presents its permanent peer→Hub Bearer
// as Authorization (= proof the prior response landed).
func (s *Server) attachPairingBearers(ctx context.Context, peerDeviceID string, resp *joinRequestResponse) {
	if s == nil || s.agents == nil || s.agents.Store() == nil || resp == nil {
		return
	}
	st := s.agents.Store()
	stash := s.loadStash(ctx, peerDeviceID)
	if stash == nil {
		return
	}
	// Revoke the prior attempt's hash if one exists. Idempotent —
	// revoking a hash we never wrote returns ErrNotFound which we
	// quietly absorb (the row was deleted by RevokePeerTokensByDevice
	// or never existed).
	if stash.ActivePeerBearerHash != "" {
		_ = st.RevokePeerTokenByHash(ctx, stash.ActivePeerBearerHash)
	}
	// Mint a fresh peer→Hub Bearer. The raw goes into the response;
	// only the hash lands in peer_tokens. If mint fails we bail
	// without touching the stash — the next poll retries.
	issued, err := st.IssuePeerToken(ctx, peerDeviceID, store.PeerTokenRolePeerToHub)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("peer-pairing: peer→hub mint failed",
				"device_id", peerDeviceID, "err", err)
		}
		return
	}
	resp.PeerBearer = issued.Raw
	if stash.HubBearer != "" {
		resp.HubBearer = stash.HubBearer
	}
	// Persist the latest hash so the NEXT poll knows what to revoke.
	stash.ActivePeerBearerHash = issued.Record.TokenHash
	flipped, _ := json.Marshal(*stash)
	_, _ = st.PutKV(ctx, &store.KVRecord{
		Namespace: pairingBearerStashNS,
		Key:       peerDeviceID,
		Value:     string(flipped),
		Type:      store.KVTypeJSON,
		Scope:     store.KVScopeMachine,
	}, store.KVPutOptions{})
}

// consumePairingStashOnAck removes the stash row. Called when the
// peer presents its permanent peer→Hub Bearer (callerHoldsPeerBearer)
// — that's the implicit ACK that the previous poll's response
// landed and the peer is now operating on the permanent credential.
// The HubBearer raw stays in OutBearerNS (Hub uses it to dial peer);
// only the delivery side-channel (the stash, which carried the
// join_secret_hash and the active peer→Hub Bearer hash) is cleared.
func (s *Server) consumePairingStashOnAck(ctx context.Context, peerDeviceID string) {
	if s == nil || s.agents == nil || s.agents.Store() == nil {
		return
	}
	st := s.agents.Store()
	if err := st.DeleteKV(ctx, pairingBearerStashNS, peerDeviceID, ""); err != nil && s.logger != nil {
		s.logger.Warn("peer-pairing: stash ack-delete failed",
			"device_id", peerDeviceID, "err", err)
	}
}

// loadHubOutBearer fetches the Hub's outbound Bearer for a given peer.
// Thin wrapper over peer.LoadOutboundBearer kept for the server-only
// callers that already hold an *Server receiver.
func (s *Server) loadHubOutBearer(ctx context.Context, peerDeviceID string) (string, error) {
	if s == nil || s.agents == nil || s.agents.Store() == nil {
		return "", errors.New("peer-pairing: store not initialized")
	}
	return peer.LoadOutboundBearer(ctx, s.agents.Store(), peerDeviceID)
}

// mintJoinSecret returns a fresh 256-bit base64 secret. The raw
// value is returned ONCE to the caller (in the /join-request
// response) and never persisted on Hub — only sha256(raw) lands on
// disk, in peer_pending.join_secret_hash (Codex review critical:
// raw kv leak would otherwise let a DB reader claim Bearer pairs).
func mintJoinSecret() (string, error) {
	return store.MintPeerTokenRaw()
}

// joinSecretHashForDevice loads sha256(raw_join_secret) for the
// given device_id, checking both peer_pending.join_secret_hash
// (pre-approval) and the stash's carry-over (post-approval, before
// the peer has acked with its permanent Bearer). Empty when no
// row matches either source.
func (s *Server) joinSecretHashForDevice(ctx context.Context, deviceID string) string {
	if s == nil || s.agents == nil || s.agents.Store() == nil {
		return ""
	}
	if rec, err := s.agents.Store().GetPeerPending(ctx, deviceID); err == nil && rec.JoinSecretHash != "" {
		return rec.JoinSecretHash
	}
	if stash := s.loadStash(ctx, deviceID); stash != nil && stash.JoinSecretHash != "" {
		return stash.JoinSecretHash
	}
	return ""
}

// extractBearerFromRequest pulls the raw Bearer out of an HTTP
// request's Authorization header. Returns "" when missing or shaped
// differently (e.g. X-Kojo-Token, basic).
func extractBearerFromRequest(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) <= len(prefix) {
		return ""
	}
	if !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// callerHoldsJoinIdentity returns true when the Authorization header
// presents EITHER the per-join secret OR the permanent peer→Hub
// Bearer for device_id. Hash comparison only — raw secrets never
// touch Hub disk (Codex review critical).
func (s *Server) callerHoldsJoinIdentity(ctx context.Context, deviceID string, r *http.Request) bool {
	if s == nil || s.agents == nil || s.agents.Store() == nil || deviceID == "" {
		return false
	}
	presented := extractBearerFromRequest(r)
	if presented == "" {
		return false
	}
	if storedHash := s.joinSecretHashForDevice(ctx, deviceID); storedHash != "" {
		if store.HashPeerToken(presented) == storedHash {
			return true
		}
	}
	tok, err := s.agents.Store().ResolvePeerToken(ctx, presented)
	if err == nil && tok.DeviceID == deviceID && tok.Role == store.PeerTokenRolePeerToHub {
		return true
	}
	return false
}

// callerHoldsPeerBearer returns true ONLY when Authorization carries
// a valid peer→Hub Bearer for device_id (not the pre-approval join
// secret). Used by the processJoinRequest approved-branch URL update
// gate, which must require the permanent credential — accepting the
// join_secret there would let an attacker who scraped the secret
// from the first /join-request response retain the ability to
// overwrite the peer's URL post-approval.
func (s *Server) callerHoldsPeerBearer(r *http.Request, deviceID string) bool {
	if s == nil || s.agents == nil || s.agents.Store() == nil || deviceID == "" {
		return false
	}
	presented := extractBearerFromRequest(r)
	if presented == "" {
		return false
	}
	tok, err := s.agents.Store().ResolvePeerToken(r.Context(), presented)
	if err != nil {
		return false
	}
	return tok.DeviceID == deviceID && tok.Role == store.PeerTokenRolePeerToHub
}
