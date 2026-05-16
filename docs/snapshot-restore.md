# Snapshot & Restore (v1 — manual failover)

`docs/multi-device-storage.md §3.6` calls for **手動 failover** in v1. This
doc is the operator runbook.

## Take a snapshot

```sh
kojo --snapshot                       # uses default <configdir>
kojo --config-dir /path/to/kojo --snapshot
```

The command writes a directory of the form

```
<configdir>/snapshots/<UTC_TS>/
├── kojo.db          # SQLite VACUUM INTO copy (consistent)
├── blobs/global/... # snapshot of the global blob tree
└── manifest.json    # version, schema_version, db_sha256, host_hint
```

`kojo.db` is a fully-consistent SQL backup — `VACUUM INTO` takes a read lock
that snapshots a point-in-time view while writers continue. Per-peer scopes
(`local`, `machine`) are intentionally **excluded**; restoring those is the
new Hub's responsibility.

The snapshot is safe to run while a kojo server is live.

### What is NOT in the snapshot

- `auth/kek.bin` — the envelope-encryption KEK. Operators MUST back this up
  separately, otherwise a leaked snapshot still cannot decrypt secret rows
  — but neither can the new Hub. **Without the KEK every secret kv row
  becomes unrecoverable on a restored Hub.**
- Pre-Phase-2c-2-slice-17 only: `auth/owner.token` and
  `auth/agent_tokens/*` disk files. Phase 5 #16 made these hashed-only
  on disk; Phase 2c-2 slice 17 then moved the canonical hashes into
  the kv table (namespace=`auth`, scope=`global`) inside `kojo.db`,
  so they ARE included in the snapshot via the kojo.db copy. Any
  surviving on-disk hash files are legacy fallback only and snapshot
  restore does not need them — the restored Hub already has the
  hashes via the kv rows. Clients holding the original raw token
  still verify successfully against those hashes (the cutover preserved
  the verifier value byte-for-byte).
- `local/` and `machine/` blob scopes.
- Per-peer subscription files (`push_subscriptions.json`).

### Cron example

```cron
# Hourly snapshot, prune anything older than 7 days
0 * * * * kojo --snapshot && find ~/.config/kojo-v1/snapshots -mindepth 1 -maxdepth 1 -mtime +7 -exec rm -rf {} +
```

## rsync to a backup peer

```sh
# On the running Hub
rsync -aP --delete ~/.config/kojo-v1/snapshots/ backup-host:~/kojo-snapshots/

# Separately mirror the KEK (out-of-band, paired with the snapshot transfer)
scp ~/.config/kojo-v1/auth/kek.bin backup-host:~/kojo-kek/kek-$(date +%Y%m%dT%H%M%SZ).bin
```

The KEK should travel through a different channel from the SQLite snapshot
when threat-modeling for a single-channel compromise (an attacker with
read access to the rsync target cannot decrypt secrets without the KEK).

## Restore on a backup peer

Pre-flight:

```sh
# Pick the most-recent snapshot on the backup peer
ts=$(ls -t ~/kojo-snapshots/ | head -1)
echo "$ts"
```

Cutover:

```sh
# 1. Stop the new Hub if it is running. (kojo --restore refuses to
#    proceed while the target configdir lock is held.)
killall kojo

# 2. Restore. The command verifies manifest + db sha256 before
#    touching the target; it copies kojo.db + blobs/global/ into the
#    configdir. By default the target must NOT already have kojo.db
#    — pass --restore-force when re-seeding a previously-used Hub.
kojo --config-dir ~/.config/kojo-v1 --restore ~/kojo-snapshots/$ts
# (re-seeding a previously-used Hub)
# kojo --config-dir ~/.config/kojo-v1 --restore ~/kojo-snapshots/$ts --restore-force

# 3. Lay down the operator-managed KEK out-of-band. The snapshot
#    intentionally excludes auth/kek.bin (a snapshot leak shouldn't
#    decrypt secret kv rows on its own — see §3.4). The restore
#    step above pre-creates <target>/auth at mode 0700, so no
#    extra mkdir is needed.
cp ~/kojo-kek/kek-<paired-ts>.bin ~/.config/kojo-v1/auth/kek.bin
chmod 600                         ~/.config/kojo-v1/auth/kek.bin

# 4. Start kojo on the backup peer
kojo --config-dir ~/.config/kojo-v1
```

The console at startup prints the new Hub's URL. Each peer's
`kojo --config-dir <peer-config>` config (`hub_url`) needs to be updated
to point at the new Hub before the peers can rejoin.

The manual `cp`-based recipe (pre-v1 binaries) is still valid; the
`--restore` flag is a thin wrapper that adds sha256 verification and
the configdir-lock safety check. Operators on installs whose target
binary predates the flag can fall back to it without losing the
runbook.

## Caveats

- **RPO = snapshot interval.** Writes between the last snapshot and the
  failover are lost. Schedule the snapshot cadence accordingly.
- **agent_locks.holder_peer** values inside `kojo.db` still reference the
  old Hub's peer id — affected leases expire after `agent_locks.expires_at`
  (default 60 s) and the new Hub's peers reacquire from there.
- **blob_refs.home_peer** likewise still references the old Hub. A future
  slice of #20 / #21 ships a `kojo blob rehome` to bulk-rewrite, but in
  v1 the new Hub simply reuses the old peer id label until the next
  natural write per blob.
- **External CLI symlinks** under `~/.claude/projects/...` etc. are
  per-peer filesystem state — not in the snapshot. Re-run
  `kojo --migrate-external-cli` if you want them re-created on the new
  Hub.

## Sweep post-cutover legacy files

After Phase 2c-2 every per-agent cron / autosummary / token state lives
in the kv table. Older v1 boots may have left the legacy on-disk files
in place — they are harmless (the runtime ignores them) but take up
inode space and confuse manual inspection. Run:

```sh
kojo --clean legacy                   # dry-run; prints what WOULD be removed
kojo --clean legacy --clean-apply     # actually remove
```

The sweep removes a file ONLY if the canonical kv row exists at the
mapped (namespace, key). A file with no kv mirror is reported under
"skipping … (kv miss)" and left in place so the runtime's lazy
migration can still pick it up on the next read.

`--clean all` runs both `snapshots` and `legacy` in one pass. (`v0`
is intentionally NOT included in `all` — see below.)

## Reclaim disk by soft-deleting the v0 dir

Once you have confirmed v1 is stable and you no longer plan to roll
back to a v0 binary, soft-delete the v0 dir to reclaim disk:

```sh
kojo --clean v0                                       # dry-run
kojo --clean v0 --clean-apply                         # apply
kojo --clean v0 --clean-apply --clean-force           # apply when v0 has been edited post-migration
```

The target renames the v0 dir (`~/.config/kojo/`) to a sibling
`~/.config/kojo.deleted-<unix-millis>/`. The rename is atomic on POSIX.
Physical deletion of the trash dir is left to the operator — see
the "Purge soft-deleted v0 trash dirs" section below for the
`--clean v0-trash` workflow that handles it with age-filtering and
anomaly detection.

Safety gates (only manifest divergence is `--clean-force` overridable;
every other gate fails closed):

- v0 dir absent → no-op.
- v0 path is a symlink → refused (renaming would move the link, not
  the target).
- v0 path exists but is not a directory → refused.
- A v0 binary is currently holding `kojo.lock` → refused (renaming
  the live dir would corrupt its open files).
- v1 `migration_complete.json` missing / parse-fails / has empty
  `v0_sha256_manifest` → refused (migration history unverifiable).
- `migration_complete.json` records a `v0_path` that does not match
  the cleanup target → refused (catches multi-config-dir mix-ups).
- v0 SHA256 manifest diverges from the value stamped in
  `migration_complete.json` → **refused unless `--clean-force` is
  set**. Divergence means v0 was edited after migration completed;
  the operator confirms intent by passing `--clean-force`.
- Trash destination dir already exists → refused (no clobber).
- At apply time, all of the gates above are re-validated (full
  re-plan). The rename is aborted in either of two cases: a new
  non-overridable block has appeared (v0 became a symlink, a v0
  binary started, etc.) — these never proceed regardless of
  `--clean-force`; or the manifest has drifted between dry-run and
  apply and `--clean-force` is not set — pass `--clean-force` to
  proceed with the drifted state.

`v0` is intentionally NOT folded into `--clean all` so periodic
snapshot/legacy housekeeping never accidentally consumes the rollback
fallback.

## Purge soft-deleted v0 trash dirs

Slice 30 added `--clean v0-trash`, the second half of the soft-delete
recovery cycle. After `--clean v0` renames the v0 dir to a sibling
`kojo.deleted-<unix-millis>/`, that trash directory sits there until
the operator decides to reclaim its disk:

```sh
kojo --clean v0-trash                                   # dry-run (default --clean-min-age-days=7)
kojo --clean v0-trash --clean-apply                     # apply: remove trash dirs ≥7 days old
kojo --clean v0-trash --clean-apply --clean-min-age-days=0
                                                        # apply: remove EVERY trash dir (defeats the 7-day recovery window)
```

The discovery walk:

- only matches sibling entries named `kojo.deleted-<digits>/`; the
  live `kojo` / `kojo-v1` dirs and any unrelated user content are
  ignored.
- skips symlinks, regular files, and entries whose stamp suffix
  fails to parse — those land in the "anomalies" bucket and are
  reported but never auto-purged.
- uses the timestamp encoded in the dir name (not mtime) for the
  age filter, so a copy-restored host that lost mtimes still gets
  consistent filtering.

`v0-trash` is also intentionally excluded from `--clean all` — the
trash dirs ARE the recovery window.

`kojo --clean v0-trash --clean-apply --clean-min-age-days=7` run
from cron is the operator-driven equivalent of the design's planned
7-day startup auto-sweep, which remains deferred to a future slice.
(`--clean-min-age-days=7` is the CLI default but spelled out here so
the cron entry is unambiguous if the default ever changes.)

## Verify a snapshot programmatically

```go
import "github.com/loppo-llc/kojo/internal/snapshot"

if err := snapshot.VerifyDB(snapshotDir); err != nil {
    log.Fatalf("snapshot %s broken: %v", snapshotDir, err)
}
```

`VerifyDB` reads the manifest's `db_sha256` and recomputes the hash over
`kojo.db`. Use this after rsync / cp to catch truncation.
