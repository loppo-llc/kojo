# Multi-device setup

> [日本語](multi-device.ja.md)

kojo v0.101.0 runs as a small cluster: one **Hub** (the machine that
serves the Web UI) and zero or more **peers** (daemon-only machines
that host agents and sessions on behalf of the cluster).

```
┌──────────────┐                               ┌──────────────┐
│   Mobile     │                               │   Laptop     │
│   browser    │                               │   (peer)     │
└───────┬──────┘                               └──────▲───────┘
        │                                             │
        │  https://kojo.<tailnet>.ts.net:8080         │  Tailscale
        │  (Web UI)                                   │  P2P
        ▼                                             │
┌──────────────────────────────────────────────────┐  │
│  Desktop (Hub)                                   │◄─┘
│    Web UI · agent storage · session orchestrator │
└──────────────────────────────────────────────────┘
```

What you get with more than one machine:

- **Sessions and agents run wherever you start them.** A long-running
  agent on the desktop keeps thinking even when the laptop is closed.
- **One UI, every machine.** The mobile browser hits the Hub URL; from
  there you can open a session on any peer.
- **Agent device-switch.** Move an agent from one machine to another
  in place. Skills, credentials, and the current conversation all
  follow.

Any combination of macOS, Linux, and Windows machines is supported.
The Hub and peers do not need to share an OS.

## Prerequisites

- All machines on the same Tailscale tailnet, signed in to the same
  account
- `tailscale status` reports each machine online from the others
- kojo v0.101.0 binary installed on each machine
- One machine designated as the Hub. The Hub holds the Web UI; peers do
  not serve a UI

## Setup

### 1. Start the Hub

On the machine you want to use as the Hub:

```sh
kojo
```

You should see:

```
kojo v0.101.0 running at:

    https://kojo.<your-tailnet>.ts.net:8080
    https://100.x.y.z:8080
```

Bookmark the `*.ts.net` URL on your phone.

If you want a different MagicDNS name (for example, you already have
another `kojo` node on the tailnet):

```sh
kojo --hostname kojo-desktop
# → https://kojo-desktop.<tailnet>.ts.net:8080
```

### 2. Start a peer

On every additional machine:

```sh
kojo --peer
```

That's it. The peer:

1. Auto-discovers the Hub by trying `https://kojo.<your-tailnet>.ts.net:8080`
2. Writes the Hub into its local peer registry
3. POSTs a join request to the Hub

If your Hub uses a different hostname, point the peer at it explicitly:

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

To refuse to start unless Tailscale is up (instead of falling back to
`0.0.0.0`):

```sh
kojo --peer --tailnet-only
```

### 3. Approve the peer

Open the Hub Web UI on your phone or browser:

1. Go to **Settings → Peers**.
2. Find the new entry under **Pending join requests**.
3. Click **Approve**.

The peer immediately gains access to the privileged surface (sessions,
files, git, agents). Reject drops the request; the peer is free to
retry.

You can revoke a peer later from the same screen.

### 4. Use it

The Hub Web UI now shows sessions and agents from every approved peer.
When you create a new session or agent, pick the host machine from the
peer selector.

## Running an agent on a specific machine

When you create or edit an agent, the **Host** field controls which
machine runs the CLI. Pick the Hub or any approved peer. The agent's
storage (memory, credentials, attachments) stays on the Hub; only the
CLI process runs on the chosen peer.

### Moving an agent between machines (device-switch)

Open the agent settings and use **Switch device**. Pick the new host.
kojo then:

1. Drains in-flight messages on the source machine.
2. Transfers the agent's workspace blobs to the target.
3. Re-installs the agent's skills on the target.
4. Resumes the conversation on the target.

The chat stays open while the switch happens; new messages are queued
and replayed once the target finishes coming up. Credentials sync
automatically — you do not re-enter API keys on the new machine.

If the source machine is offline when you trigger the switch, kojo
holds the request and fires it the moment the source reconnects.

## Hub failover (replacing the Hub machine)

The Hub holds the cluster's source-of-truth SQLite and global blob
tree. To move it to a new machine:

1. On the old Hub, take a snapshot:

   ```sh
   kojo --snapshot
   # → <kojo-v1 directory>/snapshots/<UTC_TS>/
   ```

2. Stop the old Hub (see the migration guide step 1 for OS-specific
   stop commands).

3. Copy two things to the new Hub:

   - the snapshot directory (`<kojo-v1>/snapshots/<UTC_TS>/`)
   - the `auth/kek.bin` file from the old Hub's `kojo-v1` directory

   Use any transport you trust on both ends — `scp`, `rsync`, SMB
   share, USB drive, robocopy, OneDrive, whatever. The snapshot
   directory is signed with sha256 in `manifest.json` and the restore
   step verifies it, so corruption in transit is detected.

4. On the new Hub:

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

5. Start the new Hub. Reuse the old MagicDNS hostname if you want:

   ```sh
   kojo --hostname kojo
   ```

Important:

- `auth/kek.bin` must be copied separately. Without it, encrypted
  credentials and VAPID secrets cannot be decrypted. Treat this file
  like an SSH private key.
- Delete the old Hub's Tailscale node from the admin console so the
  MagicDNS name resolves to the new machine.
- Existing peers keep working — they re-authenticate to the new Hub
  via the same Tailscale identity.

## Troubleshooting

### Peer does not appear in pending requests

Check, in order:

1. Tailscale is up on the peer:

   ```sh
   tailscale status
   ```

2. The peer can reach the Hub:

   ```sh
   curl -k https://kojo.<tailnet>.ts.net:8080/api/v1/info
   ```

   On Windows, `curl` is available as `curl.exe` on Windows 10 1803+
   and Windows 11. PowerShell users can also use
   `Invoke-RestMethod https://kojo.<tailnet>.ts.net:8080/api/v1/info`.

3. Read the peer's stderr for join-request errors.

If the Hub URL is non-default, you must pass `--hub` or set
`KOJO_HUB_URL`.

### Peer shows offline in the UI

Peer heartbeat runs every 30 seconds. Wait a minute; if it stays
offline, check `tailscale ping <peer-name>` from the Hub.

### `--hub requires --peer`

You passed `--hub` without `--peer`. The flag only makes sense in peer
mode.

### Device-switch hangs

If the target peer goes offline mid-switch, kojo retries when it
reconnects. The agent stays usable on the source until the switch
completes — you do not lose the conversation. To cancel and rerun,
trigger the switch again from the agent settings.

### I want to remove a peer

From the Hub Web UI: **Settings → Peers → Remove**. The peer is dropped
from the registry; any agents homed on that peer become offline until
device-switched away or until the peer rejoins.
