-- Server-persisted per-room read cursor for the human operator.
--
-- Group DM unread badges were previously driven entirely by a client-side
-- localStorage cursor (kojo.groupdm.lastRead.<roomId>). That cursor lives
-- only in one browser origin's storage, so it was lost whenever the daemon
-- was restarted onto a different origin/port, the PWA storage was evicted,
-- or the operator opened the UI on another device — every room then
-- re-counted from an empty cursor and all the "unread" badges came back.
--
-- Persisting the cursor server-side (keyed by seq, which is stable across
-- restarts) makes read state durable and device-independent. Single-operator
-- hub, so no user dimension is needed.
CREATE TABLE IF NOT EXISTS groupdm_read_cursors (
  groupdm_id    TEXT    PRIMARY KEY,
  last_read_seq INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
