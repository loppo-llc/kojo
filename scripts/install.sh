#!/bin/sh
# Install the latest (or pinned) kojo release binary.
# Usage: curl -fsSL https://raw.githubusercontent.com/loppo-llc/kojo/main/scripts/install.sh | sh
#
# Env:
#   KOJO_VERSION      optional tag pin, e.g. v0.117.0
#   KOJO_INSTALL_DIR  optional install directory

set -eu

REPO="loppo-llc/kojo"
RELEASES_PAGE="https://github.com/${REPO}/releases"

die() {
  printf 'kojo install: %s\n' "$*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

# --- platform detection -----------------------------------------------------

os_raw=$(uname -s)
case "$os_raw" in
  Darwin) os=darwin ;;
  Linux)  os=linux ;;
  MINGW*|MSYS*|CYGWIN*|Windows_NT)
    die "Windows is not supported by this script; download a release asset from ${RELEASES_PAGE}"
    ;;
  *)
    die "unsupported OS '$os_raw'; download a release asset from ${RELEASES_PAGE}"
    ;;
esac

arch_raw=$(uname -m)
case "$arch_raw" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64)  arch=amd64 ;;
  *)
    die "unsupported architecture '$arch_raw' on $os; download a release asset from ${RELEASES_PAGE}"
    ;;
esac

asset="kojo_${os}_${arch}.tar.gz"

# --- version / download URL -------------------------------------------------

if [ -n "${KOJO_VERSION:-}" ]; then
  base_url="https://github.com/${REPO}/releases/download/${KOJO_VERSION}"
else
  base_url="https://github.com/${REPO}/releases/latest/download"
fi

need_cmd curl
need_cmd tar
need_cmd install
need_cmd mktemp
need_cmd awk

# Prefer sha256sum (Linux); fall back to shasum -a 256 (macOS).
if command -v sha256sum >/dev/null 2>&1; then
  use_shasum=0
elif command -v shasum >/dev/null 2>&1; then
  use_shasum=1
else
  die "missing sha256sum or shasum"
fi

# --- workdir + cleanup ------------------------------------------------------

workdir=$(mktemp -d)
cleanup() {
  rm -rf "$workdir"
}
trap cleanup EXIT INT HUP TERM

printf 'kojo install: downloading %s\n' "$asset"
curl -fsSL "${base_url}/${asset}" -o "${workdir}/${asset}"
curl -fsSL "${base_url}/checksums.txt" -o "${workdir}/checksums.txt"

# --- checksum verify --------------------------------------------------------

# Extract the single line for this archive; checkers expect
# "<hex>  <filename>" relative to the current directory.
# Match the final field exactly (do not use regex — asset names contain dots).
line=$(awk -v f="$asset" 'NF >= 2 && $NF == f { print; exit }' "${workdir}/checksums.txt")
if [ -z "$line" ]; then
  die "checksums.txt has no entry for ${asset}; aborting"
fi
printf '%s\n' "$line" > "${workdir}/checksums.one"
if ! (
  cd "$workdir"
  if [ "$use_shasum" -eq 1 ]; then
    shasum -a 256 -c checksums.one
  else
    sha256sum -c checksums.one
  fi
); then
  die "checksum verification failed for ${asset}; aborting"
fi

# --- extract ----------------------------------------------------------------

# Extract ONLY the kojo member — a hostile archive could carry ../ paths;
# sha256 pins the bytes, but extraction should still be surgical.
tar -xzf "${workdir}/${asset}" -C "$workdir" kojo
if [ ! -f "${workdir}/kojo" ]; then
  die "archive did not contain a 'kojo' binary"
fi

# --- install directory ------------------------------------------------------

if [ -n "${KOJO_INSTALL_DIR:-}" ]; then
  dest="$KOJO_INSTALL_DIR"
elif [ -w /usr/local/bin ] 2>/dev/null; then
  dest=/usr/local/bin
else
  dest="${HOME}/.local/bin"
fi

mkdir -p "$dest" || die "cannot create install directory: $dest"
if [ ! -w "$dest" ]; then
  die "install directory is not writable: $dest (set KOJO_INSTALL_DIR or fix permissions)"
fi

install -m 0755 "${workdir}/kojo" "${dest}/kojo"
printf 'kojo install: installed to %s/kojo\n' "$dest"

# PATH hint when dest is not already on PATH.
case ":${PATH}:" in
  *":${dest}:"*) ;;
  *)
    printf 'kojo install: %s is not on PATH; add:\n' "$dest"
    printf '  export PATH="%s:$PATH"\n' "$dest"
    ;;
esac

# --- version + update hint --------------------------------------------------

"${dest}/kojo" --version
printf 'kojo install: later upgrades: run `kojo update` (in-place binary swap)\n'
