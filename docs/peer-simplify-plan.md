# Peer-Auth 簡素化計画

Ed25519 per-request signing + pubkey replication を廃止し、Bearer-over-TLS + Hub-issued capability に置き換える。`internal/peer/auth.go` 系の 2500〜3500 行を削減する。

## 動機

実機 bug: 新 peer が approve された直後、sibling peer から signed request が来ると AuthMiddleware が peer_registry に行を見つけられず 401 unknown_device_id を返す。push ベースの replication 設計の構造的欠陥で、patch を当てても卵鶏は別の局面で再発する。

コード内 threat model:
> kojo only speaks to authenticated peers over Tailscale (mutually-authenticated TLS at the tsnet layer). The threat model treats the wire as confidential and non-malleable.

per-request signing は Tailscale/TLS と重複防御。replication バグの複雑性に見合っていない。

## 設計

### Identity

- **Hub identity**: TLS server cert で証明する。tsnet なら ts.net 自動証明書、非 Tailscale なら operator が用意 (Let's Encrypt or 自前)。自前 cert は **CA pin or fingerprint pin** を peer 側に設定可能。
- **Peer identity**: 廃止。peer は秘密鍵を持たない。Hub から発行される Bearer token が「この peer である」の証明そのもの。
- **device_id**: 引き続き UUID。生成タイミングは peer 初回起動時。pubkey との bind は無し (pubkey 自体が無い)。

### Auth フロー

- **peer → Hub**: `Authorization: Bearer <token>`。Hub は token を hash で保存し、リクエスト毎に hash 比較。
- **Hub → peer**: 同様に Hub→peer 方向の Bearer も pairing 時に交換 (双方向 token)。Hub も peer の Bearer header を提示する。
- **peer ↔ peer**: 直接通信を **原則廃止**。後述の blob 転送のみ Hub-issued capability URL で direct 接続を許す。

### Token 規格

- 256-bit random、base64 std encoding
- scope: `(audience_device_id, role, expires_at?)`
- revocation: Hub が DB から該当 hash を削除すれば即時失効
- rotation: 任意時刻に Hub が再発行 → 旧 token は revoke
- 保存: **plaintext 禁止**。Hub は `sha256(token)` のみ DB に保持
- 伝送: **Authorization header のみ**。URL や query string への載せ替え禁止
- 発行: pairing/approve フローを経たもののみ。`POST /api/v1/peers/pending/{id}/approve` の応答に1度だけ raw token を含める

### Blob 転送 (device-switch)

数百 MB の transcript / memory blob を Hub relay すると帯域2倍 + Hub bottleneck。Hub-issued **signed capability URL** で direct peer-to-peer 転送する。

- **専用 signing key**: TLS key / Bearer 系と分離。`kv` namespace `peer-cap`、KEK 暗号化保存。kid 付き、rotation 可能。
- **capability claims**: `method`, `path`, `audience_device_id`, `source_device_id`, `expires_at`, `max_size_bytes`, `sha256` (期待 body digest), `nonce` (one-time consume)
- **TTL**: 短く (例 5分)。device-switch の orchestrator が要求した瞬間に発行
- **one-time consume**: 受け取り側が capability を呈示すると Hub に「使用済み」を記録、二度目は 401
- **size cap + streaming**: 1 GiB hard cap、io.LimitReader、sha256 を on-the-fly 計算
- **log redaction**: capability の signature 部分は log で `…` に置換

### peer_registry スキーマ

before:
```
device_id, name, url, public_key, capabilities, last_seen, status, trusted
```

after:
```
device_id, name, url, last_seen, status, trusted
```

`public_key` / `capabilities` カラムを drop。migration 1個追加。

## 削除対象

### 完全削除
- `internal/peer/auth.go` (SigningInput, CanonicalPayload, Sign, Verify, AuthDomainPrefix, ErrAuth*)
- `internal/peer/auth_middleware.go` (AuthMiddleware, writePeerAuthError, SignRequest)
- `internal/peer/nonce_cache.go`
- `internal/peer/auth_test.go`, `auth_middleware_test.go`

### 縮減
- `internal/peer/identity.go` → device_id 生成と name のみ残す。private/public key 削除。
- `internal/peer/discovery.go` → hub-info 取得は Hub URL + tailnet name のみ、pubkey verify 削除
- `internal/peer/registrar.go` → 維持 (heartbeat 自体は残す)
- `internal/peer/subscriber.go` → Bearer auth に変更、署名 header 削除
- `internal/peer/sweeper.go` → 影響なし

### server 側
- `internal/server/peer_handlers.go` の broadcastPeerRegistration / fanOutPeerRegistrations / sendPeerRegisterPush / handlePeerRegisterPush 削除
- 全 `peer.SignRequest` 呼出 (約13箇所) を `Authorization: Bearer` セットに置換
- `agent_proxy_middleware.go`, `session_proxy_middleware.go`, `websocket.go`, `agent_ws_proxy.go`, `agent_handlers.go`, `peer_handlers.go`, `switch_device_handler.go`, `peer_blob_*` 内の SignRequest 呼出すべて Bearer に
- `peer_pending_handlers.go` → 簡素化 (pubkey 関連削除、token 発行ロジックに)

### auth policy
- `internal/auth/policy.go` の RolePeer まわりは維持 (Bearer principal を RolePeer に解決する path に変更)

## 段階分割 (小さく)

1. **migration**: `peer_registry.public_key`, `capabilities` カラム drop (まだ削除はしない、column を NULL 許容化のみ。次 migration で drop)
2. **identity**: peer.Identity から private/public key 削除、kv ロード/生成削除
3. **pairing**: token 発行 endpoint 追加。`peer_tokens` テーブル新設 (`token_hash`, `device_id`, `created_at`, `revoked_at`)
4. **auth middleware**: PeerAuthMiddleware を BearerPeerMiddleware に置換 (Authorization: Bearer + DB lookup)
5. **caller 移行**: SignRequest を全箇所 Bearer header set に置換
6. **discovery**: pubkey 関連の verify 削除
7. **broadcast 廃止**: broadcastPeerRegistration / register-push 削除
8. **subscriber**: WS auth を Bearer に切替
9. **blob capability**: signed URL 方式 (device-switch の blob_pull / blob_push を capability 経由に)
10. **cleanup**: 不要 file 削除、migration 第2弾でカラム drop、テスト整理

各段階は単独 commit + ビルド + 既存テスト通過を満たす。

## 既知の懸念

- **Hub offline 時の device-switch**: blob capability は Hub に取りに行く必要があるので Hub offline では発行不能。これは設計上の制約として明文化。Hub redundancy は別議論。
- **revocation の immediacy**: token hash 削除は Hub の DB トランザクションでは即時、ただし peer 側で短期 cache を持つ場合はその TTL 内に遅延。peer 側 cache は持たない方針 (毎回 DB lookup)。
- **Tailscale 経路の Bearer は冗長か**: WhoIs で identity 取れるので Bearer は無くてもよい議論あり。**シンプルさのため統一**: Tailscale 経路でも Bearer は流す。WhoIs 検証は将来 optional 強化として残す。

## 既存 patch の扱い

- `326b922 fix(peer-subscriber): surface auth error body on WS dial failure` — 残置 (subscriber は Bearer 移行後も使う、診断 log は無害)
- `79e5553 fix(peer-onboarding): seed cluster view onto newly-approved peer` — **revert 予定**。replication 自体を廃止するので無意味になる。

## 規模見積

- 削除: 2500〜3500 行
- 追加: 500〜800 行 (token 発行/検証、blob capability、migration)
- 正味: -2000 行前後
