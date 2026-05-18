package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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

// stashedPairingBearers is the JSON envelope kv writes from approve and
// reads from the poll handler.
type stashedPairingBearers struct {
	PeerBearer string `json:"peer_bearer"` // raw Token A (peer→Hub)
	HubBearer  string `json:"hub_bearer"`  // raw Token B (Hub→peer)
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

	// Token A — peer → Hub. Hub stores the hash here; raw goes to peer.
	issuedA, err := st.IssuePeerToken(ctx, peerDeviceID, store.PeerTokenRolePeerToHub)
	if err != nil {
		return fmt.Errorf("issue peer→hub token: %w", err)
	}

	// Token B — Hub → peer. Raw stays on Hub; hash is sent to peer for
	// it to insert in its own peer_tokens.
	rawB, err := store.MintPeerTokenRaw()
	if err != nil {
		// Best-effort revoke Token A so we don't leave a dangling
		// half-paired credential.
		_ = st.RevokePeerToken(ctx, issuedA.Raw)
		return fmt.Errorf("mint hub→peer token: %w", err)
	}

	// Persist raw Token B in Hub's outbound kv.
	outRec := &store.KVRecord{
		Namespace: pairingHubOutNS,
		Key:       peerDeviceID,
		Value:     rawB,
		Type:      store.KVTypeString,
		Scope:     store.KVScopeMachine,
	}
	if _, err := st.PutKV(ctx, outRec, store.KVPutOptions{}); err != nil {
		_ = st.RevokePeerToken(ctx, issuedA.Raw)
		return fmt.Errorf("persist hub→peer raw: %w", err)
	}

	// Stash both raws for delivery on next poll.
	stash := stashedPairingBearers{PeerBearer: issuedA.Raw, HubBearer: rawB}
	body, _ := json.Marshal(stash)
	stashRec := &store.KVRecord{
		Namespace: pairingBearerStashNS,
		Key:       peerDeviceID,
		Value:     string(body),
		Type:      store.KVTypeJSON,
		Scope:     store.KVScopeMachine,
	}
	if _, err := st.PutKV(ctx, stashRec, store.KVPutOptions{}); err != nil {
		_ = st.RevokePeerToken(ctx, issuedA.Raw)
		_ = st.DeleteKV(ctx, pairingHubOutNS, peerDeviceID, "")
		return fmt.Errorf("stash pairing bearers: %w", err)
	}
	return nil
}

// attachPairingBearers consumes the kv stash, populates resp.{PeerBearer,
// HubBearer}, and deletes the stash so the next poll returns approved
// without re-issuing. A missing stash is the steady-state for subsequent
// polls — leave resp.*Bearer empty and return.
//
// Errors are logged at Warn (the operator might want to know the stash
// JSON decode broke) but the poll response itself stays state=approved;
// the peer can re-trigger via operator re-approve if Bearers never land.
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
	resp.PeerBearer = stash.PeerBearer
	resp.HubBearer = stash.HubBearer
	// Delete the stash so the raw tokens never come back over the
	// wire. A retried poll lands here too, but with an empty stash it
	// produces an empty-bearer response that the peer side treats as a
	// no-op (the peer already persisted on first receipt).
	if err := st.DeleteKV(ctx, pairingBearerStashNS, peerDeviceID, ""); err != nil && s.logger != nil {
		s.logger.Warn("peer-pairing: stash delete failed",
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
