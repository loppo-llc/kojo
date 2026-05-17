-- 0007_agent_lock_allowed_proxy.sql
--
-- agent_locks に allowed_proxy_peer 列を追加。「この lock 行に対し
-- /api/v1/agents/{id}/* を proxy 経由で叩ける signer」を明示的に
-- 保存する。holder 自身が叩く場合の合法 signer (= the orchestrator
-- that handed the runtime here) を記録するための列。
--
-- これを引かないと middleware は "self == holder" だけで判定する
-- しかなく、paired-but-untrusted な任意 peer が holder 自身でない
-- にも関わらず agent surface に届けてしまう。
--
-- セット路:
--   AcquireAgentLock (初回 / lease steal): allowed = peer (= holder
--                                          自身が同時に proxy origin)
--   TransferAgentLock (device-switch step 6): allowed = currentPeer
--                                            (= source / orchestrator)
--   ForceReclaimAgentLock: allowed = peer (= self が orchestrator)
--   RefreshAgentLock: 既存値 preserve
--
-- 既存行 backfill: AllowedProxyPeer = holder_peer。device-switch を
-- 経た既存行は次の transfer / refresh で正しい値に上書きされる。
ALTER TABLE agent_locks
  ADD COLUMN allowed_proxy_peer TEXT NOT NULL DEFAULT '';

UPDATE agent_locks SET allowed_proxy_peer = holder_peer
 WHERE allowed_proxy_peer = '';
