package peer

// PairingProtocolVersion is the wire version of the peer pairing
// protocol (hub-info + join-request). Bumped whenever the auth /
// identity contract changes in a way that makes a registry row
// minted under the prior version unsafe to honour.
//
// History:
//
//	v1 — Bearer-token issuance + Ed25519 public_key in peer_registry.
//	     Retired in commit 0b6a5ff (NodeKey-only auth).
//	v2 — Tailscale NodeKey-only. Identity is bound exclusively via
//	     tsnet WhoIs; no shared secrets cross the wire.
//
// Bumping this constant MUST be paired with a migration that wipes
// peer_registry / peer_pending so peers that previously paired under
// the old version cannot accidentally re-attach under the new auth
// contract. See migration 0017_pairing_protocol_v2.sql.
const PairingProtocolVersion = 2
