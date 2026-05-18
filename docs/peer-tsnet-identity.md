# peer auth: Tailscale identity primary, Bearer 全廃

## 動機

`peer-simplify-plan.md` の Bearer-over-TLS 設計は peer⇄Hub の片側 (Hub 発行) しか実装されておらず、peer⇄peer の Bearer 発行を欠いていた。さらに approve 後も peer は keepalive で `POST /join-request` を叩き続けるが、permanent Bearer 提示で RolePeer 判定 → `EnforceMiddleware` が当該 route を許可していないため 403 が出続ける。すなわち 2 つの構造的誤り。

1. peer⇄peer の Bearer 発行欠落 (Subscriber が常に `no outbound Bearer`)。
2. approve 後の keepalive が policy の死角で 403 ループ。

Tailscale (WireGuard mTLS) が既に on-wire の identity + 機密性を担保している。その上に Bearer を二重に被せて N×N の token を配るのは費用対効果が合わない。

## 設計

### identity

- **一次資格**: Tailscale NodeKey。`tsnet.Server.LocalClient().WhoIs(remoteAddr)` が `WhoIsResponse.Node.Key` を返す。
- **論理 ID**: 既存の `device_id` (UUID) を継続。`peer_registry.node_key` 列を新設して両者を bind する。
- **device_id ↔ node_key の bind は join-request 時 1 回**。以降変更不可。NodeKey が変わったら別 device 扱い (operator が削除 → 再 approve)。

### 認証 middleware (1 本)

すべての `/api/v1/*` request について:

1. listener が tsnet 由来なら → `LocalClient.WhoIs(r.RemoteAddr)` で `NodeKey` 取得。
2. `node_key` が local `peer_registry` に一致 → `Principal{Role: RolePeer, PeerID: device_id}`。
3. `node_key` が Hub 自身 (`self_row.node_key`) → `Principal{Role: RoleOwner}` (Tailscale reach == Owner の現行 UX を維持)。peer-mode daemon では Owner 該当行が無いので RolePeer or Guest のみ。
4. どちらでもない (tailnet 上に居るが未 approve) → `Principal{Role: RoleGuest}` (= 拒否)。
5. `--unsafe` 起動の場合は WhoIs を行わず、無条件で `RolePeer` (peer-mode) or `RoleOwner` (hub-mode) を付与。

### pairing

```
peer 起動
  └─ self_row 書き込み (device_id, name, node_key=LocalClient.Status().Self.NodeKey)
peer → POST /api/v1/peers/join-request { device_id, name, url }
  Hub: WhoIs(remoteAddr) で送信元 NodeKey 取得 (peer は NodeKey を送らない)
       → peer_pending に行を insert/update (device_id, name, url, node_key)
       → 既に peer_registry に存在し node_key 一致なら approved を即座に返す
       → 不一致なら 409 (operator に削除 → 再 approve を要求)
peer → GET /api/v1/peers/join-request/{device_id}
  Hub: peer_registry に行があれば approved、無ければ pending、どちらも無ければ 404
       ↑ 認証は不要 (device_id 知ってる = peer 本人) — leak しても害は無い (NodeKey 検証は別)
Owner Approve (Settings UI)
  Hub: peer_pending → peer_registry に昇格
       他の paired peer に新 row (device_id, name, url, node_key) を broadcast (RegisterPeerMetadata)
peer は approve 受信したら discovery loop を停止
keepalive: Registrar.TouchPeer のみ (30s tick)
```

#### join-request の race / DoS 対策

- 同じ NodeKey からの POST は idempotent: 同 NodeKey が違う device_id を主張してきたら 409。
- 同じ device_id で違う NodeKey が来たら 409 (peer は使い回しているが NodeKey が変わった = OS reinstall 等。operator が手動で消すまで通さない)。
- 1 NodeKey あたり 1 req/sec で rate limit (handler 側で in-memory map + per-key mutex)。

### peer⇄peer

Subscriber は WS upgrade に `Authorization` header を載せない。受信側で WhoIs が `peer_registry` の `node_key` 列に一致するかでのみ判定。発信側に outbound Bearer の概念なし。

### --unsafe flag

非 Tailscale 環境 (純 LAN dev, docker compose, CI) 用 escape hatch。

- `--unsafe` が立っていれば tsnet WhoIs 取得を行わない。
- 認証は素通し: 全 caller が RolePeer (peer-mode) or RoleOwner (hub-mode)。
- WAN bind は依然として拒否 (listener は `--local` か Tailscale IP か明示の `0.0.0.0` のいずれか)。
- 起動時に WARN を 1 回吐く: `kojo: --unsafe set; tailnet identity disabled. peer endpoints are open to anyone reachable on the listener.`
- `--unsafe` 無しで non-tsnet listener (`--peer` で Tailscale IP が取れない場合) → 起動失敗。

## 削除対象

### 完全削除

- `internal/peer/outbound_bearer.go`
- `internal/peer/bearer_middleware.go`, `bearer_middleware_test.go`
- `internal/store/peer_tokens.go`, `peer_tokens_test.go`
- `peer_tokens` table (migration 0013_drop_peer_tokens.sql)
- `peer_pending.join_secret_hash` 列 (migration 0014_drop_peer_pending_join_secret.sql)
- `peer_registry.node_key` 列追加 (migration 0013_peer_registry_node_key.sql)
- discovery の `joinSecret`, `attachJoinAuth`, `loadJoinSecret`, `persistJoinSecret`, `clearJoinSecret`, `lookupPermanentBearer`, `persistPairingBearers`
- discovery `OutBearerNS` 定数 (kv の row も一緒に消す、cleanup migration で WHERE namespace='peer/out_bearer' DELETE)
- server 側 `peer_pairing_bearer.go` (`callerHoldsJoinIdentity`, `callerHoldsPeerBearer`, `consumePairingStashOnAck`, `attachPairingBearers`, `mintJoinSecret` 等)
- server 側 `pair_flow_integration_test.go` (旧仕様)
- `JoinResponse.PeerBearer`, `HubBearer`, `JoinSecret` フィールド

### 縮減

- `internal/auth/auth.go`: `AuthMiddleware` / `OwnerOnlyMiddleware` を tsnet 版に置換。
- `internal/peer/discovery.go`: post + poll + stop に縮める。
- `internal/peer/subscriber.go`: WS dial から Authorization 削除。
- 全 outbound call site (`AuthorizeOutbound`, `AttachOutboundBearer` 呼出) を素のリクエストに置換。

### 影響

- `peer_pending` 表は残す (UI で list / approve / reject)。`node_key` を加え `join_secret_hash` を落とす。
- `kojo --peer-add` / `--peer-trust` / `--peer-remove` などの escape hatch は据え置き (pubkey 引数は無視)。

## 段階分割

1. migration 0013 (peer_registry.node_key 追加) + store API
2. tsnet middleware (`internal/auth/tsnet_middleware.go` 新規) + LocalClient injection 経路
3. discovery 書き直し (Bearer 廃止、approve で loop 停止)
4. join-request handler 書き直し (NodeKey を WhoIs 経由で取得、pending に保存)
5. Bearer call site 一斉削除 (server / peer 各所)
6. BearerPeerMiddleware + peer_tokens + OutBearerNS 削除 + migration 0014
7. `--unsafe` flag 配線
8. 旧 test 削除 / 新 test 追加
9. README / docs 更新

各段階で `go build ./...` + `go test ./...` を通す。
