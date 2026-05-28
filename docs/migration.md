# Upgrading to kojo v0.101.0

> [日本語](migration.ja.md)

kojo v0.101.0 ships a new on-disk config layout to support the
multi-device peer architecture. The new binary refuses to start when
the legacy `kojo/` config directory (written by v0.x releases up to
v0.20) is still present. Pick one of three paths at first launch:

| Choice | Flag | When to use |
|--------|------|------------|
| Import old data | `--migrate` | You have a working older install and want to keep sessions, agents, memory, credentials, push subscriptions |
| Start over | `--fresh` | You want a clean install. The legacy `kojo/` directory is ignored (not deleted) |
| Go back | `--rollback-external-cli` then run the older binary | You already ran `--migrate` and want to revert |

In this document, **legacy `kojo/` directory** refers to the v0.x
config directory and **`kojo-v1` directory** refers to the v0.101.0+
config directory. The new directory keeps the `-v1` suffix because it
identifies the on-disk schema generation — not the kojo release
version. Paths:

| OS | Legacy `kojo/` directory | `kojo-v1` directory |
|----|--------------------------|---------------------|
| macOS / Linux | `~/.config/kojo/` | `~/.config/kojo-v1/` |
| Windows | `%APPDATA%\kojo\` | `%APPDATA%\kojo-v1\` |

## What v0.101.0 adds

- **Multi-device peers** — Run kojo on more than one machine and share
  the same agents, sessions, files, and git surface. See
  [multi-device.md](multi-device.md).
- **Tailscale-anchored identity** — Peers authenticate via Tailscale
  NodeKey. No bearer tokens to copy by hand, no Ed25519 keys to manage.
- **Agent device-switch** — Move a running agent from one machine to
  another with a single UI action. Skills and credentials follow.
- **Agent file attachments** — Agents can attach files back to the
  operator (`kojo-attach` skill).
- **Grok Build CLI** — Added as an agent backend alongside Claude and
  Codex.
- **Workspace files for agents** — Per-agent scratch directory with
  JSONL hardening and Slack parity.
- **Snapshot / restore** — `--snapshot` and `--restore` for Hub
  failover.

## What v0.101.0 removes

- **Gemini CLI backend** — Removed. Existing Gemini agents need to be
  reassigned to another backend before migration, or recreated.
- **Gmail notification subsystem** — Removed. Use Slack or Web Push.
- **Slack `<reply>` tag protocol** — Slack agents now stream live text;
  the tag-based filtering is gone.

## Migration: step by step

### 1. Stop the old kojo

macOS / Linux:

```sh
killall kojo
ps aux | grep kojo    # confirm empty
```

Windows (PowerShell):

```powershell
Get-Process kojo -ErrorAction SilentlyContinue | Stop-Process
Get-Process kojo -ErrorAction SilentlyContinue   # confirm empty
```

The migrator refuses to touch the legacy `kojo/` directory if any of
its files were modified in the last 5 minutes. A live kojo silently
corrupts the import, so that guard is non-negotiable.

### 2. Back up the legacy `kojo/` directory (recommended)

macOS / Linux:

```sh
cp -a ~/.config/kojo ~/kojo-old-backup
```

Windows (PowerShell):

```powershell
Copy-Item -Recurse $env:APPDATA\kojo $env:USERPROFILE\kojo-old-backup
```

The migrator does **not** delete or move the legacy `kojo/` directory.
The backup just gives you a clean rollback target if something goes
sideways.

### 3. Run the migration

```sh
kojo --migrate
```

The migrator imports:

- `kojo.db` (sessions, agents, memory, kv, peer registry skeleton)
- `credentials.db` + `credentials.key` (kept as-is)
- `blobs/` (agent workspace, attachments, avatars)
- `sessions.json` (tmux session info — running tmux panes are
  re-attached automatically on next boot; macOS / Linux only)
- Claude transcript symlinks (only when `--migrate-external-cli=true`,
  the default)

Then it exits. Re-run `kojo --migrate` if it was killed mid-import — it
is idempotent and resumes.

### 4. First launch on v0.101.0

```sh
kojo                    # Tailscale mode (default)
kojo --local            # localhost only
```

Tailscale mode keeps the same MagicDNS hostname
(`kojo.<tailnet>.ts.net`), so the URL you bookmarked on mobile still
works.

The `auth/kek.bin` file is preserved across migration, which means your
encrypted credentials decrypt without re-entry, and existing Web Push
subscriptions keep working.

## Troubleshooting

### `v0 dir modified within the safety window`

The migrator refused because the legacy `kojo/` directory looked alive
within the last 5 minutes. Confirm the old kojo is actually stopped,
then re-run with `--migrate-force-recent-mtime`. Do **not** use this
flag if the old kojo might still be running — concurrent writes corrupt
the import silently.

### Migration got interrupted

Re-run `kojo --migrate`. It resumes from the last completed table.

If resume fails or you suspect partial state, discard the `kojo-v1`
directory and start over:

```sh
kojo --migrate-restart
```

This wipes the partially-imported `kojo-v1` directory and re-imports
from the legacy `kojo/` directory.

### `credentials.db` is missing after migration

`credentials.db` and `credentials.key` are copied verbatim into the
`kojo-v1` directory. If they are missing, `--migrate-restart`
re-imports them.

### I want to go back to the older release

1. Stop the new kojo (see step 1 above).
2. Roll back the claude transcript symlinks created by `--migrate`:

   ```sh
   kojo --rollback-external-cli
   ```

3. Install / run the older binary as before.

The `kojo-v1` directory stays in place. Run `kojo --migrate` again
later to return.

### I want to delete the legacy `kojo/` directory

After v0.101.0 has been running for a while and you trust the
migration:

```sh
# Dry-run first
kojo --clean v0

# Soft-delete to kojo.deleted-<ts>/ next to the original
kojo --clean v0 --clean-apply

# After 7 days, hard-delete the trash
kojo --clean v0-trash --clean-apply
```

The two-stage delete protects you from "I migrated, deleted the legacy
directory, then realized I needed something."

## What does NOT carry over

- **VAPID key pair** — Preserved. Existing push subscriptions keep
  working.
- **KEK** (`auth/kek.bin`) — Preserved. Credentials decrypt without
  re-entry.
- **Tmux running sessions** — Preserved on macOS / Linux. v0.101.0
  re-attaches existing tmux panes on first boot, even sessions older
  than the 7-day cleanup cutoff, as long as the tmux server itself
  stayed up. Windows uses ConPTY and does not persist sessions across
  kojo restarts in either v0 or v0.101.0.
- **Gemini agents** — Not preserved. Gemini support is removed;
  reassign to Claude / Codex / Grok before migrating, or recreate
  after.
- **Gmail notification settings** — Not preserved. Feature removed.
- **Slack `<reply>` filtering** — Slack agents now stream all output
  live. Update any external tooling that depended on the tag.
