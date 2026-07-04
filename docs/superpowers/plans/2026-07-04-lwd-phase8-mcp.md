# lwd Phase 8 Implementation Plan — local MCP server (`lwd-mcp`)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`). Server/tool tasks SHOULD use the mcp-builder skill.

**Goal:** A Go MCP server (`lwd-mcp`, stdio) exposing lwd deploy ops as tools, as a client of the daemon's unix socket. Zero daemon changes.

**Architecture:** `cmd/lwd-mcp` + `internal/mcp`. Uses the official `modelcontextprotocol/go-sdk` over stdio. A `ClientIface` (subset of `internal/client`) is faked in tests and satisfied by `*client.Client`. Tools map 1:1 to client methods; `lwd_apply`/`lwd_deploy_git` build a `spec.App`.

**Tech Stack:** Go 1.25+, `github.com/modelcontextprotocol/go-sdk` (mcp binary only), `internal/client`, `internal/spec`, `internal/config`.

## Global Constraints

- Go 1.25+; no cgo. Module `lwd`. Design spec governs: `docs/superpowers/specs/2026-07-04-lwd-phase8-mcp-design.md`.
- **Zero daemon changes** — `lwd-mcp` only consumes `internal/client` + read-only types (`api.AppStatus`, `store.Deployment`, `spec.App/Parse/Load/Validate`). Do NOT modify the daemon/reconciler/api/etc.
- **stdio transport, no auth** (local; daemon socket is `0600`). No network listener.
- **No tool returns a secret VALUE** (mirror the daemon/web): `lwd_secret_list` = names only; `lwd_secret_set` takes a value in but nothing returns it.
- Read tools annotated `readOnlyHint: true`; `lwd_remove` annotated `destructiveHint: true`. Rely on the MCP host's per-call approval (no custom confirm arg).
- Handlers testable via a fake `ClientIface`; the MCP binary only builds/needs the SDK.
- **README is a required deliverable** (Task 5), verified in the final review.

---

### Task 1: MCP scaffold — ClientIface + fake + server skeleton (stdio) + entrypoint

**Files:** Create `internal/mcp/server.go`, `internal/mcp/fake_client_test.go`, `internal/mcp/server_test.go`, `cmd/lwd-mcp/main.go`.

**REQUIRED:** use the **mcp-builder** skill for the server setup + tool-registration idioms of `github.com/modelcontextprotocol/go-sdk`.

**Interfaces:**
- `type ClientIface interface { Apps(ctx) ([]api.AppStatus, error); History(ctx, name string) ([]store.Deployment, error); Logs(ctx, name string, follow bool, w io.Writer) error; Apply(ctx, *spec.App) (*store.Deployment, error); Rollback(ctx, name string) (*store.Deployment, error); Remove(ctx, name string) error; SetSecret(ctx, app,key,value string) error; ListSecrets(ctx, app string) ([]string, error); DeleteSecret(ctx, app,key string) error }` — signatures MUST match `*client.Client`; add `var _ ClientIface = (*client.Client)(nil)` (import lwd/internal/client). (If a signature differs, match the client, don't change it.)
- `type Server struct { client ClientIface }`; `func NewServer(c ClientIface) *Server`; `func (s *Server) MCP() *mcp.Server` (or equivalent) that constructs the go-sdk server and registers all tools (Tasks 2–4 add handlers; Task 1 registers what exists — start with a trivial `lwd_list` or just the plumbing so the server builds and serves).
- `cmd/lwd-mcp/main.go`: `c := client.New(config.SocketPath())` (honor `LWD_SOCKET`/`LWD_DATA_DIR` via config); `srv := mcppkg.NewServer(c)`; serve over stdio (go-sdk stdio transport); log to stderr only (stdout is the MCP channel).

- [ ] **Step 1:** add the SDK: `go get github.com/modelcontextprotocol/go-sdk@latest` (pin the resolved version). If the import path/name differs, adapt; report the exact version+API used.
- [ ] **Step 2:** write a failing `server_test.go`: `TestClientAssertion` is compile-time (the `var _ ClientIface = (*client.Client)(nil)` line makes the package fail to build if it drifts — so a passing build IS the test); `TestServerConstructs` — `NewServer(&fakeClient{})` + `MCP()` returns a non-nil server with the expected tools registered (list the tool names once handlers exist; for Task 1 assert the server constructs). `fake_client_test.go`: a `fakeClient` implementing `ClientIface` with canned data + knobs.
- [ ] **Step 3:** implement server.go + main.go (register at least `lwd_list` so there's a real tool, or leave registration to Task 2 and have Task 1 assert construction).
- [ ] **Step 4:** `CGO_ENABLED=0 go build -o /tmp/lwd-mcp ./cmd/lwd-mcp` succeeds; `go test ./internal/mcp/ -v` passes; `go vet`, `gofmt -l` clean. Report the go-sdk version + the stdio-serve call used.
- [ ] **Step 5:** commit `feat: lwd-mcp scaffold — MCP server skeleton, ClientIface, stdio entrypoint`.

---

### Task 2: read tools — list / status / logs / history

**Files:** Create `internal/mcp/tools.go` (+ handlers), extend `internal/mcp/server_test.go`.

**Tools** (all `readOnlyHint: true`):
- `lwd_list` → `client.Apps` → JSON array of {name, domain, status, image}.
- `lwd_status` {name} → the matching AppStatus + the app's `History` (latest first) summarized.
- `lwd_logs` {name, tail?:int} → `client.Logs(ctx,name,false,buf)`; return the captured text, last `tail` lines if given (default e.g. 200).
- `lwd_history` {name} → `client.History` → JSON of deployments (image, status, time).

- [ ] **Step 1:** failing tests (fake client): each tool registered, returns the fake's data; `lwd_logs` respects `tail`; read tools carry the read-only annotation.
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3:** implement the 4 handlers + register with arg schemas + annotations.
- [ ] **Step 4:** `go test ./internal/mcp/ -v` PASS; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: lwd-mcp read tools (list/status/logs/history)`.

---

### Task 3: deploy tools — apply / deploy_git / rollback / remove

**Files:** `internal/mcp/tools.go`, `internal/mcp/server_test.go`.

**Tools:**
- `lwd_apply` {dir?:string, toml?:string} → if `toml` set: `spec.Parse` → `Validate`; else if `dir`: `spec.Load(dir)`; (exactly one required) → `client.Apply` → return the deployment. Validation/parse error → tool error.
- `lwd_deploy_git` {url, ref?, dockerfile?, name, domain, port, services?:[{name,image,command?,env?,secrets?,volume?}]} → build a `spec.App{Git:&{URL,Ref}, Build:&{Dockerfile}, Name, Domain, Port, Services}` → `Validate` → `client.Apply`. (Ref default "main", dockerfile default "Dockerfile".)
- `lwd_rollback` {name} → `client.Rollback`.
- `lwd_remove` {name} → `client.Remove` (`destructiveHint: true`).

- [ ] **Step 1:** failing tests (fake client): `lwd_apply` with valid toml calls Apply with the parsed app; bad toml / missing image+port → error; neither dir nor toml → error. `lwd_deploy_git` builds a valid git spec (assert the App passed to Apply has Git/Build/Domain/Port and validates); an invalid one (e.g. bad ref/url) → error (Validate runs). `lwd_rollback`/`lwd_remove` call the right method; `lwd_remove` has the destructive annotation.
- [ ] **Step 2:** FAIL → **Step 3:** implement handlers + schemas + annotations.
- [ ] **Step 4:** `go test ./internal/mcp/ -v` PASS; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: lwd-mcp deploy tools (apply/deploy_git/rollback/remove)`.

---

### Task 4: secret tools — set / list / delete

**Files:** `internal/mcp/tools.go`, `internal/mcp/server_test.go`.

**Tools:**
- `lwd_secret_set` {app, key, value} → `client.SetSecret` → confirmation (never echo the value).
- `lwd_secret_list` {app} → `client.ListSecrets` → JSON array of NAMES only.
- `lwd_secret_delete` {app, key} → `client.DeleteSecret`.

- [ ] **Step 1:** failing tests: set → list shows the name; the value NEVER appears in any tool's response (assert the set-confirmation + list output don't contain the value); delete removes it.
- [ ] **Step 2:** FAIL → **Step 3:** implement.
- [ ] **Step 4:** `go test ./internal/mcp/ -v` PASS; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: lwd-mcp secret tools (set/list/delete, values never returned)`.

---

### Task 5: integration test + README

**Files:** `internal/mcp/integration_test.go` (or `test/mcp_e2e_test.go`), `README.md`.

- [ ] **Step 1: integration test** (no Docker): start a real daemon `api.Server` on a temp unix socket with the fake node/router/compose/secrets/store stack (reuse the harness from `internal/web/integration_test.go`), build a real `client.New(sock)`, construct the MCP `Server`, and drive it: initialize + `tools/list` (assert all expected tool names present), call `lwd_list` (empty ok), call `lwd_apply` with a single-service toml → then `lwd_list` shows it. Drive via the go-sdk's in-memory/stdio client if it provides one; else call the handler functions directly through the server's registered tools. Keep hermetic (temp dir/socket, cleanup).
- [ ] **Step 2:** `go test ./...` → all pass (no Docker; existing Docker-gated e2e SKIPs).
- [ ] **Step 3:** `CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go build -o /tmp/lwd-mcp ./cmd/lwd-mcp && go vet ./... && gofmt -l .` clean.
- [ ] **Step 4: README** — add "## Agent access (lwd-mcp)": what it is (stdio MCP server, client of the daemon, no network/auth), build (`go build ./cmd/lwd-mcp`), an MCP-host config snippet (command `lwd-mcp`, stdio, env to locate the socket), the tool list, and the note that destructive tools rely on host approval. **Full pass confirming README reflects Phases 1–8** (now three binaries: lwd, lwd-web, lwd-mcp).
- [ ] **Step 5:** commit `test: lwd-mcp integration (mcp->client->daemon); docs: agent access README`.

---

## Self-Review

**Spec coverage:** scaffold+ClientIface+stdio entrypoint (T1); read tools (T2); deploy tools (T3); secret tools (T4); integration + README (T5). All tools from the spec present; annotations applied; no secret value returned; zero daemon changes. ✓

**Deferred (by design):** networked MCP + auth; multi-node targeting (Phase 9).

**README:** T5 makes it explicit + a Phases 1–8 pass; final review checks it.

**Placeholder scan:** handler logic + fake-client tests are concrete; the go-sdk wiring is behavior-contract + mcp-builder-guided (exact SDK API adapted to the resolved version, like prior infra tasks). No TBD.

**Type consistency:** `ClientIface` mirrors `*client.Client` (compile assertion). Tools use `api.AppStatus`/`store.Deployment`/`spec.App` as the daemon defines them. Tool names (`lwd_list`, `lwd_status`, `lwd_logs`, `lwd_history`, `lwd_apply`, `lwd_deploy_git`, `lwd_rollback`, `lwd_remove`, `lwd_secret_set`, `lwd_secret_list`, `lwd_secret_delete`) consistent across tasks + tests + README.

**Cross-task note:** all tasks stay in `cmd/lwd-mcp` + `internal/mcp`, consuming existing packages read-only. Every task keeps `go test ./...` green without Docker.
