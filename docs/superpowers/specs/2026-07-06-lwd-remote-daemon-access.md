# lwd — remote daemon access + lwd-web daemonization + public bind

**Status:** Design (decisions resolved; user-reported operational gap)
**Date:** 2026-07-06
**Type:** Focused feature/fix (not a roadmap phase). Builds on merged P1–P12.

## Problem (reported)

lwd-web (and the CLI/MCP) reach the daemon **only over a 0600 unix socket**
(`client.New` always `DialContext("unix", …)`; the daemon listens only on
`$LWD_DATA_DIR/lwd.sock`). So lwd-web can talk to the daemon only when it's on
the same host AND can open the root-owned socket. When lwd-web is reached over
an SSH tunnel / runs as a non-root user / would run on another host, it reports
"can't connect to the daemon." There is no network path to the daemon at all.
Separately, operators want to (a) run lwd-web as a persistent daemon and (b)
expose it beyond localhost.

## Goals

1. The daemon can OPTIONALLY listen on TCP so lwd-web (and CLI/MCP) can reach it
   over the network — **without** exposing an unauthenticated control plane.
2. lwd-web can bind a public interface (already works — confirm + document +
   the TLS caveat).
3. lwd-web can run as a managed daemon (systemd unit via the installer).
4. Zero regression: the default (no TCP env set) is exactly today — unix socket
   only, no auth, local CLI unchanged.

## Decisions

1. **Daemon keeps the unix socket ALWAYS** (0600, local CLI, no auth — the
   socket's file perms are its boundary, unchanged).
2. **Optional TCP listener** via `LWD_ADDR` (e.g. `127.0.0.1:8077`, `:8077`,
   `10.0.0.5:8077`). When set, the daemon serves the SAME API handler on that
   TCP address too (concurrently with the socket), under the graceful-shutdown
   path.
3. **TCP auth via `LWD_API_TOKEN`** (bearer, constant-time compare — reuse the
   `crypto/subtle` pattern from `internal/agent`/`internal/web`). The TCP
   listener requires `Authorization: Bearer <token>` when a token is set; the
   **unix socket never requires it** (two `http.Server`s: socket = bare handler,
   TCP = token-wrapped handler).
4. **Fail-closed on public bind:** if `LWD_ADDR` resolves to a **non-loopback**
   interface (anything other than `127.0.0.1`/`::1`/`localhost`, including `:port`
   and `0.0.0.0`) and `LWD_API_TOKEN` is empty → the daemon **refuses to start**
   with a clear error. A loopback `LWD_ADDR` may run token-less (for SSH tunnels).
   Rationale: the daemon API is full, root-equivalent control; it must never be
   exposed unauthenticated on the network by accident.
5. **Client learns TCP.** `client.New(socketPath)` (unix) is unchanged
   (back-compat). Add `client.NewHTTP(baseURL, token string) *Client` (TCP; sends
   the bearer header on every request via a wrapping RoundTripper). Add
   `client.FromEnv() *Client` — the single resolver used by the CLI, lwd-mcp, and
   lwd-web:
   - `LWD_DAEMON` set → TCP: accept `host:port` or `http://host:port`
     (normalize to a base URL), token from `LWD_API_TOKEN`.
   - else → unix: `LWD_SOCKET` or `config.SocketPath()` (today's behavior).
6. **lwd-web wiring:** `cmd/lwd-web` uses `client.FromEnv()` (so
   `LWD_DAEMON`/`LWD_API_TOKEN`/`LWD_SOCKET` all work); `web.Config` keeps the
   web-server knobs (`LWD_WEB_ADDR`/`LWD_WEB_PASSWORD`/`LWD_WEB_SECRET`) and drops
   its own socket resolution. lwd-web's own listen addr already honors
   `LWD_WEB_ADDR` (set `0.0.0.0:8079` for public) — no code change, but log a
   one-line warning when bound to a non-loopback addr that it serves plain HTTP
   (front with TLS/Caddy or tunnel; the session cookie is Secure only behind an
   `X-Forwarded-Proto: https` proxy).
7. **Daemonize lwd-web:** the installer gains an `lwd-web.service` (and
   `lwd-agent.service`) systemd unit reading an `EnvironmentFile`
   (`/etc/lwd/web.env`, `/etc/lwd/agent.env`) the operator fills in (at minimum
   `LWD_WEB_PASSWORD`). New `install.sh` flags `--web` / `--agent`.

## Non-goals / documented posture

- We do NOT add per-user/RBAC auth to the daemon — a single shared
  `LWD_API_TOKEN` is the network boundary (like the agent's token). lwd-web's own
  password gates human access; the daemon token gates lwd-web→daemon (and any
  direct TCP client).
- We do NOT auto-manage DNS or TLS for lwd-web. Public exposure = put it behind
  Caddy/TLS or tunnel it (documented).
- "Dogfooding lwd-web under lwd" becomes *possible* with the TCP endpoint (deploy
  lwd-web as a surface that sets `LWD_DAEMON`), but systemd is the recommended
  daemonization path (avoids the restart/bootstrap ordering paradox); documented
  as a note, not a supported first-class path.

## Security summary

- Unix socket: unchanged, 0600, local, no token.
- TCP listener: opt-in; token required unless loopback; fail-closed on
  public-bind-without-token. Constant-time token compare. Recommended topology:
  bind `LWD_ADDR` to loopback or a private/WireGuard-mesh address; put public
  lwd-web (password-protected, behind TLS) in front; or SSH-tunnel.
- No secret value is exposed by any of this.

## Testing (fakes/no Docker)

- config: `APIAddr()`/`APIToken()` read env; loopback-detection helper
  (`127.0.0.1`/`::1`/`localhost` → loopback; `:8077`/`0.0.0.0:x`/`10.x` → not).
- daemon start guard: non-loopback `LWD_ADDR` + empty token → startup error;
  loopback + empty → ok; any addr + token → ok. (Unit-test the guard function; a
  full daemon boot is integration-level.)
- token middleware: TCP request without/with wrong token → 401; correct → passes;
  the socket server never checks the token.
- client: `NewHTTP` sends the bearer header (httptest server asserts it); wrong
  token → the server 401s and the client surfaces the error; `New`(unix)
  unchanged. `FromEnv` picks TCP when `LWD_DAEMON` set (host:port + http:// forms),
  unix otherwise; honors `LWD_SOCKET`.
- lwd-web: `client.FromEnv` wiring compiles + a fake-daemon test still passes;
  the non-loopback-bind warning fires (log capture) without blocking startup.
- install.sh: `bash -n`; the web/agent unit + env-file templates render with the
  right paths; `--web`/`--agent` parse.
- **Zero regression:** default (no LWD_ADDR/LWD_DAEMON) → unix socket only, no
  auth; existing CLI/web/mcp tests green; `go test ./...` passes without Docker.

## Docs

README: a "Remote access & running lwd-web as a service" section — public
lwd-web bind (`LWD_WEB_ADDR=0.0.0.0:8079` + TLS/tunnel caveat), the daemon TCP
endpoint (`LWD_ADDR` + `LWD_API_TOKEN`, loopback-vs-token rule), connecting a
remote/tunneled lwd-web or CLI (`LWD_DAEMON`/`LWD_API_TOKEN`), and the
systemd units. Update the Configuration reference table with the new env vars.
