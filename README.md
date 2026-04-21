# kojo

[![Release](https://img.shields.io/github/v/release/loppo-llc/kojo)](https://github.com/loppo-llc/kojo/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/loppo-llc/kojo)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> [日本語](README.ja.md)

Remotely operate AI coding CLIs (Claude Code, Codex, Gemini CLI) from your mobile device — and run persistent AI agents that think, remember, and collaborate on their own.

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

## Features

- **Single binary** — Built with Go, web UI embedded
- **Cross-platform** — macOS/Linux (tmux + PTY) and Windows (ConPTY) native support
- **AI agents** — Persistent AI personas with memory, scheduled execution, encrypted credentials, and group DM
- **tmux-backed sessions** (macOS/Linux) — CLI tools run inside tmux for crash resilience and persistence across kojo restarts
- **Unified PTY** — All CLIs handled uniformly via PTY. No SDK dependencies
- **Tailscale P2P** — No central server or database. Encrypted with WireGuard
- **Zero config** — Just run `kojo` with Tailscale running

## Requirements

### macOS / Linux

- Go 1.25+
- Node.js 20+
- tmux
- [Tailscale](https://tailscale.com/)
- Supported CLIs: `claude`, `codex`, `gemini` (at least one)

### Windows

- Go 1.25+
- Node.js 20+
- Windows 10 1809+ / Windows 11 (ConPTY support required)
- [Tailscale](https://tailscale.com/)
- Supported CLIs: `claude`, `codex`, `gemini` (at least one)

> **Note:** On Windows, sessions run via ConPTY instead of tmux. Session persistence across kojo restarts is not available.

## Build

```bash
# Production build (macOS/Linux)
make build

# Cross-compile for Windows (from macOS/Linux)
make build-windows

# Build on Windows
build.bat

# Development (two terminals)
make dev-server   # Go server (--dev mode, proxies to Vite)
make dev-web      # Vite dev server

# Hot reload (auto rebuild on Go file changes)
make watch
```

## Usage

```bash
# Via Tailscale (default, auto HTTPS)
kojo

# Local only
kojo --local

# Custom port (auto-increments if busy)
kojo --port 9090
```

By default, kojo listens on the Tailscale network via tsnet with HTTPS.
Use `--local` or `--dev` to bind to localhost only.

## Tailscale HTTPS Setup

kojo uses [tsnet](https://tailscale.com/kb/1244/tsnet) to join your Tailscale network directly as a node named `kojo`. All traffic is encrypted with WireGuard — no ports to open, no certificates to manage.

### Prerequisites

1. Install Tailscale on your machine and your mobile device
2. Sign in to the same Tailscale account on both devices
3. Ensure both devices appear in [Tailscale admin console](https://login.tailscale.com/admin/machines)

### How it works

When you run `kojo` (without `--local`):

1. kojo starts an embedded Tailscale node via tsnet
2. The node registers itself as `kojo` on your tailnet
3. HTTPS is automatically provisioned using Tailscale's built-in Let's Encrypt integration
4. Your server becomes reachable at `https://kojo.<tailnet-name>.ts.net`

```bash
$ kojo

  kojo v0.12.0 running at:

    https://kojo.tail1234.ts.net
    https://100.x.y.z:8080
```

### First run

On the first launch, tsnet will print an authentication URL to stderr. Open it in your browser to authorize the node. This is a one-time step — credentials are cached in `~/.config/tsnet-kojo/`.

### Access from mobile

1. Install the Tailscale app on your phone
2. Sign in with the same account
3. Open `https://kojo.<tailnet-name>.ts.net` in your mobile browser

All communication is peer-to-peer via WireGuard. No data passes through a central server.

### Security model

| Layer | Protection |
|-------|-----------|
| Network | WireGuard encryption (Tailscale P2P) |
| TLS | Auto-provisioned HTTPS via Let's Encrypt |
| WebSocket | Origin restricted to Tailscale IPs (`100.*`), `*.ts.net`, and `localhost` |
| Access | Only devices on your tailnet can reach kojo |

### ACL (optional)

You can restrict which devices can access kojo using [Tailscale ACLs](https://tailscale.com/kb/1018/acls):

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

### Troubleshooting

| Problem | Solution |
|---------|----------|
| Auth URL not appearing | Check stderr output, or remove `~/.config/tsnet-kojo/` and restart |
| Cannot reach from mobile | Ensure both devices are on the same tailnet and Tailscale is connected |
| Port conflict | Use `--port <number>` (auto-increments up to 10 times if busy) |
| Want localhost only | Use `--local` or `--dev` to skip Tailscale entirely |

## What it does

### Terminal Sessions

- Manage multiple sessions simultaneously (newest first)
- Session persistence via tmux on macOS/Linux (`~/.config/kojo/sessions.json`, auto-cleanup after 7 days). Sessions survive kojo restarts and crashes
- Session restart with tool-specific resume (`claude --resume`, `codex resume`, `gemini --resume`)
- Real-time PTY output streaming (xterm.js)
- Text input (Enter for newline, Shift+Enter to send) and special keys (Esc, Tab, Ctrl, arrows)
- Working directory path completion
- File browser (syntax highlighting for text, image preview)
- File attachment (camera, images, text)
- Git panel (status, log, diff, commit diff view)
- Web Push notifications (permission prompts, completion alerts)
- Yolo mode (auto-approve permissions)
- Minimal system prompt option for claude (override default with a working-directory note)

### AI Agents

- Create persistent AI personas with custom name, personality, avatar, and backend (Claude / Codex / Gemini, plus custom Anthropic Messages API endpoints like llama-server)
- AI-assisted persona generation (Gemini API) and avatar generation
- Interactive chat with streaming responses, thinking display, and tool-use cards
- Markdown rendering in agent messages
- Scheduled autonomous execution (10 min – 24 h intervals) with auto-staggering, per-agent timeout, and cross-process dedup
- Persistent memory: long-term `MEMORY.md` + daily notes with full-text search (SQLite FTS5)
- Encrypted credential vault (AES-256-GCM SQLite) with TOTP 2FA support
- Public profile and agent directory for inter-agent discovery
- Group DM: multi-agent conversations with notification-based messaging
- Slack integration (Socket Mode) — per-agent bot with streaming replies, per-thread conversation context, and `<reply>` tag filtering
- Agent data reset (clear conversations and memory while keeping settings, persona, avatar, and credentials)
- Fork agents preserving persona and memory

## Tech Stack

| Layer | Technology |
|-------|-----------|
| Server | Go, `net/http`, `coder/websocket`, `creack/pty` (Unix) / ConPTY (Windows), tmux (Unix), `tsnet` |
| Web UI | React 19, Vite, TypeScript, Tailwind CSS, xterm.js |
| Agents | Claude / Codex / Gemini / Custom API backends, encrypted SQLite (credentials + FTS5 memory) |
| Notifications | Web Push (VAPID) |
| Network | Tailscale WireGuard P2P |

## Install

```bash
go install github.com/loppo-llc/kojo/cmd/kojo@latest
```

## License

[MIT](LICENSE)
