-- 0014_peer_pending_node_key.sql
--
-- Replaces peer_pending.join_secret_hash with node_key. The join
-- secret was the per-request credential the Bearer flow handed out
-- on first contact so subsequent /join-request polls could prove
-- they were the original requester. With docs/peer-tsnet-identity.md
-- the credential is now the Tailscale NodeKey itself (read off the
-- incoming request via LocalClient.WhoIs), so the secret column
-- becomes redundant. The handler stamps node_key on insert; a
-- repeat /join-request with the same device_id but a different
-- NodeKey is rejected with 409 instead of silently overwriting.
ALTER TABLE peer_pending DROP COLUMN join_secret_hash;
ALTER TABLE peer_pending ADD COLUMN node_key TEXT NOT NULL DEFAULT '';
-- Same race-proof backstop as peer_registry: one NodeKey can have
-- at most one pending row. Empty NodeKey skipped so rows that
-- predate this column (or that came in via --unsafe) don't collide.
CREATE UNIQUE INDEX IF NOT EXISTS idx_peer_pending_node_key
    ON peer_pending(node_key)
    WHERE node_key != '';
