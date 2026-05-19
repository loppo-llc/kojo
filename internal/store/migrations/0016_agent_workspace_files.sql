-- 0013_agent_workspace_files.sql
--
-- Per-agent workspace markdown files (user.md, checkin.md). Mirrors the
-- agent_persona / agent_memory pattern: DB is canonical, the on-disk
-- file under <v1>/global/agents/<id>/<kind>.md is a local mirror the CLI
-- process consumes. Singleton per (agent_id, kind).
--
--   * `kind` is constrained to the two workspace files we ship today.
--     Adding a new kind requires a follow-up migration so the contract
--     is visible in schema review rather than tucked inside a string
--     check in Go.
--   * `seq` is allocated from the same global sequence agent_persona /
--     agent_memory use so the change feed in the §4.x event layer
--     orders workspace-file writes alongside persona / memory edits.
--   * Soft-delete via `deleted_at`. Same semantics as agent_persona:
--     a tombstoned row paired with a missing file means the user
--     explicitly cleared the workspace; an empty body with deleted_at
--     NULL is "saved but blank" (the file removal handled by the
--     reconcile path).
--
-- Data migration of the legacy settings_json.cronMessage value lives in
-- internal/agent/manager.go's load path: the JSON extractor is awkward
-- to express portably in SQL across SQLite versions, and the migration
-- needs to coordinate with the per-agent reconcile that hydrates the
-- on-disk mirror. Manager.Load runs it once per process and clears the
-- legacy key from settings_json once the workspace row is in place.

CREATE TABLE agent_workspace_files (
  agent_id    TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL CHECK (kind IN ('user','checkin')),
  body        TEXT NOT NULL,
  body_sha256 TEXT NOT NULL,
  -- common --
  seq         INTEGER NOT NULL,
  version     INTEGER NOT NULL DEFAULT 1,
  etag        TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  deleted_at  INTEGER,
  peer_id     TEXT,
  PRIMARY KEY (agent_id, kind)
);
CREATE UNIQUE INDEX idx_agent_workspace_files_seq ON agent_workspace_files(seq);
CREATE INDEX        idx_agent_workspace_files_updated_at
  ON agent_workspace_files(updated_at);
