-- 0001_initial.sql
--
-- Initial schema for kojo v1's structured store. Layout follows
-- docs/multi-device-storage.md sections 3.2 and 3.3.
--
-- Conventions encoded here:
--   * Timestamps are INTEGER UTC epoch milliseconds (NowMillis() in Go).
--   * `seq` is monotonic per the table's documented partition.
--   * `etag` is "<version>-<sha256(canonical_record)[:8]>".
--   * Soft delete via `deleted_at`; physical GC happens after a grace period.
--   * Single-statement migrations are deliberately avoided — modernc.org/sqlite
--     does not enforce statement-at-a-time and we want all DDL applied as one
--     transaction at the Go layer.

-- ---------------------------------------------------------------------------
-- Domain: agents
-- ---------------------------------------------------------------------------

CREATE TABLE agents (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL,
  persona_ref   TEXT,                       -- soft pointer; canonical text lives in agent_persona
  settings_json TEXT NOT NULL DEFAULT '{}', -- backend, model, custom flags, etc.
  workspace_id  TEXT,                       -- 3.8: WorkDir resolved per-peer via workspace_paths
  -- common --
  seq           INTEGER NOT NULL,
  version       INTEGER NOT NULL DEFAULT 1,
  etag          TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  deleted_at    INTEGER,
  peer_id       TEXT
);
CREATE UNIQUE INDEX idx_agents_seq ON agents(seq);
CREATE INDEX        idx_agents_deleted_at ON agents(deleted_at);

-- 3.8: per-peer absolute path mapping for an agent's logical workspace.
CREATE TABLE workspace_paths (
  workspace_id TEXT NOT NULL,
  peer_id      TEXT NOT NULL,
  path         TEXT NOT NULL,
  updated_at   INTEGER NOT NULL,
  PRIMARY KEY (workspace_id, peer_id)
);

CREATE TABLE agent_persona (
  agent_id    TEXT PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
  body        TEXT NOT NULL,
  body_sha256 TEXT NOT NULL,
  -- common (one row per agent: seq is allocated globally at insert time
  -- so the change feed in 4.x has a single ordering across persona edits) --
  seq         INTEGER NOT NULL,
  version     INTEGER NOT NULL DEFAULT 1,
  etag        TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  deleted_at  INTEGER,
  peer_id     TEXT
);
CREATE UNIQUE INDEX idx_agent_persona_seq ON agent_persona(seq);

-- 2.5: MEMORY.md is a denormalized copy of the global blob. The filesystem
-- file under <v1>/global/agents/<id>/MEMORY.md is the write target for agent
-- CLIs; the daemon syncs that file into this row using the intent-file
-- protocol described in the design doc.
CREATE TABLE agent_memory (
  agent_id    TEXT PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
  body        TEXT NOT NULL,
  body_sha256 TEXT NOT NULL,
  last_tx_id  TEXT,                          -- NULL = CLI direct write origin
  -- common --
  seq         INTEGER NOT NULL,
  version     INTEGER NOT NULL DEFAULT 1,
  etag        TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  deleted_at  INTEGER,
  peer_id     TEXT
);
CREATE UNIQUE INDEX idx_agent_memory_seq ON agent_memory(seq);

CREATE TABLE agent_messages (
  id          TEXT PRIMARY KEY,
  agent_id    TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  role        TEXT NOT NULL CHECK (role IN ('user','assistant','system','tool')),
  content     TEXT,
  thinking    TEXT,
  tool_uses   TEXT,                          -- JSON
  attachments TEXT,                          -- JSON: [{kind,blob_uri,sha256,size,name}]
  usage       TEXT,                          -- JSON
  -- common --
  version     INTEGER NOT NULL DEFAULT 1,
  etag        TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  deleted_at  INTEGER,
  peer_id     TEXT,
  UNIQUE (agent_id, seq)
);
CREATE INDEX idx_agent_messages_agent_seq ON agent_messages (agent_id, seq);
CREATE INDEX idx_agent_messages_deleted_at ON agent_messages (deleted_at);

CREATE TABLE agent_tasks (
  id         TEXT PRIMARY KEY,
  agent_id   TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  seq        INTEGER NOT NULL,
  title      TEXT NOT NULL,
  body       TEXT,
  status     TEXT NOT NULL CHECK (status IN ('pending','in_progress','done','cancelled')),
  due_at     INTEGER,
  -- common --
  version    INTEGER NOT NULL DEFAULT 1,
  etag       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  peer_id    TEXT,
  UNIQUE (agent_id, seq)
);
CREATE INDEX idx_agent_tasks_agent_status ON agent_tasks(agent_id, status);

CREATE TABLE memory_entries (
  id          TEXT PRIMARY KEY,
  agent_id    TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  kind        TEXT NOT NULL CHECK (kind IN ('daily','project','topic','people','archive')),
  name        TEXT NOT NULL,
  body        TEXT NOT NULL,
  body_sha256 TEXT NOT NULL,
  -- common --
  version     INTEGER NOT NULL DEFAULT 1,
  etag        TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  deleted_at  INTEGER,
  peer_id     TEXT,
  UNIQUE (agent_id, seq)
);
-- Natural key uniqueness only applies to live (non-tombstoned) rows so a
-- soft-deleted entry does not block a same-name re-creation.
CREATE UNIQUE INDEX idx_memory_entries_alive_natkey
  ON memory_entries(agent_id, kind, name)
  WHERE deleted_at IS NULL;

-- 2.5: detection log for fs/db divergence on MEMORY.md / memory_entries.
-- `reason` is constrained to the four documented detection cases so a typo in
-- daemon code surfaces at INSERT time instead of polluting the queue. FKs
-- block orphan rows when an agent or entry is hard-deleted; resolution
-- workflows must clear the queue before deletion.
CREATE TABLE memory_merge_queue (
  id          TEXT PRIMARY KEY,
  agent_id    TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL CHECK (kind IN ('memory','memory_entry')),
  entry_id    TEXT REFERENCES memory_entries(id) ON DELETE CASCADE,
  reason      TEXT NOT NULL CHECK (reason IN (
                'ui_aborted_concurrent_cli',
                'post_write_mismatch',
                'startup_repair_diverged',
                'cli_direct_db_mismatch'
              )),
  fs_body     TEXT NOT NULL,
  db_body     TEXT NOT NULL,
  detected_at INTEGER NOT NULL,
  resolved_at INTEGER,
  resolution  TEXT CHECK (resolution IS NULL OR resolution IN ('fs','db','manual')),
  -- Cross-field constraint: kind=memory_entry rows must reference an
  -- entry; kind=memory rows must not.
  CHECK (
    (kind = 'memory_entry' AND entry_id IS NOT NULL) OR
    (kind = 'memory'       AND entry_id IS NULL)
  )
);

-- SQLite has no composite foreign key spanning a derived predicate, so we
-- enforce the (memory_merge_queue.agent_id == memory_entries.agent_id)
-- invariant via triggers. Without this the FK on entry_id would let
-- queue rows point at any agent's entry, which would mis-route the merge
-- UI's reconciliation prompts.
CREATE TRIGGER memory_merge_queue_agent_match_insert
BEFORE INSERT ON memory_merge_queue
WHEN NEW.entry_id IS NOT NULL
BEGIN
  SELECT RAISE(ABORT, 'memory_merge_queue.agent_id mismatches memory_entries.agent_id')
  WHERE NEW.agent_id <> (SELECT agent_id FROM memory_entries WHERE id = NEW.entry_id);
END;

CREATE TRIGGER memory_merge_queue_agent_match_update
BEFORE UPDATE OF agent_id, entry_id ON memory_merge_queue
WHEN NEW.entry_id IS NOT NULL
BEGIN
  SELECT RAISE(ABORT, 'memory_merge_queue.agent_id mismatches memory_entries.agent_id')
  WHERE NEW.agent_id <> (SELECT agent_id FROM memory_entries WHERE id = NEW.entry_id);
END;
CREATE INDEX idx_memory_merge_queue_unresolved ON memory_merge_queue(resolved_at) WHERE resolved_at IS NULL;

-- ---------------------------------------------------------------------------
-- Domain: groupdm
-- ---------------------------------------------------------------------------

CREATE TABLE groupdms (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  -- members_json is a JSON array of objects, NOT bare agent_ids:
  --   [{"agent_id":"ag_x","notify_mode":"realtime","digest_window":0}, ...]
  -- Per-member notify/digest preferences travel here so a future
  -- groupdm_members table (Phase 4 normalization) can be populated with a
  -- single row-per-member without schema-rewriting agent_ids.
  members_json TEXT NOT NULL,
  -- style/cooldown/venue mirror v0's GroupDM struct so the v0→v1 importer
  -- can round-trip without losing notification calibration:
  --   style    : "efficient" (token-saving, default) | "expressive"
  --   cooldown : per-group notification cooldown in seconds (0 = default)
  --   venue    : "chatroom" (default) | "colocated"
  style        TEXT NOT NULL DEFAULT 'efficient',
  cooldown     INTEGER NOT NULL DEFAULT 0,
  venue        TEXT NOT NULL DEFAULT 'chatroom',
  -- common --
  seq          INTEGER NOT NULL,
  version      INTEGER NOT NULL DEFAULT 1,
  etag         TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  deleted_at   INTEGER,
  peer_id      TEXT
);
CREATE UNIQUE INDEX idx_groupdms_seq ON groupdms(seq);

CREATE TABLE groupdm_messages (
  id          TEXT PRIMARY KEY,
  groupdm_id  TEXT NOT NULL REFERENCES groupdms(id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  agent_id    TEXT,                           -- NULL for system messages
  content     TEXT,
  attachments TEXT,                           -- JSON
  -- common --
  version     INTEGER NOT NULL DEFAULT 1,
  etag        TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  deleted_at  INTEGER,
  peer_id     TEXT,
  UNIQUE (groupdm_id, seq)
);
CREATE INDEX idx_groupdm_messages_group_seq ON groupdm_messages(groupdm_id, seq);

-- ---------------------------------------------------------------------------
-- Domain: sessions (LOCAL scope — peer-bound PTY state, never replicated)
-- ---------------------------------------------------------------------------

CREATE TABLE sessions (
  id          TEXT PRIMARY KEY,
  agent_id    TEXT,
  status      TEXT NOT NULL CHECK (status IN ('running','stopped','archived')),
  pid         INTEGER,
  cmd         TEXT,                           -- launch command (debug)
  work_dir    TEXT,
  started_at  INTEGER,
  stopped_at  INTEGER,
  exit_code   INTEGER,
  -- common --
  seq         INTEGER NOT NULL,
  version     INTEGER NOT NULL DEFAULT 1,
  etag        TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  deleted_at  INTEGER,
  peer_id     TEXT
);
CREATE INDEX idx_sessions_agent_status ON sessions(agent_id, status);
CREATE INDEX idx_sessions_status ON sessions(status);

-- ---------------------------------------------------------------------------
-- Domain: notify / chat history cursors
-- ---------------------------------------------------------------------------

CREATE TABLE notify_cursors (
  id          TEXT PRIMARY KEY,               -- composite source identifier (e.g. "agent:slack:Cxxx")
  source      TEXT NOT NULL,                  -- slack|discord|...
  agent_id    TEXT,
  cursor      TEXT NOT NULL,                  -- opaque to kojo
  -- common --
  version     INTEGER NOT NULL DEFAULT 1,
  etag        TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  deleted_at  INTEGER,
  peer_id     TEXT
);
CREATE INDEX idx_notify_cursors_agent ON notify_cursors(agent_id);

CREATE TABLE external_chat_cursors (
  id          TEXT PRIMARY KEY,
  source      TEXT NOT NULL,
  agent_id    TEXT,
  channel_id  TEXT,
  cursor      TEXT NOT NULL,
  -- common --
  version     INTEGER NOT NULL DEFAULT 1,
  etag        TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  deleted_at  INTEGER,
  peer_id     TEXT
);
CREATE INDEX idx_external_chat_cursors_agent ON external_chat_cursors(agent_id);

-- ---------------------------------------------------------------------------
-- Domain: compactions
-- ---------------------------------------------------------------------------

CREATE TABLE compactions (
  id          TEXT PRIMARY KEY,
  agent_id    TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  body        TEXT NOT NULL,
  body_sha256 TEXT NOT NULL,
  range_start INTEGER NOT NULL,               -- agent_messages.seq lower bound (inclusive)
  range_end   INTEGER NOT NULL,               -- upper bound (inclusive)
  -- common --
  version     INTEGER NOT NULL DEFAULT 1,
  etag        TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  deleted_at  INTEGER,
  peer_id     TEXT,
  UNIQUE (agent_id, seq)
);

-- ---------------------------------------------------------------------------
-- Cross-cutting infra tables (3.3 exceptions: no `id`/`seq`/`etag` etc.)
-- ---------------------------------------------------------------------------

CREATE TABLE kv (
  namespace       TEXT NOT NULL,
  key             TEXT NOT NULL,
  value           TEXT,
  value_encrypted BLOB,
  type            TEXT NOT NULL CHECK (type IN ('string','json','binary')),
  secret          INTEGER NOT NULL DEFAULT 0,
  scope           TEXT NOT NULL CHECK (scope IN ('global','local','machine')),
  -- common (kv has composite PK so `id` is absent) --
  version         INTEGER NOT NULL DEFAULT 1,
  etag            TEXT NOT NULL,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  PRIMARY KEY (namespace, key)
);
CREATE INDEX idx_kv_scope ON kv(scope);

CREATE TABLE blob_refs (
  uri               TEXT PRIMARY KEY,         -- kojo://<scope>/<path>
  scope             TEXT NOT NULL CHECK (scope IN ('global','local','machine','cas')),
  home_peer         TEXT NOT NULL,
  size              INTEGER NOT NULL,
  sha256            TEXT NOT NULL,
  refcount          INTEGER NOT NULL DEFAULT 1,
  pin_policy        TEXT,                     -- JSON: {peers:[...], cache_max:N}
  last_seen_ok      INTEGER,
  marked_for_gc_at  INTEGER,
  handoff_pending   INTEGER NOT NULL DEFAULT 0, -- 3.7: device-switch in flight
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);
CREATE INDEX idx_blob_refs_home_peer ON blob_refs(home_peer);
CREATE INDEX idx_blob_refs_marked_for_gc ON blob_refs(marked_for_gc_at) WHERE marked_for_gc_at IS NOT NULL;

-- §3.3 exception: push_subscriptions does NOT carry the standard
-- version/etag/deleted_at/peer_id columns. Rationale:
--   - identity is the opaque `endpoint` URL produced by the user agent;
--     there is no "user-edited content" to optimistically lock against.
--   - liveness is signalled by `expired_at` (set on 401/410), which acts
--     as a tombstone-without-soft-delete — no UI surface lists deleted
--     subscriptions, and re-subscription is a free re-INSERT.
--   - audit `peer_id` is supplanted by `device_id` (the originating peer's
--     device_id from peer_registry), which is the field operators actually
--     filter on.
-- Recorded here so future schema reviewers do not "fix" the apparent
-- common-column omission and break those assumptions.
CREATE TABLE push_subscriptions (
  endpoint        TEXT PRIMARY KEY,
  device_id       TEXT,
  user_agent      TEXT,
  vapid_public_key TEXT NOT NULL,
  p256dh          TEXT NOT NULL,
  auth            TEXT NOT NULL,
  expired_at      INTEGER,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);
CREATE INDEX idx_push_subscriptions_active ON push_subscriptions(expired_at) WHERE expired_at IS NULL;

CREATE TABLE peer_registry (
  device_id    TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  public_key   TEXT NOT NULL,
  capabilities TEXT,                          -- JSON
  last_seen    INTEGER,
  status       TEXT NOT NULL DEFAULT 'offline' CHECK (status IN ('online','offline','degraded'))
);

CREATE TABLE agent_locks (
  agent_id          TEXT PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
  holder_peer       TEXT NOT NULL,
  fencing_token     INTEGER NOT NULL,
  lease_expires_at  INTEGER NOT NULL,
  acquired_at       INTEGER NOT NULL
);
CREATE INDEX idx_agent_locks_holder_peer ON agent_locks(holder_peer);

CREATE TABLE migration_status (
  domain          TEXT PRIMARY KEY,           -- agents|messages|memory|...
  phase           TEXT NOT NULL CHECK (phase IN ('pending','imported','cutover','complete')),
  source_checksum TEXT,
  imported_count  INTEGER,
  started_at      INTEGER,
  finished_at     INTEGER
);

-- 3.13.1: 24h dedup window for write-API retries and op-log replay.
CREATE TABLE idempotency_keys (
  key             TEXT PRIMARY KEY,
  op_id           TEXT,
  request_hash    TEXT NOT NULL,
  response_status INTEGER NOT NULL,
  response_etag   TEXT,
  response_body   TEXT,
  expires_at      INTEGER NOT NULL
);
CREATE INDEX idx_idempotency_keys_expires_at ON idempotency_keys(expires_at);

-- 3.19: cron / notify dedup at the (agent_id, scheduled_at) granularity.
CREATE TABLE cron_runs (
  cron_run_id     TEXT PRIMARY KEY,
  agent_id        TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  scheduled_at    INTEGER NOT NULL,
  claimed_by_peer TEXT,
  status          TEXT NOT NULL CHECK (status IN ('claimed','completed','failed')),
  started_at      INTEGER,
  finished_at     INTEGER,
  UNIQUE (agent_id, scheduled_at)
);
CREATE INDEX idx_cron_runs_status ON cron_runs(status);
