package peer

import (
	"context"
	"errors"
	"net/http"

	"github.com/loppo-llc/kojo/internal/store"
)

// ErrNoOutboundBearer is returned by LoadOutboundBearer when the kv has no
// row for the requested peer. Callers map this to "fall back to the legacy
// signing path" during the dual-stack window (docs/peer-simplify-plan.md);
// after step 9 deletes signing, the same condition becomes a hard 401 on
// the receiver side.
var ErrNoOutboundBearer = errors.New("peer: no outbound Bearer for target")

// LoadOutboundBearer reads the raw Bearer this kojo uses when calling
// target peer `peerDeviceID`. Returns ErrNoOutboundBearer for missing rows
// so the caller can distinguish "not paired with Bearer yet" from generic
// kv failure.
//
// Single source of truth for the OutBearerNS namespace: every outbound
// call site that needs Authorization should route through this helper
// rather than reach into kv directly.
func LoadOutboundBearer(ctx context.Context, st *store.Store, peerDeviceID string) (string, error) {
	if st == nil {
		return "", errors.New("peer.LoadOutboundBearer: nil store")
	}
	if peerDeviceID == "" {
		return "", errors.New("peer.LoadOutboundBearer: peer device_id required")
	}
	rec, err := st.GetKV(ctx, OutBearerNS, peerDeviceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", ErrNoOutboundBearer
		}
		return "", err
	}
	if rec.Value == "" {
		return "", ErrNoOutboundBearer
	}
	return rec.Value, nil
}

// AttachOutboundBearer stamps `Authorization: Bearer <token>` on req,
// loading the raw token from kv on the fly. Returns ErrNoOutboundBearer
// when no row exists so the caller can decide whether to fall back to the
// legacy Ed25519 signing path or fail closed.
//
// The helper does NOT clear any existing Authorization header — callers
// that mix peer Bearer with other auth flows on the same client are
// presumed to know what they're doing. In practice every caller in
// internal/server / internal/peer builds a fresh *http.Request, so this
// detail rarely matters.
func AttachOutboundBearer(ctx context.Context, st *store.Store, req *http.Request, peerDeviceID string) error {
	if req == nil {
		return errors.New("peer.AttachOutboundBearer: nil request")
	}
	raw, err := LoadOutboundBearer(ctx, st, peerDeviceID)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	return nil
}

// AuthorizeOutbound stamps the `Authorization: Bearer …` header on req,
// loading the raw token from kv via AttachOutboundBearer. Returns
// ErrNoOutboundBearer when no Bearer is paired for peerDeviceID so the
// caller can surface a clean failure — there is no longer a signing
// fallback (the Ed25519 signing path was deleted in step 9 of
// docs/peer-simplify-plan.md).
//
// A nil store handle is treated the same as a missing Bearer.
func AuthorizeOutbound(ctx context.Context, st *store.Store, req *http.Request, peerDeviceID string) error {
	if st == nil {
		return ErrNoOutboundBearer
	}
	return AttachOutboundBearer(ctx, st, req, peerDeviceID)
}
