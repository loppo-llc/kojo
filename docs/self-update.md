# Self-update

kojo ships prebuilt binaries on every published GitHub Release and can
upgrade itself in place — from the CLI, the Web UI, or (for peers) from
a newer Hub on the same platform.

## Release pipeline

When a maintainer publishes a GitHub release (tag + notes),
`.github/workflows/release.yml` runs once on `ubuntu-latest`:

1. Checkout the release tag.
2. Build the Web UI once (`cd web && KOJO_VERSION=$TAG npm run build`).
   The same `web/dist` is embedded into every platform binary.
3. Cross-compile with `CGO_ENABLED=0` for:
   - `darwin/arm64`, `darwin/amd64`
   - `linux/amd64`, `linux/arm64`
   - `windows/amd64`
4. Package each binary:
   - Unix: `kojo_<goos>_<goarch>.tar.gz` containing a single member `kojo`
   - Windows: `kojo_windows_amd64.zip` containing `kojo.exe`
5. Write `checksums.txt` in `sha256sum` format (`<hex>  <filename>`).
6. Upload every file in `dist/` to the release with `gh release upload`
   (`--clobber`).

Version stamp: `-ldflags "-s -w -X main.version=<tag>"`.

Asset names match what `internal/selfupdate` and peer auto-update
already expect (`kojo_<goos>_<goarch>.tar.gz` / `.zip`, plus
`checksums.txt`).

## Install script

```sh
curl -fsSL https://raw.githubusercontent.com/loppo-llc/kojo/main/scripts/install.sh | sh
```

`scripts/install.sh` is POSIX `sh`. It:

- Detects `darwin`/`linux` and `arm64`/`amd64` via `uname`
- Downloads `kojo_${os}_${arch}.tar.gz` and `checksums.txt`
- Verifies the archive with `sha256sum -c` (or `shasum -a 256 -c` on macOS)
- Installs to `$KOJO_INSTALL_DIR`, else `/usr/local/bin` when writable
  (no automatic `sudo`), else `$HOME/.local/bin`

Env overrides:

| Variable | Effect |
|----------|--------|
| `KOJO_VERSION` | Pin a tag (e.g. `v0.117.0`) instead of `latest` |
| `KOJO_INSTALL_DIR` | Install directory |

Windows is not supported by the script; use a release asset directly.

## `kojo update`

```sh
kojo update          # check, download, verify, swap in place
kojo update -check   # report only
```

Implemented in `cmd/kojo/update_cmd.go`. It talks to GitHub Releases,
pulls the platform archive, checks `checksums.txt`, extracts next to
the running executable, and calls `selfupdate.SwapExecutable`.

The running daemon keeps the old image mapped until it restarts. After
a CLI update, restart via the Web UI, `POST /api/v1/system/restart`, or
relaunch the process.

## HTTP: `/api/v1/system/update`

Owner or a privileged agent only (same gate as restart).

### GET

Returns the last (or freshly-fetched) check snapshot:

```json
{
  "supported": true,
  "current": "v0.117.0",
  "latest": "v0.118.0",
  "updateAvailable": true,
  "notesUrl": "https://github.com/loppo-llc/kojo/releases/tag/v0.118.0",
  "checkedAt": "2026-07-10T12:00:00Z"
}
```

- `supported` is `true` only when an in-place restart path is wired
  (Unix daemon with re-exec). When no checker is configured the body is
  just `{"supported":false}` with HTTP 200 so the UI can hide the control.
- `?refresh=1` forces `CheckNow` (20s timeout). Upstream failure → **502**
  `upstream_error`.

### POST

Downloads the latest platform binary, swaps it, then starts the same
graceful restart drain as `POST /api/v1/system/restart`.

Optional JSON body (same as restart):

```json
{ "wake": true, "agentId": "ag_..." }
```

`wake` arms a one-shot system chat turn after re-exec so an agent can
verify the deploy. Agents may only wake themselves; the Owner must name
`agentId` explicitly. Wake validation failures return 400/403/404/501 as
on restart.

| Status | When |
|--------|------|
| **202** `{"status":"pending","from","to"}` | Swap succeeded; restart drain started |
| **202** `{"status":"already_pending","from","to"}` | Swap succeeded; a restart drain was already in flight |
| **200** `{"status":"up_to_date"}` | No newer release |
| **409** `already_updating` | Another Apply is mid-download/swap |
| **501** `unsupported` | No checker, or no in-place restart path (e.g. Windows daemon) |
| **502** `assets_not_ready` / `update_failed` | Missing platform asset or download/swap error |

Apply uses a 5-minute context derived from
`context.WithoutCancel(r.Context())` so a dropped client connection
cannot abort a half-applied swap.

The Web UI polls GET for the banner and POSTs to apply (download, swap,
graceful restart).

## Periodic checker

On boot the daemon builds a `selfupdate.Checker` always (so GET still
works). The background loop starts only when:

- version is a parseable release tag (dev/`git describe --dirty` stamps skip)
- neither `--no-update-check` nor `KOJO_NO_UPDATE_CHECK=1` is set

Timing: first check **30s** after boot, then every **6h**. Network
errors log at Debug. The first time a newer tag is seen, an Info log
suggests `kojo update` or the Web UI.

## Peer auto-update

Peers that connect to a Hub may pull that Hub's binary instead of
hitting GitHub:

1. Hub advertises `version`, `goos`, `goarch`, and `binarySha256` on
   hub-info.
2. Peer gates: AutoUpdate enabled, restart path present, Hub version
   strictly newer and parseable, same `GOOS`/`GOARCH`, non-empty SHA,
   and this Hub version has not been attempted yet in this process.
3. Download `GET /api/v1/peers/binary` over the paired Tailscale path.
4. **Double-SHA pin**: hub-info `binarySha256` and response header
   `X-Kojo-Binary-SHA256` must both equal the SHA-256 of the body.
5. Swap in place and request graceful restart.

One attempt per Hub version per process (marked before download so a
crash mid-transfer does not tight-loop). Platform mismatch logs once and
asks the operator to run `kojo update` on that peer. Empty
`binarySha256` skips with a warning.

Opt-out: `--no-peer-autoupdate` or `KOJO_NO_PEER_AUTOUPDATE=1`.

## Windows caveats

- Release assets are zip (`kojo_windows_amd64.zip` → `kojo.exe`).
- `kojo update` (CLI) swaps via rename-to-`.exe.old` then place the new
  exe; leftover `.old` files are reaped on next boot.
- Daemon endpoints answer **501** for self-update apply when no in-place
  re-exec path is available — that path is Unix-only. CLI update still
  works; restart the service or process manually afterward.
- The install script does not support Windows; download assets from the
  releases page.
