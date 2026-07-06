# lwd remote daemon access — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`).

**Goal:** Optional token-guarded TCP listener on the daemon + a TCP-capable client so a remote/tunneled/non-root lwd-web (or CLI/MCP) can reach the daemon; plus systemd units to run lwd-web as a service. Default (no new env) = today: unix socket only, no auth.

**Architecture:** Two `http.Server`s in `runDaemon` — the unix socket (bare handler, unchanged) and, when `LWD_ADDR` is set, a TCP listener serving the same handler wrapped in bearer-token auth. The client gains an HTTP mode + a `FromEnv` resolver used by CLI/MCP/web.

**Tech Stack:** Go 1.25+, no cgo; `internal/{config,api,cli,client,web}`, `cmd/lwd-web`, `cmd/lwd-mcp`, `install.sh`.

## Global Constraints

- Go 1.25+; no cgo. Spec: `docs/superpowers/specs/2026-07-06-lwd-remote-daemon-access.md`.
- **Zero regression:** with no `LWD_ADDR`/`LWD_DAEMON` set, behavior is byte-for-byte today (unix socket only, no token, local CLI unchanged). Existing tests pass.
- Daemon TCP listener is **opt-in** (`LWD_ADDR`). Token = `LWD_API_TOKEN`, constant-time compare (reuse the `crypto/subtle` pattern in `internal/agent/auth.go`/`internal/web/auth.go`). Unix socket NEVER requires the token.
- **Fail-closed:** non-loopback `LWD_ADDR` + empty `LWD_API_TOKEN` → daemon refuses to start. Loopback (`127.0.0.1`/`::1`/`localhost`) may run token-less.
- Client: `New(socket)` unchanged; add `NewHTTP(baseURL, token)`; add `FromEnv()`.
- No secret value exposed. README + config table updated (final task).

---

### Task 1: daemon TCP listener + token middleware + config + fail-closed guard

**Files:** Modify `internal/config/config.go` (+test), `internal/cli/cli.go` (`runDaemon`), Create `internal/cli/apiauth.go` (or add to cli) + a small test.

**Interfaces produced:**
- `config.APIAddr() string` (env `LWD_ADDR`, default ""), `config.APIToken() string` (env `LWD_API_TOKEN`, default "").
- `func isLoopbackAddr(addr string) bool` (exported from cli or a helper): true iff the host part is `127.0.0.1`, `::1`, `localhost`, or empty-host loopback; `:8077`, `0.0.0.0:x`, and any routable IP/host → false. (Parse with `net.SplitHostPort`; empty/`""` host in `:port` → NOT loopback — treat a bare `:port`/`0.0.0.0` as public.)
- `func bearerMiddleware(token string, h http.Handler) http.Handler` — if `token=="" ` returns `h` unchanged; else requires `Authorization: Bearer <token>` (constant-time via `subtle.ConstantTimeCompare`), 401 + a JSON `{"error":...}` otherwise. (Model on `internal/agent/auth.go`.)
- `runDaemon` change: after building `httpSrv` for the socket, if `config.APIAddr() != ""`:
  - guard: `if !isLoopbackAddr(addr) && config.APIToken() == "" { print error "LWD_ADDR binds a non-loopback interface but LWD_API_TOKEN is unset — refusing to expose an unauthenticated control plane"; return 1 }`.
  - `tcpLn, err := net.Listen("tcp", addr)` (error → return 1).
  - `tcpSrv := &http.Server{Handler: bearerMiddleware(config.APIToken(), srv.Handler())}`; `go func(){ serveErr <- tcpSrv.Serve(tcpLn) }()` (share the serveErr channel or a second one); include `tcpSrv.Shutdown(sctx)` in the graceful-shutdown branch. Log `lwd daemon also listening on tcp <addr> (auth: <on|off>)`.

- [ ] **Step 1: failing tests** — config: `TestAPIAddrEnv`/`TestAPITokenEnv`. cli: `TestIsLoopbackAddr` (`127.0.0.1:8077`→true, `localhost:1`→true, `[::1]:8077`→true, `:8077`→false, `0.0.0.0:8077`→false, `10.0.0.5:8077`→false), `TestBearerMiddleware` (no token→passthrough; token set + missing/wrong header→401; correct→200 via a stub handler), and `TestAPIListenGuard` (a pure guard function `apiListenAllowed(addr, token) error`: non-loopback+empty→error; loopback+empty→nil; any+token→nil) — factor the guard into a testable func called by runDaemon.
- [ ] **Step 2:** `go test ./internal/config/ ./internal/cli/ -run 'API|Loopback|Bearer' -v` → FAIL.
- [ ] **Step 3:** implement config accessors, `isLoopbackAddr`, `bearerMiddleware`, `apiListenAllowed`, and the `runDaemon` dual-listen wiring.
- [ ] **Step 4:** `go test ./...` PASS; `CGO_ENABLED=0 go build ./...`; vet/gofmt clean. (Daemon-boot itself is integration; the pieces are unit-tested.)
- [ ] **Step 5:** commit `feat: daemon optional TCP listener (LWD_ADDR) + bearer auth (LWD_API_TOKEN), fail-closed on public bind`.

---

### Task 2: client TCP + token support + `FromEnv` resolver

**Files:** Modify `internal/client/client.go` (+test).

**Interfaces produced (consumed by T3):**
- Refactor `Client` to hold `http *http.Client`, `base string`, and inject the token via a wrapping `http.RoundTripper` (so no per-method change): 
  - `New(socketPath string) *Client` — UNCHANGED public behavior: unix dialer, `base="http://lwd"`, no token.
  - `NewHTTP(baseURL, token string) *Client` — `base` = normalized baseURL (accept `host:port` → `http://host:port`; pass `http(s)://…` through), default `http.Transport`, and if `token!=""` a RoundTripper that sets `Authorization: Bearer <token>` on each request. Trim trailing slash.
  - `url(path)` uses `c.base`.
  - `FromEnv() *Client`: if `os.Getenv("LWD_DAEMON") != ""` → `NewHTTP(daemon, os.Getenv("LWD_API_TOKEN"))`; else `New(LWD_SOCKET or config.SocketPath())`. (client may import internal/config — verify no cycle; config imports nothing of client, so fine.)
- Keep all existing methods calling `c.http.Do(...)`; the token/base changes are transparent.

- [ ] **Step 1: failing tests** — `TestNewHTTPSendsBearer` (httptest server asserts the `Authorization: Bearer tok` header on a request via a `NewHTTP(srv.URL,"tok")` client method call, e.g. `Apps`); `TestNewHTTPNoToken` (no header when token=""); `TestNewHTTPNormalizesHostPort` (`NewHTTP("h:8077","")` → requests go to `http://h:8077/...`); `TestFromEnvPicksTCPvsUnix` (`t.Setenv LWD_DAEMON=127.0.0.1:8077` → HTTP base; unset → unix, base "http://lwd"). Use an httptest server + a real client method (`Apps`) to exercise the header, or expose a tiny test seam.
- [ ] **Step 2:** `go test ./internal/client/ -run 'HTTP|FromEnv|Bearer' -v` → FAIL.
- [ ] **Step 3:** implement the refactor + `NewHTTP` + `FromEnv`.
- [ ] **Step 4:** `go test ./...` PASS (existing client tests unchanged); build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: client TCP+bearer support (NewHTTP) + FromEnv resolver`.

---

### Task 3: wire CLI / lwd-mcp / lwd-web to `FromEnv` + public-bind warning

**Files:** Modify `internal/cli/cli.go` (`newClient`), `cmd/lwd-mcp/main.go`, `cmd/lwd-web/main.go`, `internal/web/config.go` (+ its test + `internal/web/*_test.go` if they build Config), `internal/web/server.go` if needed.

**Interfaces produced:**
- cli `newClient()` → `client.FromEnv()`.
- lwd-mcp main → `client.FromEnv()` (drop the direct `config.SocketPath()` construction).
- lwd-web: `cmd/lwd-web/main.go` builds its daemon client via `client.FromEnv()` instead of `client.New(cfg.SocketPath)`; `web.Config` DROPS `SocketPath` (and its `LWD_SOCKET` resolution moves entirely into `client.FromEnv`), keeping `Addr`/`Password`/`SigningKey`. Update `web.LoadConfig` + any test that referenced `cfg.SocketPath`.
- lwd-web public-bind warning: in `cmd/lwd-web/main.go`, if `cfg.Addr`'s host is non-loopback (reuse a small check — either export `isLoopbackAddr` from a shared spot or inline), log a one-line warning: `lwd-web: listening on a non-loopback address over plain HTTP — put it behind TLS (Caddy) or an SSH tunnel; the session cookie is Secure only behind an X-Forwarded-Proto: https proxy`. Do NOT block startup.

- [ ] **Step 1: failing/adjusted tests** — `internal/web` tests that construct `Config` with `SocketPath` must be updated (compile). Add `TestLoadConfigNoSocketField` (LoadConfig returns Addr/Password/key, no socket). A cli/mcp compile check. (The client resolution itself is tested in T2.)
- [ ] **Step 2:** `go build ./...` → FAIL (SocketPath removed) until wired.
- [ ] **Step 3:** implement the FromEnv wiring across cli/mcp/web + drop web.Config.SocketPath + the public-bind warning.
- [ ] **Step 4:** `go test ./...` PASS; `CGO_ENABLED=0 go build ./...` + all four binaries; vet/gofmt clean.
- [ ] **Step 5:** commit `feat: cli/mcp/web use client.FromEnv (LWD_DAEMON/LWD_API_TOKEN); lwd-web public-bind warning`.

---

### Task 4: install.sh — lwd-web + lwd-agent systemd units + env files

**Files:** Modify `install.sh`.

- [ ] **Step 1:** add flags `--web` and `--agent`. Add functions `maybe_web()` / `maybe_agent()` (guarded by systemctl present, like `maybe_systemd`).
- [ ] **Step 2:** `--web`: create `/etc/lwd/web.env` **only if absent** (skeleton, 0600, root):
  ```
  # lwd-web configuration. Set a strong password. This file is 0600.
  LWD_WEB_PASSWORD=CHANGE_ME
  LWD_WEB_ADDR=127.0.0.1:8079     # set 0.0.0.0:8079 to expose (put behind TLS/tunnel!)
  # LWD_WEB_SECRET=<32+ random bytes to persist sessions across restarts>
  # Remote daemon (if lwd-web is not co-located with the daemon socket):
  # LWD_DAEMON=127.0.0.1:8077
  # LWD_API_TOKEN=<must match the daemon's LWD_API_TOKEN>
  ```
  Install `/etc/systemd/system/lwd-web.service`: `EnvironmentFile=/etc/lwd/web.env`, `ExecStart=$PREFIX/lwd-web`, `After=lwd.service network-online.target`, `Restart=on-failure`, `WantedBy=multi-user.target`. `daemon-reload`; `systemctl enable lwd-web` (do NOT auto-start if the password is still `CHANGE_ME` — print a notice to edit `/etc/lwd/web.env` then `systemctl start lwd-web`). Detect the placeholder and skip `start` with a clear message.
- [ ] **Step 3:** `--agent`: `/etc/lwd/agent.env` skeleton (`LWD_AGENT_TOKEN=CHANGE_ME`, `LWD_AGENT_ADDR=:8078`) + `/etc/systemd/system/lwd-agent.service` (`ExecStart=$PREFIX/lwd-agent`, `EnvironmentFile=/etc/lwd/agent.env`, `After=docker.service`, `Requires=docker.service`, `Restart=on-failure`). Same enable-but-don't-start-on-placeholder behavior.
- [ ] **Step 4:** update `next_steps()` to mention `--web`/`--agent` and the env files; extend `--help` (the header comment) with the two flags.
- [ ] **Step 5:** `bash -n install.sh`; render-check the units/env (echo them in a dry harness or read them back); commit `feat(install): --web/--agent systemd units + /etc/lwd env files`.

---

### Task 5: docs (README) + config reference + whole-branch verification

**Files:** Modify `README.md`.

- [ ] **Step 1:** add a **"Remote access & running lwd-web as a service"** section under/after the Web UI section: (a) public bind `LWD_WEB_ADDR=0.0.0.0:8079` + the plain-HTTP/TLS/tunnel caveat; (b) the daemon TCP endpoint — `LWD_ADDR` + `LWD_API_TOKEN`, the loopback-token rule + fail-closed, recommended topology (loopback/mesh + tunnel or private); (c) connecting a remote/tunneled lwd-web or CLI via `LWD_DAEMON` + `LWD_API_TOKEN`; (d) the `install.sh --web`/`--agent` systemd units + `/etc/lwd/*.env`; (e) a short SSH-tunnel example (`ssh -L 8079:127.0.0.1:8079 server` → open localhost:8079). (f) a one-line note that dogfooding lwd-web under lwd is possible via `LWD_DAEMON` but systemd is recommended.
- [ ] **Step 2:** update the **Configuration reference** table with `LWD_ADDR`, `LWD_API_TOKEN`, `LWD_DAEMON` (and confirm `LWD_WEB_ADDR` public note). Ensure every new env matches the code.
- [ ] **Step 3:** `go test ./... -race` green; `CGO_ENABLED=0 go build ./...` + four binaries; vet/gofmt clean; `bash -n install.sh`.
- [ ] **Step 4:** commit `docs: remote daemon access + lwd-web service + config reference`.

---

## Self-Review

**Spec coverage:** daemon TCP+token+fail-closed (T1); client TCP+FromEnv (T2); cli/mcp/web wiring + public-bind warning (T3); systemd units for web/agent (T4); docs + config table (T5). ✓
**Zero regression:** default no-env path = unix socket only, no auth (T1 guard only triggers when LWD_ADDR set; client default = unix); existing tests pass. ✓
**Security:** fail-closed on public-bind-without-token (T1); constant-time token; socket never token-gated; public lwd-web warned + documented behind TLS/tunnel. ✓
**Type consistency:** `config.APIAddr/APIToken` + `isLoopbackAddr` + `apiListenAllowed` + `bearerMiddleware` (T1) used by runDaemon; `client.NewHTTP`/`FromEnv` (T2) used by cli/mcp/web (T3); `web.Config` loses `SocketPath` (T3) — all its readers updated. ✓
**Cross-task risk:** T3 removes `web.Config.SocketPath` → any test/reader referencing it must update (compile-driven). `client.FromEnv` importing `internal/config` — verify no import cycle (config is leaf; fine).
