# kojo - Architecture

## システム構成

```
┌─ macOS Machine ──────────────────────────────────────────────┐
│                                                               │
│  kojo server (Go single binary)                              │
│  │                                                            │
│  ├─ HTTP Server ──────────────────────────┐                  │
│  │  ├─ REST API (/api/v1/...)             │ Tailscale IP     │
│  │  ├─ WebSocket (/ws)                    │ e.g. 100.x.y.z  │
│  │  └─ Static Files (embedded Web UI)     │ :8080            │
│  │                                        │                  │
│  ├─ Session Manager                       │                  │
│  │  ├─ Session 1: claude (PTY)            │                  │
│  │  ├─ Session 2: codex  (PTY)            │                  │
│  │  └─ Session N: gemini (PTY)            │                  │
│  │                                                            │
│  ├─ Notification Manager                                     │
│  │  └─ Web Push (VAPID)                                      │
│  │                                                            │
│  └─ File Browser                                             │
│     └─ OS filesystem read-only access                        │
│                                                               │
└───────────────────────────────────────────────────────────────┘
         ▲
         │ Tailscale WireGuard (encrypted P2P)
         ▼
┌─ Mobile Device ──────────────────────────────────────────────┐
│                                                               │
│  Safari / Chrome                                             │
│  └─ kojo Web UI                                              │
│     ├─ Session List (Dashboard)                              │
│     ├─ Terminal View (xterm.js)                               │
│     ├─ File Browser (directory picker)                       │
│     ├─ New Session Dialog                                    │
│     └─ Web Push Subscription                                 │
│                                                               │
└───────────────────────────────────────────────────────────────┘
```

## コンポーネント詳細

### 1. HTTP Server

Go 標準ライブラリ (`net/http`) + coder/websocket。

- **Static Files**: `embed.FS` で React ビルド成果物をバイナリに埋め込み
- **REST API**: セッション管理、ファイルブラウザ、通知設定
- **WebSocket**: PTY I/O のリアルタイムストリーミング

リッスンアドレスは Tailscale IP (`100.x.y.z:8080`)。localhost にはバインドしない。

### 2. Session Manager

各セッションは以下を保持:

```go
type Session struct {
    mu        sync.Mutex // 状態変更の排他制御
    ID        string
    Tool      string    // "claude" | "codex" | "gemini"
    WorkDir   string    // 作業ディレクトリ
    PTY       *os.File  // PTY master fd
    Cmd       *exec.Cmd // 子プロセス
    CreatedAt time.Time
    Status    string    // "running" | "exited"
    ExitCode  *int      // プロセス終了コード（running 中は nil）
    YoloMode  bool      // 自動承認モード
}
```

- PTY は `github.com/creack/pty` で生成
- PTY の出力は goroutine で読み取り、接続中の全 WebSocket クライアントにブロードキャスト
- 出力は ring buffer（直近 1MB）に保持。新規接続時にスクロールバックとして送信
- セッション終了時（プロセス exit）はステータスを更新し、通知を送信

### 3. PTY 管理

```
kojo server
    │
    ├── PTY master ◄──── read ──── goroutine ──── broadcast to WebSocket clients
    │       │
    │       └── PTY slave ──── claude / codex / gemini (child process)
    │
    └── WebSocket input ──── write ──── PTY master
```

PTY のウィンドウサイズ（行数・列数）はクライアントの xterm.js から送信し、`TIOCSWINSZ` で PTY に反映。

### 4. Notification Manager

Web Push (RFC 8030 + VAPID) で通知を送信。

**通知トリガー:**
- セッションがユーザー入力待ち（permission prompt 検出）
- セッション完了（プロセス終了）
- セッションエラー

**Permission prompt 検出:**
PTY 出力から特定パターンをマッチング:
- Claude Code: `Do you want to` / `Allow` / `Deny` 等のパターン
- Codex: 同様のパターン
- Gemini: 同様のパターン

これはヒューリスティクスであり、完全ではない。Phase 2 で改善可能。

**パターンマッチのバッファリング:**
PTY 出力はチャンク単位で届くため、パターンが複数チャンクにまたがる可能性がある。直近 512 バイトの出力をバッファリングし、結合した上でパターンマッチを実行する。マッチ後はバッファをクリアする。

### 5. Yolo Mode (自動承認)

セッション単位で有効化できる自動承認モード。有効時、サーバー側で PTY 出力を監視し、permission prompt を検出したら自動で承認応答を PTY に書き込む。

**自動承認する（人間に聞かない）:**
- Permission prompt: `(y/n)`, `[Y/n]`, `Allow/Deny`
- ファイル編集・コマンド実行の承認

**自動承認しない（人間に聞く）:**
- エージェントからの質問（複数選択肢）: `(1-N)`, 自由入力を求めるプロンプト
- ユーザーへの確認ではなく、判断を求めている場面

**判定ロジック:**

```
PTY 出力を監視
    │
    ├─ Permission パターン検出 (y/n, Allow/Deny)
    │   └─ YoloMode ON → 自動で "y" / "Allow" + Enter を PTY に書き込み
    │   └─ YoloMode OFF → 通常通り（クイックアクション表示 + 通知）
    │
    ├─ 質問パターン検出 (選択肢, 自由入力)
    │   └─ YoloMode ON/OFF 問わず → クイックアクション表示 + Web Push 通知
    │
    └─ それ以外 → 何もしない
```

**パターン分類のヒューリスティクス:**

| 分類 | パターン例 | Yolo 時の挙動 |
|------|-----------|--------------|
| Permission (承認系) | `(y/n)`, `[Y/n]`, `Allow`, `Deny`, `Do you want to` | 自動で肯定応答 |
| Question (質問系) | `(1/2/3)`, `Which`, `Choose`, `Select`, `?` + 選択肢リスト | 通知して人間に聞く |

完全な分類は困難なため、不明な場合は安全側（人間に聞く）に倒す。

### 6. File Browser / Viewer

REST API でファイルシステムを読み取り専用で公開。

- ディレクトリ一覧（ファイル・ディレクトリ両方）
- テキストファイルの内容表示（サイズ上限 1MB）
- 画像ファイルの表示（png, jpeg, gif, webp）
- ホームディレクトリ以下に制限
- 隠しディレクトリ（`.git` 等）はデフォルトで非表示（トグル可能）
- パストラバーサル防止

### 7. Git Manager

REST API で git 操作を提供。

- `git status`, `git log`, `git diff` を構造化データで返す専用エンドポイント
- `git exec` で任意の git サブコマンドを実行可能
- 実行可能コマンドは `git` のみに制限（シェルインジェクション防止）
- 引数は文字列配列で受け取り、`exec.Command("git", args...)` で直接実行（シェル経由しない）

## 技術スタック

### Server (Go)

| 用途 | ライブラリ |
|------|-----------|
| HTTP サーバー | `net/http` (stdlib) |
| WebSocket | `github.com/coder/websocket` |
| PTY | `github.com/creack/pty` |
| Web Push | `github.com/SherClockHolmes/webpush-go` |
| 静的ファイル埋め込み | `embed` (stdlib) |
| ログ | `log/slog` (stdlib) |

外部依存を最小限にする。

### Web UI (React + TypeScript)

| 用途 | ライブラリ |
|------|-----------|
| フレームワーク | React 19 |
| ビルド | Vite |
| ターミナル | xterm.js + @xterm/addon-fit + @xterm/addon-web-links |
| スタイリング | Tailwind CSS |
| 通信 | native WebSocket API / fetch |
| ルーティング | React Router |

## ディレクトリ構成

```
kojo/
├── cmd/
│   └── kojo/
│       └── main.go            # エントリポイント
├── internal/
│   ├── server/
│   │   ├── server.go          # HTTP サーバー
│   │   └── websocket.go       # WebSocket ハンドラ
│   ├── session/
│   │   ├── manager.go         # セッション管理
│   │   ├── session.go         # セッション構造体
│   │   └── pty.go             # PTY 操作
│   ├── notify/
│   │   └── webpush.go         # Web Push 通知
│   └── filebrowser/
│       └── browser.go         # ファイルブラウザ
├── web/                       # React アプリ
│   ├── src/
│   │   ├── App.tsx
│   │   ├── components/
│   │   │   ├── Dashboard.tsx  # セッション一覧
│   │   │   ├── Terminal.tsx   # xterm.js ターミナル
│   │   │   ├── FileBrowser.tsx
│   │   │   └── NewSession.tsx
│   │   ├── hooks/
│   │   └── lib/
│   ├── index.html
│   ├── package.json
│   ├── vite.config.ts
│   └── tsconfig.json
├── docs/
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

## ビルド・配布

```makefile
# Web UI ビルド → Go embed → シングルバイナリ
build:
	cd web && npm run build
	go build -o kojo ./cmd/kojo

# 開発時
dev:
	# ターミナル 1: Web UI (Vite dev server)
	cd web && npm run dev
	# ターミナル 2: Go server (Web UI は Vite にプロキシ)
	go run ./cmd/kojo --dev
```

本番ビルドでは `web/dist/` を `embed.FS` でバイナリに埋め込み。開発時は Vite dev server にプロキシ。

## セキュリティ

- **ネットワーク**: Tailscale WireGuard で暗号化。公開ネットワークには露出しない
- **認証**: Tailscale のデバイス認証に委譲。追加認証なし
- **ファイルアクセス**: ファイルブラウザはディレクトリ一覧のみ。パストラバーサル防止
- **プロセス実行**: 起動可能な CLI は許可リスト制（claude, codex, gemini）

### Graceful Shutdown

サーバー終了時（SIGINT / SIGTERM）のシャットダウン手順:

1. 新規 WebSocket 接続の受付を停止
2. 全 WebSocket クライアントに close フレームを送信
3. 全 PTY セッションに SIGTERM を送信、5 秒の猶予後に SIGKILL
4. アップロード一時ファイルのクリーンアップ
5. HTTP サーバーの graceful shutdown（`http.Server.Shutdown`、タイムアウト 10 秒）

### VAPID 鍵管理

Web Push に必要な VAPID 鍵ペアの管理:

- 初回起動時に ECDSA P-256 鍵ペアを生成
- `~/.config/kojo/vapid.json` に保存（秘密鍵 + 公開鍵）
- 以降の起動では保存済みの鍵を読み込み
- 鍵が変わるとクライアントの既存サブスクリプションが無効になるため、永続化は必須
