-- 0008_peer_pending.sql
--
-- `peer_pending` テーブル: peer mode の自動 onboarding で peer が
-- Hub に投げた `POST /api/v1/peers/join-request` を Owner 承認まで
-- 保留するための row 群。Owner が Settings UI で Approve すると
-- 最新 pending row が `peer_registry` に昇格し (trusted=true)、
-- pending row 自体は削除される。Reject なら row 削除のみ。
--
-- 列は plan (docs/peer-onboarding-plan.md) と一対一:
--
--   device_id    PRIMARY KEY  pairing 中の peer の UUID
--   name         TEXT NOT NULL OS hostname など人間向けラベル
--   url          TEXT NOT NULL peer が advertise してきた dial URL
--                              (http://host:port)
--   public_key   TEXT NOT NULL Ed25519 公開鍵 base64
--   first_seen   INTEGER NOT NULL unix millis 初回 join 要求
--   last_seen    INTEGER NOT NULL unix millis 直近 heartbeat
--
-- pending row は heartbeat (60s) ごとに無条件で上書きされる
-- (plan: "既存 row は上書き")。peer が identity を作り直して別
-- public_key で再要求した場合も pending では受け入れる。
-- public_key immutability は peer_registry 側の話で、Approve 時
-- に Hub が既存 registry 行の鍵と pending の鍵を突き合わせて
-- 不一致なら 409 (store.ErrPeerPendingPubkeyMismatch) を返す。
CREATE TABLE peer_pending (
  device_id   TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  url         TEXT NOT NULL,
  public_key  TEXT NOT NULL,
  first_seen  INTEGER NOT NULL,
  last_seen   INTEGER NOT NULL
);

CREATE INDEX peer_pending_last_seen_idx ON peer_pending (last_seen DESC);
