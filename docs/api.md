# kojo - API

## 概要

kojo server は REST API と WebSocket の 2 つのインターフェースを提供する。

- **REST API**: セッション管理、ファイルブラウザ
- **WebSocket**: PTY I/O のリアルタイムストリーミング

ベース URL: `http://<tailscale-ip>:8080`

### エラーレスポンス

全エンドポイント共通のエラーレスポンス形式:

```json
{
  "error": {
    "code": "not_found",
    "message": "session not found: s_abc123"
  }
}
```

| HTTP ステータス | code | 説明 |
|----------------|------|------|
| 400 | `bad_request` | リクエストのバリデーションエラー |
| 404 | `not_found` | リソースが見つからない |
| 409 | `conflict` | 状態の競合（例: 既に停止済みのセッションを停止） |
| 413 | `payload_too_large` | アップロードサイズ超過 |
| 415 | `unsupported_media_type` | 非対応のファイル形式 |
| 500 | `internal_error` | サーバー内部エラー |

---

## REST API

### Sessions

#### `GET /api/v1/sessions`

全セッション一覧を返す。

**Response:**
```json
{
  "sessions": [
    {
      "id": "s_abc123",
      "tool": "claude",
      "workDir": "/Users/you/GitHub/kojo",
      "args": ["--model", "opus"],
      "status": "running",
      "yoloMode": false,
      "createdAt": "2026-02-23T10:00:00Z"
    }
  ]
}
```

#### `POST /api/v1/sessions`

新規セッションを作成・起動する。

**Request:**
```json
{
  "tool": "claude",
  "workDir": "/Users/you/GitHub/kojo",
  "args": ["--model", "opus"],
  "yoloMode": false
}
```

- `tool`: `"claude"` | `"codex"` | `"gemini"`
- `workDir`: 作業ディレクトリの絶対パス
- `args`: CLI に渡す追加引数（省略可）
- `yoloMode`: 自動承認モード（省略可、デフォルト: `false`）

**Response:**
```json
{
  "id": "s_abc123",
  "tool": "claude",
  "workDir": "/Users/you/GitHub/kojo",
  "status": "running",
  "createdAt": "2026-02-23T10:00:00Z"
}
```

#### `GET /api/v1/sessions/:id`

特定セッションの詳細を返す。

**Response:**
```json
{
  "id": "s_abc123",
  "tool": "claude",
  "workDir": "/Users/you/GitHub/kojo",
  "status": "running",
  "createdAt": "2026-02-23T10:00:00Z",
  "exitCode": null
}
```

#### `DELETE /api/v1/sessions/:id`

セッションを終了する。実行中のプロセスに SIGTERM を送信し、猶予後に SIGKILL。

**Response:**
```json
{
  "ok": true
}
```

#### `PATCH /api/v1/sessions/:id`

セッション設定を更新する。現在は yolo モードの切り替えに使用。

**Request:**
```json
{
  "yoloMode": true
}
```

**Response:**
```json
{
  "id": "s_abc123",
  "tool": "claude",
  "workDir": "/Users/you/GitHub/kojo",
  "status": "running",
  "yoloMode": true,
  "createdAt": "2026-02-23T10:00:00Z"
}
```

#### `POST /api/v1/sessions/:id/restart`

終了済みセッションを同一 ID で再起動する。スクロールバック履歴は維持される。
`claude` の場合、`--continue` フラグを自動付与して会話を再開する。

**Response:**
```json
{
  "id": "s_abc123",
  "tool": "claude",
  "workDir": "/Users/you/GitHub/kojo",
  "args": ["--model", "opus", "--continue"],
  "status": "running",
  "createdAt": "2026-02-23T10:00:00Z"
}
```

- 実行中のセッションに対して呼ぶと `400 Bad Request`
- セッションが見つからない場合は `404 Not Found`

---

### Directory Suggestions

#### `GET /api/v1/dirs?prefix=<path>`

Working Directory 入力時のパス補完候補を返す。

**Query Parameters:**
- `prefix`: 入力中のパス文字列。`~` はホームディレクトリに展開される

**Response:**
```json
{
  "dirs": [
    "/Users/you/GitHub/kojo",
    "/Users/you/GitHub/kojo-web"
  ]
}
```

- 隠しディレクトリ（`.` で始まるもの）は除外
- 最大 10 件
- 大文字小文字を区別しないマッチ

---

### File Browser

#### `GET /api/v1/files?path=<dir>`

指定ディレクトリの内容を返す。

**Query Parameters:**
- `path`: ディレクトリの絶対パス（省略時はホームディレクトリ）
- `hidden`: `true` で隠しファイルを含める（デフォルト: `false`）

**Response:**
```json
{
  "path": "/Users/you/GitHub",
  "entries": [
    { "name": "kojo", "type": "dir", "modTime": "2026-02-23T10:00:00Z" },
    { "name": "happy", "type": "dir", "modTime": "2026-02-22T15:30:00Z" }
  ]
}
```

#### `GET /api/v1/files/view?path=<file>`

ファイルの内容を返す。テキストと画像のみ対応。

**Query Parameters:**
- `path`: ファイルの絶対パス

**Response (テキスト):**
```json
{
  "path": "/Users/you/GitHub/kojo/main.go",
  "type": "text",
  "content": "package main\n\nfunc main() {\n...",
  "size": 1234,
  "language": "go"
}
```

**Response (画像):**
```json
{
  "path": "/Users/you/GitHub/kojo/screenshot.png",
  "type": "image",
  "mime": "image/png",
  "size": 204800,
  "url": "/api/v1/files/raw?path=/Users/you/GitHub/kojo/screenshot.png"
}
```

- テキスト: サイズ上限 1MB。`language` はファイル拡張子から推定
- 画像: `image/png`, `image/jpeg`, `image/gif`, `image/webp` に対応。内容は `url` から取得
- バイナリ等の非対応ファイルは `415 Unsupported Media Type`

#### `GET /api/v1/files/raw?path=<file>`

ファイルの生データを返す。画像表示用。`Content-Type` ヘッダーに MIME タイプを設定。

---

### Git

#### `GET /api/v1/git/status?workDir=<dir>`

指定ディレクトリの `git status` を返す。

**Response:**
```json
{
  "branch": "main",
  "ahead": 2,
  "behind": 0,
  "staged": ["src/main.go"],
  "modified": ["README.md"],
  "untracked": ["tmp/debug.log"]
}
```

#### `GET /api/v1/git/log?workDir=<dir>&limit=<n>`

コミットログを返す。

**Query Parameters:**
- `workDir`: リポジトリのパス
- `limit`: 件数（デフォルト: 20）

**Response:**
```json
{
  "commits": [
    {
      "hash": "abc1234",
      "message": "fix: resolve login bug",
      "author": "loppo",
      "date": "2026-02-23T10:00:00Z"
    }
  ]
}
```

#### `GET /api/v1/git/diff?workDir=<dir>&ref=<ref>`

diff を返す。

**Query Parameters:**
- `workDir`: リポジトリのパス
- `ref`: 比較対象（省略時は unstaged changes。`--staged` で staged changes）

**Response:**
```json
{
  "diff": "diff --git a/main.go b/main.go\n..."
}
```

#### `POST /api/v1/git/exec`

任意の git サブコマンドを実行する。

**Request:**
```json
{
  "workDir": "/Users/you/GitHub/kojo",
  "args": ["add", "."]
}
```

**Response:**
```json
{
  "exitCode": 0,
  "stdout": "",
  "stderr": ""
}
```

- 実行可能なコマンドは `git` のみ（他のコマンドは拒否）
- 破壊的操作（`push --force`, `reset --hard` 等）は確認なしで実行される点に注意。UI 側で警告を出す

---

### File Upload

#### `POST /api/v1/upload`

モバイルからファイルをアップロードする。macOS 上の一時ディレクトリに保存し、パスを返す。

**Request:** `multipart/form-data`
- `file`: アップロードするファイル

**Response:**
```json
{
  "path": "/tmp/kojo/upload/img_001.png",
  "name": "img_001.png",
  "size": 204800,
  "mime": "image/png"
}
```

- 保存先: `/tmp/kojo/upload/` 以下に `{ULIDv7}_{元ファイル名}` のユニーク名で保存
- サイズ制限: 20MB
- サーバー graceful shutdown 時に自動クリーンアップ

---

### Server Info

#### `GET /api/v1/info`

サーバー情報を返す。

**Response:**
```json
{
  "version": "0.1.0",
  "hostname": "loppos-mac",
  "homeDir": "/Users/you",
  "tools": {
    "claude": { "available": true, "path": "/usr/local/bin/claude" },
    "codex": { "available": true, "path": "/usr/local/bin/codex" },
    "gemini": { "available": false, "path": "" }
  }
}
```

---

## WebSocket

### 接続

```
ws://<tailscale-ip>:8080/api/v1/ws?session=<session_id>
```

1 つの WebSocket 接続が 1 つのセッションに対応。複数セッションを同時に見る場合は複数接続を張る。

### メッセージ形式

全メッセージは JSON。`type` フィールドで種別を判定。

### Server → Client

#### `output`

PTY からの出力データ。

```json
{
  "type": "output",
  "data": "base64 encoded bytes"
}
```

`data` は Base64 エンコード。xterm.js にそのままデコードして書き込む。

#### `exit`

プロセスが終了した。

```json
{
  "type": "exit",
  "exitCode": 0,
  "live": true
}
```

- `live`: `true` はリアルタイムの終了通知。`false` は接続時にすでに終了済みだった場合

#### `yolo_debug`

Yolo モードのデバッグ情報。自動承認の検出状況を通知する。

```json
{
  "type": "yolo_debug",
  "tail": "last lines of PTY output being analyzed..."
}
```

#### `scrollback`

接続時に送信される、直近の出力バッファ。

```json
{
  "type": "scrollback",
  "data": "base64 encoded bytes"
}
```

### Client → Server

#### `input`

ユーザーのキー入力。

```json
{
  "type": "input",
  "data": "base64 encoded bytes"
}
```

#### `resize`

ターミナルサイズの変更。

```json
{
  "type": "resize",
  "cols": 80,
  "rows": 24
}
```

---

## WebSocket 再接続

モバイルブラウザはバックグラウンド遷移やネットワーク切り替えで頻繁に WebSocket が切断される。クライアントは以下の再接続戦略を実装する:

- 切断検出後、exponential backoff で再接続を試行（初回 1 秒、最大 30 秒）
- 再接続成功時にサーバーから `scrollback` メッセージを受信し、表示を復元
- 再接続中は UI にインジケーター（「Reconnecting...」）を表示
- サーバー側は WebSocket 接続に紐づく状態を保持しない（ステートレス）。再接続は新規接続と同じ扱い
