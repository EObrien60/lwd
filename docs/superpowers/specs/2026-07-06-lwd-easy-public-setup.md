# lwd — robust Caddy startup + one-command public setup

**Status:** Design (user-reported: `bind for 0.0.0.0:80 failed: port is already
allocated`, and "too much config — must be easy, a script that stands it up as a
fully public service with no interaction"). Focused fix/feature on merged main.
**Date:** 2026-07-06

## Problem

1. **Port conflict / stale Caddy.** `CaddyRouter.EnsureUp` decides whether to
   create the managed `lwd-caddy` via `caddyRunning`, which only matches a
   container that is named `lwd-caddy`, carries the CURRENT `lwd.role=system`
   label, AND is `running`. An `lwd-caddy` from an older build (different/no
   label), a **stale exited** `lwd-caddy`, or any other service on host :80/:443
   makes `caddyRunning` return false → EnsureUp tries to create a fresh Caddy →
   Docker fails to bind :80 → `bind for 0.0.0.0:80 failed: port is already
   allocated` (or "name already in use"), and the daemon can't come up.
2. **Config sprawl.** Standing up a public dashboard currently means the operator
   hand-sets several env vars (`LWD_WEB_PASSWORD`, `LWD_WEB_ADDR`,
   `LWD_WEB_SECRET`, sometimes `LWD_DAEMON`/`LWD_API_TOKEN`) and edits
   `/etc/lwd/web.env`. Too much. A single box should need ONE command and zero
   interaction.

## Goals

- `EnsureUp` robustly adopts an existing `lwd-caddy` (any label/state), cleans up
  a stale one, and gives a CLEAR, actionable error when :80/:443 is genuinely
  held by something else — instead of the raw Docker bind error.
- `install.sh --public`: one command, zero interaction, stands up lwd as a
  public-facing service (daemon + dashboard) with all secrets auto-generated and
  the credentials printed. No env editing.

## Decisions

### 1. Robust Caddy startup (internal/router)
- Replace the label-only `caddyRunning` check with a by-NAME lookup
  (`findCaddy(ctx) (node.Container, bool)`) that lists ALL containers and finds
  the one named `lwd-caddy` regardless of label/state. In `EnsureUp`:
  - found + `running` → **adopt** it (skip create); proceed to admin-ready +
    Reload. (An old/unlabeled-but-running Caddy is reused, not duplicated.)
  - found + not running (exited/created) → **remove** it (stale) via
    `RemoveContainer`, then create fresh.
  - not found → create fresh (today's path).
- Wrap the create/start error: if it contains `port is already allocated` (or
  `address already in use`), return a clear message:
  `router: cannot start lwd's ingress proxy — host port 80 or 443 is already in
  use by another process/container. lwd needs 80 and 443 for HTTP/HTTPS ingress;
  stop the conflicting service (see: docker ps / ss -ltnp) and retry.` If it
  contains `name ... already in use`, remove the named container and retry once.
- Behavior when nothing conflicts is unchanged (create fresh, adopt a running
  lwd-caddy across daemon restarts exactly as today).

### 2. `install.sh --public` (zero-interaction public setup)
- `--public` implies: install (build+binaries), Docker if missing (like
  `--docker`), the daemon systemd unit (`--systemd`), and the lwd-web unit
  (`--web`) — but with REAL auto-generated secrets and STARTED (not the
  `CHANGE_ME` hold), bound publicly.
- Secret generation helper (`gen_secret`): prefer `openssl rand`, fall back to
  `head -c N /dev/urandom | base64`. Generate:
  - `LWD_WEB_PASSWORD` — a strong random password UNLESS `--web-password X` was
    given.
  - `LWD_WEB_SECRET` — 32 random bytes (persists sessions across restarts).
- `/etc/lwd/web.env` (0600) written with `LWD_WEB_PASSWORD`, `LWD_WEB_SECRET`,
  and `LWD_WEB_ADDR=0.0.0.0:8079`. **Never clobbered:** if it already exists, it
  is reused as-is (re-runs / updates don't rotate the password); the script says
  so and does not reprint a password it didn't generate.
- **No daemon-connection config needed:** both units run as root and are
  co-located, so lwd-web reaches the daemon over the local 0600 socket — NO
  `LWD_DAEMON`/`LWD_API_TOKEN`. That is the whole point: on one box the only
  "config" is the (auto-generated) web password.
- Start `lwd.service` then `lwd-web.service`.
- Final output prints, prominently: the dashboard URL(s)
  (`http://<primary-ip>:8079` — best-effort IP via `hostname -I`), and the
  generated password (ONLY when freshly generated), with a "save this" note and
  the plain-HTTP/TLS caveat.
- **Security posture (documented, honest):** `--public` serves the dashboard
  over plain HTTP on 0.0.0.0:8079 with a strong generated password. For a real
  internet-facing box, terminate TLS in front (the app-ingress Caddy already has
  80/443; a future `--domain` will front the dashboard with Let's Encrypt) or
  restrict 8079 to a VPN/tunnel. The script warns loudly. (TLS-fronting the
  dashboard via Caddy is a follow-up, not in this change.)

## Non-goals

- Auto-TLS for the dashboard (follow-up `--domain`). No RBAC. No change to the
  daemon's own security model (socket 0600; optional token-guarded TCP unchanged).

## Testing

- router: `findCaddy` (fake node) returns the lwd-caddy by name across states; a
  fake `node.Node` whose RunContainer returns a "port is already allocated" error
  → `EnsureUp` returns the friendly message; a stale (exited) lwd-caddy →
  EnsureUp removes it then creates; a running one → adopted (no create call). Use
  a fake node that records RunContainer/RemoveContainer calls.
- install.sh: `bash -n`; dry-run `--public` (stub asroot/systemctl/openssl,
  temp /etc): generates web.env with real (non-CHANGE_ME) values + 0.0.0.0 addr,
  starts both units, prints URL + password; a pre-existing web.env is reused
  (password not rotated/reprinted); `gen_secret` produces non-empty output via
  both openssl and the urandom fallback.
- Zero regression: default EnsureUp path (no conflict) unchanged; `install.sh`
  without `--public` unchanged.

## Docs

README: Quickstart gets a "one-command public setup" note
(`sudo ./install.sh --public`), and the troubleshooting section gets the
port-80/443-in-use guidance (lwd owns 80/443 for ingress; free them or stop the
conflicting service). Note the plain-HTTP caveat for `--public`.
