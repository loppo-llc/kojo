-- 0013_peer_registry_node_key.sql
--
-- Adds peer_registry.node_key so the Hub/peer can bind each row to
-- a Tailscale stable NodeKey. After docs/peer-tsnet-identity.md, this
-- column is the primary credential — the BearerPeerMiddleware /
-- peer_tokens path is retired in a later migration. Nullable so the
-- column lands cleanly on an existing cluster; rows imported before
-- this migration carry NULL until the next join-request re-stamps
-- them. A NULL node_key means "identity not yet observed"; the
-- tsnet identity middleware refuses to admit such a row.
ALTER TABLE peer_registry ADD COLUMN node_key TEXT;
-- Partial UNIQUE index: a single NodeKey can be bound to at most ONE
-- device_id. NULL / empty values are deliberately excluded so rows
-- imported from the pre-migration schema (node_key not yet observed)
-- don't all collide on the empty-string sentinel. The handler-level
-- collision check (GetPeerByNodeKey) is therefore an early-reject
-- convenience; this UNIQUE constraint is the race-proof backstop.
CREATE UNIQUE INDEX IF NOT EXISTS idx_peer_registry_node_key
    ON peer_registry(node_key)
    WHERE node_key IS NOT NULL AND node_key != '';
