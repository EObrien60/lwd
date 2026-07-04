# lwd Phase 5 Implementation Plan — web UI (`lwd-web`)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax. The frontend task (Task 5) MUST use the frontend-design skill.

**Goal:** A genuinely nice, self-contained web dashboard for lwd — overview, live logs, deployment history + rollback, secrets, redeploy, config edit — as a separate `lwd-web` binary that is just another client of the daemon's unix-socket API (zero daemon changes).

**Architecture:** `cmd/lwd-web` + `internal/web`. A Go server serves embedded, buildless UI assets (hand-written CSS + vendored Alpine.js, no node/CDN) and an authenticated JSON+SSE API that proxies to the daemon via a `DaemonClient` interface (satisfied by `internal/client`, faked in tests). Single-password → HMAC-signed session cookie.

**Tech Stack:** Go 1.25+ (stdlib `net/http`, `crypto/hmac`, `crypto/sha256`, `embed`), `internal/client`, `internal/spec`; frontend = hand-written HTML/CSS + vendored Alpine.js.

## Global Constraints

- Go floor **1.25**; **no cgo** (CGO_ENABLED=0). Module `lwd`; imports `lwd/internal/<pkg>`.
- Design spec governs: `docs/superpowers/specs/2026-07-04-lwd-phase5-web-ui-design.md`.
- **Zero daemon changes** — `lwd-web` only consumes the existing daemon API via `internal/client`. Do NOT modify `internal/api`, `internal/reconciler`, the daemon, etc.
- **No node toolchain, no CDN, no external asset requests** — all CSS/JS embedded via `go:embed`; Alpine.js vendored as a local file. CSP-safe (no inline-script reliance beyond what Alpine needs; prefer external embedded files).
- **No secret value is ever returned** by any `lwd-web` endpoint (mirrors the daemon).
- Auth: constant-time password compare; `HttpOnly`, `SameSite=Lax` session cookie, `Secure` when TLS; HMAC-SHA256 signed with expiry.
- **README is a required deliverable** (Task 6), accuracy verified in the final review.
- Handlers testable via a fake `DaemonClient`; no Docker needed for the web layer's unit tests.

---

### Task 1: web config + auth (login / session cookie / middleware)

**Files:** Create `internal/web/config.go`, `internal/web/auth.go`, `internal/web/auth_test.go`.

**Interfaces:**
- `config.go`: `type Config struct { Addr, Password, SocketPath string; SigningKey []byte }`; `func LoadConfig() (Config, error)` reading `LWD_WEB_ADDR` (default `127.0.0.1:8079`), `LWD_WEB_PASSWORD` (required — error if empty), `LWD_WEB_SECRET` (if set, its bytes are the signing key; else generate 32 random bytes), socket via `config.SocketPath()` (reuse `internal/config`; allow `LWD_SOCKET` override if easy).
- `auth.go`:
  - `func signSession(key []byte, expiry time.Time) string` and `func verifySession(key []byte, cookie string) (ok bool)` — cookie value = `base64(expiryUnix) + "." + base64(hmacSHA256(key, expiryUnixBytes))`; verify recomputes the HMAC (constant-time compare) and checks `expiry > now`.
  - `func checkPassword(configured, provided string) bool` — `hmac.Equal`/`subtle.ConstantTimeCompare` on the raw bytes.
  - `type Authenticator struct { key []byte; password string; ttl time.Duration }` with `NewAuthenticator(key []byte, password string) *Authenticator`; methods `Login(w, r)` (parse form `password`; on match set signed cookie `lwd_session`, redirect `/`; on mismatch 401), `Logout(w, r)` (clear cookie), `Middleware(next http.Handler) http.Handler` (allow `/login`, `/logout`, and static assets; otherwise require a valid session cookie → else 401 for `/api/*`, redirect to `/login` for pages).

- [ ] **Step 1: Write failing tests** `auth_test.go`:
  - `TestSignVerifyRoundTrip`: sign with a future expiry → verify true; tamper a byte → false.
  - `TestVerifyRejectsExpired`: sign with a past expiry → verify false.
  - `TestCheckPasswordConstantTime`: correct → true, wrong → false, empty → false.
  - `TestLoadConfigRequiresPassword`: unset `LWD_WEB_PASSWORD` → error; set → ok with default addr.
  - `TestMiddlewareBlocksUnauthed`: a request to `/api/apps` with no cookie → 401; with a valid cookie → passes through (use httptest + a stub next handler).
  - `TestLoginSetsCookie`: POST `/login` with the right password → 303/302 + a `lwd_session` cookie that `verifySession` accepts.
- [ ] **Step 2: Run** `go test ./internal/web/ -v` → FAIL.
- [ ] **Step 3: Implement** config.go + auth.go (stdlib crypto; no external deps).
- [ ] **Step 4: Run** `go test ./internal/web/ -v` → PASS; `CGO_ENABLED=0 go build ./...`, `go vet`, `gofmt -l internal/web/` clean.
- [ ] **Step 5: Commit** `git add internal/web/ && git commit -m "feat: lwd-web config + password/session-cookie auth"`

---

### Task 2: DaemonClient interface + browser JSON API handlers

**Files:** Create `internal/web/server.go`, `internal/web/api.go`, `internal/web/fake_client_test.go`, `internal/web/api_test.go`.

**Interfaces:**
- `server.go`:
  - `type DaemonClient interface {`
      `Apps(ctx context.Context) ([]api.AppStatus, error);`
      `History(ctx context.Context, name string) ([]store.Deployment, error);`
      `Logs(ctx context.Context, name string, follow bool, w io.Writer) error;`
      `Apply(ctx context.Context, app *spec.App) (*store.Deployment, error);`
      `Rollback(ctx context.Context, name string) (*store.Deployment, error);`
      `Remove(ctx context.Context, name string) error;`
      `SetSecret(ctx context.Context, app, key, value string) error;`
      `ListSecrets(ctx context.Context, app string) ([]string, error);`
      `DeleteSecret(ctx context.Context, app, key string) error }`
    (Confirm `*client.Client` already has these exact method signatures — from Phases 1–4 it should. If a signature differs, adapt the interface to match the real client, do NOT change the client.)
  - `type Server struct { client DaemonClient; auth *Authenticator; ... }`; `func NewServer(c DaemonClient, a *Authenticator) *Server`; `func (s *Server) Handler() http.Handler` — mux with auth middleware wrapping `/api/*` and pages, static assets served (Task 5 fills assets; for now a placeholder route is fine but prefer wiring the embed in Task 5).
- `api.go`: handlers using `s.client`:
  - `GET /api/apps` → JSON `[]api.AppStatus`.
  - `GET /api/apps/{name}` → JSON `{status: AppStatus, history: []Deployment}` (status = the matching entry from Apps; history from History). Secret VALUES never included (Deployment has none).
  - `POST /api/apps/{name}/rollback` → Rollback → 200 JSON deployment or 500.
  - `POST /api/apps/{name}/redeploy` → History → take newest, unmarshal `.Spec` into `spec.App`, Apply → 200 or error (404 if no history).
  - `POST /api/apply` → read body as an lwd.toml text, `spec.Parse` → `Validate` → Apply → 200 or 400 (parse/validate) / 500.
  - `DELETE /api/apps/{name}` → Remove → 204.
  - `GET /api/apps/{name}/secrets` → ListSecrets → JSON []string. `POST` (form `key`,`value`) → SetSecret → 204 (400 if key empty). `DELETE /api/apps/{name}/secrets/{key}` → 204.
  - Consistent JSON error shape `{"error": "..."}`; daemon-unreachable → 502.

- [ ] **Step 1: Write the fake client + failing api tests.** `fake_client_test.go`: a `fakeDaemon` implementing `DaemonClient` with canned data + knobs. `api_test.go` (httptest against `Server.Handler()` with a pre-authed request — set a valid session cookie via the Authenticator, or test the handlers with auth disabled by injecting a valid cookie):
  - `TestApiApps`: returns the fake's apps as JSON.
  - `TestApiAppDetail`: merges status + history.
  - `TestApiApplyParsesToml`: POST a valid lwd.toml → Apply called with the parsed app; POST invalid toml → 400.
  - `TestApiApplyRejectsBadSpec`: a toml missing image/port → 400 (validate).
  - `TestApiRollback` / `TestApiRedeploy` / `TestApiDelete`: call the right client method; redeploy uses newest history spec.
  - `TestApiSecrets`: set → list shows the name, response never contains the value; delete removes.
  - `TestApiRequiresAuth`: no cookie → 401 for an `/api` route.
- [ ] **Step 2: Run** `go test ./internal/web/ -v` → FAIL.
- [ ] **Step 3: Implement** server.go + api.go.
- [ ] **Step 4: Run** `go test ./internal/web/ -v` → PASS; build/vet/gofmt clean.
- [ ] **Step 5: Commit** `git add internal/web/ && git commit -m "feat: lwd-web daemon client interface + browser JSON API"`

---

### Task 3: SSE live-logs endpoint

**Files:** Modify `internal/web/api.go` (or add `internal/web/logs.go`), `internal/web/logs_test.go`.

**Interfaces:** `GET /api/apps/{name}/logs` → SSE. The handler calls `s.client.Logs(ctx, name, true, w)` but adapts the raw log stream into SSE frames: wrap so each line becomes `data: <line>\n\n`, set `Content-Type: text/event-stream`, `Cache-Control: no-cache`, flush per frame; stop when the client disconnects (`r.Context().Done()`). Implement by passing an `io.Writer` to `Logs` that reframes lines to the ResponseWriter + flushes (a small `sseWriter` type that buffers to newline and writes `data:` frames).

- [ ] **Step 1: Write failing test** `logs_test.go`: fake client whose `Logs` writes a few `line\n` chunks to the writer; hit the SSE endpoint with httptest; assert the response body contains `data: line1\n\n` etc. and the content-type is `text/event-stream`.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** the SSE handler + `sseWriter`.
- [ ] **Step 4: Run** `go test ./internal/web/ -v` → PASS; build/vet/gofmt clean.
- [ ] **Step 5: Commit** `git add internal/web/ && git commit -m "feat: lwd-web SSE live-logs endpoint"`

---

### Task 4: cmd/lwd-web entrypoint + static-asset serving wiring

**Files:** Create `cmd/lwd-web/main.go`, `internal/web/assets.go` (the `go:embed` + static handler), `internal/web/assets/` placeholder files (real UI is Task 5).

**Interfaces:**
- `internal/web/assets.go`: `//go:embed assets/*` into an `embed.FS`; `func (s *Server) staticHandler() http.Handler` serving those files; route `/` → `index.html` (app shell), `/login` → `login.html`, `/static/*` → assets. (Create minimal placeholder `assets/index.html`, `assets/login.html`, `assets/app.css`, `assets/app.js` so the embed compiles and the server runs; Task 5 replaces them with the real crafted UI.)
- `cmd/lwd-web/main.go`: `LoadConfig`; build `client.New(cfg.SocketPath)`; `NewAuthenticator(cfg.SigningKey, cfg.Password)`; `NewServer(client, auth)`; `http.ListenAndServe(cfg.Addr, server.Handler())`. Clear startup log (addr, socket). Refuse to start (exit 1, clear message) if password unset. `version` sub-behavior optional.

- [ ] **Step 1: Write a small failing test** for the static wiring: `assets_test.go` — `GET /login` serves the login shell (200, contains a known placeholder marker); `GET /api/apps` still requires auth. (Auth + api already tested; this asserts the embed + routing.)
- [ ] **Step 2: Run** → FAIL (or build fail until embed files exist).
- [ ] **Step 3: Implement** assets.go + placeholder asset files + cmd/lwd-web/main.go.
- [ ] **Step 4: Verify** `CGO_ENABLED=0 go build -o /tmp/lwd-web ./cmd/lwd-web` succeeds; `go test ./internal/web/ -v` passes; `LWD_WEB_PASSWORD=x /tmp/lwd-web` starts and serves `/login` (kill it after a moment — or just assert build+unit tests, don't leave it running).
- [ ] **Step 5: Commit** `git add cmd/lwd-web/ internal/web/ && git commit -m "feat: lwd-web entrypoint + embedded static asset serving (placeholder UI)"`

---

### Task 5: the crafted UI (frontend-design) — REQUIRED SKILL

**Files:** Replace `internal/web/assets/{index.html,login.html,app.css,app.js}`; add vendored `internal/web/assets/alpine.min.js`.

**REQUIRED:** The implementer MUST use the **frontend-design skill** for this task — the goal is a genuinely nice, distinctive dashboard, not a generic admin template. Build against the JSON+SSE API from Tasks 2–3 and the auth from Task 1.

**Deliver these views** (design/craft per the skill; information design per the spec):
- **Login** (`login.html`): a clean, single-password screen; posts to `/login`.
- **App shell** (`index.html` + `app.js` + `app.css`): an Alpine-driven SPA-in-one-page:
  - **Overview**: grid of app cards — name, domain (link opens the live site), status pill (running/failed/retired), current image tag, health dot, last-deployed time. A prominent **Deploy** button → modal to paste an `lwd.toml` → `POST /api/apply`; inline error on 400. Light polling of `/api/apps` for freshness.
  - **App detail** (in-page drawer/route): header (name, domain link, status) + sections:
    - **Logs**: live SSE from `/api/apps/{name}/logs` with a follow toggle, monospaced, auto-scroll.
    - **Deployments**: history table (image, status, time) with per-row **Roll back** and a **Redeploy** action.
    - **Secrets**: list of names; add (name+value, value write-only, never shown back); delete.
    - **Config**: view/edit the current spec as `lwd.toml` and **Apply**.
    - **Danger**: delete app (confirm dialog).
- Cohesive design system (CSS custom properties), **light + dark**, responsive, keyboard-friendly, no external requests (Alpine vendored locally; no CDN). Handle loading/empty/error states gracefully (incl. a clear "cannot reach lwd daemon" banner on 502).

**Constraints:** no node build; hand-written CSS; Alpine.js vendored as `assets/alpine.min.js` (fetch the minified file content and commit it locally — no CDN link). Keep JS readable and organized in `app.js`.

- [ ] **Step 1: Invoke the frontend-design skill** and build the assets per the views above, wired to the real API/auth/SSE endpoints.
- [ ] **Step 2: Verify build + run**: `CGO_ENABLED=0 go build -o /tmp/lwd-web ./cmd/lwd-web`; `go test ./internal/web/ -v` still passes (update the placeholder-marker assertion from Task 4 to match the real login page if needed). Drive the running server (see the `run`/browser tooling) to confirm login works and the overview renders against a daemon (or a stub) — capture a screenshot if the tooling allows; at minimum confirm the pages load and the SSE/logs view connects. Do not leave a server running.
- [ ] **Step 3: Commit** `git add internal/web/ && git commit -m "feat: crafted lwd-web dashboard UI (overview, logs, history, secrets, config)"`

---

### Task 6: integration test + README + final wiring check

**Files:** Create `internal/web/integration_test.go` (or `test/web_e2e_test.go`); modify `README.md`.

- [ ] **Step 1: Integration test** — start `lwd-web`'s `Server.Handler()` (httptest) backed by a **real `internal/client`** pointed at a **real daemon `api.Server`** running on a temp unix socket with a **fake node** stack (no Docker needed — reuse the fake node/router/store/secrets wiring from existing tests). Then: `POST /login` (get cookie) → `GET /api/apps` (200, JSON) → `POST /api/apply` with a single-service lwd.toml → `GET /api/apps` shows it. This exercises lwd-web → client → daemon end to end without Docker.
- [ ] **Step 2: Run** `go test ./... -v` → all pass (this integration test runs without Docker; existing Docker-gated e2e still SKIPs).
- [ ] **Step 3: Verify full build**: `CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go build -o /tmp/lwd-web ./cmd/lwd-web && go vet ./... && gofmt -l .` all clean.
- [ ] **Step 4: README** — add an "## Web UI (lwd-web)" section: what it is (separate binary, dashboard), build (`go build ./cmd/lwd-web`), run (env `LWD_WEB_PASSWORD`, `LWD_WEB_ADDR`, locating the daemon socket), auth model, that it's a client of the daemon API (no daemon changes), and how to expose it safely (localhost + SSH tunnel, or front with Caddy). **Full pass to confirm README reflects Phases 1–5.**
- [ ] **Step 5: Commit** `git add -A && git commit -m "test: lwd-web integration (web->client->daemon); docs: web UI README"`

---

## Self-Review

**Spec coverage:** config+auth (T1); DaemonClient + JSON API incl. apply-from-toml/rollback/redeploy/delete/secrets (T2); SSE logs (T3); entrypoint + embedded assets (T4); crafted UI via frontend-design (T5); integration test + README (T6). ✓

**Zero daemon changes:** every task stays in `cmd/lwd-web` + `internal/web`, consuming `internal/client`/`internal/spec`/`internal/api` types read-only. No edits to daemon/reconciler/api/router/node/store/secrets/compose.

**Deferred (by design):** deploy-from-git in UI, multi-user, full compose-create-from-UI; Phase 6 (lwd.toml skill), Phase 7 (MCP).

**README:** T6 makes it explicit + a Phases 1–5 accuracy pass; final review checks it.

**Placeholder scan:** auth/api/SSE logic have concrete code+tests; the frontend (T5) is intentionally spec+skill-driven (design craft can't be pre-written as verbatim code, but the views, endpoints, and constraints are precise). No TBD in the logic tasks.

**Type consistency:** `DaemonClient` mirrors `*client.Client`'s real signatures (T2 verifies before defining). Handlers use `api.AppStatus`, `store.Deployment`, `spec.App`/`spec.Parse` exactly as the daemon defines them. Auth cookie name `lwd_session` consistent across auth + tests.

**Cross-task note:** T4's placeholder assets let the binary build before T5's real UI; T5 updates any placeholder-marker test assertion. All tasks keep `go test ./...` green without Docker.
