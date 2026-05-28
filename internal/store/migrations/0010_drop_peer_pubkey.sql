-- 0010_drop_peer_pubkey.sql
--
-- Finalises docs/peer-simplify-plan.md: the Ed25519 signing layer is
-- gone (step 9), so the public_key columns and the kv rows that backed
-- the per-peer keypair carry no behaviour. Drop them in one migration
-- so a fresh install never sees the schema, and existing installs lose
-- the stale data.
--
-- SQLite ≥ 3.35 supports `ALTER TABLE ... DROP COLUMN` directly — modernc.org/sqlite
-- bundles 3.45+ so the simple statements work on every platform kojo runs on.

ALTER TABLE peer_registry DROP COLUMN public_key;
ALTER TABLE peer_registry DROP COLUMN capabilities;
ALTER TABLE peer_pending DROP COLUMN public_key;

-- Clear the kv rows that stored the local peer's keypair. Use IN over
-- two DELETEs so the namespace constraint is checked once and the WAL
-- carries a single transaction.
DELETE FROM kv WHERE namespace = 'peer' AND key IN ('public_key', 'private_key');
