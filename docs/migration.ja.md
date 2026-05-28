# kojo v0.101.0 へのアップグレード

> [English](migration.md)

kojo v0.101.0 はマルチデバイス peer アーキテクチャに対応するため、
on-disk の設定ディレクトリ構成を刷新しました。旧 `kojo/` 設定
ディレクトリ (v0.20 までのリリースが書いていたもの) が残った状態では、
新バイナリは起動を拒否します。初回起動時に次の 3 通りから 1 つを
選んでください。

| 選択 | フラグ | 用途 |
|------|--------|------|
| 旧データを取り込む | `--migrate` | 旧インストールのセッション・agent・記憶・認証情報・push 購読を引き継ぐ |
| 新規に始める | `--fresh` | 旧 `kojo/` ディレクトリを無視してクリーンインストール。旧ディレクトリは残る |
| 旧版に戻す | `--rollback-external-cli` → 旧バイナリ起動 | `--migrate` 後に旧版へ戻したい |

本ドキュメントでは **旧 `kojo/` ディレクトリ** は v0.x の設定
ディレクトリ、**`kojo-v1` ディレクトリ** は v0.101.0 以降の設定
ディレクトリを指します。新ディレクトリ名に `-v1` が付いているのは
on-disk スキーマ世代の識別子であり、kojo のリリースバージョンとは別物
です。実パス。

| OS | 旧 `kojo/` ディレクトリ | `kojo-v1` ディレクトリ |
|----|------------------------|------------------------|
| macOS / Linux | `~/.config/kojo/` | `~/.config/kojo-v1/` |
| Windows | `%APPDATA%\kojo\` | `%APPDATA%\kojo-v1\` |

## v0.101.0 で増えたもの

- **マルチデバイス peer** — 複数台で同じ agent / セッション / ファイル
  / git を共有できます。詳細は [multi-device.ja.md](multi-device.ja.md)
- **Tailscale 由来の identity** — peer 認証は Tailscale NodeKey に
  紐付きます。bearer token のコピペや Ed25519 鍵の管理は不要
- **Agent device-switch** — 走っている agent を UI 操作 1 回で別マシン
  に移せます。skill と認証情報は自動追従
- **Agent からのファイル添付** — `kojo-attach` skill で agent が操作者
  にファイルを返せます
- **Grok Build CLI** — Claude / Codex に加えて agent backend として追加
- **Agent workspace ファイル** — agent 単位のスクラッチ領域。JSONL を
  堅牢化し、Slack と挙動を揃えました
- **スナップショット / リストア** — `--snapshot` / `--restore` で
  Hub の引っ越しに対応

## v0.101.0 で消えたもの

- **Gemini CLI backend** — 削除しました。既存の Gemini agent は移行前
  に別 backend へ振り直すか、後で作り直してください
- **Gmail 通知サブシステム** — 削除しました。Slack または Web Push を
  使ってください
- **Slack `<reply>` タグプロトコル** — Slack agent はライブで stream
  する仕様に戻りました。タグでフィルタする外部ツールがあれば書き換えて
  ください

## 手順

### 1. 旧 kojo を止める

macOS / Linux:

```sh
killall kojo
ps aux | grep kojo    # 空になっていることを確認
```

Windows (PowerShell):

```powershell
Get-Process kojo -ErrorAction SilentlyContinue | Stop-Process
Get-Process kojo -ErrorAction SilentlyContinue   # 空になっていることを確認
```

旧 `kojo/` ディレクトリの mtime が直近 5 分以内に動いていると、移行
ツールは取り込みを拒否します。生きた kojo を取り込むと silent に
破損するための gate です。

### 2. 旧 `kojo/` ディレクトリをバックアップ (推奨)

macOS / Linux:

```sh
cp -a ~/.config/kojo ~/kojo-old-backup
```

Windows (PowerShell):

```powershell
Copy-Item -Recurse $env:APPDATA\kojo $env:USERPROFILE\kojo-old-backup
```

移行ツールは旧 `kojo/` ディレクトリを削除も移動もしません。失敗時の
ロールバック先として手元バックアップを残しておきます。

### 3. 移行を実行

```sh
kojo --migrate
```

取り込み対象。

- `kojo.db` (sessions, agents, memory, kv, peer_registry の骨格)
- `credentials.db` + `credentials.key` (そのままコピー)
- `blobs/` (agent workspace, attachments, avatars)
- `sessions.json` (tmux セッション情報。次回起動時に既存 tmux pane に
  自動再 attach します。macOS / Linux のみ)
- claude transcript の symlink (`--migrate-external-cli=true`、default)

完了後に exit します。途中で kill された場合は同じコマンドを再実行
すれば resume します (idempotent)。

### 4. v0.101.0 を起動

```sh
kojo                    # Tailscale モード (default)
kojo --local            # localhost のみ
```

Tailscale モードは MagicDNS hostname (`kojo.<tailnet>.ts.net`) を
そのまま保つため、モバイルでブックマークした URL もそのまま使えます。

`auth/kek.bin` は移行で保持されるので、暗号化された認証情報は再入力
なしで復号でき、既存の Web Push 購読もそのまま使えます。

## トラブルシュート

### `v0 dir modified within the safety window`

旧 `kojo/` ディレクトリ内のファイルが直近 5 分以内に触られた状態です。
旧 kojo が確実に停止していることを確認してから
`--migrate-force-recent-mtime` を付けて再実行してください。**旧 kojo
がまだ動いている状態でこのフラグを使うと、取り込みが silent に
壊れます**。確証なく使わないこと。

### 移行が途中で死んだ

`kojo --migrate` を再実行してください。完了した table から resume
します。

resume でも進まない、または部分状態が怪しい場合は `kojo-v1`
ディレクトリを破棄して最初からやり直します。

```sh
kojo --migrate-restart
```

部分取り込み済みの `kojo-v1` ディレクトリを捨てて、旧 `kojo/`
ディレクトリから再取り込みします。

### 移行後に credentials.db が無い

`credentials.db` と `credentials.key` は `kojo-v1` ディレクトリに
そのまま複製されます。無いなら `--migrate-restart` で再取り込みして
ください。

### 旧リリースに戻したい

1. v0.101.0 を停止します (上の手順 1 と同じ)。
2. `--migrate` が作った claude transcript symlink を巻き戻します。

   ```sh
   kojo --rollback-external-cli
   ```

3. 旧バイナリを以前通り起動します。

`kojo-v1` ディレクトリは残るので、後でまた `kojo --migrate` を実行
すれば戻れます。

### 旧 `kojo/` ディレクトリを消したい

v0.101.0 が安定して動くようになり、移行を信用できるようになったら:

```sh
# 1. dry-run
kojo --clean v0

# 2. kojo.deleted-<ts>/ にリネームしてソフト削除
kojo --clean v0 --clean-apply

# 3. 7 日後に物理削除
kojo --clean v0-trash --clean-apply
```

2 段階削除は「移行して旧ディレクトリを消したら必要なものが無かった」
を避けるための gate です。

## 移行が引き継がないもの

- **VAPID 鍵ペア** — 引き継ぎます。push 購読はそのまま生存
- **KEK** (`auth/kek.bin`) — 引き継ぎます。認証情報は再入力不要
- **走行中の tmux セッション** — macOS / Linux では引き継ぎます。
  tmux server さえ生きていれば v0.101.0 起動時に再 attach します
  (7 日 cutoff より古い pane も特例で保持)。Windows は ConPTY で動作
  するため、v0 / v0.101.0 のどちらでも kojo 再起動を跨いだセッション
  永続化はありません
- **Gemini agent** — 引き継ぎません。Gemini 削除のため、移行前に
  Claude / Codex / Grok に振り直すか、後で作り直してください
- **Gmail 通知設定** — 引き継ぎません。機能削除
- **Slack `<reply>` フィルタ** — Slack agent はライブ stream に戻り
  ました。タグ依存の外部ツールがあれば書き換えてください
