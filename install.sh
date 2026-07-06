#!/usr/bin/env bash
#
# lwd installer — fetches build dependencies, builds the four binaries, and
# installs them. Works on Debian/Ubuntu (apt) and RHEL-family (dnf/yum:
# Fedora, RHEL, CentOS, Rocky, AlmaLinux) with automatic package-manager
# switching.
#
# Usage:
#   ./install.sh [options]
#   curl -fsSL https://raw.githubusercontent.com/EObrien60/lwd/main/install.sh | bash -s -- [options]
#
# Options:
#   --docker        Also install Docker Engine + the compose plugin if missing
#                   (via https://get.docker.com; lwd needs a Docker daemon at runtime).
#   --systemd       Install and enable a systemd unit for `lwd daemon`
#                   (lwd.service, LWD_DATA_DIR=/var/lib/lwd, root, After=docker).
#   --prefix DIR    Install binaries to DIR (default: /usr/local/bin).
#   --go-version V  Force a specific Go toolchain version to download (e.g. 1.25.4);
#                   default: reuse a system Go >= 1.25, else fetch the latest stable.
#   --repo URL      Git URL to clone when not run from inside a checkout
#                   (default: https://github.com/EObrien60/lwd).
#   --ref REF       Git ref to build when cloning (default: main).
#   -h, --help      Show this help.
#
# It never uses cgo (CGO_ENABLED=0, pure-Go SQLite), so no C toolchain is
# required — only Go, git, curl and tar.

set -euo pipefail

# ---- defaults ---------------------------------------------------------------
MIN_GO="1.25"
PREFIX="/usr/local/bin"
INSTALL_DOCKER=0
INSTALL_SYSTEMD=0
GO_VERSION=""
REPO_URL="https://github.com/EObrien60/lwd"
REPO_REF="main"
GOROOT_DL="/usr/local/go"          # where a downloaded Go toolchain lands
BINS=(lwd lwd-web lwd-mcp lwd-agent)

# ---- pretty output ----------------------------------------------------------
c_blue=$'\033[1;34m'; c_green=$'\033[1;32m'; c_red=$'\033[1;31m'; c_dim=$'\033[2m'; c_off=$'\033[0m'
log()  { printf '%s==>%s %s\n' "$c_blue" "$c_off" "$*"; }
ok()   { printf '%s ok%s %s\n' "$c_green" "$c_off" "$*"; }
warn() { printf '%swarn%s %s\n' "$c_red" "$c_off" "$*" >&2; }
die()  { printf '%serror%s %s\n' "$c_red" "$c_off" "$*" >&2; exit 1; }

usage() { sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

# ---- arg parsing ------------------------------------------------------------
while [ $# -gt 0 ]; do
  case "$1" in
    --docker)   INSTALL_DOCKER=1 ;;
    --systemd)  INSTALL_SYSTEMD=1 ;;
    --prefix)   PREFIX="${2:?--prefix needs a directory}"; shift ;;
    --go-version) GO_VERSION="${2:?--go-version needs a version}"; shift ;;
    --repo)     REPO_URL="${2:?--repo needs a URL}"; shift ;;
    --ref)      REPO_REF="${2:?--ref needs a git ref}"; shift ;;
    -h|--help)  usage 0 ;;
    *)          die "unknown option: $1 (see --help)" ;;
  esac
  shift
done

# ---- root / sudo ------------------------------------------------------------
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  if command -v sudo >/dev/null 2>&1; then
    SUDO="sudo"
  else
    die "this script needs root (to install packages/binaries) and sudo was not found; re-run as root"
  fi
fi
asroot() { if [ -n "$SUDO" ]; then $SUDO "$@"; else "$@"; fi; }

# ---- distro / package manager detection ------------------------------------
PKG=""          # apt | dnf | yum
DISTRO_ID=""
detect_distro() {
  [ -r /etc/os-release ] || die "cannot read /etc/os-release — unsupported system"
  # shellcheck disable=SC1091
  . /etc/os-release
  DISTRO_ID="${ID:-unknown}"
  local like="${ID_LIKE:-}"
  case " $DISTRO_ID $like " in
    *" debian "*|*" ubuntu "*) PKG="apt" ;;
    *" fedora "*|*" rhel "*|*" centos "*)
      if command -v dnf >/dev/null 2>&1; then PKG="dnf"; else PKG="yum"; fi ;;
    *)
      # last resort: sniff for a package manager
      if   command -v apt-get >/dev/null 2>&1; then PKG="apt"
      elif command -v dnf     >/dev/null 2>&1; then PKG="dnf"
      elif command -v yum     >/dev/null 2>&1; then PKG="yum"
      else die "unsupported distro '$DISTRO_ID' (need apt, dnf, or yum)"; fi ;;
  esac
  ok "detected ${PRETTY_NAME:-$DISTRO_ID} → package manager: $PKG"
}

pkg_install() {  # pkg_install pkg1 pkg2 ...
  [ $# -gt 0 ] || return 0
  log "installing packages: $*"
  case "$PKG" in
    apt) asroot env DEBIAN_FRONTEND=noninteractive apt-get update -qq
         asroot env DEBIAN_FRONTEND=noninteractive apt-get install -y "$@" ;;
    dnf) asroot dnf install -y "$@" ;;
    yum) asroot yum install -y "$@" ;;
  esac
}

# ---- version helpers --------------------------------------------------------
ver_ge() { [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" = "$2" ]; }  # $1 >= $2 ?

go_bin() {  # echo a usable go binary path, or nothing
  if command -v go >/dev/null 2>&1; then command -v go; return; fi
  if [ -x "$GOROOT_DL/bin/go" ]; then echo "$GOROOT_DL/bin/go"; return; fi
}

go_ver() { "$1" version 2>/dev/null | awk '{print $3}' | sed 's/^go//'; }  # go_ver /path/to/go

go_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    armv6l)        echo armv6l ;;
    *) die "unsupported CPU architecture: $(uname -m)" ;;
  esac
}

# ---- ensure go >= MIN_GO ----------------------------------------------------
GO=""
ensure_go() {
  local existing; existing="$(go_bin || true)"
  if [ -n "$existing" ] && [ -z "$GO_VERSION" ]; then
    local v; v="$(go_ver "$existing")"
    if [ -n "$v" ] && ver_ge "$v" "$MIN_GO"; then
      GO="$existing"; ok "using existing Go $v ($GO)"; return
    fi
    warn "system Go '${v:-none}' is older than required $MIN_GO — installing a private toolchain"
  fi

  local want="$GO_VERSION"
  if [ -z "$want" ]; then
    log "resolving latest stable Go version"
    want="$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -n1 | sed 's/^go//')" \
      || die "could not reach go.dev to resolve the latest Go version (pass --go-version)"
  fi
  ver_ge "$want" "$MIN_GO" || die "requested Go $want is older than the required $MIN_GO"

  local arch tarball url tmp
  arch="$(go_arch)"
  tarball="go${want}.linux-${arch}.tar.gz"
  url="https://go.dev/dl/${tarball}"
  tmp="$(mktemp -d)"
  log "downloading $url"
  curl -fSL --retry 3 -o "$tmp/$tarball" "$url" || die "failed to download Go $want for linux/$arch"
  log "installing Go to $GOROOT_DL"
  asroot rm -rf "$GOROOT_DL"
  asroot tar -C "$(dirname "$GOROOT_DL")" -xzf "$tmp/$tarball"
  rm -rf "$tmp"
  GO="$GOROOT_DL/bin/go"
  [ -x "$GO" ] || die "Go install failed: $GO not found"
  ok "installed Go $(go_ver "$GO")"
}

# ---- locate (or fetch) the source ------------------------------------------
SRC=""
CLEANUP_SRC=0
locate_source() {
  local self_dir=""
  # BASH_SOURCE is empty when piped via curl|bash; guard it.
  if [ -n "${BASH_SOURCE:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
    self_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  fi
  if [ -n "$self_dir" ] && [ -f "$self_dir/go.mod" ] && grep -q '^module lwd$' "$self_dir/go.mod" 2>/dev/null; then
    SRC="$self_dir"; ok "building from checkout at $SRC"; return
  fi
  # Not inside a checkout (e.g. curl|bash) — clone it.
  command -v git >/dev/null 2>&1 || pkg_install git
  SRC="$(mktemp -d)"; CLEANUP_SRC=1
  log "cloning $REPO_URL ($REPO_REF) → $SRC"
  git clone --depth 1 --branch "$REPO_REF" "$REPO_URL" "$SRC" \
    || git clone "$REPO_URL" "$SRC" || die "failed to clone $REPO_URL"
  ok "cloned lwd to $SRC"
}

# ---- build + install --------------------------------------------------------
build_install() {
  log "building four binaries (CGO_ENABLED=0)"
  ( cd "$SRC" && for b in "${BINS[@]}"; do
      printf '    %sgo build %s%s\n' "$c_dim" "$b" "$c_off"
      CGO_ENABLED=0 "$GO" build -o "$SRC/.dist/$b" "./cmd/$b"
    done )
  log "installing to $PREFIX"
  asroot install -d "$PREFIX"
  for b in "${BINS[@]}"; do asroot install -m 0755 "$SRC/.dist/$b" "$PREFIX/$b"; done
  ok "installed: ${BINS[*]} → $PREFIX"
  "$PREFIX/lwd" version 2>/dev/null || true
}

# ---- optional: Docker -------------------------------------------------------
maybe_docker() {
  [ "$INSTALL_DOCKER" -eq 1 ] || return 0
  if command -v docker >/dev/null 2>&1; then ok "docker already present ($(docker --version 2>/dev/null))"; return; fi
  log "installing Docker Engine via get.docker.com"
  curl -fsSL https://get.docker.com | asroot sh
  asroot systemctl enable --now docker 2>/dev/null || true
  ok "docker installed"
}

# ---- optional: systemd unit for the daemon ---------------------------------
maybe_systemd() {
  [ "$INSTALL_SYSTEMD" -eq 1 ] || return 0
  command -v systemctl >/dev/null 2>&1 || { warn "systemctl not found — skipping --systemd"; return; }
  log "installing systemd unit lwd.service"
  asroot install -d /var/lib/lwd
  asroot tee /etc/systemd/system/lwd.service >/dev/null <<UNIT
[Unit]
Description=lwd deploy engine (daemon)
After=docker.service network-online.target
Requires=docker.service
Wants=network-online.target

[Service]
Type=simple
Environment=LWD_DATA_DIR=/var/lib/lwd
ExecStart=$PREFIX/lwd daemon
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT
  asroot systemctl daemon-reload
  asroot systemctl enable --now lwd.service
  ok "lwd.service enabled and started (LWD_DATA_DIR=/var/lib/lwd)"
}

# ---- next steps -------------------------------------------------------------
next_steps() {
  cat <<STEPS

${c_green}lwd installed.${c_off}

Next steps:
  1. Ensure Docker is running (install with --docker, or your distro's docker package).
STEPS
  if [ "$INSTALL_SYSTEMD" -eq 1 ]; then
    cat <<STEPS
  2. The daemon is running via systemd:
       systemctl status lwd
       journalctl -u lwd -f
STEPS
  else
    cat <<STEPS
  2. Start the daemon (root, needs Docker):
       sudo LWD_DATA_DIR=/var/lib/lwd lwd daemon &
     (or re-run this installer with --systemd to manage it via systemd.)
STEPS
  fi
  cat <<STEPS
  3. Deploy an app:
       mkdir myapp && cat > myapp/lwd.toml <<'EOF'
       name = "app"
       image = "traefik/whoami:latest"
       domain = "app.localhost"
       port = 80
       EOF
       lwd apply ./myapp
       curl -H "Host: app.localhost" http://127.0.0.1/

  4. Web dashboard (optional) — set a password yourself; there is NO default:
       LWD_WEB_PASSWORD='choose-a-strong-password' lwd-web
     then open http://127.0.0.1:8079 and log in with that password
     (login is password-only; no username).

See the README for multi-node fleets, replicas/scaling, secrets, and the MCP server.
STEPS
}

# ---- main -------------------------------------------------------------------
detect_distro
# Base build deps (no C toolchain — CGO is disabled). curl+tar for the Go download.
DEPS=(git curl ca-certificates tar)
[ "$PKG" = "apt" ] && DEPS+=(coreutils)      # sort -V lives in coreutils (present, but explicit)
pkg_install "${DEPS[@]}"
ensure_go
locate_source
build_install
maybe_docker
maybe_systemd
[ "$CLEANUP_SRC" -eq 1 ] && rm -rf "$SRC"
next_steps
