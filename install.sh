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
#   --web           Install and enable a systemd unit for `lwd-web`
#                   (lwd-web.service, /etc/lwd/web.env). Won't auto-start until
#                   you set LWD_WEB_PASSWORD in the env file (no default password).
#   --agent         Install and enable a systemd unit for `lwd-agent`
#                   (lwd-agent.service, /etc/lwd/agent.env). Won't auto-start
#                   until you set LWD_AGENT_TOKEN in the env file.
#   --public        One-command, zero-interaction public setup: implies
#                   --systemd --web (and --docker if Docker isn't already
#                   installed). Auto-generates a strong LWD_WEB_PASSWORD and
#                   LWD_WEB_SECRET, binds the dashboard on 0.0.0.0:8079, and
#                   starts both services immediately — no env editing. The
#                   generated password is printed at the end (also see
#                   --web-password). Served over plain HTTP; put it behind
#                   TLS or a tunnel for real internet exposure.
#   --web-password P  Use P as the dashboard password instead of generating
#                   one (only takes effect when /etc/lwd/web.env is being
#                   created for the first time; implies real secrets even
#                   without --public).
#   --prefix DIR    Install binaries to DIR (default: /usr/local/bin).
#   --go-version V  Force a specific Go toolchain version to download (e.g. 1.25.4);
#                   default: reuse a system Go >= 1.25, else fetch the latest stable.
#   --repo URL      Git URL to clone when not run from inside a checkout
#                   (default: https://github.com/EObrien60/lwd).
#   --ref REF       Git ref to build when cloning (default: main).
#   --update        Update an existing install: pull latest (if run from a
#                   git checkout), rebuild, reinstall the binaries, and
#                   restart any running lwd/lwd-web/lwd-agent services.
#                   Does not touch /etc/lwd env files or any config.
#   --destroy, --remove  Uninstall: stop/disable/remove the lwd/lwd-web/
#                   lwd-agent systemd units and remove the installed
#                   binaries from $PREFIX. Never builds or installs
#                   anything. By default, interactively asks (default No)
#                   whether to also remove /etc/lwd, /var/lib/lwd (secret
#                   key + deployment DB), and all deployed apps + the
#                   lwd-caddy container + the lwd Docker network.
#   --destroy-all   Like --destroy, but also removes config, daemon state,
#                   and all deployed apps + the lwd-caddy container + the
#                   lwd network, with no prompt. Named Docker volumes
#                   (backing DB data) are always left intact.
#   --no-interactive  With --destroy (not --destroy-all), skip the prompt
#                   and default to NOT removing config/apps (install-only
#                   uninstall). Has no effect outside --destroy.
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
INSTALL_WEB=0
INSTALL_AGENT=0
PUBLIC=0
WEB_PASSWORD=""
GENERATED_WEB_PW=""
GO_VERSION=""
REPO_URL="https://github.com/EObrien60/lwd"
REPO_REF="main"
UPDATE=0
DESTROY=0
DESTROY_ALL=0
NONINTERACTIVE=0
GOROOT_DL="/usr/local/go"          # where a downloaded Go toolchain lands
BINS=(lwd lwd-web lwd-mcp lwd-agent)

# ---- pretty output ----------------------------------------------------------
c_blue=$'\033[1;34m'; c_green=$'\033[1;32m'; c_red=$'\033[1;31m'; c_dim=$'\033[2m'; c_off=$'\033[0m'
log()  { printf '%s==>%s %s\n' "$c_blue" "$c_off" "$*"; }
ok()   { printf '%s ok%s %s\n' "$c_green" "$c_off" "$*"; }
warn() { printf '%swarn%s %s\n' "$c_red" "$c_off" "$*" >&2; }
die()  { printf '%serror%s %s\n' "$c_red" "$c_off" "$*" >&2; exit 1; }

usage() { awk 'NR==1{next} /^#/{sub(/^# ?/,""); print; next} {exit}' "$0"; exit "${1:-0}"; }

# gen_secret N — print N random bytes, base64-encoded, no trailing newline.
gen_secret() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -base64 "$1" | tr -d '\n'
  else
    head -c "$1" /dev/urandom | base64 | tr -d '\n'
  fi
}

# ---- arg parsing ------------------------------------------------------------
while [ $# -gt 0 ]; do
  case "$1" in
    --docker)   INSTALL_DOCKER=1 ;;
    --systemd)  INSTALL_SYSTEMD=1 ;;
    --web)      INSTALL_WEB=1 ;;
    --agent)    INSTALL_AGENT=1 ;;
    --public)   PUBLIC=1 ;;
    --web-password) WEB_PASSWORD="${2:?--web-password needs a value}"; shift ;;
    --prefix)   PREFIX="${2:?--prefix needs a directory}"; shift ;;
    --go-version) GO_VERSION="${2:?--go-version needs a version}"; shift ;;
    --repo)     REPO_URL="${2:?--repo needs a URL}"; shift ;;
    --ref)      REPO_REF="${2:?--ref needs a git ref}"; shift ;;
    --update)   UPDATE=1 ;;
    --destroy|--remove) DESTROY=1 ;;
    --destroy-all) DESTROY=1; DESTROY_ALL=1 ;;
    --no-interactive) NONINTERACTIVE=1 ;;
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
    SRC="$self_dir"; ok "building from checkout at $SRC"
    if [ "$UPDATE" -eq 1 ] && command -v git >/dev/null 2>&1 && git -C "$SRC" rev-parse --git-dir >/dev/null 2>&1; then
      local before after
      before="$(git -C "$SRC" rev-parse --short HEAD)"
      log "pulling latest (git pull --ff-only)"
      if git -C "$SRC" pull --ff-only; then
        after="$(git -C "$SRC" rev-parse --short HEAD)"
        if [ "$before" = "$after" ]; then
          ok "already up to date ($after)"
        else
          ok "updated source $before → $after"
        fi
      else
        warn "git pull --ff-only failed (local changes or diverged?) — building the current checkout as-is"
      fi
    fi
    return
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

# ---- update: restart any running lwd services ------------------------------
restart_services() {
  command -v systemctl >/dev/null 2>&1 || { warn "systemctl not found — restart services manually"; return 0; }
  local restarted=0 u
  for u in lwd lwd-web lwd-agent; do
    if [ -f "/etc/systemd/system/${u}.service" ]; then
      if asroot systemctl is-active --quiet "${u}.service"; then
        log "restarting ${u}.service"
        asroot systemctl restart "${u}.service" \
          && ok "${u}.service restarted (now running the new binary)" \
          || warn "failed to restart ${u}.service — check: journalctl -u ${u}"
        restarted=1
      else
        log "${u}.service installed but not active — start it with: systemctl start ${u}"
      fi
    fi
  done
  [ "$restarted" -eq 1 ] || log "no running lwd services were restarted (start them with systemctl start <unit>)"
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

# ---- optional: systemd unit for lwd-web ------------------------------------
maybe_web() {
  [ "$INSTALL_WEB" -eq 1 ] || return 0
  command -v systemctl >/dev/null 2>&1 || { warn "systemctl not found — skipping --web"; return; }
  log "installing systemd unit lwd-web.service"
  asroot install -d -m 0755 /etc/lwd
  if [ ! -f /etc/lwd/web.env ]; then
    if [ "$PUBLIC" -eq 1 ] || [ -n "$WEB_PASSWORD" ]; then
      local pw secret web_addr; pw="${WEB_PASSWORD:-$(gen_secret 18)}"; secret="$(gen_secret 32)"
      web_addr="127.0.0.1:8079"; [ "$PUBLIC" -eq 1 ] && web_addr="0.0.0.0:8079"
      asroot tee /etc/lwd/web.env >/dev/null <<ENV
# lwd-web configuration (this file is 0600). Generated by install.sh.
LWD_WEB_PASSWORD=$pw
LWD_WEB_SECRET=$secret
# Dashboard listen address (0.0.0.0:8079 = public; put behind TLS/tunnel).
LWD_WEB_ADDR=$web_addr
# If lwd-web is NOT co-located with the daemon socket, point it at the daemon's TCP endpoint:
# LWD_DAEMON=127.0.0.1:8077
# LWD_API_TOKEN=<must match the daemon's LWD_API_TOKEN>
ENV
      GENERATED_WEB_PW="$pw"
    else
      asroot tee /etc/lwd/web.env >/dev/null <<'ENV'
# lwd-web configuration (this file is 0600). Set a strong password.
LWD_WEB_PASSWORD=CHANGE_ME
# Set 0.0.0.0:8079 to expose (put behind TLS/tunnel!)
LWD_WEB_ADDR=127.0.0.1:8079
# LWD_WEB_SECRET=<32+ bytes to persist sessions across restarts>
# If lwd-web is NOT co-located with the daemon socket, point it at the daemon's TCP endpoint:
# LWD_DAEMON=127.0.0.1:8077
# LWD_API_TOKEN=<must match the daemon's LWD_API_TOKEN>
ENV
    fi
    asroot chmod 0600 /etc/lwd/web.env
    asroot chown root:root /etc/lwd/web.env 2>/dev/null || true
  else
    ok "reusing existing /etc/lwd/web.env (password unchanged)"
  fi
  asroot tee /etc/systemd/system/lwd-web.service >/dev/null <<UNIT
[Unit]
Description=lwd web dashboard
After=lwd.service network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/lwd/web.env
ExecStart=$PREFIX/lwd-web
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT
  asroot systemctl daemon-reload
  asroot systemctl enable lwd-web
  if asroot grep -q 'CHANGE_ME' /etc/lwd/web.env; then
    warn "edit /etc/lwd/web.env (set LWD_WEB_PASSWORD), then: systemctl start lwd-web"
  else
    asroot systemctl start lwd-web
    ok "lwd-web.service enabled and started"
  fi
}

# ---- optional: systemd unit for lwd-agent ----------------------------------
maybe_agent() {
  [ "$INSTALL_AGENT" -eq 1 ] || return 0
  command -v systemctl >/dev/null 2>&1 || { warn "systemctl not found — skipping --agent"; return; }
  log "installing systemd unit lwd-agent.service"
  asroot install -d -m 0755 /etc/lwd
  if [ ! -f /etc/lwd/agent.env ]; then
    asroot tee /etc/lwd/agent.env >/dev/null <<'ENV'
# lwd-agent configuration (this file is 0600). Set a strong token.
LWD_AGENT_TOKEN=CHANGE_ME
LWD_AGENT_ADDR=:8078
ENV
    asroot chmod 0600 /etc/lwd/agent.env
    asroot chown root:root /etc/lwd/agent.env 2>/dev/null || true
  else
    ok "/etc/lwd/agent.env already exists — leaving it untouched"
  fi
  asroot tee /etc/systemd/system/lwd-agent.service >/dev/null <<UNIT
[Unit]
Description=lwd node agent
After=docker.service network-online.target
Requires=docker.service
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/lwd/agent.env
ExecStart=$PREFIX/lwd-agent
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT
  asroot systemctl daemon-reload
  asroot systemctl enable lwd-agent
  if asroot grep -q 'CHANGE_ME' /etc/lwd/agent.env; then
    warn "edit /etc/lwd/agent.env (set LWD_AGENT_TOKEN), then: systemctl start lwd-agent"
  else
    asroot systemctl start lwd-agent
    ok "lwd-agent.service enabled and started"
  fi
}

# ---- next steps -------------------------------------------------------------
next_steps() {
  if [ "$UPDATE" -eq 1 ]; then
    cat <<STEPS

${c_green}lwd updated.${c_off}

  Binaries rebuilt and reinstalled to $PREFIX.
  Running services were restarted to pick up the new binaries (see log above);
  services that exist but weren't running were left stopped.

  Check status:
       systemctl status lwd lwd-web lwd-agent
       journalctl -u lwd -f

  Note: /etc/lwd/*.env config files were left untouched.

  Installed version:
STEPS
    "$PREFIX/lwd" version 2>/dev/null || true
    return 0
  fi

  if [ "$PUBLIC" -eq 1 ]; then
    local ip
    ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
    cat <<STEPS

${c_green}lwd is up (public).${c_off}

  Dashboard:
STEPS
    [ -n "$ip" ] && printf '       http://%s:8079\n' "$ip"
    printf '       http://<server-ip>:8079\n'
    if [ -n "$GENERATED_WEB_PW" ]; then
      cat <<STEPS

  Dashboard password (SAVE THIS): $GENERATED_WEB_PW
  (login is password-only, no username)
STEPS
    else
      cat <<STEPS

  Using the existing password in /etc/lwd/web.env.
STEPS
    fi
    cat <<STEPS

  WARNING: the dashboard is served over plain HTTP. For a real
  internet-facing box, terminate TLS in front of it (a future --domain
  flag will do this with Let's Encrypt) or restrict :8079 to a VPN/SSH
  tunnel instead of leaving it open. Host ports 80 and 443 must stay
  free — lwd's own Caddy needs them to route deployed apps.

  Check status:
       systemctl status lwd lwd-web
       journalctl -u lwd-web -f

See the README for multi-node fleets, replicas/scaling, secrets, and the MCP server.
STEPS
    return 0
  fi

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
     (or re-run this installer with --web to manage it via systemd —
     edit /etc/lwd/web.env first, it installs with a CHANGE_ME placeholder.)

  5. Node agent (optional, for multi-node fleets) — set a token yourself:
       LWD_AGENT_TOKEN='choose-a-strong-token' lwd-agent
     (or re-run this installer with --agent to manage it via systemd —
     edit /etc/lwd/agent.env first, it installs with a CHANGE_ME placeholder.)
STEPS
  if [ "$INSTALL_WEB" -eq 1 ] || [ "$INSTALL_AGENT" -eq 1 ]; then
    cat <<STEPS

  Note: /etc/lwd/*.env holds the lwd-web/lwd-agent config (mode 0600). Units
  installed with --web/--agent are enabled but only auto-started once their
  CHANGE_ME placeholder has been replaced with a real password/token:
       systemctl status lwd-web lwd-agent
       journalctl -u lwd-web -u lwd-agent -f
STEPS
  fi
  cat <<STEPS

See the README for multi-node fleets, replicas/scaling, secrets, and the MCP server.
STEPS
}

# ---- destroy / uninstall ----------------------------------------------------
do_destroy() {
  log "uninstalling lwd (units + binaries in $PREFIX)"

  # --- always: stop/disable/remove systemd units --------------------------
  if command -v systemctl >/dev/null 2>&1; then
    local u
    for u in lwd lwd-web lwd-agent; do
      if [ -f "/etc/systemd/system/${u}.service" ]; then
        log "removing ${u}.service"
        asroot systemctl stop "${u}.service" 2>/dev/null || true
        asroot systemctl disable "${u}.service" 2>/dev/null || true
        asroot rm -f "/etc/systemd/system/${u}.service"
        ok "${u}.service stopped, disabled, and removed"
      else
        log "${u}.service not installed — skipping"
      fi
    done
    asroot systemctl daemon-reload
  else
    warn "systemctl not found — skipping unit removal"
    warn "if a bare 'lwd daemon' process is running, stop it yourself (not touching it here)"
  fi

  # --- always: remove installed binaries -----------------------------------
  local b removed_bins=()
  for b in "${BINS[@]}"; do
    if [ -f "$PREFIX/$b" ]; then
      asroot rm -f "$PREFIX/$b"
      ok "removed $PREFIX/$b"
      removed_bins+=("$b")
    else
      log "$PREFIX/$b not present — skipping"
    fi
  done

  # --- decide whether to also purge config/state/apps ----------------------
  local purge=0
  if [ "$DESTROY_ALL" -eq 1 ]; then
    purge=1
  elif [ "$NONINTERACTIVE" -eq 1 ]; then
    purge=0
  else
    local ans=""
    if [ -t 0 ] || [ -r /dev/tty ]; then
      printf 'Also remove lwd configuration (/etc/lwd), daemon state (/var/lib/lwd — includes the encrypted secret key + deployment DB), and ALL deployed apps + the lwd-caddy container + the lwd network? [y/N] ' >&2
      if ! read -r ans </dev/tty 2>/dev/null; then
        ans=""
      fi
    else
      log "no controlling tty — defaulting to keep config/apps (re-run with --destroy-all to remove them)"
      ans=""
    fi
    case "$ans" in
      [Yy]*) purge=1 ;;
      *)     purge=0 ;;
    esac
  fi

  # --- purge: docker containers/network + config/state ---------------------
  local vols_note=0
  if [ "$purge" -eq 1 ]; then
    if command -v docker >/dev/null 2>&1; then
      local ids
      ids="$(
        { docker ps -aq --filter 'label=lwd.app' 2>/dev/null
          docker ps -aq --filter 'label=lwd.role=system' 2>/dev/null
          docker ps -aq --filter 'name=^/lwd-' 2>/dev/null
        } | sort -u
      )"
      if [ -n "$ids" ]; then
        log "removing lwd-managed containers"
        # shellcheck disable=SC2086
        docker rm -f $ids >/dev/null 2>&1 || true
        ok "removed lwd-managed containers"
      else
        log "no lwd-managed containers found"
      fi
      docker network rm lwd >/dev/null 2>&1 || true
      ok "removed docker network 'lwd' (if it existed)"
    else
      warn "docker not found — skipping container/network cleanup"
    fi

    local data_dir="${LWD_DATA_DIR:-/var/lib/lwd}"
    asroot rm -rf /etc/lwd
    ok "removed /etc/lwd"
    asroot rm -rf "$data_dir"
    ok "removed $data_dir"
    vols_note=1
  fi

  # --- summary ---------------------------------------------------------------
  cat <<STEPS

${c_green}lwd uninstall complete.${c_off}

  Systemd units: stopped/disabled/removed where present (lwd, lwd-web, lwd-agent).
  Binaries removed from $PREFIX: ${removed_bins[*]:-none present}.
STEPS
  if [ "$purge" -eq 1 ]; then
    cat <<STEPS
  Config + apps: PURGED — /etc/lwd, ${LWD_DATA_DIR:-/var/lib/lwd}, deployed
  app containers, the lwd-caddy container, and the 'lwd' Docker network were
  all removed.
STEPS
  else
    cat <<STEPS
  Config + apps: KEPT — /etc/lwd, /var/lib/lwd, and any deployed apps were
  left untouched. Re-run with --destroy-all to remove them too.
STEPS
  fi
  if [ "$vols_note" -eq 1 ]; then
    cat <<STEPS

  NOTE: named data volumes (e.g. postgres/minio data) were left intact.
  List with: docker volume ls
  Remove manually with: docker volume rm <name>   (if you truly want the data gone)
STEPS
  fi
}

# ---- main -------------------------------------------------------------------
if [ "$DESTROY" -eq 1 ]; then
  do_destroy
  exit 0
fi
if [ "$PUBLIC" -eq 1 ]; then
  INSTALL_SYSTEMD=1
  INSTALL_WEB=1
  if ! command -v docker >/dev/null 2>&1; then INSTALL_DOCKER=1; fi
fi
detect_distro
# Base build deps (no C toolchain — CGO is disabled). curl+tar for the Go download.
DEPS=(git curl ca-certificates tar)
[ "$PKG" = "apt" ] && DEPS+=(coreutils)      # sort -V lives in coreutils (present, but explicit)
pkg_install "${DEPS[@]}"
ensure_go
locate_source
build_install
[ "$UPDATE" -eq 1 ] && restart_services
maybe_docker
maybe_systemd
maybe_web
maybe_agent
[ "$CLEANUP_SRC" -eq 1 ] && rm -rf "$SRC"
next_steps
