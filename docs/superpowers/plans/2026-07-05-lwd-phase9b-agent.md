# lwd Phase 9b Implementation Plan — dumb agent + node health + node UX

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`).

**Goal:** Add a dumb `lwd-agent` (HTTP server exposing Node primitives, token-auth, mesh-bound) as the preferred node transport with the P9a docker-over-SSH fallback; report node reachability; surface node UX in lwd-web and lwd-mcp.

**Architecture:** `cmd/lwd-agent`+`internal/agent` is a thin HTTP server that delegates every request to a local `node.Node` (NewLocal). `internal/node/agent.go`'s `agentNode` is the mirror-image HTTP client implementing `node.Node`. The 9a `RegistryResolver` gains a per-node transport choice: `agentNode` when `agent_url` is set and `/healthz` pings OK, else `sshDocker`. Node reachability + agent URL surface through the existing `/nodes` API into CLI/web/MCP.

**Tech Stack:** Go 1.25+ (stdlib net/http, crypto/subtle), `internal/{node,store,client,config,web,mcp}`; the fourth binary is `lwd-agent`.

## Global Constraints

- Go 1.25+; no cgo. Module `lwd`. Spec: `docs/superpowers/specs/2026-07-05-lwd-phase9b-agent-design.md`; north star `docs/VISION.md`.
- **Agent is DUMB:** execute Docker ops, report health, stream logs, load images. NO build/compose/scheduling/orchestration. The controller decides.
- **Extend, don't replace.** Zero regression to single-node and to P9a's docker-over-SSH path (ssh stays the fallback).
- **Transport selection per node:** `agent_url` set AND `/healthz` OK → `agentNode`; else `sshDocker` (P9a).
- **Auth:** bearer `LWD_AGENT_TOKEN` (required to start the agent), constant-time compare; agent binds `LWD_AGENT_ADDR` (default `:8078`) — operator binds the mesh interface. No secret value is ever returned by any endpoint/tool.
- **Capacity is OUT of scope (P11).** P9b reports only health/reachability.
- **README + VISION progress** updated in the final task; verified in the final review.
- Tests use fakes/httptest; anything needing a real agent/ssh is guarded by `LWD_DOCKER_TEST` and never required for `go test ./...`.

---

### Task 1: store — nodes.agent_url column

**Files:** Modify `internal/store/store.go`, `internal/store/store_test.go`.

**Interfaces:** `store.Node` gains `AgentURL string` (json `agent_url`). `nodes` table gains `agent_url TEXT NOT NULL DEFAULT ''` via a guarded idempotent migration (mirror the Phase-2 `spec`/Phase-4 `compose` ADD-COLUMN pattern). `AddNode` persists it; `GetNode`/`ListNodes` select+scan it.

- [ ] **Step 1: failing tests** — `AddNode` with `AgentURL:"http://100.64.0.2:8078"` → GetNode round-trips it; upsert updates agent_url; a pre-9b `nodes` table (without agent_url) migrates on `Open` (create the old DDL raw, insert a row, reopen → agent_url defaults ""). Use `openTemp`.
- [ ] **Step 2:** `go test ./internal/store/ -run Node -v` → FAIL.
- [ ] **Step 3:** add the field + column + `migrateAddAgentURLColumn` (called in `Open`) + update AddNode/GetNode/ListNodes SQL (column lists + scan).
- [ ] **Step 4:** `go test ./internal/store/ -v` → PASS; build clean.
- [ ] **Step 5:** commit `feat: store nodes.agent_url column`.

---

### Task 2: agent wire contract (shared types)

**Files:** Create `internal/agent/wire.go`, `internal/agent/wire_test.go`.

**Interfaces:** the request/response JSON types shared by the agent server (Task 3) and the `agentNode` client (Task 4), so both sides can't drift:
- `RunRequest{ Spec node.RunSpec }` → `RunResponse{ Container node.Container }`
- `RemoveRequest{ ID string }`; `ListRequest{ Labels map[string]string }` → `ListResponse{ Containers []node.Container }`
- `EnsureImageRequest{ Ref string }`; `ImagePresentRequest{ Ref string }` → `ImagePresentResponse{ Present bool }`
- `EnsureNetworkRequest{ Name string }`; `ConnectNetworkRequest{ ContainerID, Network string }`
- `ContainerHealthRequest{ ID string }` → `ContainerHealthResponse{ State, DockerHealth string }`
- `HealthCheckRequest{ Container node.Container; Health node.HealthSpec }`
- `ErrorResponse{ Error string }`
- path constants: `PathHealthz="/healthz"`, `PathRun="/run"`, `PathRemove="/remove"`, `PathList="/list"`, `PathEnsureImage="/ensure-image"`, `PathImagePresent="/image-present"`, `PathLoad="/load"` (tar stream), `PathLogs="/logs"` (stream), `PathEnsureNetwork="/ensure-network"`, `PathConnectNetwork="/connect-network"`, `PathContainerHealth="/container-health"`, `PathHealth="/health"`.

- [ ] **Step 1: failing test** — a round-trip JSON marshal/unmarshal of `RunRequest{Spec: node.RunSpec{Name:"x",Image:"i",Port:80,Network:"lwd"}}` preserves fields; `ImagePresentResponse{Present:true}` round-trips. (Guards that node types serialize cleanly over the wire — add json tags to node.RunSpec/Container/HealthSpec if any field doesn't round-trip.)
- [ ] **Step 2:** `go test ./internal/agent/ -v` → FAIL.
- [ ] **Step 3:** write `wire.go` (types + path consts); add json tags to `node.RunSpec`/`Container`/`HealthSpec`/`PortMapping` if needed for clean round-trip.
- [ ] **Step 4:** PASS; build clean.
- [ ] **Step 5:** commit `feat: agent wire contract (shared request/response types)`.

---

### Task 3: agent server + `cmd/lwd-agent`

**Files:** Create `internal/agent/server.go`, `internal/agent/server_test.go`, `internal/agent/auth.go`, `cmd/lwd-agent/main.go`.

**Interfaces:** `NewServer(n node.Node, token string) *Server`; `(*Server).Handler() http.Handler` — routes per the wire paths, each decoding its request, calling the corresponding `n` method, encoding the response (or `ErrorResponse` + 500/400). `GET /healthz` → 200 if `n.ImagePresent(ctx,"")`-style docker-reachability probe works (use a cheap `n.ListContainers(ctx, map[string]string{"lwd.probe":"x"})` or a dedicated `n.Ping()` — add `Ping(ctx) error` to Node if cleaner; else reuse ListContainers) → 200/503. `/logs` streams (copy `n.ContainerLogs` to the ResponseWriter, flush). `/load` reads the request body as the tar and calls `n.LoadImage`. All non-healthz routes require `Authorization: Bearer <token>` (constant-time compare via `crypto/subtle`); 401 otherwise. `auth.go`: the bearer middleware. `cmd/lwd-agent/main.go`: read `LWD_AGENT_TOKEN` (required → exit 1 if empty), `LWD_AGENT_ADDR` (default `:8078`); build `node.NewLocal()`; serve `Handler()`. Log to stderr.

- [ ] **Step 1: failing tests** (httptest + a fake `node.Node`): `POST /run` with a bearer token → calls the fake's RunContainer, returns the container; missing/wrong token → 401; `GET /healthz` (no auth) → 200 when the fake is healthy, 503 when its probe errors; `/ensure-network`, `/image-present`, `/remove`, `/container-health` delegate correctly.
- [ ] **Step 2:** `go test ./internal/agent/ -v` → FAIL.
- [ ] **Step 3:** implement auth.go + server.go (all routes) + cmd/lwd-agent/main.go. (Add `node.Node.Ping(ctx) error` if you chose that for healthz — implement on Local + Fake; else use ListContainers.)
- [ ] **Step 4:** `go test ./internal/agent/ -v` PASS; `CGO_ENABLED=0 go build -o /tmp/lwd-agent ./cmd/lwd-agent` succeeds; `LWD_AGENT_TOKEN=x /tmp/lwd-agent` starts (kill it; don't leave running); vet/gofmt clean.
- [ ] **Step 5:** commit `feat: lwd-agent dumb Node-primitive HTTP server`.

---

### Task 4: `agentNode` — HTTP-client Node impl

**Files:** Create `internal/node/agent.go`, `internal/node/agent_test.go`.

**Interfaces:** `func NewAgentNode(baseURL, token string) *AgentNode` implementing `node.Node`: each method POSTs the wire request (bearer token) to the agent path and decodes the response; `LoadImage` streams `r` to `/load`; `ContainerLogs` returns the streaming response body; `SaveImage` POSTs to a `/save` endpoint (add it to Task 3's server too) returning the stream (needed for interface completeness; the controller mainly uses local SaveImage in 9a, but implement it). Add `(*AgentNode) Ping(ctx) error` → GET `/healthz`. `var _ node.Node = (*AgentNode)(nil)`.

- [ ] **Step 1: failing tests** (httptest server mimicking the agent OR the real `agent.NewServer(fakeLocal, token).Handler()` — prefer the REAL agent handler wrapping a fake local node, so client+server are tested together): `AgentNode.RunContainer` round-trips through the agent to the fake local node; `ImagePresent`, `EnsureNetwork`, `ConnectContainerToNetwork`, `ContainerHealth` work; `LoadImage` streams a tar and the fake records it; `Ping` hits `/healthz`; a wrong token → error.
- [ ] **Step 2:** `go test ./internal/node/ -run Agent -v` → FAIL.
- [ ] **Step 3:** implement `agent.go` (+ the `/save` endpoint in agent/server.go if not added in Task 3).
- [ ] **Step 4:** `go test ./internal/node/ -v` PASS; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: agentNode HTTP-client Node impl`.

---

### Task 5: resolver — agent-preferred transport + reachability

**Files:** Modify `internal/node/resolver.go`, `internal/node/resolver_test.go`.

**Interfaces:** the resolver's node lookup already returns sshHost + meshAddr; extend it to also return `agentURL`. In `RegistryResolver.Resolve`/`ResolveMeta`, for a registered node: if `agentURL != ""` and `NewAgentNode(agentURL, token).Ping(ctx)` succeeds → cache+use `agentNode`; else fall back to `NewRemoteSSH(sshHost)` (P9a). The agent token comes from daemon config (`LWD_AGENT_TOKEN` on the controller side too, shared) — thread it into `NewRegistryResolver`. Add `Reachable(name) (transport string, ok bool)` (transport = "agent"/"ssh"/"local"; ok = ping/version succeeded) for the CLI/web status. Keep `Invalidate` (9a). The lookup closure signature grows `agentURL`.

- [ ] **Step 1: failing tests** — a node with a reachable agent URL → `Resolve` returns an agentNode (assert via type or a probe hook); agent URL set but ping fails → falls back to sshDocker; no agent URL → sshDocker; `""`/`"local"` → local; `Reachable` reports the right transport. (Use an injectable ping/agent-constructor or an httptest agent for the reachable case.)
- [ ] **Step 2:** `go test ./internal/node/ -run Resolver -v` → FAIL.
- [ ] **Step 3:** implement the transport choice + `Reachable` + token threading; update `NewRegistryResolver` signature + the lookup closure (+ the daemon call site passes agent_url + token).
- [ ] **Step 4:** `go test ./...` PASS (update the daemon lookup in cli.go to supply agent_url + token); build/vet/gofmt clean. Report call sites.
- [ ] **Step 5:** commit `feat: resolver agent-preferred transport + node reachability`.

---

### Task 6: CLI + API node UX (agent url, reachability)

**Files:** Modify `internal/api/api.go`, `internal/api/api_test.go`, `internal/client/client.go`, `internal/cli/cli.go`.

**Interfaces:** `POST /nodes` accepts optional `agent_url` (validate: if present, a valid http/https URL). `GET /nodes` response includes `agent_url` + a `reachable`/`transport` field (the Server asks the resolver's `Reachable(name)` per node). Client `AddNode` gains an `agentURL` param (or an options struct); `Nodes()` returns the enriched records. CLI: `lwd node add <name> <ssh-host> <mesh-addr> [--agent <url>]`; `lwd node ls` shows a TRANSPORT + REACHABLE column.

- [ ] **Step 1: failing api tests** — POST /nodes with `agent_url` persists it; GET /nodes includes agent_url + reachable/transport (use a fake reachability reporter); invalid agent_url → 400.
- [ ] **Step 2:** `go test ./internal/api/ -v` → FAIL.
- [ ] **Step 3:** implement API (Server gets a `Reachable` reporter — the resolver; api.New already has the resolver as the invalidator from 9a, extend/interface it) + client `--agent`/reachability + CLI `--agent` flag + ls columns.
- [ ] **Step 4:** `CGO_ENABLED=0 go build -o /tmp/lwd ./cmd/lwd && go test ./... && go vet ./... && gofmt -l .` green.
- [ ] **Step 5:** commit `feat: node agent_url + reachability in API/CLI`.

---

### Task 7: web + MCP node UX; e2e; README + VISION

**Files:** Modify `internal/web/assets/*` (frontend-design skill for the Nodes view + deploy picker), `internal/web/api.go` (proxy `/nodes` if not already), `internal/mcp/tools.go` + `server_test.go`, `test/e2e_test.go`, `README.md`, `docs/VISION.md`.

- [ ] **Step 1: MCP tools** (TDD, fake client): `lwd_node_list` (→ nodes + reachability), `lwd_node_add` {name,ssh_host,mesh_addr,agent_url?}, `lwd_node_remove` {name}; add an optional `node` arg to `lwd_apply`/`lwd_deploy_git` (set `spec.App.Node`). Add DaemonClient methods if missing (AddNode/Nodes/RemoveNode already exist from 9a — reuse). Tests assert the tools call the right client methods + node arg flows into the applied spec.
- [ ] **Step 2: web** (invoke frontend-design): a Nodes view (list with transport+reachability, add form incl. optional agent URL, remove) reachable from the dashboard; a `node` `<select>` in the deploy modal populated from `GET /api/nodes` (+ `local`) that adds `node = "..."` to the generated lwd.toml. Add a `GET /api/nodes` (+ add/remove) to the web server proxying the daemon client (authed). Keep the design system.
- [ ] **Step 3: verify** — `go test ./...` PASS; `CGO_ENABLED=0 go build ./... && all 4 binaries (lwd, lwd-web, lwd-mcp, lwd-agent) && go vet && gofmt -l .` clean. Drive the web Nodes view + deploy picker if the tooling allows (screenshot); don't leave a server running.
- [ ] **Step 4: e2e** (guarded): start a real `lwd-agent` (LWD_AGENT_TOKEN=test, bound to 127.0.0.1:8078) in-process or as a subprocess; register a node with that agent_url + mesh_addr 127.0.0.1; deploy an app pinned to it THROUGH THE AGENT transport (assert the resolver chose agent, app reachable). If Docker/agent can't run, SKIP cleanly (don't fake).
- [ ] **Step 5: docs** — README: agent section (build `lwd-agent`, `LWD_AGENT_TOKEN`/`LWD_AGENT_ADDR`, `lwd node add --agent`, agent-vs-ssh transport, mesh-bound + token trust boundary); note four binaries now. `docs/VISION.md`: mark **P9b done**, P10 next. Full README pass for Phases 1–9b.
- [ ] **Step 6: commit** `feat: web+MCP node UX; lwd-agent e2e; docs P9b`.

---

## Self-Review

**Spec coverage:** agent_url store (T1); shared wire contract (T2); agent server+binary (T3); agentNode client (T4); resolver agent-preferred+reachability (T5); API/CLI node UX (T6); web+MCP UX + e2e + docs (T7). ✓
**Deferred (by design):** capacity (P11), continuous reconciler (P10), agent-run build, mTLS.
**Non-regression:** ssh path (P9a) stays the fallback when no agent/unreachable; single-node unchanged (resolver `local` path untouched).
**Placeholder scan:** store/wire/agent/agentNode/resolver logic have concrete tests; the web view is frontend-design + fixed endpoints; e2e guarded. No TBD.
**Type consistency:** the wire types (T2) are the single source shared by agent server (T3) + agentNode client (T4) — no drift. `node.Node` gains `Ping` (if used for healthz) on Local+Fake+agentNode. `store.Node.AgentURL` consistent across store/api/client/resolver. Resolver lookup closure + `NewRegistryResolver` grow `agentURL`+token — daemon call site updated (T5). `agentNode` satisfies `node.Node` (compile assertion). Reachability transport strings ("agent"/"ssh"/"local") consistent across resolver/api/cli/web/mcp.
**Cross-task note:** T2's wire contract is imported by both T3 and T4. T5 changes the resolver lookup signature; the daemon call site updates there. All tasks keep `go test ./...` green without Docker/agent; real agent path is exercised by T7's guarded e2e.
