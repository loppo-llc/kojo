-- 0022_groupdm_hops_mentions_dm.sql
--
-- Group DM v2 features:
--   * groupdms.max_hops — per-room agent-to-agent relay depth limit.
--     0 means "use the built-in default" (see agent.defaultMaxHops).
--   * groupdms.kind — 'group' (default) or 'dm' (first-class 1:1 room
--     sugar; same machinery, listed separately in the UI).
--   * groupdm_messages.hop — relay depth of the message: 0 for messages
--     posted outside a notification-triggered turn, trigger-hop + 1 for
--     messages an agent posted while handling a group-DM notification.
--     Persisted so hop chains survive restarts.
--   * groupdm_messages.mentions — JSON array of mentioned member ids
--     (agent ids or the reserved "user" sentinel) parsed from @name
--     tokens at post time. NULL when no mentions.
--   * groupdm_dead_letters — permanently failed notification deliveries
--     (after bounded retry) recorded instead of silently dropped.

ALTER TABLE groupdms ADD COLUMN max_hops INTEGER NOT NULL DEFAULT 0;
ALTER TABLE groupdms ADD COLUMN kind TEXT NOT NULL DEFAULT 'group';
-- dm_member_key is the canonical member-set key (sorted agent ids joined
-- with a newline) populated only for kind='dm' rows. The partial UNIQUE index
-- makes find-or-create race-safe across processes/daemons — the in-process
-- dmMu only serializes one daemon.
ALTER TABLE groupdms ADD COLUMN dm_member_key TEXT;
CREATE UNIQUE INDEX idx_groupdms_dm_member_key
  ON groupdms(dm_member_key)
  WHERE kind = 'dm' AND deleted_at IS NULL;

ALTER TABLE groupdm_messages ADD COLUMN hop INTEGER NOT NULL DEFAULT 0;
ALTER TABLE groupdm_messages ADD COLUMN mentions TEXT;

CREATE TABLE groupdm_dead_letters (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  groupdm_id  TEXT NOT NULL,
  agent_id    TEXT NOT NULL,
  reason      TEXT NOT NULL,
  payload     TEXT,
  attempts    INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL
);

CREATE INDEX idx_groupdm_dead_letters_group
  ON groupdm_dead_letters(groupdm_id, created_at);
