# kojo - Multi-Device Storage Design

複数デバイス間で同一エージェントを継続実行可能にするためのストレージ層再設計。
構造化データは DB に集約し、それ以外は native blob API (補助として WebDAV) でアクセスする。

> Status: **DRAFT v2** — 実装前の設計提案。Codex レビューを反映。

## 1. 背景

### 現状

すべての永続データはローカルファイル直アクセス。

```
~/.config/kojo/
  agents.json                          # agent metadata
  sessions.json                        # session metadata (PTY)
  credentials.db                       # SQLite, machine-bound 暗号化
  credentials.key                      # machine-bound encryption key
  notify_cursors.json
  push_subscriptions.json
  vapid.json                           # VAPID private key を含む
  kojo.lock                            # 単一プロセスロック
  auth/
    owner.token                        # owner Bearer
    agent_tokens/<agent_id>            # per-agent Bearer
  agents/
    agents.json
    groupdms/
      groups.json
      gd_<id>/_channel.jsonl
    <agent_id>/
      avatar.png|svg
      persona.md, persona_summary.md
      MEMORY.md                        # ★ agent CLI が直接 write する
      messages.jsonl
      tasks.json
      autosummary_marker
      cron_paused
      credentials.json                 # machine-bound 暗号化
      books/*.pdf                      # 添付
      temp/*.png                       # 一時画像
      outbox/*                         # 送信予約
      memory/
        recent.md
        YYYY-MM-DD.md
        projects/<name>.md
        topics/<name>.md
        people/<name>.md               # 漏れていた
        archive/*                      # 漏れていた
      index/memory.db                  # SQLite (RAG index, derived)
  chat_history/                        # 外部チャットの差分取得 cursor + 本体
  compactions/                         # 圧縮済みアーカイブ
```

### 問題

1. **マルチデバイス同期不可**: ファイル直書き、lock file は単一プロセス前提
2. **全 sync の容量制約**: 全 peer に full replicate するとサイズが `min(peers)` に縛られる
3. **構造化と非構造化の混在**: JSON / JSONL / Markdown / バイナリの扱いが不統一
4. **Web UI からの編集経路が薄い**: マルチデバイス化すると編集衝突管理が必須

## 2. 設計方針

### 2.1 二系統 + scope 分離

**系統:**

| 系統 | 中身 | 物理 | アクセス |
|------|------|------|---------|
| **構造化データ** | metadata, message, task, memory, settings | SQLite (Hub 集約) | REST API、UI、Agent tool |
| **ブロブ** | 画像, PDF, attachments, RAG index | filesystem | native blob API (主), WebDAV (補助 mount) |

**Scope:**

| scope | 同期 | 例 |
|-------|------|---|
| `global` | Hub に正本、他 peer から read-through | DB rows、avatar、books、outbox、persona.md |
| `local` | peer 固定、同期しない | sessions, PTY state, RAG index, temp/, derived caches |
| `machine` | peer 固定 + machine-bound 暗号化 | credentials.key, credentials.db (per-peer), local cache の machine-only secret |
| `cas` | content-addressed、pin policy で部分複製 | 大型モデル / dataset (将来) |

### 2.2 構造化 vs ブロブの判定基準

「ファイル全体が DB の TEXT 列に入っても破綻しないサイズ・編集頻度」かどうかで分ける。

- 構造化: 行単位の追加・編集・検索が発生 / size 通常 < 1MB / 整合性が要る
- ブロブ: バイナリ or 大きい markdown / 行単位の編集なし / 同時書き込みが少ない

### 2.3 構造化 DB へ集約する対象

| 旧ファイル | 新テーブル | scope |
|-----------|-----------|-------|
| `agents.json` | `agents` | global |
| `sessions.json` | `sessions` | **local** (peer 固有 PTY) |
| `groupdms/groups.json` | `groupdms` | global |
| `groupdms/<id>/_channel.jsonl` | `groupdm_messages` | global |
| `agents/<id>/messages.jsonl` | `agent_messages` | global |
| `agents/<id>/tasks.json` | `agent_tasks` | global |
| `agents/<id>/persona.md` | `agent_persona` | global |
| `agents/<id>/persona_summary.md` | **derived** (再生成可能) | local |
| `agents/<id>/memory/*.md` (date/project/topic/people/archive) | `memory_entries` | global |
| `agents/<id>/MEMORY.md` | `agent_memory` | global (※ 2.6 参照) |
| `agents/<id>/autosummary_marker` | `agent_flags` (KV) | global |
| `cron_paused` (現状 global flag) | `kv` (namespace=`scheduler`, key=`paused`) | global |
| `notify_cursors.json` | `notify_cursors` | global |
| `push_subscriptions.json` | `push_subscriptions` | global |
| `chat_history/` の cursor | `external_chat_cursors` | global |
| `chat_history/` 本体 | (外部から再取得可能なので) **local cache** | local。再取得不可 platform / 権限切れ時は UI に degraded 表示 |
| `compactions/` | `compactions` | global |
| `vapid.json` (public part) | `kv` (namespace=`notify`) | global |
| `vapid.json` (private key) | `kv` (namespace=`notify`, secret=true, envelope encrypted) | global (KEK は cluster-bound、3.4 節参照) |
| `auth/owner.token` (生値) | (migration 時は **import せず v1 で新規生成** = 5.5 step 8。v1 通常運用では Hub kv に bcrypt hash を保存、scope=global) | global (hash) |
| `auth/agent_tokens/<id>` (生値) | 同上 | 同上 |

### 2.4 ブロブ (native blob API)

| 旧パス | 新 URI | scope |
|-------|--------|-------|
| `agents/<id>/avatar.{png,svg}` | `kojo://global/agents/<id>/avatar.<ext>` | global |
| `agents/<id>/books/*` | `kojo://global/agents/<id>/books/*` | global |
| `agents/<id>/outbox/*` | `kojo://global/agents/<id>/outbox/*` | global |
| `agents/<id>/temp/*` | `kojo://local/agents/<id>/temp/*` | local |
| `agents/<id>/index/memory.db` | `kojo://local/agents/<id>/index/memory.db` | local (絶対 mount しない) |
| `agents/<id>/credentials.{json,key}` | `kojo://machine/agents/<id>/credentials.*` | machine |

URI スキーム: `kojo://<scope>/<path>`

**アクセス経路:**

1. **native blob API (主経路)** — REST/HTTP `/api/v1/blob/...`、ETag / If-Match 対応、PUT は temp + sha256 検証 + atomic publish
2. **WebDAV (補助)** — 既存 OS のファイラから mount したい時のみ。後述の制約あり

### 2.5 外部 CLI が書く markdown ファイル (MEMORY.md など) の扱い

claude / codex / gemini など外部 CLI は kojo を介さず直接 `MEMORY.md` を書く。これを DB 正本化するには 3 案:

| 案 | 内容 | 採用 |
|---|------|------|
| A | MCP tool / hook で CLI に DB 経由 write させる | △ ベンダー依存 |
| B | filesystem watcher で MEMORY.md → DB を sync | △ race / 順序問題 |
| **C** | **MEMORY.md は global blob として WebDAV write-through、DB には ETag と body の denormalized copy のみ持つ** | **採用** |

C は「ファイルとして書ける」「他 peer から read 可能」「Web UI からも編集可能」を両立する。
**MEMORY.md は consistency モデルの例外** として扱い、以下のルールを明文化:

- 物理: agent の lock holder peer (= active peer) の `~/.config/kojo/global/agents/<id>/MEMORY.md`
- agent CLI はその active peer のローカル path に write する (mount/FUSE 不要)
- 他 peer (= active 以外) からは **read-only API + WebDAV mount** で参照のみ
- **device switch 時に MEMORY.md home も active peer に追従して移管** する (3.7 節で詳述)
- write 順序の規約:
  1. **agent CLI direct write** → debounce 1〜2 秒 → 安定 sha256 確認 → kojo daemon が DB row を `If-Match: <prev-etag>` で update → 失敗時は merge queue に積み Web UI に通知
  2. **Web UI / API write** → 同一 API ハンドラ内で次の順序で実行する:
     1. `tx_id` (UUID v7) を生成
     2. **intent file** を書く: `MEMORY.md.intent` (temp 名で write → fsync → rename → 親 dir fsync) に `{tx_id, prev_sha256, target_sha256, target_body, started_at}` を JSON 保存。`prev_sha256` は rollback 用に旧 DB body の sha256 を入れる
     3. DB tx open + 現在 etag 検証 + `agent_memory` を UPDATE (body, body_sha256, version+1, **last_tx_id = tx_id**, etag, updated_at)。まだ commit しない
     4. atomic write: temp file 書き込み → fsync(temp_fd)
     5. **pre-rename sha check**: 現 `MEMORY.md` を re-read して sha256 を計算 → `intent.prev_sha256` と一致するか確認
        - 不一致 → CLI が gate 外で割り込み write した → UI write を **abort**:
          - DB tx rollback、temp 削除、intent 削除
          - `memory_merge_queue` に `(kind="memory", reason="ui_aborted_concurrent_cli", fs_body=現在の CLI write 内容, db_body=intent.target_body)` を記録
          - client に 409 + 現状 etag を返す
        - 一致 → そのまま rename へ
     6. rename → fsync(parent_dir_fd)
     7. rename + dir fsync 成功なら DB tx commit
     8. intent file を削除 (rename → unlink、parent dir fsync)
     7. 失敗時の補償:
        - 任意の step 失敗時は intent file は残したまま 5xx を返す。次の起動時に repair が走る
        - DB tx は context に紐付けてあり handler 終了時に rollback される
  3. **crash recovery (起動時 repair)**:
     起動時に `MEMORY.md.intent` の有無を agent ごとにチェック。
     - **intent file あり** (UI write の中途):
       - DB の `agent_memory.last_tx_id == intent.tx_id`: tx は commit 済 → fs が intent.target_body と一致するか sha256 検証。mismatch なら intent.target_body で fs を再書き込み (atomic) → intent 削除
       - DB の `last_tx_id != intent.tx_id`: tx は commit されていない → fs を **intent.prev_sha256 に対応する body に戻す**。prev body は DB の現 body (commit されてないので prev のまま) なのでそれで上書き。temp 残骸も cleanup → intent 削除
     - **intent file なし** (CLI direct write 中の crash 含む):
       - `fs sha256 == agent_memory.body_sha256` → 整合、何もしない
       - mismatch:
         - **fs を正と見なして DB を更新** (CLI が直近に書いた値を保存)
         - last_tx_id は NULL に、version をインクリメント、新 etag 発行、invalidation broadcast
         - merge queue table に「UI との潜在競合」として記録、UI で人間レビュー可能に
     注: 「DB 正で fs 上書き」一辺倒だと未同期の CLI direct write を破壊する。intent file + `last_tx_id` 永続列で write 元を区別することで両方向の repair を成立させる
  4. fsnotify は CLI direct write の検出専用。UI 経由の自己 write は intent file の存在 + inode/path で除外
  5. **UI write と CLI direct write の競合制御** (per-agent write gate):
     - kojo daemon は agent ごとに in-memory `sync.Mutex` (`memoryWriteGate[agent_id]`) を持つ
     - UI write は handler 開始時に **gate を Lock** し、case 2 の step 1〜6 完了まで保持。完了で Unlock
     - fsnotify event を受けた CLI sync 処理は **gate を TryLock**:
       - 取れた → 即 DB sync (case 1 の通常経路)
       - 取れなかった (UI write 進行中) → event を queue し、Unlock 後に **post-write sha256 再検証** を実施:
         - fs sha256 == intent.target_sha256 → UI write の結果のまま (CLI は何も書かなかった or UI と同じ内容)
         - fs sha256 != intent.target_sha256 → UI window 中に CLI が上書きした → `memory_merge_queue` に「fs / db 両 body」を記録、UI で人間レビュー
     - これにより intent file 存在中の CLI direct write が「UI write の取り違え」として消失するのを防ぐ
     - **agent CLI 側を停止する設計は採らない**: PTY を止めると UX が悪化するため、merge queue で事後 reconcile する方針
     - **保証レベル (明示的 decision)**:
       - **v1 = best-effort**: pre-rename sha check と rename の間 (μs オーダー) の race は理論上残る。debounce 1〜2 秒で実用上ほぼ起きないが、**完全な喪失防止は保証しない**
       - 検知経路: post-write 検証 (上記 5.) + 起動時 repair (3.) + 定期 scrub → `memory_merge_queue` 経由で人間レビュー
       - **既知の未検知ケース**: pre-rename check 後、UI rename 直前に CLI が write → UI rename がそれを上書き → 結果 sha == target なので post-write 検証もパス → 起動時 repair も intent file と DB 整合のため検知不能。最終的に **silent loss となる可能性** が μs window で存在する
       - 実装着手の可否はこの best-effort 保証 (silent loss の極小可能性を含む) を受け入れることが前提
     - **agent CLI 側を停止する設計は採らない**: PTY を止めると UX が悪化するため、merge queue で事後 reconcile する方針
  6. **v1.x で完全保証に格上げ**: agent CLI の MEMORY.md write を **kojo MCP tool 経由 (`kojo_memory_put`)** に強制移行することで race を完全に消す。具体的に:
     - kojo daemon が MCP server として `kojo_memory_put(agent_id, body, prev_etag)` を expose
     - 各 CLI の system prompt から「MEMORY.md を直接 edit せよ」の指示を削除し、**MCP tool 経由 write を必須化**
     - MCP 経由 write は daemon の per-agent gate の中で実行されるため、UI write と同じ排他下で動く
     - これは memory_entries の cutover 前提条件と同等の運用変更。完了後に 2.5 case 1 (CLI direct write 経路) は **deprecated**、case 2 のみが正規経路となり race が消える
     - v1 ↔ v1.x の移行期間は両経路併存。`memory_merge_queue` の発生頻度をモニタリングして cutover 判断
- DB の `agent_memory` 行は `agent_id (PK), body, body_sha256, last_tx_id, version, etag, created_at, updated_at, peer_id` を持つ。`last_tx_id` は repair 時に「UI write の commit 状態」を判別するのに使う (NULL = CLI direct write 起源、UUID = UI write の最終 tx)

`memory/*.md` (日次・project・topic・people・archive) は粒度が細かく数も多いので **DB 正本** にする。Web UI 編集が中心、外部 CLI は kojo MCP tool 経由で `memory_entry_get` / `memory_entry_put` を呼ぶ運用に移す。
**cutover 前提条件**: memory_entries の cutover phase に進む前に **(a) MCP tool API 提供 (b) 各 CLI の system prompt から `memory/*.md` 直接 edit 指示を削除し MCP tool 経由 write に置換** を必須完了項目とする。

### 2.6 Web UI の Edit / Export 要件

| データ | View | Edit | 競合管理 | Export |
|--------|------|------|---------|--------|
| agent metadata | ✓ | ✓ | etag (If-Match) | JSON |
| persona | ✓ | ✓ markdown editor | etag | `.md` |
| MEMORY.md | ✓ | ✓ | ETag + atomic publish | `.md` |
| memory_entries | ✓ list + render | ✓ | etag | `.md` ごと / zip |
| messages / DM | ✓ | ✓ row edit/delete | per-row version + tombstone | `.jsonl` |
| tasks | ✓ | ✓ | etag | JSON |
| settings / kv | ✓ | ✓ | etag | JSON (secret は redacted) |
| compactions | ✓ | read-only | — | `.md` / JSON |
| 全 agent 一括 | — | — | — | `.zip` (legacy 互換, secret redacted) |
| import | dry-run UI | conflict policy 選択 | schema version validate | `.zip` 入力 |

**Export 方針:**

- legacy ファイル layout を **論理的に同等** に再生成 (bit-identical は諦める。timestamp / JSON key 順は normalize)
- secret 列 (token, credential 平文) は **redacted export** がデフォルト。`--include-secrets` を明示時のみ encrypted blob で含める (受け取り側は復号鍵が要る)
- blob 欠損 / home peer unreachable / sha256 mismatch は zip 内 `manifest.json` に記録

## 3. ストレージ構成

### 3.1 Hub と Peer

```
┌─ Hub peer ─────────────────────────────────────────┐
│  kojo.db (SQLite primary, WAL mode)                  │
│   ├─ schema_version, migrations                      │
│   ├─ agents, agent_messages, ...                     │
│   ├─ blob_refs (URI ↔ home_peer ↔ size, sha256, refcount) │
│   └─ peer_registry, agent_locks                      │
│                                                       │
│  global blob store (filesystem)                      │
│   └─ ~/.config/kojo/global/agents/<id>/...            │
│                                                       │
│  HTTP API + WebDAV server (補助)                      │
└───────────────────────────────────────────────────────┘

┌─ Other peer ────────────────────────────────────────┐
│  kojo.db (read-through cache, write は Hub に投げる) │
│   └─ subscribe で invalidate (後述)                   │
│  local blob store + machine secret store             │
│  HTTP API + WebDAV server (local/machine)            │
│  WebDAV client (Hub の global を補助 mount, read-only) │
└───────────────────────────────────────────────────────┘
```

### 3.2 共通カラム規約

ユーザー編集可能な domain table に以下を持たせる (kv / blob_refs / peer_registry / agent_locks / migration_status は性質が違うので例外、3.3 末尾の例外表参照):

```sql
id          TEXT PRIMARY KEY
seq         INTEGER NOT NULL    -- 単調増加。partition は table ごと指定
version     INTEGER NOT NULL DEFAULT 1   -- optimistic lock
etag        TEXT NOT NULL       -- "<version>-<sha256(canonical_record)[:8]>"
created_at  INTEGER NOT NULL    -- UTC epoch milliseconds
updated_at  INTEGER NOT NULL
deleted_at  INTEGER             -- soft delete (tombstone, GC は grace period 後)
peer_id     TEXT                -- 作成 peer (audit / debug)
```

**`etag` の canonical_record**: テーブルごとに hash 対象フィールドを明示する:

| table | canonical fields |
|-------|------------------|
| agents | id, name, persona_ref, settings_json |
| agent_messages | id, agent_id, seq, role, content, thinking, tool_uses, attachments, usage |
| memory_entries | id, agent_id, kind, name, body |
| agent_persona | agent_id, body |
| agent_memory | agent_id, body |
| tasks | id, agent_id, title, body, status, due_at |
| kv | namespace, key, value/value_encrypted, type |

**`seq` の partition**:

| table | partition | 用途 |
|-------|-----------|------|
| agent_messages | per `agent_id` | 会話順序 |
| memory_entries | per `agent_id` | 編集順序 |
| groupdm_messages | per `groupdm_id` | チャット順序 |
| 他 | global | 通常用 |

時刻は **UTC epoch ms**。表示時に timezone 適用。順序依存の処理は `seq` を使い、timestamp order に依存しない。

### 3.3 主要テーブル

```sql
CREATE TABLE agent_messages (
  id TEXT PRIMARY KEY,
  agent_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  role TEXT NOT NULL,                   -- user|assistant|system|tool
  content TEXT,
  thinking TEXT,
  tool_uses TEXT,                       -- JSON
  attachments TEXT,                     -- JSON: [{kind, blob_uri, sha256, size, name}]
  usage TEXT,                           -- JSON
  version INTEGER NOT NULL DEFAULT 1,
  etag TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  peer_id TEXT,
  UNIQUE (agent_id, seq)
);
CREATE INDEX idx_agent_messages_agent_seq ON agent_messages (agent_id, seq);

CREATE TABLE agent_memory (
  agent_id TEXT PRIMARY KEY,             -- 1 row per agent
  body TEXT NOT NULL,                    -- MEMORY.md 内容 (denormalized copy)
  body_sha256 TEXT NOT NULL,
  last_tx_id TEXT,                       -- 最後の UI write の tx_id (UUID v7)。NULL = CLI direct write 起源
  -- 共通カラム --
  version INTEGER NOT NULL DEFAULT 1,
  etag TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  peer_id TEXT
);
-- 物理 fs (kojo://global/agents/<id>/MEMORY.md) と body は denormalized copy。
-- crash recovery は 2.5 節のルールに従う。

CREATE TABLE memory_merge_queue (
  id TEXT PRIMARY KEY,                    -- UUID
  agent_id TEXT NOT NULL,
  kind TEXT NOT NULL,                     -- "memory" | "memory_entry"
  entry_id TEXT,                          -- memory_entries.id (kind=memory_entry の時)
  reason TEXT NOT NULL,                   -- "ui_aborted_concurrent_cli" | "post_write_mismatch" | "startup_repair_diverged" | "cli_direct_db_mismatch"
  fs_body TEXT NOT NULL,
  db_body TEXT NOT NULL,
  detected_at INTEGER NOT NULL,
  resolved_at INTEGER,
  resolution TEXT                         -- "fs"|"db"|"manual"
);

CREATE TABLE idempotency_keys (
  key TEXT PRIMARY KEY,                  -- client が生成した UUID
  op_id TEXT,                            -- op-log entry の op_id (3.13.1 から flush 時)
  request_hash TEXT NOT NULL,            -- body の sha256 (re-send 検証用)
  response_status INTEGER NOT NULL,
  response_etag TEXT,
  response_body TEXT,
  expires_at INTEGER NOT NULL            -- 24h
);

CREATE TABLE cron_runs (
  cron_run_id TEXT PRIMARY KEY,           -- UUID
  agent_id TEXT NOT NULL,
  scheduled_at INTEGER NOT NULL,
  claimed_by_peer TEXT,                   -- 先勝 claim
  status TEXT NOT NULL,                   -- claimed|completed|failed
  started_at INTEGER,
  finished_at INTEGER,
  UNIQUE (agent_id, scheduled_at)         -- 同一 schedule の二重実行防止
);

CREATE TABLE memory_entries (
  id TEXT PRIMARY KEY,
  agent_id TEXT NOT NULL,
  seq INTEGER NOT NULL,                  -- per agent_id partition
  kind TEXT NOT NULL,                    -- daily|project|topic|people|archive
  name TEXT NOT NULL,                    -- date or slug
  body TEXT NOT NULL,
  body_sha256 TEXT NOT NULL,
  -- 共通カラム --
  version INTEGER NOT NULL DEFAULT 1,
  etag TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  peer_id TEXT,
  UNIQUE (agent_id, kind, name),
  UNIQUE (agent_id, seq)
);

CREATE TABLE kv (
  namespace TEXT NOT NULL,              -- auth|notify|settings|...
  key TEXT NOT NULL,
  value TEXT,                            -- 平文 (非 secret)
  value_encrypted BLOB,                  -- secret=true の時に envelope 暗号化 (KEK は scope に応じて cluster-bound または machine-bound)
  type TEXT NOT NULL,                    -- string|json|binary
  secret INTEGER NOT NULL DEFAULT 0,
  scope TEXT NOT NULL,                   -- global|local|machine
  -- 共通カラム --
  version INTEGER NOT NULL DEFAULT 1,
  etag TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (namespace, key)
);

CREATE TABLE blob_refs (
  uri TEXT PRIMARY KEY,                  -- kojo://<scope>/<path>
  scope TEXT NOT NULL,
  home_peer TEXT NOT NULL,
  size INTEGER NOT NULL,
  sha256 TEXT NOT NULL,
  refcount INTEGER NOT NULL DEFAULT 1,
  pin_policy TEXT,                       -- JSON: {peers:[...], cache_max:N}
  last_seen_ok INTEGER,                  -- home peer health
  marked_for_gc_at INTEGER,              -- mark phase: refcount=0 になった時刻
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
-- GC: marked_for_gc_at が 24h より古い blob を mark/sweep で物理削除。
-- import rollback / undo 操作のための grace period。

CREATE TABLE push_subscriptions (
  endpoint TEXT PRIMARY KEY,
  device_id TEXT,                        -- peer_registry.device_id (発行元)
  user_agent TEXT,
  vapid_public_key TEXT NOT NULL,        -- key rotation 時の対応
  p256dh TEXT NOT NULL,
  auth TEXT NOT NULL,
  expired_at INTEGER,                    -- 401/410 受領後にセット
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE peer_registry (
  device_id TEXT PRIMARY KEY,             -- stable, GUID
  name TEXT NOT NULL,
  public_key TEXT NOT NULL,               -- peer 認証鍵 (Bearer とは別物)
  capabilities TEXT,                      -- JSON: {os, arch, gpu, ...}
  last_seen INTEGER,
  status TEXT                             -- online|offline|degraded
);

CREATE TABLE agent_locks (
  agent_id TEXT PRIMARY KEY,
  holder_peer TEXT NOT NULL,
  fencing_token INTEGER NOT NULL,         -- 単調増加
  lease_expires_at INTEGER NOT NULL,
  acquired_at INTEGER NOT NULL
);

CREATE TABLE migration_status (
  domain TEXT PRIMARY KEY,                -- agents|messages|memory|...
  phase TEXT NOT NULL,                    -- pending|imported|cutover|complete
  source_checksum TEXT,                   -- 旧ファイル群の SHA256 マニフェスト
  imported_count INTEGER,
  started_at INTEGER,
  finished_at INTEGER
);
```

**共通カラム規約の例外**:

| table | 例外内容 | 理由 |
|-------|---------|------|
| `kv` | composite PK (namespace, key)、`id` なし | KV 構造 |
| `blob_refs` | `version`/`etag`/`deleted_at` なし、代わりに `sha256` が identity | blob hash で一意 |
| `peer_registry` | seq なし、`device_id` PK | 構造が異なる |
| `agent_locks` | seq なし、fencing_token が代わり | lock 専用 |
| `migration_status` | seq なし | one-shot |

将来拡張: schema migration tool (`golang-migrate` または自前 numbered SQL files) を必須。

### 3.4 認証とトークン取り扱い

- **owner / agent token の生値はどこにも保存しない**
  - 各クライアント (Web UI / agent CLI) が初回受領時に local に記憶
  - **Hub DB に hash (bcrypt or scrypt) を保存** = global 正本。Hub に集約することで全 peer が同じ hash 集合で検証可能
  - 各 peer は Hub からこの hash 集合を pull (read-through cache)。Bearer 受信時に local hash と比較
  - 旧 `auth/owner.token` `auth/agent_tokens/*` は migration 時 **import せず**、v1 で新規 token を生成 → Web UI / agent CLI に再配布要求 (5.5 step 8)。v0 の生値ファイルは v1 から書き換えず、`kojo clean` まで残す
- WebDAV 用に **別の短命 token** を発行 (scope 限定、TTL 数時間、UI から発行/失効可能)
- VAPID **private** key の扱い:
  - v1 は **Hub kv (secret=true) に envelope 暗号化** で保存
    - データ鍵 (DEK): VAPID private を AES-GCM で暗号化
    - 鍵暗号化鍵 (KEK): Hub と backup peer に同じ KEK を pre-shared (machine-bound ではなく **kojo-cluster-bound**)
    - Hub snapshot (3.6 節) には暗号化済み private + KEK 識別子を含める。backup peer は自身の KEK で復号して新 Hub として継続可能
  - 完全な machine-bound 暗号化にすると failover 時に復号不能になるため避ける
  - KEK のローテーションは v2
  - 公開鍵は global kv (平文)
- credentials.db (現存) は **据え置き** で各 peer が独自鍵で暗号化保持。Hub 統合はしない (envelope encryption + device grant プロトコルが必要なため、本ドキュメント範囲外)
  - **制約**: device switch 先 peer に該当 credential が無ければ credential-dependent task は失敗する。UI に明示

### 3.5 Consistency モデル (v1)

割り切りの簡素化を優先:

- **All owner-admin write goes to Hub.** Web UI / API 経由の編集は Hub オフライン中は**全て拒否** (queue しない)
- **agent-runtime write は bounded op-log で例外的に queue 可** (3.13.1 で詳述)
  - 対象は agent CLI が動作中に発火する transcript append / MEMORY.md update / memory_entries の MCP write
  - 上限到達で agent session 強制終了、silent loss 禁止
- **Read** は peer のローカル cache から行う。Hub は変更時に WebSocket / SSE で `(table, id, etag)` invalidation を push
- **Read-your-writes**: write API は Hub 側が確定後に新 etag を返す。peer はその etag を local cache に反映してから 200 を返す
- **Stale 表示ルール**: invalidation 受信後の cache 失効は次の read で再取得。古い表示は etag mismatch を検知して reload
- **Optimistic lock**: 全 write は `If-Match: <etag>` を要求。mismatch なら 409 + 現状を返す → UI で diff/merge
- **agent identity lock + fencing token**: write 種別を 2 つに分ける
  - **agent-runtime write** (agent CLI 自身が発火する transcript append、MEMORY.md update、tool side effect): lock holder peer + 一致する fencing token を要求。他 peer からの同種 write は拒否
  - **owner-admin write** (Web UI から人間が行う metadata 編集、persona 編集、message edit/delete、task 編集など): lock 状態に関係なく実行可。If-Match (etag) のみで保護
  - 区別は API path / 認証主体 (owner Bearer vs agent Bearer) で行う
- **再同期用 cursor**: invalidation broadcast の event drop に備え、`GET /api/v1/changes?since=<seq>` で domain ごとの差分を取得可能にする。peer 起動時 / 再接続時に必ず flush
- **idempotency key**: 全 write API は `Idempotency-Key: <UUID>` ヘッダ必須。Hub は最近 24h の key を `idempotency_keys(key, response_etag, op_id, expires_at)` テーブルに保存し、同一 key の重複リクエストは保存済みレスポンスを返す。これにより client retry / op-log replay が安全

### 3.6 SPOF / failover (v1 = manual)

Hub 落ちると write 不可。最初は **手動 failover** で割り切る。**snapshot 起点復旧** を主経路とし、live rsync は補助。

**事前要件 (常時)**:

- Hub は `~/.config/kojo/snapshots/<ts>/` に SQLite `.backup` API + blob global tree + envelope-encrypted secret 列の snapshot を **定期取得** (cron で 1 時間ごと等)。machine-bound な per-peer secret (credentials.db, machine credentials.key) は snapshot に含めない
- snapshot は backup peer に rsync で逐次転送 (Tailscale 上)

**Hub が live (graceful failover)**:

```
1. 旧 Hub の SQLite を停止 (process kill, fsync 完了確認)
2. 最新差分を新 Hub に rsync
3. 新 Hub の config で hub_role=primary に
4. 各 peer の config で hub_url を新 Hub に変更
5. agent_locks の lease 切れを待つ (デフォルト 60s)
6. blob_refs.home_peer も必要に応じ書き換え
```

**Hub が完全故障 (snapshot 起点)**:

```
1. backup peer 上の最新 snapshot から kojo.db / blob global tree / envelope-encrypted secret 列を展開 (KEK は backup peer が保持)
2. 同 peer を新 Hub として起動
3. 各 peer の config で hub_url を新 Hub に変更
4. snapshot 取得 ts 以降の write は失われる (RPO = snapshot 周期)
5. UI に「snapshot ts まで復旧、以降の編集は失われた可能性あり」を表示
```

leader election (Raft / litestream replica) は v2。

### 3.7 Device switch 時の MEMORY.md home 移管

`kojo_switch_device(target)` 実行時の MEMORY.md handoff:

```
1. source peer: agent CLI 終了確認 (debounce 待ち、最終 sha256 確定)
2. source peer: DB row を最終 sha256 で update + 新 etag
3. blob_refs.handoff_pending = true をセット (まだ home_peer は source のまま)
   この間 source / target どちらの write も「handoff 中」として 409 を返す
4. target peer: blob を pull (sha256 検証)
5. pull 成功なら blob_refs.home_peer = target、handoff_pending = false に切替
   pull 失敗 / timeout なら handoff_pending を解除し source 継続 (rollback)
6. agent_locks の holder + fencing_token を target に移譲
7. invalidation broadcast: 全 peer の WebDAV mount を再 mount または cache invalidate
8. agent CLI を target peer で resume
```

`handoff_pending` 状態を挟むことで、target pull 完了前に他 peer が新 home から read してしまう競合を防ぐ。

`memory/*.md` (DB 正本) は home 概念がないので switch しても変更不要。

### 3.8 WorkDir の扱い

agent metadata の `WorkDir` は **絶対パス** で machine-local。global テーブルにそのまま入れると意味が壊れる。対策:

- agent table には `workspace_id` (logical) のみ持つ
- 別テーブル `workspace_paths(workspace_id, peer_id, path)` で peer ごとのマッピングを管理
- agent CLI 起動時に「現 peer の path mapping」を解決
- mapping 未設定の peer に switch しようとしたら UI でセットアップ要求

### 3.9 Cron / Notify / GroupDM の delivery 担当

「どの peer が実行するか」を明示しないと重複通知が起きる。ルール:

- **cron 実行 / notify poll** は agent の **lock holder peer のみ** が行う
- agent が休眠中 (lock 未取得) の cron は Hub が代理で起動指令を出す (どの peer に立ち上げるかは pin policy / 直近 lock holder)
- groupDM の outbound 配送は Hub 一元化 (前項 DM 集約と一致)

### 3.10 Heartbeat / lease / 時計

- **両方向 heartbeat**: peer ↔ Hub の双方向で 10 秒間隔。片方向のみ通る partition を検知
- **lease は Hub 時刻基準**: agent_locks.lease_expires_at は Hub の clock で記録。peer の時計は信用しない
- heartbeat レスポンスに Hub の現在時刻を載せ、peer は時計差分 (skew) を計算。skew > 5s で UI 警告
- peer は次 heartbeat までの余裕を Hub clock 基準で計算
- lease のデフォルト 60s、heartbeat 5 失敗で expire

## 3-bis. 障害シナリオ

すべての failure mode を列挙し、検知 / 影響 / 復旧手順を定義する。

### 3.11 Hub 障害

| 種別 | 検知 | 影響 | 復旧 |
|------|------|------|------|
| Hub crash (graceful) | peer の heartbeat 失敗 | 全 peer が write 拒否 (read は cache) | live failover (3.6) |
| Hub partial (NIC 死亡) | 同上 (= 全 peer から unreachable) | 同上 | 同上 or Hub 自己診断 → 自身を degraded |
| Hub disk full | write 5xx + Hub 内部 alert | 全 write 失敗 | 容量回復 or snapshot rotation で GC |
| snapshot 取得失敗 (継続) | cron alert + UI に snapshot lag 表示 | 直接の影響なし、ただし RPO 悪化 | snapshot 路復旧 (disk / rsync 経路) |
| 完全故障 (DB 破損 / disk 喪失) | peer 全停止 | snapshot 起点の RPO 損失 | snapshot 起点 failover (3.6) |

### 3.12 Non-Hub peer 障害

#### 3.12.1 Lock holder peer

```
1. 該当 peer の heartbeat 停止
2. Hub: lease_expires_at を経過 (default 60s) → agent_locks 行を release
3. fencing_token を新 holder にインクリメント発行
4. 旧 holder が後で復活して write しても fencing token mismatch で 409 拒否
5. 該当 agent の PTY セッション (local scope) は失われる
   - claude なら別 peer で --continue で再開 (transcript 末尾数 turn が失われる可能性)
   - codex / gemini も同等の resume 機構 / kojo session ID 維持
6. transcript append: peer 側 buffer に積まれていた未 ack 分は消失
   buffer 上限超過時の policy: session 強制終了 + UI 通知 (silent loss は禁止)
```

該当 peer に MEMORY.md home があった場合:

- 該当 blob の global tree が物理欠損
- handoff_pending = false の状態なら DB の denormalized body から **DB → filesystem repair** で再生成 (新 active peer 上で)
- handoff_pending = true (switch 中に死んだ) 場合: source/target 両方の blob が中途半端 → snapshot から復元

#### 3.12.2 Lock holder ではない peer

- Hub が peer_registry.status = offline に更新
- その peer の **local blob** (RAG index, temp/, chat_history cache) は他 peer から見えない
  - switch 候補から外す
  - 該当 agent が他 peer に switch しようとしたら index は target peer で再構築 (cold start)
- **global blob の home がそこにあった場合** → 前項と同じ修復経路
- 復旧時:
  1. peer 起動 → Hub に heartbeat
  2. `GET /api/v1/changes?since=<seq>` で各 domain の差分を pull
  3. blob_refs.last_seen_ok 更新
  4. status = online

#### 3.12.3 Peer disk full

- 該当 peer は write 拒否、status = degraded
- 新規 agent 起動も拒否
- 既存 lock は expire させて他 peer に逃がす運用
- UI で容量警告

### 3.13 Network 障害

#### 3.13.1 Peer ↔ Hub partition

**owner-admin write** (Web UI 経由の人間編集) は全拒否:

- peer 上の Web UI は read-only モード、edit ボタン disabled
- read は local cache から提供 (UI に stale 警告バッジ)

**agent-runtime write** は **bounded op-log** に書いて partition 復帰後に flush:

```
~/.config/kojo/oplog/<agent_id>/
  current.jsonl            # 現在の append-only log
  rotated.<seq>.jsonl      # ローテーション後 (flush 待ち)
```

各 entry の構造:

```json
{
  "op_id": "<UUID v7, client 生成>",
  "agent_id": "ag_xxx",
  "fencing_token": 42,
  "seq": 1234,                  // partition 中の暫定 seq (Hub で実 seq に再採番)
  "table": "agent_messages",
  "op": "insert|update|delete",
  "body": {...},
  "client_ts": 1735000000000
}
```

挙動:

1. PTY セッションは Hub 関係なく local で動き続ける
2. agent-runtime write はまず op-log に append + fsync → 成功で agent CLI に 200 返す
3. UI 上は **「pending: N op queued」** バッジを agent ごとに常時表示
4. 上限ポリシー:
   - **size**: 10 MB / agent
   - **count**: 5000 entries / agent
   - **age**: 1 時間
   - いずれか到達で **agent session を強制終了** + UI に「部分喪失リスク」を明示通知。silent loss 禁止
5. partition 復帰検知 (heartbeat 成功) で flush:
   1. fencing_token を Hub と照合。mismatch (lease expired済) なら**全 entry を reject** し UI に「partition 中の編集は失われました」を提示。op-log は archive して人手レビュー可能に
   2. mismatch なし: op_id 順に Hub の write API を idempotency-key 付きで叩く
   3. 各 entry 成功で op-log から削除、最終的に `current.jsonl` 空で完了
   4. flush 中に再 partition: 中断、次回再開
6. fsync 失敗 / disk full: agent session 即停止 (op-log への書き込みが信用できない)

**op-log を許す範囲は agent-runtime write のみ**:
- ✓ agent_messages append
- ✓ agent_memory (MEMORY.md) update
- ✓ memory_entries の MCP tool 経由 write
- ✗ owner-admin write (UI 編集)
- ✗ agent metadata / persona (Hub 必須)
- ✗ groupdm send (Hub 一元化)

理由: agent-runtime は **fencing_token + op_id の二重保護** で重複 / split-brain を防げる。owner-admin は人間入力なので queue より「書けないことを明示」する方が UX が良い。

#### 3.13.2 Tailscale 全障害

- 全 peer が孤立
- Hub は `--local` 相当のローカル loopback で単独運用可能
- 他 peer は完全 read-only。新規 agent 起動も不可
- 復旧で自動的に通常運用に戻る (state は Hub が保持)

#### 3.13.3 片方向疎通

Tailscale で発生し得る (NAT / firewall の状態次第)。例: peer → Hub は通るが Hub → peer は通らない。

- **両方向 heartbeat** で検知 (3.10)
- 片方向しか通らない場合は通常の partition と同じ扱い (write 不可)
- fencing token mismatch だけが split-brain の最終的な調停手段

### 3.14 Split-brain

構造的に発生しない:

- All write goes to Hub (single source of truth)
- 複数 peer が同時に lock 取れない (Hub の atomic transaction で fencing_token 発行)
- 旧 lock holder の遅延 write は fencing token mismatch で拒否

注意点: 「peer から見て Hub down」と「Hub から見て peer down」の判定がずれることはある。fencing token を全 agent-runtime write の必須条件にすることで構造的に防ぐ。

### 3.15 In-flight write 中の障害

| シナリオ | 検知 | 復旧 |
|---------|------|------|
| client → Hub 送信中に切断 | client が ack 未受領 | client retry。message append は seq + If-Match で重複検知 |
| Hub commit 後 ack 前に peer 切断 | client が ack 未受領 | retry すると 409 (etag 進行)。client は `?since=` で確認後 reconcile |
| dual-write (旧ファイル併存): DB commit 後 fs 失敗 | startup repair | 起動時に DB body と fs sha256 を比較 → DB を正として fs 上書き (旧ファイルは migration window でしか正本でないので) |
| dual-write: fs rename 後 DB 失敗 | DB tx 自動 rollback | client は 5xx 受領、retry。fs の temp は cleanup |
| MEMORY.md UI write の rename 後 daemon クラッシュ | intent file 検出 | 2.5 節 case 2 の crash recovery: intent.target_sha256 で fs 再書き込み |
| MEMORY.md CLI direct write 後、DB sync 前に daemon クラッシュ | intent file なし + fs/DB sha256 mismatch | **fs を正として DB を更新** (CLI の最終 write を保存)。merge queue に潜在競合を記録 |

### 3.15-bis Blob integrity / repair

| シナリオ | 検知 | 復旧 |
|---------|------|------|
| global blob の sha256 mismatch (DB と filesystem) | read 時 / 定期 scrub | DB body が正なら fs を上書き、blob_refs を新 sha256 に更新。逆 (fs しかない) は handoff_pending 残骸として snapshot 復元 |
| home_peer unreachable (read 不可) | client read 失敗 / blob_refs.last_seen_ok 古い | UI に degraded 表示 + 「他 peer に handoff」を提案。home peer 復旧 or admin が手動 home 移管 |
| GC 誤削除 (refcount=0 の grace 中に参照復活) | read 失敗 + DB row 健在 | 24h grace 内なら blob 物理削除前なので復活可能。grace 後は snapshot から復元 |
| peer の local cache (kojo.db) corruption | SQLite integrity check 失敗 | local DB drop → Hub から `?since=0` で全 domain 再 pull |
| KEK loss (Hub と backup peer 両方) | secret 復号失敗 | secret 全再生成: owner / agent token 全 revoke + 再 issue、VAPID 鍵再生成 (push subscription 全失効、再購読要求) |
| KEK mismatch (backup peer の KEK が古い) | snapshot restore 後の secret 復号失敗 | 起動時に KEK 識別子を verify。mismatch なら起動拒否 + admin に通知 |
| snapshot restore 失敗 (corrupted) | backup peer 起動時 | より古い snapshot に fallback、それも失敗なら手動 forensics |
| 定期 scrub | cron で 1 日 1 回 blob_refs 全件の sha256 検証 | mismatch 行を alert + 自動 repair attempt |

### 3.16 Token revoke 中の peer

- admin が UI から peer の Bearer を revoke → Hub kv の hash 削除
- peer は次 write で 401 拒否
- automated re-grant はしない。admin に通知 → 人間判断
- 該当 peer が lock を持っていた agent は lease expire 後に他 peer に移譲

### 3.17 WebDAV mount 経由のフリーズ

- macOS Finder / Windows Explorer は mount 先の peer が落ちるとアプリごと固まる
- 対策:
  - 「**mount は補助、native API が主経路**」を運用ドキュメントで強調
  - WebDAV server 側に request timeout / TCP idle timeout
  - mount client 側の timeout 短く (Finder は調整困難なので諦める)
  - UI に「mount 先 peer の health」を表示

### 3.18 ユーザーへの可視化 (UI 必須項目)

障害時の混乱を防ぐため、以下を常時表示:

- 各 peer の status (`online` / `offline` / `degraded`) と最終 heartbeat 時刻
- 自 peer の Hub 接続状態 (`connected` / `partition` / `failover-in-progress`)
- 自 peer が **read-only モード**に入っているか (大きなバナー)
- 最新 snapshot timestamp と lag (Hub 視点)
- agent ごとの lock holder peer + lease expiry
- blob_refs.last_seen_ok が古い blob のリスト (orphan / unreachable 候補)
- transcript buffer の使用量 (partition 中の peer)

silent な failure は禁止。書けない時は「なぜ書けないか」を必ず表示する。

### 3.19 Cron / Notify lease handoff 時の重複配送

agent の lock holder が変わる瞬間に cron / notify が二重実行される懸念:

- cron 実行 / notify poll は `cron_run_id` (UUID) を発行し、Hub の `cron_runs` テーブルに `(agent_id, scheduled_at, cron_run_id, status)` で記録
- lock holder peer は実行前に Hub に `INSERT OR FAIL` で claim を取る。先勝 (claim 取れた peer のみ実行)
- 通知配送も同様に `notify_id` で deduplicate
- groupDM 配送は Hub 一元化なので構造的に重複しない

### 3.20 障害テスト matrix (実装フェーズ完了の DoD)

実装完了の判定基準として、以下の組み合わせを **自動テスト or 手動 drill で必ず実施**:

| カテゴリ | テスト |
|---------|--------|
| Hub 障害 | live failover、snapshot 起点 failover、disk full、partial NIC 死亡 |
| Peer 障害 | lock holder crash、non-holder crash、disk full、token revoke |
| Network | partition、Tailscale 全停止、片方向疎通、間欠切断 |
| Op-log | partition 中 5000 entries 到達、flush 中再 partition、fencing mismatch reject |
| Blob | sha256 mismatch、home unreachable、GC 誤削除、scrub 検出 |
| Secret | KEK loss、KEK mismatch、snapshot 復号失敗 |
| Idempotency | 同 key で N 回 retry、24h 経過後の同 key、key 衝突 |
| Migration | v0 → v1 import 完走、import 中の kill -9、再実行で resume、v0 dir read-only 保証、kojo clean が v1 dir を touch しないこと |
| Restore drill | 年 1 回の backup → restore → write 確認を運用ドキュメントで義務化 |

## 4. API 設計

### 4.1 構造化データ

既存 `/api/v1/agents/...` を DB バックエンドに切替。新規:

```
GET    /api/v1/export/agent/{id}?secrets=redacted|encrypted
POST   /api/v1/import/agent          (multipart: zip + dry_run flag + conflict policy)
GET    /api/v1/kv/{namespace}/{key}
PUT    /api/v1/kv/{namespace}/{key}  (If-Match required)
GET    /api/v1/memory/{agent}/{kind}/{name}
PUT    /api/v1/memory/{agent}/{kind}/{name}   (If-Match required)
GET    /api/v1/memory/{agent}/list?kind=...
GET    /api/v1/peers
POST   /api/v1/peers
GET    /api/v1/peers/{id}/health
```

WebSocket: `/api/v1/events` で invalidation broadcast (`{table, id, etag, op}`)

### 4.2 ブロブ

```
GET    /api/v1/blob/<scope>/<path>            (Range, ETag)
HEAD   /api/v1/blob/<scope>/<path>
PUT    /api/v1/blob/<scope>/<path>            (If-Match, sha256 trailer, atomic publish)
DELETE /api/v1/blob/<scope>/<path>            (If-Match)
GET    /api/v1/blob/<scope>/?prefix=...        (listing)
POST   /api/v1/blob/migrate                    {src_uri, dst_peer}
POST   /api/v1/blob/pin                        {uri, peers:[...], cache_max?}
```

### 4.3 WebDAV (補助)

```
/dav/<scope>/<path>   # PROPFIND/GET/PUT/DELETE/LOCK/UNLOCK
```

- 認証: WebDAV 専用短命 token (Basic auth password 欄に格納)
- write が許される scope は `local` (その peer のみ) と home peer の `global` のみ
- **`/dav/<scope>/...` で SQLite ファイル (memory.db, kojo.db) を expose しない** (mount 越しに開かれると壊れる)
- **blob path 規約**:
  - 文字エンコーディング: NFC 正規化必須 (macOS Finder は NFD で書き込むので server 側で normalize)
  - case-sensitive: server 側は case-sensitive。同名 (case 違い) は衝突として 409
  - 予約ファイル名: `.DS_Store`, `Thumbs.db`, `desktop.ini`, `~$*`, `.AppleDouble/`, `._*` は **server 側で 自動破棄**
  - PUT は temp + sha256 検証 + atomic rename。If-Match (etag) を要求、なければ 412
- 既知の落とし穴: macOS Finder の WebDAV は LOCK の挙動が古い、Windows native mount は LOCK / 大きい PUT で詰まる、Linux davfs は cache 整合性
  → **「mount は便利機能、メイン経路は native API」と運用ドキュメントで明記**

## 5. マイグレーション手順 (v0 → v1)

### 5.1 設計方針 (decision)

v1 は **breaking release**。後方互換 dual-write は採らず、**別ディレクトリへの片方向 import** で行う:

- v0 dir は v1 から **read-only**。一切書き換えない (data loss を構造的に防止)
- v1 dir は完全に独立した新スキーマ
- 失敗時のロールバック = v0 binary を再起動するだけ
- v0 dir の削除は `kojo clean` で **ユーザーが明示的に** 行う。kojo は自動削除しない
- v1 → v0 の逆方向 migration はサポートしない (片方向)

これにより旧ドキュメントの dual-write window / cutover state machine / legacy copy は全て不要になり、import コードは one-shot で完結する。

### 5.2 Path

| OS / 環境 | v0 | v1 |
|----------|-----|-----|
| macOS | `~/.config/kojo/` | `~/.config/kojo-v1/` |
| Linux (XDG_CONFIG_HOME 未設定) | `~/.config/kojo/` | `~/.config/kojo-v1/` |
| Linux (XDG_CONFIG_HOME 設定済) | `$XDG_CONFIG_HOME/kojo/` | `$XDG_CONFIG_HOME/kojo-v1/` |
| Windows | `%APPDATA%\kojo` | `%APPDATA%\kojo-v1` |

将来 v2 で再 breaking する場合は `kojo-v2` を使う。`internal/configdir/` は major version suffix を resolver として持つ。

### 5.3 起動時 trigger

```
v1 binary 起動時:
  if v1 dir 存在 (= migration_complete.json あり):
      通常起動
  elif v0 dir 存在:
      print "v0 data detected at <path>."
      print "Run with --migrate to import, --fresh for new install."
      exit 1
  else:
      新規セットアップ → v1 dir 作成 → 起動
```

`--migrate`, `--fresh` フラグで明示的に分岐。interactive prompt は採らない (CI / headless 対応)。

### 5.4 二重起動防止

- `--migrate` 実行前に v0 dir の `kojo.lock` を check
- v0 が動作中なら migration 拒否、「v0 を停止してから --migrate を実行」
- v1 自身も v1 dir に `kojo.lock` を取得 (既存実装)

### 5.5 Migration の流れ (`--migrate` 実行時)

1. v0 dir の `kojo.lock` を check (動作中なら拒否)
2. v0 dir の最近 5 分以内の mtime walk → 直近変更があれば「外部書き換えの可能性。本当に migration するか確認」
3. v0 dir 全体の sha256 manifest を取得 → `migration_in_progress.lock` ファイルに記録 (中断検出用)
4. ディスク容量チェック (v0 size × 1.2 の余裕)
5. v1 dir 作成、SQLite schema 適用
6. domain ごとに **v0 file → v1 DB に import** (idempotent、各 domain の `migration_status.source_checksum` を記録):

| v0 source | v1 destination |
|-----------|----------------|
| `agents.json` | `agents` table |
| `sessions.json` | `sessions` table (local scope、peer_id 付与、**status は全て `archived` に強制**。live import 不可、PTY は v0 停止と同時に消失) |
| `agents/<id>/messages.jsonl` | `agent_messages` |
| `agents/groupdms/groups.json` + `gd_<id>/_channel.jsonl` | `groupdms` + `groupdm_messages` |
| `agents/<id>/tasks.json` | `agent_tasks` |
| `agents/<id>/memory/*.md` | `memory_entries` |
| `agents/<id>/MEMORY.md` | `kojo://global/agents/<id>/MEMORY.md` (blob) + `agent_memory` denormalized |
| `agents/<id>/persona.md`, `persona_summary.md` | `agent_persona` (summary は再生成可能なので skip) |
| `notify_cursors.json` | `notify_cursors` |
| `push_subscriptions.json` | `push_subscriptions` |
| `vapid.json` (public) | `kv` namespace=`notify` (平文) |
| `vapid.json` (private) | `kv` envelope encrypted |
| `auth/owner.token` | (import せず、v1 で新規生成。v0 file 自体は kojo clean まで残る) |
| `auth/agent_tokens/<id>` | 同上 |
| `chat_history/` cursor | `external_chat_cursors` |
| `chat_history/` 本体 | (再取得対象、import しない) |
| `compactions/` | `compactions` |
| `credentials.db`, `credentials.key` | v1 dir に **そのままコピー** (machine-bound 据え置き) |

7. blob (avatar, books, outbox, temp, index/memory.db) を v1 blob store にコピー、`blob_refs` 登録
   - **agent dir 直下の hidden dotfile も含める**:
     - `.claude/settings.local.json`, `.claude/hooks/`, `.claude/captures/` (claude project-local hooks / capture history)
     - `.codex/...`, `.gemini/...` (もし存在すれば)
     - `.cron_last` (cron 実行 marker)
   - これらは agent CLI が CWD = agent dir 上で参照する project-local file。layout は変えず agent dir 配下にそのまま再配置
   - **コピー時の制約**:
     - symlink を follow しない (`os.Lstat` で判定、`os.Symlink` で再現せず skip + warning)。v0 dir の中に予想外の symlink が混入していてもそれを辿って外部参照しない
     - v0 dir 外への遷移を禁止 (filepath.EvalSymlinks の結果が v0 root prefix で始まることを assert)
8. owner / agent token は **新規生成** して既存クライアントに再配布要求
   - **decision**: import せず再生成する。理由は (a) v0 では平文ファイル保存だったが v1 は hash のみ保存に変更する **breaking security upgrade**、(b) breaking release のタイミングで token rotation を兼ねる、の 2 点。技術的には旧 token を hash 化して持ち込むことも可能だが採用しない
   - **Web UI の owner Bearer**: 再ログイン要求 (UI で新 token を入力)
   - **agent CLI の per-agent Bearer**: 各 agent の再 setup
   - **Web Push subscription**: VAPID private は import するため subscription **は失効しない** (browser 側 endpoint と vapid_public_key は不変)
9. (optional) `--migrate-backup <path>` 指定時、v0 dir 全体を `<path>/kojo-v0-<ts>.zip` に固める (read-only verify とは別の安心材料として)
10. **post-import manifest verify**: v0 dir の sha256 manifest を **再計算** し、step 3 で記録した値と一致するか確認
    - 不一致 → import 中に v0 が外部から書き換えられた → migration を **fail** させる
    - v1 dir はそのまま残す (`--migrate-restart` で再実行可能)
    - `migration_complete.json` は作成しない
11. 一致した場合のみ `migration_in_progress.lock` を `migration_complete.json` に rename。中身:
    ```json
    {
      "v0_path": "...",
      "v0_sha256_manifest": "...",
      "v1_schema_version": 1,
      "completed_at": 1735000000000,
      "migrator_version": "v1.0.0"
    }
    ```
    起動時に **manifest と v1 schema_version の整合性を verify**。改竄 / 不整合検出時は起動拒否
12. 起動メッセージに「migration 完了。v0 data は <path> に残っています。kojo clean で削除可能」を表示

### 5.5.1 外部 CLI transcript dir の継続性

各 agent backend (`internal/agent/backend_{claude,codex,gemini}.go`) は CWD を agent dir に設定する:

```go
cmd.Dir = agentDir(agent.ID)   // = configdir.Path() + "/agents/<id>/"
```

claude / codex は **CWD の絶対 path から hash を導出** して transcript / session を `<claude_config_dir>/projects/<hash>/`、`~/.codex/sessions/<hash>/` などに保存する。

- claude config dir は `$CLAUDE_CONFIG_DIR` 環境変数があればそれを優先、なければ `~/.claude` (`internal/agent/backend_claude.go` の `claudeConfigDir`)。migration コードも同じ resolver を使うこと。`~/.claude` 固定の hard-code は禁止
- codex は実装時に確認

gemini は **path hash ではなく** `~/.gemini/projects.json` の `absDir → projectName` mapping を保持する設計 (`internal/agent/backend_gemini.go` の `hasGeminiSession`)。session は `~/.gemini/tmp/<projectName>/chats/*.json` に格納される。**hash 方式とは別設計** が必要 (5.5.1.2 で別途扱う)。

v0 → v1 で agent dir path が変わるため hash も変わり、**既存 transcript dir が orphan 化** する (claude --continue が効かなくなる、in-CLI 文脈喪失)。kojo 側 `messages.jsonl` と MEMORY.md は import されるので UI 履歴と persona は維持されるが、CLI 内部の細かい context は失われる。

#### 5.5.1.1 claude / codex (path-hash 方式) の対応: symlink

`--migrate` 時、`--migrate-external-cli` フラグ (**default on**) で:

```
For each agent in v0:
  v0_cwd = <v0 dir>/agents/<id>
  v1_cwd = <v1 dir>/agents/<id>
  for each cli in {claude, codex}:
    v0_hash = cli.hashPath(v0_cwd)
    v1_hash = cli.hashPath(v1_cwd)
    base    = cli.transcriptBase()    # claudeConfigDir() 等
    target  = base/<v0_hash>/         # 実体 (directory)
    link    = base/<v1_hash>/         # 新 hash のエントリ
    if target exists (= directory) and link does not exist:
      Unix:    os.Symlink(target, link)
      Windows: junction (`mklink /J link target` または equiv API)
```

#### 5.5.1.2 gemini (mapping 方式) の対応: projects.json への entry 追加

gemini は `~/.gemini/projects.json` の `Projects: {absDir: projectName}` map を持つ。migration では mapping を **追加**:

```
projects.json:
  既存:  { "/Users/loppo/.config/kojo/agents/ag_xxx": "kojo-ag_xxx" }
  追加:  { "/Users/loppo/.config/kojo-v1/agents/ag_xxx": "kojo-ag_xxx" }
```

同じ `projectName` に v0 と v1 の両 absDir を写像することで、`~/.gemini/tmp/<projectName>/chats/` の実体を共有する。symlink は不要。

注意:
- projects.json は gemini 側のファイルなので、**読んで → 追記して → atomic rename** で書き戻す。format 不明な場合は skip + warning
- mapping 方式は将来 gemini 側仕様変更で壊れるリスクあり。失敗時は fresh session fallback
- gemini が新規 projectName を勝手に発番するなら、その規則を `internal/agent/backend_gemini.go` から再利用

#### 5.5.1.3 共通

- 失敗時 (権限エラー、disk full、target 不在、format 不明等): warning ログを出すだけ。**fresh session に自然 fallback** するので機能継続には影響しない
- `--migrate-external-cli=false` 指定時は何もしない (= 強制 fresh session)
- **Windows junction の制約**:
  - 作成対象は **directory のみ** (junction は file には不可)。target が file なら skip
  - 削除時は junction の target を辿らない (`os.Remove` は junction 自体を消すべき。`os.RemoveAll` は target 配下まで消すので使わない)

#### 5.5.1.4 hash 関数 / config dir resolver の出所

| CLI | hash / mapping | 出所 |
|-----|----------------|------|
| claude | `claudeEncodePath(absDir)` | `internal/agent/backend_claude.go` |
| claude config root | `claudeConfigDir()` (`$CLAUDE_CONFIG_DIR` 優先、fallback `~/.claude`) | 同上 |
| codex | TODO: 実装時に確認 | `~/.codex/` 配下を実機検証 |
| gemini | `~/.gemini/projects.json` map | `internal/agent/backend_gemini.go` |

実装フェーズで codex の hash 関数 (or mapping) を実機確認すること。判明しない CLI については migration で skip → fresh session で許容。

#### 5.5.1.5 制約と非ゴール

- 旧 v0 transcript store (claude/codex の v0 hash dir、gemini の v0 absDir entry に紐付く chats dir) 自体は **kojo は touch しない** (外部 CLI 管轄)。ユーザーが各 CLI の clean 機能で削除する
- 「外部 CLI transcript の取り込みは非ゴール」(8 節) は変えない。symlink / projects.json mapping は**継続性のためだけ**の薄い互換 layer
- v1 から v1.x で agent dir path 変更が起きる際 (例えば configdir のさらなる変更) は同様の問題が再発する。その時もこの仕組みを再利用する

#### 5.5.1.6 v0 rollback 時の transcript 混在リスク

v1 で symlink / projects.json mapping を作ったあと v0 binary に rollback すると:

- v1 で claude / codex / gemini が更新した transcript は **v0 と共通の transcript store 実体** に書かれている (claude / codex は symlink target、gemini は同一 projectName 経由で同じ chats dir)
- → v0 binary を起動すると、その「v1 で増えた turn」も同じ transcript store から見える
- v0 binary は v1 と互換性のない transcript format を踏むかもしれず (claude のスキーマ変更等)、最悪 corrupt 扱いで失敗する
- claude / codex / gemini どの方式でも実体共有 = rollback 時の混在リスクは同等

**rollback の前提として**:

- ユーザーが v0 binary を起動する場合、**`--migrate-external-cli` の効果も巻き戻す手順** を想定する:
  - claude: v1 hash dir の symlink を `os.Remove` で削除 (target 実体は触らない)
  - gemini: projects.json から v1 absDir entry を削除 (mapping 追加分のみ revert)
- これは `kojo --rollback-external-cli` のような **migration の逆操作 subcommand** として v1 binary が提供する。v0 dir に戻る前に v1 で実行
- 実行しない場合の動作は **未定義**。混在状態として UI で警告
- 設計書として「v0 rollback 時は kojo --rollback-external-cli を先に実行すること」を運用ドキュメントで義務化

**v0 read-only 保証の実装規約**:

- migration コードは v0 file を **必ず `O_RDONLY`** で open する。`O_RDWR` / `O_WRONLY` を使ったら panic する assertion を `internal/migrate/` に組み込む
- v0 dir 配下への write / mkdir / unlink syscall を一切呼ばない。code review でこれを明示的に check
- 「auth 生値破棄」とは「v1 dir に持ち込まない」の意味。v0 file 自体は `kojo clean` まで残る

### 5.6 中断・再実行

- `migration_in_progress.lock` が残ったまま終了 → 次回 `--migrate` で:
  - v0 dir の現 sha256 manifest と lock 内 manifest を比較
  - 一致 → 各 domain の `migration_status.phase` から **resume**
  - 不一致 → v0 が変更された可能性。v1 dir を破棄して clean restart 推奨 (`--migrate-restart`)
- 部分的に import 済みの v1 dir は廃棄 or 続行をフラグで選択
- **`--migrate-restart` の削除範囲**: v1 dir 内の incomplete 状態 (= `migration_complete.json` が存在しない) のみ削除可能。`migration_complete.json` が既にある v1 dir は touch せず error
  - これにより「migration 完了済の v1 を誤って `--migrate-restart` で消す」事故を防ぐ

### 5.7 Rollback

1. v1 binary を停止
2. **`kojo --rollback-external-cli` を v1 binary で実行** (5.5.1.6 参照):
   - claude / codex の v1 hash dir symlink を削除 (`os.Remove`、target 実体は触らない)
   - gemini の `~/.gemini/projects.json` から v1 absDir entry を削除
   - skip 時は v0 / v1 transcript 混在状態で「v0 binary 起動時の動作未定義」警告を表示
3. v0 binary (v0.x release) を再起動
4. v0 dir は無傷なのでそのまま動作
5. v1 dir はそのまま残るが v0 binary は読まないので無害
6. 必要に応じてユーザーが手動削除 (`rm -rf ~/.config/kojo-v1`)

v1 で行った編集 (kojo 側 DB) は失われる (片方向 migration の前提)。
外部 CLI transcript の v1 で増えた turn は **v0 / v1 共通の transcript store** (claude/codex は symlink target、gemini は同一 projectName 配下) に物理的に残るが、v0 binary が読めるかは v0 / v1 の format 互換性次第。

### 5.8 `kojo clean` コマンド

```
kojo clean                # v0 dir 一覧を表示、確認プロンプトで soft-delete (rename)
kojo clean --dry-run      # 削除対象表示のみ
kojo clean --force        # 確認スキップ (manifest mismatch 時にも必要)
kojo clean --keep-blobs   # DB / config 系のみ削除、blob (books/, temp/) は残す
kojo clean --hard-delete  # soft-delete を経ず即座に物理削除
kojo clean --purge-trash  # `kojo.deleted-<ts>/` を即時削除 (7 日待たない)
```

実装規約:

- 削除対象の path は **v0 dir に限定** (configdir.V0Path() の戻り値のみ)、v1 dir は絶対に touch しない (ハードコード assertion)
- **canonical path / symlink guard**: 削除前に `filepath.EvalSymlinks` で実体 path を解決し、それが configdir の絶対 path 規則と一致することを verify。symlink で v0 dir が v1 dir を指していた場合などの事故を防ぐ
- v1 dir の `migration_complete.json` が存在しない場合 (= migration 未完了) は拒否
- **migration_complete.json の manifest verify**: clean 実行前に v0 dir の現 sha256 manifest を再計算 → `migration_complete.json` 内の manifest と一致するか check
  - 一致 → そのまま削除へ進める
  - 不一致 → 「migration 完了後に v0 が変更されています。本当に削除するか?」を表示し、`--force` 必須に格上げ
- **soft-delete を v1 必須**: `rm -rf` ではなく `~/.config/kojo.deleted-<ts>/` (Windows は `%APPDATA%\kojo.deleted-<ts>\`) に rename。7 日後の起動時に物理削除 (cron なし、起動時 sweep)
  - `kojo clean --hard-delete` 明示時のみ即座に物理削除
  - これにより誤削除からの復旧 window を保証

### 5.9 起動時の常時表示と manifest verify

v1 起動時:

- v0 dir 残存 + `migration_complete.json` あり → UI ヘッダーに `v0 data is still present (<size> GB). Run kojo clean to remove.` を表示。v1.1 リリース後でも残存しているユーザーには alert color
- v0 dir 不在 (= clean 済) + `migration_complete.json` あり → 通常起動。manifest verify は **skip** (v0 が無いので比較対象なし、これは正常な完了状態)
- v0 dir 残存 + `migration_complete.json` なし → migration 未実行扱い (5.3 trigger に従う)
- v0 dir 不在 + `migration_complete.json` なし → 新規セットアップ

`migration_complete.json` 自体の self-integrity は schema_version + migrator_version の一致だけ check。v0 manifest との比較は v0 dir が存在する時のみ (clean 後は無視)。

**自動削除はしない**。v0 dir 削除は完全にユーザー意思 (`kojo clean`)。

### 5.10 v0 が外部書き換えされている場合

- claude / codex / gemini が v0 dir に直書きしている最中に migration 開始 → 部分 import の危険
- 5.5 step 2 の mtime walk で検出
- 完全停止確認のため、`kojo doctor` (仮、実装は v1.x) で v0 関連プロセスを表示する経路も用意

### 5.11 影響を受けるパッケージ

| パッケージ | 変更 |
|-----------|------|
| `internal/configdir/` | major version suffix で path 解決 (`Path()` = v1, `V0Path()` = 旧) |
| `internal/migrate/` | 新設 (v0 → v1 import 専用) |
| `cmd/kojo/main.go` | `--migrate`, `--migrate-restart`, `--fresh`, `--migrate-external-cli` (default on), `--migrate-backup`, `--rollback-external-cli` フラグ |
| `cmd/kojo/clean.go` | `kojo clean` サブコマンド |
| `internal/store/` | schema 適用 + `migration_status` テーブル管理 |
| `internal/agent/backend_*.go` | claude / codex は path-hash 関数 (claude は既存の `claudeEncodePath`、codex は実装時に確認・追加) を、gemini は projects.json mapping 操作 (既存の `hasGeminiSession` から read 部分を再利用) を、それぞれ migration が呼び出せる形で expose |

### 5.12 互換性検証 (DoD)

- v0 (v0.x の最新) で agents 100+ / messages 10000+ / blob 1GB+ のデータを生成
- v1 binary で `--migrate` → 完走 → 全データが v1 で read 可能
- migration 中の中断 (kill -9) → 再実行で完走
- v0 dir の sha256 manifest が import 前後で **変化しない** (read-only 保証)
- `kojo clean --dry-run` で v0 dir のみ列挙され、v1 dir は含まれない
- `--migrate-external-cli` 有効時、claude / codex の v1 hash dir から v0 hash dir のファイルが read 可能 (symlink 経由)
- claude `--continue` が v1 起動後も正常動作する (symlink で過去 transcript に到達)
- gemini は `~/.gemini/projects.json` に v1 absDir entry が追記され、`hasGeminiSession(v1_cwd)` が true を返す
- `kojo --rollback-external-cli` 実行で claude / codex の v1 symlink が削除され、gemini projects.json から v1 absDir entry が消える (target / v0 entry は残存)
- rollback-external-cli を実行せずに v0 binary に戻した場合の動作 (混在状態) を別途記録 (= 動作未定義のまま CI で確認しないが、UI 警告が出ることは検証)

## 6. オープンクエスチョン (v2 以降)

1. Hub leader election (litestream / rqlite / Raft)
2. オフライン write の op-log + CAS 衝突解決
3. credentials envelope encryption + device grant プロトコル
4. embedding cache (memory.db の中身) の `model + content_hash` ベース CAS 共有
5. 大型モデル / dataset の CAS pin policy
6. external CLI (claude / codex / gemini) の transcript / project state を kojo に取り込むか、別 sync 戦略 (Syncthing) で扱うか
7. agent CLI が直接書く `MEMORY.md` を MCP tool 経由 write に寄せるか、blob write-through で押し切るか

## 7. 影響を受けるパッケージ

| パッケージ | 変更内容 |
|-----------|---------|
| `internal/store/` | 新設 (DB アクセス層、migration、共通カラム) |
| `internal/blob/` | 新設 (native blob API) |
| `internal/webdav/` | 新設 (補助 mount) |
| `internal/agent/store.go` | DB バックエンドに切替、export/import |
| `internal/agent/memory.go`, `memory_index.go` | DB row + RAG index は local 維持 |
| `internal/agent/groupdm*.go` | jsonl → table |
| `internal/agent/credential.go` | 据え置き (machine-bound) |
| `internal/auth/store.go` | token は hash 化、生値は machine 保持 |
| `internal/notify/webpush.go` | subscription / cursor / vapid public は kv table、private は global kv (envelope encrypted, cluster-bound KEK) |
| `internal/session/store.go` | sessions.json → local table |
| `internal/chathistory/store.go` | cursor は global table、本体は local cache |
| `internal/configdir/` | major version suffix で path 解決 (v1 = `kojo-v1`, v0 = `kojo`) |
| `internal/migrate/` | 新設 (v0 → v1 import) |
| `web/src/` | Edit / Export UI、conflict (etag) 対応、blob URI 対応 |
| `cmd/kojo/main.go` | `--migrate`, `--migrate-restart`, `--fresh` フラグ |
| `cmd/kojo/clean.go` | `kojo clean` サブコマンド |

## 8. 非ゴール

- claude / codex / gemini の transcript / project state の取り込み
- E2E 暗号化 (Tailscale で十分とする)
- 自動 leader election
- オフライン書き込みの自動 conflict resolve
