-- 0023_handoff_queued_messages.sql
--
-- Hub 側 queue-and-forward (§3.7 device-switch / handoff)。
-- agent の holder peer が offline のとき、POST /agents/{id}/messages を
-- 即時拒否せずここへ積み、holder の復帰 (peer online 遷移) または
-- holdership のローカル回帰 (force-reclaim / handoff complete /
-- switch-device) を契機に順番どおり配送する。
--
-- 設計上の制約:
--   * canonical な agent_messages には一切書かない。配送成功時に
--     holder 側 (またはローカル runtime) の通常 Chat 経路が書く。
--   * agent_id への FK は張らない (remote agent の local 行が transient に
--     欠ける瞬間があるため — remote_message_mirror と同じ理由)。
--   * holder_peer は enqueue 時点のスナップショット (表示用)。配送先は
--     drain 時に agent_locks から解決し直す。
--   * 配送済み行は DELETE する。status カラムは将来の拡張用に CHECK 付きで
--     'queued' のみ許容。
--   * per-agent 上限は Go 層 (MaxHandoffQueuedPerAgent) で強制。

CREATE TABLE handoff_queued_messages (
  id          TEXT PRIMARY KEY,
  agent_id    TEXT NOT NULL,
  holder_peer TEXT NOT NULL,
  content     TEXT NOT NULL,
  created_at  INTEGER NOT NULL,             -- millis-since-epoch
  status      TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued'))
);

CREATE INDEX idx_handoff_queued_agent
  ON handoff_queued_messages (agent_id, created_at, id);
