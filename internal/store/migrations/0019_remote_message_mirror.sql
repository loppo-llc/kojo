-- 0019_remote_message_mirror.sql
--
-- Hub 側で、§3.7 device-switch により他 peer に転移している agent
-- (HolderPeer != self) の messages を一時キャッシュする mirror テーブル。
-- canonical な agent_messages / oplog 経路には一切参加しない。
--
-- 目的: peer が一時的に offline になると proxyPeerGetMessages /
-- fetchRemoteLatestMessage が fail し、Hub の local agent_messages は
-- 転移後の messages を持たないので Dashboard の LastMessage や Chat 画面が
-- 空 (or stale) になる。直前の proxy 成功時のスナップショットをここに残し、
-- offline 時の read fallback に使う。
--
-- 設計上の制約:
--   * canonical な agent_messages との二重書きを避けるため、別テーブル。
--   * fencing_token / op_id / seq を持たない。oplog flush とは独立。
--   * agent_id への FK は張らない。remote agent の local agents 行が
--     transient に存在しない瞬間 (sync 直前等) でも mirror が有効でいられる
--     ようにするため。
--   * 帰還 (force-reclaim / arrival で holder==self になったタイミング) と
--     ReleaseAgentLocally で agent_id 単位に DELETE する。
--
-- attachments / tool_uses / usage は JSON 文字列のまま不透明に保存。
-- attachments の blob 本体は holder 側にあるため、offline 中 thumbnail は
-- 404 になるが、本文・role・timestamp は復元できる。Phase 2 で blob mirror を
-- 入れるかは別 PR。

CREATE TABLE remote_message_mirror (
  agent_id     TEXT NOT NULL,
  message_id   TEXT NOT NULL,
  role         TEXT NOT NULL CHECK (role IN ('user','assistant','system','tool')),
  content      TEXT NOT NULL DEFAULT '',
  thinking     TEXT NOT NULL DEFAULT '',
  tool_uses    TEXT,                       -- JSON, opaque
  attachments  TEXT,                       -- JSON, opaque
  usage        TEXT,                       -- JSON, opaque
  timestamp    TEXT NOT NULL,              -- RFC3339 (wire format)
  holder_peer  TEXT NOT NULL,
  mirrored_at  INTEGER NOT NULL,           -- millis-since-epoch
  PRIMARY KEY (agent_id, message_id)
);
CREATE INDEX idx_remote_message_mirror_agent_ts
  ON remote_message_mirror(agent_id, timestamp DESC, message_id DESC);
