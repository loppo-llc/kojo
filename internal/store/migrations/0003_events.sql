-- 0003_events.sql
--
-- Hub-side event log backing the `GET /api/v1/changes?since=<seq>`
-- resync cursor described in docs/multi-device-storage.md §3.5 / §4.1.
--
-- The WebSocket invalidation broadcast (Phase 4 slice 3a) is best-effort:
-- if a peer falls behind or its subscriber is dropped on overflow, it
-- recovers by polling /changes?since=<last-seen-seq>. The events table
-- is the durable record those polls read from.
--
-- Schema:
--   * One row per logical mutation (insert / update / delete) on a
--     domain table that the Hub wants peers to invalidate.
--   * `seq` is allocated from the same NextGlobalSeq() source as
--     domain rows, so a peer's "last seen seq from any domain" is a
--     valid `since` cursor here too — no per-table cursor bookkeeping.
--   * `etag` is the post-write etag of the affected row (NULL for
--     delete — no row left to ETag).
--   * `ts` is unix millis at the time of the write (server clock —
--     used for staleness reporting only, not for ordering).
--
-- Retention: rows accumulate forever in v1. A future slice can add a
-- background trimmer that drops rows older than the longest peer's
-- expected reconnect window (default: 7 days). The cursor read path
-- already tolerates trims by returning the SMALLEST seq still present
-- as the "watermark" so peers can detect "your cursor predates my
-- retention; please full-resync".

CREATE TABLE events (
  seq        INTEGER PRIMARY KEY,
  table_name TEXT    NOT NULL,
  row_id     TEXT    NOT NULL,
  etag       TEXT,
  op         TEXT    NOT NULL CHECK (op IN ('insert','update','delete')),
  ts         INTEGER NOT NULL
);

-- Per-table seq scan: a peer that wants only one domain can pass
-- ?table=agents and we still get to use an index. Without this the
-- ?since query would fall back to the PK scan which is fine for
-- small N but degrades when retention grows.
CREATE INDEX idx_events_table_seq ON events(table_name, seq);
