#!/bin/sh
# aprgo one-line installer.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/cartpauj/aprgo/main/get.sh | sudo sh
#
# What it does:
#   1. Detects your distro family (Debian-like vs Red Hat-like).
#   2. Detects your CPU arch (amd64, arm64, armhf, armhf-v6, i386).
#   3. Downloads the matching .deb or .rpm from the latest GitHub release.
#   4. Installs it via apt-get or dnf — pulling in bluez (+ bluez-tools
#      on Debian) as hard deps. Soundcard (direwolf) and KISS-over-TCP
#      (tnc-server) helpers are Suggests, so install those yourself if
#      you need them.
#   5. Tells you the next step.
#
# If your system isn't supported (different distro family, or an arch we
# don't ship binaries for), the script bails with the build-from-source
# recipe.

set -eu

GH_REPO="cartpauj/aprgo"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

# ─── pretty output helpers ────────────────────────────────────────────
info()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn()  { printf '\033[1;33m!!\033[0m  %s\n' "$*" >&2; }
fail()  { printf '\033[1;31mxx\033[0m  %s\n' "$*" >&2; exit 1; }

# ─── must be root ─────────────────────────────────────────────────────
if [ "$(id -u)" -ne 0 ]; then
    fail "this installer must be run as root — try:  curl -fsSL https://raw.githubusercontent.com/${GH_REPO}/main/get.sh | sudo sh"
fi

# ─── tool sanity ──────────────────────────────────────────────────────
need() {
    command -v "$1" >/dev/null 2>&1 || fail "required tool '$1' not found in PATH"
}
need curl
need uname

# ─── distro detection ─────────────────────────────────────────────────
if [ ! -r /etc/os-release ]; then
    fail "/etc/os-release missing — can't identify your distro"
fi
. /etc/os-release

DISTRO_FAMILY=""
case " ${ID:-} ${ID_LIKE:-} " in
    *" debian "*|*" ubuntu "*|*" raspbian "*)
        DISTRO_FAMILY="debian"
        ;;
    *" fedora "*|*" rhel "*|*" centos "*|*" rocky "*|*" almalinux "*|*" ol "*|*" amzn "*|*" suse "*|*" opensuse "*|*" opensuse-tumbleweed "*|*" opensuse-leap "*)
        DISTRO_FAMILY="rhel"
        ;;
esac

# ─── arch detection ───────────────────────────────────────────────────
RAW_ARCH="$(uname -m)"
DEB_ARCH=""
RPM_ARCH=""
case "$RAW_ARCH" in
    x86_64|amd64)
        DEB_ARCH="amd64"
        RPM_ARCH="x86_64"
        ;;
    aarch64|arm64)
        DEB_ARCH="arm64"
        RPM_ARCH="aarch64"
        ;;
    armv7l|armv7)
        DEB_ARCH="armhf"
        RPM_ARCH="armv7hl"
        ;;
    armv6l|armv6)
        # Pi Zero / Pi 1 — both ARMv6 and ARMv7 packages are tagged
        # Architecture: armhf, so we disambiguate via filename suffix.
        DEB_ARCH="armhf-armv6"   # composed filename suffix; not a real Debian arch
        RPM_ARCH=""
        ;;
    i386|i486|i586|i686)
        DEB_ARCH="i386"
        RPM_ARCH="i686"
        ;;
esac

# ─── bail with build-from-source recipe if unsupported ────────────────
unsupported() {
    cat >&2 <<EOF

aprgo prebuilt packages are not available for your system:
  OS:    ${NAME:-${ID:-unknown}}${VERSION:+ ${VERSION}}
  Arch:  ${RAW_ARCH}

Reason: ${1}

Build aprgo from source instead — it's a pure-Go binary, easy to compile:

  # 1. Install Go 1.26 or newer.
  #    Debian/Ubuntu:  sudo apt install golang-1.26 git
  #    Fedora:         sudo dnf install golang git
  #    Arch:           sudo pacman -S go git
  #    Alpine:         sudo apk add go git build-base
  #    macOS/other:    https://go.dev/dl/

  # 2. Clone and build.
  git clone https://github.com/${GH_REPO}
  cd aprgo
  CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o aprgo ./cmd/aprgo

  # 3. Install (Linux only).
  sudo ./deploy/install.sh ./aprgo
  sudo systemctl start aprgo

Runtime helpers (install whichever your TNC needs):
  Bluetooth TNCs:           bluez (+ bluez-tools on Debian/Ubuntu)
  Soundcard TNCs:           direwolf
  KISS-over-TCP TNCs:       tnc-server

Questions, hardware not listed, or RISC-V / MIPS / POWER:
https://github.com/${GH_REPO}/issues
EOF
    exit 1
}

if [ -z "$DISTRO_FAMILY" ]; then
    unsupported "your distro family is not Debian-like (apt) or Red Hat-like (dnf)."
fi

case "$DISTRO_FAMILY" in
    debian)
        [ -z "$DEB_ARCH" ] && unsupported "no prebuilt .deb available for arch '${RAW_ARCH}'."
        ;;
    rhel)
        [ -z "$RPM_ARCH" ] && unsupported "no prebuilt .rpm available for arch '${RAW_ARCH}' (Pi Zero / ARMv6 hardware does not have a Red Hat-family distro target)."
        ;;
esac

# ─── locate latest release ────────────────────────────────────────────
info "querying GitHub for the latest aprgo release…"
API="https://api.github.com/repos/${GH_REPO}/releases/latest"
RELEASE_JSON="${TMPDIR}/release.json"
if ! curl -fsSL "$API" -o "$RELEASE_JSON"; then
    fail "couldn't reach GitHub API ($API) — check your network"
fi

# Parse the tag name without requiring jq. Looks for: "tag_name": "v1.2.3"
TAG="$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$RELEASE_JSON" | head -n1)"
[ -z "$TAG" ] && fail "couldn't parse tag from GitHub API response"
VERSION="${TAG#v}"
info "latest release: ${TAG}"

# ─── download the right asset ─────────────────────────────────────────
case "$DISTRO_FAMILY" in
    debian)
        ASSET="aprgo_${VERSION}_${DEB_ARCH}.deb"
        ;;
    rhel)
        # RPM filenames embed a "release" number after the version:
        # aprgo-<version>-<release>.<arch>.rpm. We pin release=1 in
        # deploy/nfpm.yaml so this stays predictable across builds.
        ASSET="aprgo-${VERSION}-1.${RPM_ARCH}.rpm"
        ;;
esac

URL="https://github.com/${GH_REPO}/releases/download/${TAG}/${ASSET}"
DEST="${TMPDIR}/${ASSET}"

info "downloading ${ASSET}…"
if ! curl -fsSL "$URL" -o "$DEST"; then
    fail "couldn't download ${URL} — your arch may not be in this release; report at https://github.com/${GH_REPO}/issues"
fi

# ─── install ──────────────────────────────────────────────────────────
case "$DISTRO_FAMILY" in
    debian)
        info "installing with apt-get…"
        # `apt-get install ./path` resolves Recommends + Depends correctly,
        # which `dpkg -i` does not. We don't need to apt-get update first.
        DEBIAN_FRONTEND=noninteractive apt-get install -y "$DEST"
        ;;
    rhel)
        info "installing with dnf…"
        # dnf handles Recommends similarly when install_weak_deps is enabled
        # (the default on Fedora; some hardened RHEL setups disable it).
        dnf install -y "$DEST"
        ;;
esac

# ─── done ─────────────────────────────────────────────────────────────
# The package's postinst script already printed the "aprgo installed" banner
# with the URLs and default login — nothing more to say here.
