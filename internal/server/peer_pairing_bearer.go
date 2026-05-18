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
	// peerJoinSecretNS holds a per-pending-peer one-time secret
	// minted on the FIRST /join-request POST. All subsequent
	// /join-request POSTs and GET polls for that device_id must
	// present the raw secret in Authorization: Bearer until the
	// permanent peer→Hub Bearer is delivered on approve. Without
	// this binding any host that knows a victim's device_id could
	// race-poll /join-request right after approve and capture the
	// Hub→peer / peer→Hub credentials (Codex review P1 finding).
	peerJoinSecretNS = "peer/join_secret"
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
	// NeedsPeerBearerMint is true between approve and the first
	// authenticated poll. When the poll arrives the handler mints
	// the peer→Hub Bearer, ships the raw in the response, stores
	// only the hash in peer_tokens, and flips this flag false.
	NeedsPeerBearerMint bool `json:"needs_peer_bearer_mint"`
	// HubBearer is the raw Hub→peer Bearer. Hub keeps it so it
	// can present `Authorization: Bearer …` when calling the peer.
	// Stays in the stash until the peer presents a permanent
	// peer→Hub Bearer (proof of receipt), at which point the
	// whole stash row can be deleted — the raw also lives in
	// OutBearerNS for Hub's runtime dial path.
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
func (s *Server) mintAndStashPairingBearers(ctx context.Context, peerDeviceID string) error {
	if s == nil || s.agents == nil || s.agents.Store() == nil {
		return errors.New("peer-pairing: store not initialized")
	}
	if peerDeviceID == "" {
		return errors.New("peer-pairing: peer device_id required")
	}
	st := s.agents.Store()

	// Hub → peer raw. Hub keeps the raw in OutBearerNS (needs it to
	// dial peer) and a copy in the stash for delivery on the first
	// authenticated poll.
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

	// Peer → Hub Bearer is DEFERRED. We record a `NeedsPeerBearerMint`
	// flag in the stash; the first authenticated poll mints the
	// Bearer, hashes it into peer_tokens, and ships the raw on the
	// wire — the raw never touches Hub disk. See the file-level
	// comment on stashedPairingBearers for the Codex-flagged
	// rationale.
	stash := stashedPairingBearers{NeedsPeerBearerMint: true, HubBearer: rawB}
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

// attachPairingBearers reads the stash and populates resp.{PeerBearer,
// HubBearer}. The peer→Hub Bearer is minted lazily HERE — the stash
// only carries a NeedsPeerBearerMint flag, so a Hub DB read-only
// leak between approve and the authenticated poll can never yield
// the peer's permanent credential (Codex review critical).
//
// The stash is NOT deleted on attach. ACK-based consumption: the
// stash row stays until the peer presents its permanent peer→Hub
// Bearer on a later call (consumePairingStashOnAck). That way a
// dropped first response leaves the peer recoverable — the next
// authenticated poll re-reads the same stash and re-delivers the
// already-minted credentials (idempotent on the peer side; peer's
// PutKV / StorePeerTokenHash both no-op on identical values).
//
// Errors are logged at Warn (the operator might want to know the
// stash JSON decode broke) but the poll response itself stays
// state=approved; the peer can re-trigger via operator re-approve
// if Bearers never land.
func (s *Server) attachPairingBearers(ctx context.Context, peerDeviceID string, resp *joinRequestResponse) {
	if s == nil || s.agents == nil || s.agents.Store() == nil || resp == nil {
		return
	}
	st := s.agents.Store()
	rec, err := st.GetKV(ctx, pairingBearerStashNS, peerDeviceID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) && s.logger != nil {
			s.logger.Warn("peer-pairing: stash read failed",
				"device_id", peerDeviceID, "err", err)
		}
		return
	}
	var stash stashedPairingBearers
	if jerr := json.Unmarshal([]byte(rec.Value), &stash); jerr != nil {
		if s.logger != nil {
			s.logger.Warn("peer-pairing: stash JSON decode failed",
				"device_id", peerDeviceID, "err", jerr)
		}
		// Drop the corrupt row so a future approve can refresh it.
		_ = st.DeleteKV(ctx, pairingBearerStashNS, peerDeviceID, "")
		return
	}
	if stash.NeedsPeerBearerMint {
		issued, err := st.IssuePeerToken(ctx, peerDeviceID, store.PeerTokenRolePeerToHub)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("peer-pairing: peer→hub mint failed",
					"device_id", peerDeviceID, "err", err)
			}
			return
		}
		resp.PeerBearer = issued.Raw
		stash.NeedsPeerBearerMint = false
		// Persist the flag flip so a retried (same-stash) poll
		// can't double-mint. We deliberately keep HubBearer in
		// the stash so retries still hand it over alongside an
		// empty PeerBearer (the peer has it now and discards
		// duplicate hash inserts via INSERT OR IGNORE).
		flipped, _ := json.Marshal(stash)
		_, _ = st.PutKV(ctx, &store.KVRecord{
			Namespace: pairingBearerStashNS,
			Key:       peerDeviceID,
			Value:     string(flipped),
			Type:      store.KVTypeJSON,
			Scope:     store.KVScopeMachine,
		}, store.KVPutOptions{})
	}
	if stash.HubBearer != "" {
		resp.HubBearer = stash.HubBearer
	}
}

// consumePairingStashOnAck removes the stash + the per-pending
// join_secret. Called when the peer presents its permanent peer→Hub
// Bearer (callerHoldsPeerBearer) — that's the implicit ACK that the
// previous poll's response landed and the peer is now operating on
// the permanent credential. The HubBearer raw stays in OutBearerNS
// (Hub uses it to dial peer); only the delivery side-channel
// (stash + join_secret) is cleaned up.
func (s *Server) consumePairingStashOnAck(ctx context.Context, peerDeviceID string) {
	if s == nil || s.agents == nil || s.agents.Store() == nil {
		return
	}
	st := s.agents.Store()
	if err := st.DeleteKV(ctx, pairingBearerStashNS, peerDeviceID, ""); err != nil && s.logger != nil {
		s.logger.Warn("peer-pairing: stash ack-delete failed",
			"device_id", peerDeviceID, "err", err)
	}
	s.consumeJoinSecret(ctx, peerDeviceID)
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

// mintJoinSecret returns a fresh 256-bit base64 secret. Same shape +
// entropy as MintPeerTokenRaw; broken out so the join-request flow
// can stash a raw value in kv (rather than a hash) without dragging
// in the peer_tokens semantics.
func mintJoinSecret() (string, error) {
	return store.MintPeerTokenRaw()
}

// persistJoinSecret records the raw join_secret keyed by device_id.
// Subsequent calls overwrite (the legit peer never sees its existing
// secret evicted — only the FIRST POST writes, and the corresponding
// peer immediately receives the value in the response).
func (s *Server) persistJoinSecret(ctx context.Context, deviceID, secret string) error {
	if s == nil || s.agents == nil || s.agents.Store() == nil {
		return errors.New("peer-pairing: store not initialized")
	}
	_, err := s.agents.Store().PutKV(ctx, &store.KVRecord{
		Namespace: peerJoinSecretNS,
		Key:       deviceID,
		Value:     secret,
		Type:      store.KVTypeString,
		Scope:     store.KVScopeMachine,
	}, store.KVPutOptions{})
	return err
}

// loadJoinSecret returns the raw secret for device_id, or empty when
// no secret exists (already consumed, or never minted).
func (s *Server) loadJoinSecret(ctx context.Context, deviceID string) string {
	if s == nil || s.agents == nil || s.agents.Store() == nil {
		return ""
	}
	rec, err := s.agents.Store().GetKV(ctx, peerJoinSecretNS, deviceID)
	if err != nil {
		return ""
	}
	return rec.Value
}

// consumeJoinSecret removes the kv row. Called on successful Bearer
// delivery so the secret becomes single-use.
func (s *Server) consumeJoinSecret(ctx context.Context, deviceID string) {
	if s == nil || s.agents == nil || s.agents.Store() == nil {
		return
	}
	_ = s.agents.Store().DeleteKV(ctx, peerJoinSecretNS, deviceID, "")
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
// Bearer for device_id. The /join-request endpoints use this as their
// pre-update / pre-bearer-attach gate: legitimate peer always holds
// one of the two; an attacker who only knows the UUID-shaped
// device_id holds neither.
func (s *Server) callerHoldsJoinIdentity(ctx context.Context, deviceID string, r *http.Request) bool {
	if s == nil || s.agents == nil || s.agents.Store() == nil || deviceID == "" {
		return false
	}
	presented := extractBearerFromRequest(r)
	if presented == "" {
		return false
	}
	if secret := s.loadJoinSecret(ctx, deviceID); secret != "" && presented == secret {
		return true
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
