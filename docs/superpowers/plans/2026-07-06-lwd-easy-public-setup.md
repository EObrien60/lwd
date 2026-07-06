# lwd easy public setup — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`).

**Goal:** (1) Robustly start/adopt the managed Caddy so `bind … :80 … already allocated` / stale-Caddy no longer wedge the daemon, with a clear error on a genuine conflict; (2) `install.sh --public` — one command, zero interaction, auto-generated secrets, public dashboard + daemon via systemd.

**Tech:** Go 1.25+, no cgo; `internal/router`, `install.sh`, `README.md`. Spec: `docs/superpowers/specs/2026-07-06-lwd-easy-public-setup.md`.

## Global Constraints
- Zero regression: no-conflict EnsureUp path unchanged; `install.sh` without `--public` unchanged. Tests fakes/no Docker.
- `--public` needs NO daemon-connection env (co-located → unix socket); only the auto-generated web password. Never clobber an existing `/etc/lwd/web.env`.

---

### Task 1: robust Caddy adopt/cleanup + clear port-conflict error (internal/router)

**Files:** Modify `internal/router/router.go` (+ `internal/router/router_test.go`); `internal/node/fake.go` if the fake needs a RunContainer error hook (check — it has `RunErr`).

**What:**
- Add `func (c *CaddyRouter) findCaddy(ctx) (node.Container, bool, error)` — `ListContainers(ctx, nil)` (no label filter → all), return the container whose `Name == caddyContainerName` (any state/label), else not-found.
- Rewrite the top of `EnsureUp` (after EnsureNetwork): `ct, found, err := c.findCaddy(ctx)`:
  - `found && ct.State == "running"` → adopt: skip create, go to `waitForAdminReady` + `Reload`.
  - `found && ct.State != "running"` → `c.node.RemoveContainer(ctx, ct.ID)` (stale; log), then create fresh.
  - `!found` → create fresh.
- Wrap the create error: after `RunContainer(...)` err, if `strings.Contains(err.Error(), "already allocated") || strings.Contains(err.Error(), "address already in use")` → return `fmt.Errorf("router: cannot start lwd's ingress proxy — host port 80 or 443 is already in use by another process/container. lwd needs 80 and 443 for HTTP/HTTPS ingress; stop the conflicting service (docker ps / ss -ltnp) and retry: %w", err)`. If `strings.Contains(err.Error(), "already in use")` (name) → `RemoveContainer` the found/named one and retry the create once; if still failing, return wrapped.
- Keep `caddyRunning` if used elsewhere (grep) — or replace its callers with findCaddy. Keep the bootstrap-Caddyfile create block exactly as today for the create path.

- [ ] **Step 1: failing tests** (fake node recording calls; `node.Fake` supports `RunErr`/`Calls`/seeded containers): `TestEnsureUpAdoptsRunningCaddy` (a running lwd-caddy present → NO RunContainer call; Reload happens); `TestEnsureUpRemovesStaleCaddy` (an exited lwd-caddy → RemoveContainer called then a create); `TestEnsureUpPortConflictFriendlyError` (fake RunContainer returns `errors.New("...bind for 0.0.0.0:80 failed: port is already allocated")` and no existing caddy → EnsureUp returns an error containing "host port 80 or 443 is already in use"). Use the existing router test fakes; ensure the fake node can (a) be seeded with a container named lwd-caddy in a given State and (b) return a RunContainer error.
- [ ] **Step 2:** `go test ./internal/router/ -run 'EnsureUp' -v` → FAIL.
- [ ] **Step 3:** implement findCaddy + the EnsureUp rewrite + error wrapping (+ any node.Fake hook needed for seeding a named container / RunErr).
- [ ] **Step 4:** `go test ./... -race` PASS (existing router/reconciler tests unchanged — adopt-a-running-caddy matches today's reuse); build/vet/gofmt clean.
- [ ] **Step 5:** commit `fix: EnsureUp adopts/cleans lwd-caddy by name + clear :80/:443-in-use error`.

---

### Task 2: `install.sh --public` (zero-interaction) + README

**Files:** Modify `install.sh`, `README.md`.

**What (install.sh):**
- Flag `--public` → `PUBLIC=1` (default 0); optional `--web-password X` → `WEB_PASSWORD` var. Document both in the `--help` header.
- `gen_secret()` helper: `if command -v openssl; then openssl rand -base64 "$1"; else head -c "$1" /dev/urandom | base64 | tr -d '\n'; fi` (arg = bytes; strip newlines/`/+=` if you want URL-safe — a base64 password is fine, just trim trailing newline).
- In `main`, if `PUBLIC=1`: set `INSTALL_SYSTEMD=1`, `INSTALL_WEB=1`, and `INSTALL_DOCKER=1` only if `! command -v docker` (auto-install docker when missing — zero interaction). (Compose with existing flags.)
- Refactor `maybe_web` so that when `PUBLIC=1` (or `--web-password` given), CREATING `/etc/lwd/web.env` writes REAL values:
  `LWD_WEB_PASSWORD=<WEB_PASSWORD or gen_secret 18>`, `LWD_WEB_SECRET=<gen_secret 32>`, `LWD_WEB_ADDR=0.0.0.0:8079`, plus the same commented LWD_DAEMON/LWD_API_TOKEN hints. Capture the generated password in a script var (`GENERATED_WEB_PW`) for the summary. If web.env EXISTS → reuse (do NOT clobber, do NOT set GENERATED_WEB_PW). The CHANGE_ME start-guard still applies, but a real password means it STARTS.
- `next_steps`: when `PUBLIC=1`, print a PUBLIC summary: the dashboard URL(s) — best-effort primary IP via `ip="$(hostname -I 2>/dev/null | awk '{print $1}')"; [ -n "$ip" ] && echo "http://$ip:8079"` plus `http://<this-host>:8079` — and, if `GENERATED_WEB_PW` is set, print it prominently ("Dashboard password (SAVE THIS): <pw>"); if web.env was reused, say "using the existing password in /etc/lwd/web.env". Include the plain-HTTP warning (put behind TLS / VPN / tunnel for real public use) and that ports 80/443 must be free for app ingress.
- Keep non-`--public` behavior identical (maybe_web still writes CHANGE_ME + doesn't start when only `--web` is passed without `--public`/`--web-password`).

**README:**
- Quickstart: add a "One-command public setup" callout: `sudo ./install.sh --public` → installs, generates secrets, starts the daemon + dashboard on `0.0.0.0:8079`, prints the URL + password. Note plain-HTTP caveat + `--web-password` to choose your own.
- Troubleshooting: add ":80/:443 already in use" — lwd's Caddy owns host 80+443 for ingress; free them / stop the conflicting service; the daemon now adopts an existing lwd-caddy and errors clearly otherwise.

- [ ] **Step 1:** implement `--public`/`--web-password`/`gen_secret`, the `maybe_web` real-secret path + `GENERATED_WEB_PW`, the public `next_steps`, and the README edits.
- [ ] **Step 2: validate** — `bash -n install.sh`; `bash install.sh --help` shows `--public`/`--web-password`, no spill; sandbox dry-run `--public` (stub `asroot`/`systemctl`/`openssl` OR let openssl run; temp `/etc`): assert web.env has a real `LWD_WEB_PASSWORD=` (not CHANGE_ME) + `LWD_WEB_ADDR=0.0.0.0:8079`, both units "started", and the summary prints a URL + the password; a SECOND dry-run with the env pre-existing reuses it (no new password, no clobber). Prove non-`--public` is unchanged (`--web` alone still writes CHANGE_ME + doesn't start).
- [ ] **Step 3:** `go build ./...` (no Go change, but confirm) + `bash -n`; commit `feat(install): --public one-command zero-interaction public setup (auto secrets, systemd, printed creds)`.

---

## Self-Review
- Coverage: port/stale-caddy robustness + friendly error (T1); one-command public setup (T2). ✓
- Zero regression: EnsureUp no-conflict path unchanged (adopt-running == today's reuse); `install.sh` sans `--public` unchanged; web.env never clobbered. ✓
- Security honesty: `--public` = plain HTTP + strong generated password + loud TLS caveat; daemon security model untouched. ✓
- Cross-task: T1 needs `node.Fake` to seed a named container in a State + return a RunContainer error — check the fake has/needs these hooks. T2 reuses T1 only at runtime (independent code).
