# Setup Guide

`kojo` の起動・移行・peer 構成手順と、間違いやすい箇所の解説。

## 用語

- **Hub** — cluster の SQLite 正本と blob global tree を持つ主機。`kojo
  user` (操作者) はスマホ / ブラウザから Hub の Web UI にアクセスする
- **Peer** — Hub 以外の機。自身の agents を PTY で動かしつつ Hub と同期
- **configdir** — `~/.config/kojo-v1/` (macOS / Linux) または
  `%APPDATA%\kojo-v1\` (Windows)

## CLI parameter spec

### 基本

| Flag | Default | 説明 |
|------|---------|------|
| `--port N` | `8080` | listen port。被ったら最大 10 回まで自動 increment |
| `--local` | `false` | tsnet を使わず `127.0.0.1` だけに bind |
| `--dev` | `false` | Vite dev server (`:5173`) にプロキシ。`--local` を強制 |
| `--hostname NAME` | `kojo` | tsnet machine name。`<name>.<tailnet>.ts.net` で公開される。`--local` / `--dev` 時は無視 |
| `--config-dir PATH` | platform default | configdir を上書き |
| `--no-auth` | `false` | agent-facing auth listener を無効化 (`--local` / `--dev` 必須) |
| `--peer` | `false` | daemon 専用 peer モード。tsnet 起動せず `tailscale ip -4` で取得した Tailscale IP に bind (取得失敗時のみ `0.0.0.0` fallback + warning)、plain HTTP listen。Web UI / Owner 向け routes / WebDAV / push / kv / oplog / agents / sessions / files / git は **全て 404**。残るのは `/api/v1/peers/events` と `/api/v1/peers/blobs/*` のみ (Ed25519 sign 認証)。Owner-fallthrough middleware も無効化され、無署名 request は principal 無しで peer endpoints の `Owner OR Peer` gate を通れない。peer_registry の self 行は `http://<ts-ip>:<port>` 形式で登録される。**起動時の v0 → v1 startup gate は bypass する** (peer は migration の関心外、v0 dir があっても起動を妨げない)。`--dev` / `--local` / `--no-auth` / `--migrate` / `--migrate-restart` / `--fresh` / `--rollback-external-cli` と相互排他、`--hostname` は無視 (tailnet identity は OS の tailscaled から借用) |
| `--version` | — | バージョン表示して exit |

### Migration (v0 → v1)

`--migrate` / `--migrate-restart` / `--fresh` / `--rollback-external-cli` は **primary mode flags** で **相互排他**。同時指定すると起動 gate が拒否する。

| Flag | 説明 |
|------|------|
| `--migrate` | v0 dir から v1 dir に import して exit。idempotent (途中 kill された場合の resume OK) |
| `--migrate-restart` | 部分 import 済みの v1 dir を破棄して v0 から再 import |
| `--migrate-external-cli=true/false` | claude / codex / gemini の transcript link 移行。default `true`。modifier flag なので primary mode と併用 |
| `--migrate-backup PATH` | `--migrate` 前に v0 dir を read-only zip として PATH に保存。modifier flag |
| `--migrate-force-recent-mtime` | v0 dir の直近 5 分以内の mtime check を bypass。**v0 が確実に停止している** ことを確認してから使う。modifier flag |
| `--fresh` | v0 を無視して v1 を新規セットアップ |
| `--rollback-external-cli` | `--migrate-external-cli=true` で作った symlink を巻き戻す (v0 binary に戻す前に必要) |

### Peer registry

| Flag | 説明 |
|------|------|
| `--peer-self` | 自機の identity triple (device_id / name / public_key) を表示 |
| `--peer-list` | peer_registry の全 row を表示 (read-only、daemon と共存可) |
| `--peer-add SPEC` | peer を登録。`SPEC = <device_id>\|<name>\|<base64-public-key>` (pipe 区切り、`<name>` に `host:port` を入れられる) |
| `--peer-remove DEVICE_ID` | peer を削除。self は refuse |

### Snapshot / Restore (Hub failover)

| Flag | 説明 |
|------|------|
| `--snapshot` | `<configdir>/snapshots/<UTC_TS>/` に kojo.db + blobs/global を保存して exit |
| `--restore PATH` | snapshot から復元 (manifest + sha256 検証)。target configdir に既存 DB があれば refuse |
| `--restore-force` | `--restore`: 既存 kojo.db を上書き許可 |

### Cleanup

| Flag | 説明 |
|------|------|
| `--clean TARGET` | `snapshots` / `legacy` / `v0` / `v0-trash` / `all` のいずれか。default dry-run。`all` = `snapshots + legacy` (v0 / v0-trash は除外、誤削除防止のため明示指定必須) |
| `--clean-apply` | `--clean` の実行 (dry-run でなく実削除) |
| `--clean-keep-latest N` | snapshots: 直近 N 件は保護 (default 3) |
| `--clean-max-age-days N` | snapshots: N 日より古いものを削除 (default 7) |
| `--clean-min-age-days N` | v0-trash: N 日より新しいものは残す (default 7) |
| `--clean-force` | v0 manifest divergence 検出時に bypass。v0 を migration 後に編集したケースで使う |

### 環境変数

| Var | 説明 |
|-----|------|
| `KOJO_OWNER_TOKEN` | owner token を上書き (デフォルトは初回起動時に生成) |
| `KOJO_REQUIRE_IF_MATCH=1` | optimistic concurrency の strict mode (412 → 428 移行用) |
| `CUSTOM_API_BASE_URL` | session manager 用 custom OpenAI base URL |

## 初回起動 (Hub)

### Tailscale モード (default)

```sh
kojo --fresh
```

起動 log:

```
kojo v0.19.0 running at:

    https://kojo.<your-tailnet>.ts.net:8080
    https://100.64.x.x:8080
    agent API: http://127.0.0.1:8081  (Bearer required)
```

このモードでは public listener は **OwnerOnly middleware** が付き、Tailscale 経由で到達できる全 request を Owner として扱う (= スマホで開けば即操作可能、token 不要)。Tailscale 上に居れば信頼するという割り切り。

同一マシン上の agent CLI / curl 用に `127.0.0.1:8081` の auth listener が立ち、Bearer token を要求する。別マシンの agents はこの listener には到達できない (loopback) — そちらは自機の kojo daemon を別途立てる。

### `--local` モード

```sh
kojo --local --fresh
```

```
kojo v0.19.0 running at:

    http://127.0.0.1:8080

  open this URL once to authorize the UI:
    http://127.0.0.1:8080/?token=<owner-token>
```

Local listener は OwnerOnly ではなく **AuthMiddleware** 経由。初回のみ `?token=` 付き URL を踏んで localStorage に owner token を保存する必要がある。

### 間違いやすい箇所

- **v0 dir が存在すると起動拒否**。`--migrate` か `--fresh` を明示
- **`--no-auth` は `--local`/`--dev` 専用**。Tailscale 公開で no-auth は絶対にしない (Owner 権限が無防備に晒される)
- **`--hostname` を変えると Tailscale 側の machine name も変わる**。既存 device は別 entry として残るので Tailscale admin console で旧 entry を削除
- **default Tailscale モードでは `?token=` URL は表示されない**。Owner として自動認証されるため。`--local` の手順と混同しないこと

## Migration (v0 → v1)

```sh
# 1. v0 を停止
killall kojo

# 2. backup + migrate
kojo --migrate --migrate-backup ~/kojo-v0-$(date +%Y%m%d).zip

# 3. 移行後 v1 で起動 (上の Hub 起動と同じ)
kojo
```

### 間違いやすい箇所

| 症状 | 原因 | 対処 |
|------|------|------|
| `v0 dir mtime too recent` で拒否 | 直近 5 分以内に v0 file が触れられた (ls -la でも mtime は変わらないが、`tail -f` / backup tool は触る) | v0 が完全停止していることを確認 → `--migrate-force-recent-mtime`。確認せずに使うと **mid-import の data race で silent corruption** |
| migration が途中で死んだ | kill -9 / 電源断 | 同じコマンドで再実行 → resume。`migration_in_progress.lock` が消えるまで再実行可 |
| `migration_in_progress.lock` のままハマってる | 別マシンの v0 を import しようとした | `--migrate-restart` で v1 dir を捨ててやり直す |
| v0 dir を消したい | migration 完了後 | `kojo --clean v0` (dry-run) → `--clean-apply` で `kojo.deleted-<ts>/` に soft-delete → 7 日後 `--clean v0-trash --clean-apply` で物理削除 |
| v0 binary に戻したくなった | migration したけど v1 で問題あり | `kojo --rollback-external-cli` (transcript symlink を巻き戻し) → v0 binary を起動 |
| credentials.db が消えた | migration バグ ではなく v0 → v1 で credentials.{db,key} は **そのまま** v1 dir に残るはず | `~/.config/kojo-v1/credentials.{db,key}` を確認。無いなら `--migrate-restart` で再 import |

### Migration がやらないこと

- v0 dir の削除 (`--clean v0` を明示する必要あり)
- v0 binary の差し替え (`go install` 等で kojo binary 自体は別途更新)
- KEK の新規生成 (v1 初回起動時に `<configdir>/auth/kek.bin` を自動作成。既存 install ではそのまま保持)
- VAPID pair の rotation (既存 push subscription を維持するため)

### tmux session (live PTY) の引き継ぎ

`--migrate` の sessions importer は `sessions` table に **status=archived 強制**で書き込むだけ (将来用)。一方で runtime side では `internal/session/store.go` の Load() が、v1 dir に `sessions.json` が無ければ v0 dir の `sessions.json` を読んで kv にミラーする。TmuxSessionName を含む全 SessionInfo がそのまま保持されるため:

- v0 で動いていた tmux pane (`kojo_<id>` 接頭辞) は v1 起動時にそのまま再 attach される
- 「PTY は v0 停止と同時に消失」というのは **kojo プロセス** の停止の話ではなく、tmux server 自体を落とした場合のみ。`killall kojo` で kojo binary を止めても tmux server は別 process tree で生き残るので、v1 binary を起動すれば再 attach される
- 7 日以上前から走り続けている v0 tmux pane でも引き継がれる。`maxAge` (= 7d) cutoff は `Status=running && TmuxSessionName != ""` の row を例外的に残す: 落とすと `cleanupOrphanedTmuxSessions` の known set から外れて v1 が起動時に kill してしまうため

v0 fallback が有効になる条件:

- `migration_complete.json` が v1 dir に存在 (= `--migrate` 完了済み) **かつ** v0 dir が残っている
- `--fresh` 起動時は **無効**。v0 を読まないという契約を runtime も守る
- 新規 install (v0/v1 両方無し) でも当然 **無効**

注意:

- v0 dir の `sessions.json` は v1 が **削除しない** (rollback 用 + `kojo --clean v0` 領分)
- v1 dir に既に `sessions.json` または kv 行が存在する場合は v1 側が優先
- v0 binary が別 process で動いたまま v1 を起動するのは禁止。tmux session の所有が二重になり、kv mirror が `cleanupOrphanedTmuxSessions` の known set 計算と競合する

## Peer 構成 (multi-device)

### 前提

- 全機が同じ **Tailscale tailnet** にいる
- 各機に **独立した configdir** がある (`--config-dir` で path を分ける、または別ホーム)
- Hub (UI を出す機) と Peer (daemon-only) で起動方式が違う:
  - **Hub**: 通常起動 (`kojo` または `kojo --hostname <name>`)。tsnet で TLS listen、Web UI を serve
  - **Peer**: `kojo --peer`。tsnet を使わず OS の tailscaled が確立済みの NIC で plain HTTP listen。Web UI なし

### 重要

- **Hub は機ごとに `--hostname` を分ける**。default `kojo` のまま複数機を起動すると tsnet で重複して `kojo-1`, `kojo-2` 等 suffix が付き URL が不安定になる
- **Peer は `--hostname` を使わない**。tailnet identity は OS の tailscaled から借用 (`tailscale status` で見えるそのもの)。`--peer --hostname X` は warning 付きで無視
- **Peer の HTTP surface は最小**。`/api/v1/peers/events` (cross-subscribe WS) と `/api/v1/peers/blobs/*` (cross-peer blob fetch) のみ。それ以外は 404 を返す
- **Peer 認証は Ed25519 sign**。Tailscale WireGuard 経由前提なので TLS 不要 (peer middleware が署名を検証)。peer mode では Owner-fallthrough middleware も無効化されており、無署名 request が Owner principal を取得して peer endpoints に到達することはない
- **Peer は Tailscale IP に bind する**。`tailscale ip -4` で取得した IP に listen する (取得失敗時のみ `0.0.0.0` で起動 + warning)。LAN 側に hostname が解決されても peer endpoints は Tailscale interface 経由でしか届かない
- **Peer の self-row は scheme prefix 付き**。`http://<ts-ip>:<port>` の形で `peer_registry.name` に書き込まれ、Hub 側 Subscriber がそのまま dial する。scheme なしの Hub-side row は historical な `https://` 互換 (tsnet TLS) として扱われる

### 手順 (Hub A + Peer B 構成)

```sh
# === Peer 機 (B) ===
# 0. B で tailscaled が up していること (tailscale status で確認)
#    `tailscale ip -4` が IP を返すこと

# 1. B で daemon を起動 (常駐、Web UI なし)
kojo --peer &
# B の peer_registry self-row は http://<ts-ipv4>:8080 形式で書き込まれる

# 2. B の identity を表示
kojo --peer-self
# 出力 (1 行 pipe 区切り):
# 7f3a1c2e-4d5b-6a78-9012-3456789abcde|http://100.64.0.42:8080|dGVzdHB1YmtleWJhc2U2NAo=
# 形式: <canonical-uuid>|<scheme://host:port>|<base64-pubkey>

# === Hub 機 (A) ===
# 3. A で通常起動 (Web UI 用)
kojo --hostname alpha &
# A は https://alpha.<tailnet>.ts.net:8080 で UI を出す

# 4. A に B を登録 (B の --peer-self 出力をそのまま渡す)
#    name 欄に Tailscale MagicDNS FQDN を入れたければ scheme 付きで手書きしてもよい
#    (例: http://bravo.<tailnet>.ts.net:8080)
kojo --peer-add '7f3a1c2e-4d5b-6a78-9012-3456789abcde|http://100.64.0.42:8080|dGVzdHB1YmtleWJhc2U2NAo='

# 5. A の identity を B に登録 (双方向)
kojo --peer-self                                     # A 機
kojo --peer-add '<A の出力をそのまま貼る>'           # B 機

# 6. 確認
kojo --peer-list
# self + 登録した peer が online で並ぶ
```

### 間違いやすい箇所

| 症状 | 原因 | 対処 |
|------|------|------|
| Peer の self-row に Tailscale IP ではなく OS hostname (`http://macbook-bravo:8080` 等) が出る | `tailscale ip -4` が失敗 (tailscaled 未起動 / tailscale CLI 未インストール / 3s timeout) → bind は `0.0.0.0` に fallback、advertise 名は OS hostname に fallback | tailscaled を up し直し、`tailscale ip -4` が IP を返す状態にしてから `kojo --peer` を再起動。CLI が無い環境では fallback 起動するが warning がログに出る (`peer: could not read Tailscale IPv4 ...`) |
| Peer で `--peer-self` の name 欄が IPv4 リテラル (`http://100.64.0.42:8080`) | `--peer` 起動は MagicDNS FQDN ではなく Tailscale IP を advertise する | そのまま使う、もしくは Hub 側 `--peer-add` で渡す spec の `<name>` 欄を `http://<fqdn>:8080` に手書きで上書き |
| Hub-side Subscriber が peer に繋がらず `tls: first record does not look like a TLS handshake` | peer の name 欄が scheme なし | peer mode が書く name は必ず `http://` 始まり。手で書き換えた場合は scheme prefix を忘れない。scheme なし name は historical な Tailscale TLS shape として `https://` で dial される |
| Hub の 2 peer 起動したらどちらかが `kojo-1` になった | 両方 Hub として `--hostname` 未指定 | Hub には `--hostname` を明示。daemon-only にしたい機は `--peer` |
| `--peer` 起動で `/` が 404 | 仕様。Web UI は Hub 側のみ | Hub 側 (`kojo` 通常起動) の URL を使う |
| `--peer` と `--dev` / `--local` / `--no-auth` / `--migrate*` / `--fresh` / `--rollback-external-cli` を同時指定 → 起動拒否 | 相互排他 (peer は migration の関心外) | どれか一方だけ |
| peer 機で v0 dir (`~/.config/kojo` 等) が残っていて、通常起動だと migrate 要求される | peer 機は migration 対象外。Hub 側で migrate するもの | `kojo --peer` で起動。peer mode は startup gate を skip するので v0 dir があっても問題なく起動する |
| peer-list で peer が `offline` のまま、`--peer-self` を daemon 起動前に叩いて OS hostname (port なし) が name に入った状態で peer-add してしまった | self-row が未生成のまま OS hostname を spec に出した → Hub の Subscriber が `https://<hostname>` を dial して TLS / port なしで失敗 | Win11 / Linux 側で `kojo --peer` (or Hub なら `kojo`) を起動して self-row が `http://<ts-ip>:<port>` 形式になるのを待つ → `kojo --peer-self` を再実行 → Hub で `kojo --peer-add` を **上書き** (RegisterPeerMetadata なので status / last_seen は維持される) |
| peer-list で peer が `offline` のまま | heartbeat (30s) が一度も通っていない | 1 分待つ。NIC 通ってるか `tailscale ping <peer-name>` で確認 |
| `kojo --peer-self` が `this binary has not advertised a dial address yet` で refuse | 先に daemon (`kojo` か `kojo --peer`) を起動して self-row を書かせていない | エラーメッセージ通り daemon を 1 回起動 → `kojo --peer-self` を再実行 |
| peer-add で `public_key shape invalid` | base64 標準形式じゃない | URL-safe / 改行混入 / `=` パディング欠落をチェック |
| peer-add で `spec must be ...` + zsh/bash の `command not found: <name>` / `command not found: <pubkey>=` が連発 | spec の `\|` を **shell pipe として未 quote で渡した**。pipe で 3 つのコマンドに分解されて kojo には spec 1 個目 (`<uuid>`) しか届かない | 必ず quote する: `kojo --peer-add '<uuid>\|<name>\|<key>='`。`--peer-self` 実行時の stderr ヒントに各 shell 用の正しい形式が出る |
| peer-add で `spec must be ...` + 他の症状なし | colon を separator と勘違いした (旧仕様) | separator は pipe (`\|`)。shell で quote する (`'spec'`) |
| 自分を peer-remove しようとした | self は refuse される | configdir 丸ごと削除して再生成 |

## Hub 引っ越し (failover / マシン交換)

```sh
# 旧 Hub で snapshot
kojo --snapshot
# 出力: ~/.config/kojo-v1/snapshots/<ts>/

# 別経路で新 Hub に転送
scp -r ~/.config/kojo-v1/snapshots/<ts>/ newhub:/tmp/
scp ~/.config/kojo-v1/auth/kek.bin newhub:/tmp/

# 旧 Hub 停止
killall kojo

# 新 Hub で restore
kojo --restore /tmp/<ts>/
cp /tmp/kek.bin ~/.config/kojo-v1/auth/kek.bin
chmod 600 ~/.config/kojo-v1/auth/kek.bin

# 新 Hub 起動
kojo --hostname kojo  # 旧 Hub と同じ hostname を取り戻すならこれ
```

### 間違いやすい箇所

| 症状 | 原因 | 対処 |
|------|------|------|
| restore 後 起動はするが encrypted kv (VAPID 等) の復号で warn / 失敗 | KEK 不整合 — 旧 Hub の KEK を持ってきていない、または起動時に新 KEK が自動生成された | 起動を停止 → `auth/kek.bin` に旧 Hub の KEK を 600 で配置 → 再起動。**新 KEK で起動してしまった場合は kojo identity 自体が別物として再生成され得るので restore からやり直す** |
| restore 後 push 通知が来ない | (a) 旧 KEK 不在で encrypted VAPID rows が復号できないが自動再生成もされない (= push 機能 dead) / (b) 旧 KEK 喪失して新規生成した | (a) 旧 KEK を配置して再起動 (これが第一)。(b) KEK 喪失確定なら docs §3.15-bis の手順で `kv` の `notify/vapid_*` と `secret=true` の secret rows を明示削除 → 起動時に新 VAPID pair が生成される → スマホで全 device 再購読 |
| `target configdir is in use` で refuse | 旧 Hub が動いてる / 別 process が configdir lock 持ってる | `killall kojo`、`<configdir>/kojo.lock` を確認 |
| restore が `manifest sha256 mismatch` | snapshot が rsync 中に切れた / 改竄 | snapshot を取り直す。partial dir は手動で `rm -rf` |
| 旧 Hub の Tailscale device が残ってる | tsnet 経由で claim した machine を kick してない | Tailscale admin console で旧 entry を削除 |

詳細手順は `docs/snapshot-restore.md`。

## Device switch (agent を別 peer に引っ越し)

`agent X` を peer A から peer B に移したい:

```sh
# A 機で agent CLI を停止 (running なら)

# Hub に handoff begin (owner-only)
curl -X POST https://kojo.<tailnet>.ts.net/api/v1/agents/<id>/handoff/begin \
     -H "Authorization: Bearer $OWNER_TOKEN" \
     -d '{"target_peer_id":"<B の device_id>"}'

# blob を B に rsync (v1 は operator 手動)
rsync -av ~/.config/kojo-v1/blobs/global/agents/<id>/ \
      bravo:~/.config/kojo-v1/blobs/global/agents/<id>/

# complete
curl -X POST https://kojo.<tailnet>.ts.net/api/v1/agents/<id>/handoff/complete \
     -H "Authorization: Bearer $OWNER_TOKEN" \
     -d '{"target_peer_id":"<B の device_id>"}'

# complete 前で rsync が失敗したら abort
# (abort は handoff_pending をクリアするだけ。home_peer / lock は touch しない)
curl -X POST https://kojo.<tailnet>.ts.net/api/v1/agents/<id>/handoff/abort \
     -H "Authorization: Bearer $OWNER_TOKEN"
```

### 間違いやすい箇所

- **complete を rsync 前に呼ぶと取り返しがつかない**。home_peer は target に書き換わるが blob 本体が無い。abort は handoff_pending しか戻さないので、この状態からの復旧は (a) 不足 blob を target に rsync して complete を再実行 (idempotent)、または (b) snapshot から restore のいずれか。確実に rsync 完了を確認してから complete を呼ぶ
- **target_peer_id を自機にすると 400**。single-peer cluster で device switch は意味がないため refuse
- **target が peer_registry にない場合 400**。先に `--peer-add` で登録
- **lock holder が target になった後の complete 再呼出は no-op** (fencing token は進めない)。idempotent
- **abort は complete 後の巻き戻しには使えない**。complete 後は state machine の前進専用

## トラブルシュート FAQ

### 起動はするが Web UI が開けない

```sh
# 1. listen address を確認
kojo  # 起動 log に URL あり
# 2. Tailscale 接続確認
tailscale ping kojo.<tailnet>.ts.net
# 3. 別 peer から curl
curl -k https://kojo.<tailnet>.ts.net:8080/api/v1/info
```

### owner token を忘れた

Tailscale モード (default) では public listener が OwnerOnly で全 request を Owner として扱うので token を覚えてなくても UI は開ける (`https://kojo.<tailnet>.ts.net:8080`)。

`--local` モードや、agent CLI / curl から auth listener (`http://127.0.0.1:8081`) を叩く場合は Bearer が必要。忘れた場合:

```sh
# 1. token を環境変数で上書きしつつ起動
KOJO_OWNER_TOKEN=$(openssl rand -hex 32) kojo --local
# 2. 起動 log に新 URL + ?token= 付きが出る → ブラウザで踏み直す
# 3. localStorage に保存される。永続化したいなら KOJO_OWNER_TOKEN を
#    systemd / launchd で永続セットする
# 4. KOJO_OWNER_TOKEN は kv に保存されない (override 用) ので、
#    持続的に変えたい場合は kv の auth/owner.token を直接消して再生成
```

### kv が腐ってる (起動拒否)

```sh
# 1. integrity check
sqlite3 ~/.config/kojo-v1/kojo.db "PRAGMA integrity_check;"
# 2. snapshot から復元
kojo --restore <snapshot-dir>
# 3. snapshot もなければ --migrate-restart で v0 からやり直し
```

### `kojo.lock` が残ってる

```sh
# 1. 別 kojo process が動いてないか
ps aux | grep kojo
# 2. 動いてないなら lock file 削除
rm ~/.config/kojo-v1/kojo.lock
```

### push 通知が来ない

```sh
# 1. VAPID public key を確認
curl -s -H "Authorization: Bearer $OWNER_TOKEN" \
     https://kojo.<tailnet>.ts.net:8080/api/v1/push/vapid
# 2. 復元後に subscription 全失効した場合は再購読 (UI から)
# 3. macOS Safari で許可ダイアログが出ない場合は Settings → Safari → Notifications を確認
```
