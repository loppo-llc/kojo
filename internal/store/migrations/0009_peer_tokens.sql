-- 0009_peer_tokens.sql
--
-- `peer_tokens` テーブル: docs/peer-simplify-plan.md の Bearer-over-TLS
-- 移行に向けて、Hub が peer に発行する Bearer token の hash を保管する。
--
-- 設計方針:
--
--   - plaintext token は DB に置かない。Hub は raw token を pairing/
--     approve の応答で 1度だけ返し、以降は受信時に sha256(token) を
--     計算して `token_hash` と突き合わせる。DB-only leak で token が
--     即時流出することを避ける (Codex review の必須条件)。
--   - 双方向 Bearer: Hub→peer 方向と peer→Hub 方向で別の token を
--     発行する。両方ともこのテーブルに入る。`role` 列で区別。
--   - revocation は `revoked_at` を立てるだけ。row 物理削除は `kojo
--     --peer-remove` 等の運用コマンドが行う。
--
-- 列:
--
--   token_hash   PRIMARY KEY  sha256(raw_token) を base64 std
--   device_id    TEXT NOT NULL  対応する peer の device_id
--   role         TEXT NOT NULL  'hub_to_peer' | 'peer_to_hub'
--                              (peer↔peer 直接通信は廃止予定なので
--                               この2種のみ; 将来 'blob_cap_kid' 等
--                               の用途が増えたら CHECK を緩める)
--   created_at   INTEGER NOT NULL  unix millis
--   revoked_at   INTEGER         unix millis、有効な間は NULL
--
-- index: device_id 単位で active token を検索する operator UI 用。
CREATE TABLE peer_tokens (
  token_hash  TEXT PRIMARY KEY,
  device_id   TEXT NOT NULL,
  role        TEXT NOT NULL CHECK (role IN ('hub_to_peer', 'peer_to_hub')),
  created_at  INTEGER NOT NULL,
  revoked_at  INTEGER
);

CREATE INDEX peer_tokens_device_idx ON peer_tokens (device_id, revoked_at);
