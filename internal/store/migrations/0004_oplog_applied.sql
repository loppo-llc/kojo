-- 0004_oplog_applied.sql
--
-- docs/multi-device-storage.md §3.13.1 — applied-op ledger for the
-- Hub-side op-log replay path. Without this, a peer that retried
-- the same op_id after a network blip (between the dispatch
-- commit and the response delivery) would re-apply the entry:
--   * agent_memory.update would bump the row's version + updated_at
--     and re-fire the events subscriber without a content change.
--   * memory_entries.update would 409 against the if_match the
--     prior call already advanced.
--   * agent_messages.insert is safe by op_id-as-row-id, but the
--     post-write event broadcast would double-fire.
--
-- This table records every op_id we have observed land at the Hub
-- along with a content fingerprint over (table, op, body) so a
-- replay with a DIFFERENT shape (op_id reused for an unrelated
-- write — peer bug) is refused rather than silently returning the
-- prior result. The op-log receive handler probes this BEFORE
-- dispatching: a row with a matching fingerprint means "already
-- done, return the saved etag". A row is recorded in the SAME
-- transaction as the dispatch write so a crash between commit and
-- ledger insert is impossible.
--
-- Retention: rows accumulate forever in v1. A future slice can
-- prune entries older than the longest peer's expected reconnect
-- window (mirroring the events-table retention TODO).

CREATE TABLE oplog_applied (
  op_id        TEXT    PRIMARY KEY,
  agent_id     TEXT    NOT NULL,
  fingerprint  TEXT    NOT NULL,   -- sha256(table || 0x1f || op || 0x1f || body)
  result_etag  TEXT    NOT NULL,
  applied_at   INTEGER NOT NULL
);

-- Per-agent scan: a future "show me everything peer X has flushed
-- for agent Y" admin query needs an index. Cheap to add up-front
-- now; populating it on a back-migration is harder.
CREATE INDEX idx_oplog_applied_agent_id ON oplog_applied(agent_id);
