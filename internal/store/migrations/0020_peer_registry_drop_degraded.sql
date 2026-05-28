-- 0020_peer_registry_drop_degraded.sql
--
-- Removes 'degraded' from peer_registry.status CHECK constraint.
-- No producer ever emitted it; the value was speculative dead surface
-- (a "reachable but flaky" detector was contemplated but never built).
--
-- SQLite cannot ALTER a CHECK constraint in place, so the table is
-- rebuilt: copy → drop → rename, preserving every column added by
-- 0001/0005/0010/0012/0013 and the node_key unique index from 0013.
-- No other table holds FKs back to peer_registry, so foreign_keys can
-- stay enabled (the migration runner already wraps this in a tx).
--
-- Any row that somehow holds 'degraded' (manual edit, future-rollback
-- artefact) is coerced to 'offline' so the new CHECK accepts the row.

CREATE TABLE peer_registry_new (
  device_id    TEXT PRIMARY KEY,
  url          TEXT NOT NULL,
  name         TEXT NOT NULL DEFAULT '',
  last_seen    INTEGER,
  status       TEXT NOT NULL DEFAULT 'offline' CHECK (status IN ('online','offline')),
  node_key     TEXT
);

INSERT INTO peer_registry_new (device_id, url, name, last_seen, status, node_key)
SELECT device_id,
       url,
       name,
       last_seen,
       CASE WHEN status = 'degraded' THEN 'offline' ELSE status END,
       node_key
  FROM peer_registry;

DROP TABLE peer_registry;
ALTER TABLE peer_registry_new RENAME TO peer_registry;

CREATE UNIQUE INDEX IF NOT EXISTS idx_peer_registry_node_key
    ON peer_registry(node_key)
    WHERE node_key IS NOT NULL AND node_key != '';
