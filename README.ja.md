# kojo

[![Release](https://img.shields.io/github/v/release/loppo-llc/kojo)](https://github.com/loppo-llc/kojo/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/loppo-llc/kojo)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> [English](README.md)

AI コーディング CLI（Claude Code, Codex, Gemini CLI）をモバイルからリモート操作し、記憶・思考・協調する永続 AI エージェントを運用するツール。

```
┌───────────────────┐       Tailscale        ┌──────────────────┐
│  macOS / Win / Linux│◄─────(P2P encrypted)─────►│  Mobile Browser  │
│                     │                        │                  │
│  kojo server        │   WebSocket / HTTP     │  Web UI          │
│  ├─ PTY sessions    │◄─────────────────────►│  ├─ xterm.js     │
│  ├─ AI agents       │                        │  ├─ React        │
│  └─ credentials DB  │                        │  └─ Web Push     │
└───────────────────┘                        └──────────────────┘
```

## 特徴

- **シングルバイナリ** — Go 製、Web UI を埋め込み
- **クロスプラットフォーム** — macOS/Linux（tmux + PTY）と Windows（ConPTY）のネイティブ対応
- **AI エージェント** — 記憶・スケジュール実行・暗号化クレデンシャル・グループ DM を持つ永続 AI ペルソナ
- **tmux バックドセッション**（macOS/Linux）— CLI ツールを tmux 内で実行。kojo の再起動・クラッシュ後もセッション継続
- **統一 PTY** — すべての CLI を PTY 経由で統一的に制御。SDK 依存なし
- **Tailscale P2P** — 中央サーバーやデータベース不要。WireGuard で暗号化
- **ゼロコンフィグ** — Tailscale を起動した状態で `kojo` を実行するだけ

## 必要なもの

### macOS / Linux

- Go 1.25+
- Node.js 20+
- tmux
- [Tailscale](https://tailscale.com/)
- 対応 CLI: `claude`, `codex`, `gemini`（いずれか1つ以上）

### Windows

- Go 1.25+
- Node.js 20+
- Windows 10 1809+ / Windows 11（ConPTY 対応が必要）
- [Tailscale](https://tailscale.com/)
- 対応 CLI: `claude`, `codex`, `gemini`（いずれか1つ以上）

> **注意:** Windows ではセッションは tmux ではなく ConPTY で動作します。kojo 再起動時のセッション永続化は利用できません。

## ビルド

```bash
# プロダクションビルド（macOS/Linux）
make build

# macOS/Linux から Windows 向けクロスコンパイル
make build-windows

# Windows 上でのビルド
build.bat

# 開発（ターミナル2つ）
make dev-server   # Go サーバー (--dev モード、Vite にプロキシ)
make dev-web      # Vite dev server

# ホットリロード（Go ファイル変更時に自動リビルド）
make watch
```

## 使い方

```bash
# Tailscale 経由（デフォルト、自動 HTTPS）
kojo

# ローカルのみ
kojo --local

# ポート指定（使用中なら自動インクリメント）
kojo --port 9090
```

デフォルトでは kojo は tsnet 経由で Tailscale ネットワーク上に HTTPS でリッスンします。
`--local` または `--dev` で localhost のみにバインドします。

## Tailscale HTTPS セットアップ

kojo は [tsnet](https://tailscale.com/kb/1244/tsnet) を使って、`kojo` という名前のノードとして Tailscale ネットワークに直接参加します。すべての通信は WireGuard で暗号化されます。ポートの開放や証明書の管理は不要です。

### 前提条件

1. マシンとモバイルデバイスに Tailscale をインストール
2. 両方のデバイスで同じ Tailscale アカウントにサインイン
3. 両方のデバイスが [Tailscale 管理コンソール](https://login.tailscale.com/admin/machines) に表示されていることを確認

### 仕組み

`kojo` を `--local` なしで実行すると：

1. kojo が tsnet 経由で Tailscale ノードを起動
2. ノードが tailnet 上に `kojo` として登録される
3. Tailscale 組み込みの Let's Encrypt 連携により HTTPS が自動プロビジョニング
4. `https://kojo.<tailnet名>.ts.net` でアクセス可能に

```bash
$ kojo

  kojo v0.18.0 running at:

    https://kojo.tail1234.ts.net
    https://100.x.y.z:8080
```

### 初回起動

初回起動時、tsnet が認証 URL を stderr に出力します。ブラウザで開いてノードを認可してください。これは一度だけの操作です。認証情報は `~/.config/tsnet-kojo/` にキャッシュされます。

### モバイルからのアクセス

1. スマートフォンに Tailscale アプリをインストール
2. 同じアカウントでサインイン
3. モバイルブラウザで `https://kojo.<tailnet名>.ts.net` を開く

すべての通信は WireGuard による P2P です。中央サーバーを経由しません。

### セキュリティモデル

| レイヤー | 保護 |
|---------|------|
| ネットワーク | WireGuard 暗号化（Tailscale P2P） |
| TLS | Let's Encrypt による自動 HTTPS |
| WebSocket | Origin を Tailscale IP（`100.*`）、`*.ts.net`、`localhost` に制限 |
| アクセス | tailnet 上のデバイスのみが kojo に到達可能 |

### ACL（オプション）

[Tailscale ACL](https://tailscale.com/kb/1018/acls) を使って、kojo にアクセスできるデバイスを制限できます：

```json
{
  "acls": [
    {
      "action": "accept",
      "src": ["tag:mobile"],
      "dst": ["tag:kojo:*"]
    }
  ],
  "tagOwners": {
    "tag:mobile": ["autogroup:admin"],
    "tag:kojo":   ["autogroup:admin"]
  }
}
```

### トラブルシューティング

| 問題 | 解決策 |
|------|--------|
| 認証 URL が表示されない | stderr の出力を確認。または `~/.config/tsnet-kojo/` を削除して再起動 |
| モバイルから接続できない | 両方のデバイスが同じ tailnet 上にあり、Tailscale が接続中であることを確認 |
| ポート競合 | `--port <番号>` を使用（最大10回まで自動インクリメント） |
| localhost のみで使いたい | `--local` または `--dev` で Tailscale をスキップ |

## 機能

### ターミナルセッション

- 複数セッションの同時管理（新しい順に表示）
- macOS/Linux では tmux によるセッション永続化（`~/.config/kojo/sessions.json`、7日後に自動クリーンアップ）。kojo の再起動・クラッシュ後もセッション継続
- セッション再起動（ツール固有の resume: `claude --resume`, `codex resume`, `gemini --resume`）
- リアルタイム PTY 出力ストリーミング（xterm.js）
- テキスト入力（Enter で改行、Shift+Enter で送信）と特殊キー（Esc, Tab, Ctrl, 矢印）
- 作業ディレクトリのパス補完
- ファイルブラウザ（テキストのシンタックスハイライト、画像プレビュー）
- ファイル添付（カメラ、画像、テキスト）
- Git パネル（status, log, diff, コミット diff 表示）
- Web Push 通知（権限プロンプト、完了アラート）
- Yolo モード（権限の自動承認）
- 最小システムプロンプトオプション（claude のデフォルトを作業ディレクトリ情報のみで上書き）

### AI エージェント

- カスタム名・性格・アバター・バックエンド（Claude / Codex / Gemini、加えてカスタム Anthropic Messages API エンドポイント対応）を持つ永続 AI ペルソナの作成
- AI によるペルソナ自動生成（Gemini API）とアバター生成
- ストリーミング応答・思考表示・ツール使用カード付きのインタラクティブチャット
- エージェントメッセージの Markdown レンダリング
- スケジュール自律実行（10分〜24時間間隔）、自動スタガリング、エージェントごとのタイムアウト、プロセス間重複排除
- 永続記憶: 長期 `MEMORY.md` + 日次ノート、全文検索（SQLite FTS5）
- 暗号化クレデンシャル保管庫（AES-256-GCM SQLite）、TOTP 2FA 対応
- パブリックプロフィールとエージェントディレクトリによる相互発見
- グループ DM: 通知ベースのマルチエージェント会話
- Slack 連携（Socket Mode）— エージェントごとの Bot、ストリーミング応答、スレッド別会話コンテキスト、`<reply>` タグフィルタリング
- エージェントデータリセット（設定・ペルソナ・アバター・クレデンシャルを保持したまま会話・記憶をクリア）
- エージェント Fork（ペルソナと記憶を引き継いで派生）

## 技術スタック

| レイヤー | 技術 |
|---------|------|
| サーバー | Go, `net/http`, `coder/websocket`, `creack/pty` (Unix) / ConPTY (Windows), tmux (Unix), `tsnet` |
| Web UI | React 19, Vite, TypeScript, Tailwind CSS, xterm.js |
| エージェント | Claude / Codex / Gemini / Custom API バックエンド、暗号化 SQLite（クレデンシャル + FTS5 記憶） |
| 通知 | Web Push (VAPID) |
| ネットワーク | Tailscale WireGuard P2P |

## インストール

```bash
go install github.com/loppo-llc/kojo/cmd/kojo@latest
```

## ライセンス

[MIT](LICENSE)
