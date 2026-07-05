-- 0021_workspace_kind_status.sql
--
-- Adds 'status' to the agent_workspace_files kind CHECK.
--
-- status is the agent's self-maintained state file (status.json on
-- disk): freeform key-value pairs (mood, energy, sleepiness, affection,
-- ...) injected into the system prompt tail and edited by the agent
-- itself as its state drifts. Same DB-canonical / disk-mirror contract
-- as user.md / checkin.md — only the on-disk basename differs
-- (status.json, JSON not markdown; see workspaceFileDiskName).
--
-- SQLite cannot alter a CHECK constraint in place, so this is the
-- standard table rebuild: create-with-new-check → copy → drop → rename.
-- Runs inside the migration transaction like every other file here.

CREATE TABLE agent_workspace_files_new (
  agent_id    TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL CHECK (kind IN ('user','checkin','status')),
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

INSERT INTO agent_workspace_files_new SELECT * FROM agent_workspace_files;
DROP TABLE agent_workspace_files;
ALTER TABLE agent_workspace_files_new RENAME TO agent_workspace_files;

CREATE UNIQUE INDEX idx_agent_workspace_files_seq ON agent_workspace_files(seq);
CREATE INDEX        idx_agent_workspace_files_updated_at
  ON agent_workspace_files(updated_at);
