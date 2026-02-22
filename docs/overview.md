# kojo - Overview

macOS 上で動作する AI コーディング CLI（Claude Code, Codex, Gemini CLI）を、モバイルデバイスからリモート操作するためのツール。

## コンセプト

Tailscale による P2P 接続で、macOS マシン上のサーバーとモバイルブラウザを直接つなぐ。中央サーバー不要。

```
┌─────────────────┐        Tailscale        ┌──────────────────┐
│  macOS Machine   │◄──────(P2P encrypted)──────►│  Mobile Browser  │
│                  │                         │                  │
│  kojo server     │   WebSocket / HTTP      │  Web UI          │
│  ├─ PTY: claude  │◄──────────────────────►│  ├─ xterm.js     │
│  ├─ PTY: codex   │                         │  ├─ React        │
│  └─ PTY: gemini  │                         │  └─ Web Push     │
└─────────────────┘                         └──────────────────┘
```

## 設計方針

- **ミニマル**: 中央サーバー・データベース・暗号化レイヤー不要。Tailscale が全て担保する
- **PTY 統一**: 全 CLI ツールを PTY 経由で統一的に扱う。SDK 依存なし
- **シングルバイナリ**: Go で実装。Web UI を埋め込み、1 バイナリで配布
- **ゼロ設定**: Tailscale が動いていれば、`kojo` 起動だけで使える

## happy との比較

| 項目 | happy | kojo |
|------|-------|------|
| 通信経路 | Socket.IO → 中央サーバー → クライアント | Tailscale P2P 直接接続 |
| サーバーインフラ | PostgreSQL + Redis + S3 | なし（Go プロセスのみ） |
| 暗号化 | E2E (NaCl / AES-256-GCM) | Tailscale WireGuard |
| 認証 | チャレンジ・レスポンス + JWT | Tailscale デバイス認証 |
| CLI 連携 | Claude Code SDK (JSON lines) | PTY 統一（全ツール共通） |
| モバイルクライアント | React Native (iOS/Android/Web) | Web UI（ブラウザ） |
| デプロイ | サーバー + DB + アプリストア | シングルバイナリ |

## 対応 CLI ツール

- **Claude Code** (`claude`)
- **Codex** (`codex`)
- **Gemini CLI** (`gemini`)

将来的に任意のインタラクティブ CLI を追加可能。

## 機能一覧

### Phase 1 (MVP)

- [ ] 複数セッションの同時管理（起動・停止・切り替え）
- [ ] PTY 出力のリアルタイムストリーミング（xterm.js）
- [ ] テキスト入力の送信
- [ ] ファイルブラウザによる作業ディレクトリ選択
- [ ] Web Push 通知（permission 待ち、完了）
- [ ] Yolo モード（permission 自動承認、質問は通知）
- [ ] ファイル添付（カメラ撮影・画像・テキストをアップロード）
- [ ] ファイルビューア（テキスト: シンタックスハイライト、画像: ピンチズーム）
- [ ] Git パネル（status、log、diff、任意の git コマンド実行）

### Phase 2

- [ ] ネイティブモバイルアプリ（React Native）
- [ ] セッション履歴の永続化
- [ ] カスタム CLI ツールの追加設定
