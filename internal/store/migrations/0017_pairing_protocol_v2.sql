-- 0017_pairing_protocol_v2.sql
--
-- Bumps the peer pairing protocol from v1 (Bearer + Ed25519
-- public_key) to v2 (Tailscale NodeKey-only). v1 was retired in
-- commit 0b6a5ff but no schema-level marker was added to refuse
-- v1 rows, so a Hub upgraded to v2 kept honouring pre-existing
-- peer_registry rows that were minted under the old contract —
-- producing 403s on the §3.7 inter-peer surface when the cached
-- NodeKey did not match the current tsnet identity.
--
-- This migration wipes every row in peer_registry and peer_pending
-- so both sides re-handshake from scratch under the v2 contract.
-- The local self-row is reseeded at the next kojo boot by
-- peer.Registrar.Start; the Hub row on peer-mode hosts is
-- repopulated by peer.Discovery's first hub-info tick. No operator
-- action is required beyond restarting kojo on both ends.
--
-- The wire surface (hub-info / join-request) carries a
-- `protocolVersion` field starting with this release. A peer that
-- fetches hub-info from a binary built before the field existed
-- (or any Hub whose version disagrees) wipes its local
-- peer_registry row for that Hub before retrying — that prevents a
-- stale row minted under the wrong auth contract from being
-- consulted later when the Hub dials back in. The Hub side
-- symmetrically rejects /join-request bodies whose protocolVersion
-- does not match, so neither end can keep a half-paired record
-- across the v1→v2 boundary.

DELETE FROM peer_registry;
DELETE FROM peer_pending;
