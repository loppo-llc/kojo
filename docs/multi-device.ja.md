# マルチデバイス構成

> [English](multi-device.md)

kojo v0.101.0 は小さなクラスタとして動きます。**Hub** (Web UI を出す機)
が 1 台と、**peer** (Web UI を持たない daemon) が 0 台以上の構成です。

```
┌──────────────┐                               ┌──────────────┐
│  モバイル    │                               │  ノート PC   │
│  ブラウザ    │                               │   (peer)     │
└───────┬──────┘                               └──────▲───────┘
        │                                             │
        │  https://kojo.<tailnet>.ts.net:8080         │  Tailscale
        │  (Web UI)                                   │   P2P
        ▼                                             │
┌──────────────────────────────────────────────────┐  │
│  デスクトップ (Hub)                              │◄─┘
│  Web UI / agent ストレージ / セッション統括      │
└──────────────────────────────────────────────────┘
```

複数台にする利点。

- **agent もセッションも起動した機の上で動き続けます**。デスクトップ
  で走らせた agent は、ノートを閉じても考え続けます
- **UI は 1 つ**。モバイルブラウザは Hub URL を開くだけ。そこから任意
  の peer 上のセッションに入れます
- **agent device-switch**。agent を別マシンに in-place で移せます。
  skill / 認証情報 / 会話状態が追従します

macOS / Linux / Windows を任意の組み合わせで構成できます。Hub と peer
で OS を揃える必要はありません。

## 前提

- 全マシンが同じ Tailscale tailnet にいて、同じアカウントで sign-in
  済み
- `tailscale status` で互いに online と見える
- 各マシンに kojo v0.101.0 バイナリがインストール済み
- 1 台を Hub に指定する。Web UI は Hub にしか出ない

## セットアップ

### 1. Hub を起動

Hub にしたいマシンで。

```sh
kojo
```

出力。

```
kojo v0.101.0 running at:

    https://kojo.<your-tailnet>.ts.net:8080
    https://100.x.y.z:8080
```

`*.ts.net` URL をスマホにブックマークしてください。

別の MagicDNS 名にしたい場合 (tailnet に別の `kojo` ノードがある等):

```sh
kojo --hostname kojo-desktop
# → https://kojo-desktop.<tailnet>.ts.net:8080
```

### 2. peer を起動

追加するマシンで。

```sh
kojo --peer
```

これだけです。peer は次を自動で実行します。

1. `https://kojo.<your-tailnet>.ts.net:8080` で Hub を自動発見
2. Hub を自分の peer registry に書き込み
3. Hub に join request を POST

Hub の hostname を default から変えている場合は明示してください。

```sh
# macOS / Linux
kojo --peer --hub https://kojo-desktop.<tailnet>.ts.net:8080
KOJO_HUB_URL=https://kojo-desktop.<tailnet>.ts.net:8080 kojo --peer
```

```powershell
# Windows (PowerShell)
kojo --peer --hub https://kojo-desktop.<tailnet>.ts.net:8080
$env:KOJO_HUB_URL = "https://kojo-desktop.<tailnet>.ts.net:8080"; kojo --peer
```

Tailscale が落ちている時に `0.0.0.0` フォールバックを禁止して起動を
拒否させたい場合は次を使います。

```sh
kojo --peer --tailnet-only
```

### 3. peer を承認する

スマホやブラウザで Hub の Web UI を開きます。

1. **Settings → Peers** を開く
2. **Pending join requests** に新しいエントリがある
3. **Approve** を押す

peer は即座に privileged surface (sessions / files / git / agents) に
アクセスできるようになります。Reject なら request を捨てます。peer は
再要求できます。

後から外したくなれば同じ画面で revoke できます。

### 4. 使う

Hub の Web UI に、承認済み peer のセッションと agent が並びます。
新しいセッションや agent を作る時は、ホストマシンを peer 選択 UI で
選んでください。

## 特定のマシンで agent を動かす

agent を作成・編集する時の **Host** 欄が、CLI を走らせる機を決めます。
Hub でも、承認済み peer でも選べます。agent の storage (memory / 認証
情報 / 添付) は Hub に置かれたまま、CLI プロセスだけが選んだ peer で
動きます。

### agent を別マシンに引っ越す (device-switch)

agent の設定で **Switch device** を開き、新しい host を選びます。
kojo は次を実行します。

1. 移行元で処理中のメッセージを排出
2. agent の workspace blob を移行先に転送
3. agent の skill を移行先に再インストール
4. 移行先で会話を resume

切り替え中もチャットは開いたままです。新メッセージはキューに入り、
移行先が立ち上がり次第再送されます。認証情報も自動で同期するので、
新マシンで API key を入れ直す必要はありません。

切り替え発火時に移行元が offline なら、kojo は request を保持して
再接続した瞬間に発火します。

## Hub の引っ越し (Hub マシン交換)

Hub はクラスタの正本 (SQLite と global blob ツリー) を持ちます。別
マシンに移す手順。

1. 旧 Hub でスナップショットを取ります。

   ```sh
   kojo --snapshot
   # → <kojo-v1 ディレクトリ>/snapshots/<UTC_TS>/
   ```

2. 旧 Hub を停止します (移行ガイドの手順 1 に OS 別の停止コマンドが
   あります)。

3. 次の 2 つを新 Hub に転送します。

   - スナップショットディレクトリ (`<kojo-v1>/snapshots/<UTC_TS>/`)
   - 旧 Hub の `kojo-v1` ディレクトリ内の `auth/kek.bin`

   両端で使える任意の手段で構いません — `scp`, `rsync`, SMB 共有, USB
   ドライブ, robocopy, OneDrive など。スナップショットディレクトリは
   `manifest.json` に sha256 が記録され、リストア時に検証されるので、
   転送中の破損は検出されます。

4. 新 Hub で復元します。

   ```sh
   # macOS / Linux
   kojo --restore /path/to/copied/snapshot/
   install -m 600 /path/to/copied/kek.bin ~/.config/kojo-v1/auth/kek.bin
   ```

   ```powershell
   # Windows (PowerShell)
   kojo --restore C:\path\to\copied\snapshot\
   Copy-Item C:\path\to\copied\kek.bin $env:APPDATA\kojo-v1\auth\kek.bin
   icacls $env:APPDATA\kojo-v1\auth\kek.bin /inheritance:r /grant:r "${env:USERNAME}:F"
   ```

5. 新 Hub を起動します。旧 Hub と同じ MagicDNS 名を取り戻すなら:

   ```sh
   kojo --hostname kojo
   ```

重要。

- `auth/kek.bin` は別経路で必ずコピーしてください。これが無いと暗号化
  された認証情報と VAPID secret が復号できません。SSH 秘密鍵と同じ扱い
  で管理してください
- 旧 Hub の Tailscale ノードは admin console で削除してください。
  MagicDNS 名が新マシンに解決されるように
- 既存 peer はそのまま使えます。同じ Tailscale identity で新 Hub に
  再認証されます

## トラブルシュート

### peer が pending requests に出ない

順に確認してください。

1. peer 側で Tailscale が up している。

   ```sh
   tailscale status
   ```

2. peer から Hub に到達できる。

   ```sh
   curl -k https://kojo.<tailnet>.ts.net:8080/api/v1/info
   ```

   Windows 10 1803+ / Windows 11 では `curl.exe` が標準で使えます。
   PowerShell からは
   `Invoke-RestMethod https://kojo.<tailnet>.ts.net:8080/api/v1/info`
   でも構いません。

3. peer の stderr に join-request エラーが出ていないか読む。

Hub URL を default から変えている場合は `--hub` か `KOJO_HUB_URL` の
指定が必要です。

### peer が UI で offline のまま

peer の heartbeat は 30 秒間隔です。1 分待っても変わらないなら、Hub
から `tailscale ping <peer-name>` で疎通を確認してください。

### `--hub requires --peer`

`--hub` を `--peer` 無しで指定しました。peer mode 専用のフラグです。

### device-switch が止まったまま

移行先が switch 中に offline になっても、kojo は再接続を待って
リトライします。移行が完了するまで移行元で agent はそのまま使えます。
会話は失われません。手動でやり直したい場合は、agent の設定からもう
一度 switch を発火してください。

### peer を外したい

Hub Web UI で **Settings → Peers → Remove**。peer は registry から
落ちます。その peer に home している agent は offline 扱いになり、別
peer に device-switch するか peer が rejoin するまで使えなくなります。
