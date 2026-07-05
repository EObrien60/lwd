# lwd Phase 9a Implementation Plan — federation foundation (multi-node deploys)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`).

**Goal:** Deploy an app to a chosen registered node over docker-over-SSH, moving locally-built images to the node via `docker save|load`, with the central Caddy reaching the remote surface over the node's WireGuard address. Single-node stays unchanged.

**Architecture:** The reconciler stops holding one `node.Node` and instead resolves `app.Node` → a `node.Node` via a new `NodeResolver` (backed by a `nodes` registry in the store; `local` → the local Docker, a registered node → docker-over-SSH). Remote surfaces publish an ephemeral port on the node's mesh address; the Router's Caddy upstream becomes `<meshAddr>:<port>`. Extends Node/Router/Reconciler/Store; the dumb `lwd-agent` + capacity + web/MCP node UX are Phase 9b.

**Tech Stack:** Go 1.25+, Docker SDK (ssh conn-helper via `client.WithHost("ssh://…")`), `internal/{node,reconciler,router,store,client,config}`.

## Global Constraints

- Go 1.25+; no cgo. Module `lwd`. Specs: `docs/superpowers/specs/2026-07-05-lwd-phase9-federation-design.md` + north star `docs/VISION.md`.
- **Extend, don't replace** Node/Router/Reconciler/Store. **Single-node must not regress** — `node=""`/`"local"` behaves exactly as today.
- **Suckless:** remote nodes = Docker daemons over SSH (the box's ssh + Docker SDK ssh conn-helper); lwd manages no ssh credentials. Images move via `docker save | load` (no registry). No custom agent in 9a (that's 9b).
- Build stays on the **controller** (Phase 6 unchanged); the built image is shipped to the target node via save/load. The agent is out of scope for 9a.
- Routing: a **remote** surface publishes an ephemeral host port bound to the node's **mesh address**; Caddy upstream = `<meshAddr>:<port>`. A **local** surface is unchanged (no host port; upstream = container name on the `lwd` network). Blue-green preserved (swap the upstream).
- Placement: explicit `node = "<name>"` in `lwd.toml` (already parsed; default `local`).
- **README + VISION progress** updated in the final task; accuracy checked in the final review.
- Tests use fakes + temp dirs; anything needing a real remote Docker/ssh is guarded by `LWD_DOCKER_TEST` (+ skipped if no second endpoint) and never required for `go test ./...`.

---

### Task 1: store — nodes registry

**Files:** Modify `internal/store/store.go`, `internal/store/store_test.go`.

**Interfaces:**
- `type Node struct { Name, SSHHost, MeshAddr string; CreatedAt time.Time }`
- `func (s *Store) AddNode(n Node) error` (upsert on Name), `GetNode(name) (*Node, error)` (nil,nil when absent), `ListNodes() ([]Node, error)` (sorted by Name), `DeleteNode(name) error`.
- Schema: `CREATE TABLE IF NOT EXISTS nodes (name TEXT PRIMARY KEY, ssh_host TEXT NOT NULL, mesh_addr TEXT NOT NULL, created_at INTEGER NOT NULL);` (idempotent; `local` is NOT stored — it's implicit).

- [ ] **Step 1: failing tests** in store_test.go (temp dir): AddNode→GetNode round-trip; AddNode upsert (same name overwrites ssh_host/mesh_addr); GetNode absent → (nil,nil); ListNodes sorted; DeleteNode removes.
- [ ] **Step 2:** `go test ./internal/store/ -run Node -v` → FAIL.
- [ ] **Step 3:** add the table to the base `schema` const + the 4 methods (parameterized SQL, `ON CONFLICT(name) DO UPDATE`, `sql.ErrNoRows`→(nil,nil)).
- [ ] **Step 4:** `go test ./internal/store/ -v` → PASS; build clean.
- [ ] **Step 5:** commit `feat: store nodes registry`.

---

### Task 2: node — image save/load + remote-SSH impl

**Files:** Modify `internal/node/node.go`, `internal/node/local.go`, `internal/node/fake.go`, `internal/node/fake_test.go`; create `internal/node/remote.go`.

**Interfaces:**
- Add to `Node`: `ImagePresent(ctx, ref string) (bool, error)`; `SaveImage(ctx, ref string) (io.ReadCloser, error)` (a tar stream of the image); `LoadImage(ctx, r io.Reader) error` (load a tar stream).
- `func NewRemoteSSH(sshHost string) (*Local, error)` — build a `*Local` whose Docker client uses `client.WithHost("ssh://"+sshHost)` + `WithAPIVersionNegotiation()`. (Reuse the `Local` type; only the client's host differs. If `Local`'s constructor can't be reused cleanly, extract a `newLocalWithClient(cli *client.Client) *Local`.)
- `Fake` gains: `Images map[string]bool` (present set), `SaveErr`/`LoadErr` knobs; `ImagePresent`/`SaveImage` (returns a canned reader)/`LoadImage` (marks the ref present via a knob or records the call). Keep `var _ Node` for Local + Fake + (remote uses Local so covered).

- [ ] **Step 1: failing fake tests** — `ImagePresent` reflects the `Images` set; `SaveImage` returns a non-empty reader (or records the call); `LoadImage` records/marks. Compile assertions hold.
- [ ] **Step 2:** `go test ./internal/node/ -v` → FAIL.
- [ ] **Step 3:** implement the 3 interface methods on `Local` (SDK: `ImageInspectWithRaw` for present; `ImageSave([]string{ref})` → reader; `ImageLoad(ctx, r, quiet)` → drain the response body) + on `Fake`; add `NewRemoteSSH`. Adapt to the resolved SDK signatures (behavior is the contract; keep imports clean).
- [ ] **Step 4:** `go test ./internal/node/ -v` → PASS; `CGO_ENABLED=0 go build ./...` clean.
- [ ] **Step 5:** commit `feat: node image save/load + docker-over-ssh remote impl`.

---

### Task 3: node resolver + reconciler placement refactor

**Files:** Create `internal/node/resolver.go`, `internal/node/resolver_test.go`; modify `internal/reconciler/reconciler.go`, `internal/reconciler/reconciler_test.go`.

**Interfaces:**
- `type Resolver interface { Resolve(nodeName string) (Node, error) }` (empty or `"local"` → the local node; a registered name → a cached remote-ssh node; unknown → error).
- `type RegistryResolver struct { ... }`; `func NewRegistryResolver(local Node, lookup func(name string) (*store.Node, error)) *RegistryResolver` — wait, avoid importing store into node (cycle risk). Instead: `NewRegistryResolver(local Node, lookup func(name string) (sshHost string, ok bool, err error))` — the daemon supplies a lookup closure over the store. Caches remote `Node`s by name. A `FakeResolver` (map name→Node) for reconciler tests.
- **Reconciler:** replace the single `node node.Node` field with `resolver node.Resolver`. `New(...)` takes a `node.Resolver` in place of the `node.Node` arg (all other deps unchanged). Everywhere the reconciler used `r.node`, it now does `n, err := r.resolver.Resolve(app.Node)` once at the top of a deploy and uses `n`. `EnsureNetwork("lwd")` etc. run on the resolved node. Local behavior is identical (resolver returns the local node for `""`/`"local"`).

- [ ] **Step 1: failing tests** — resolver: `Resolve("")`/`Resolve("local")` → the local node; `Resolve("web1")` → a remote node (via the lookup + NewRemoteSSH; in the test, inject a fake constructor or assert the lookup is called and cached); unknown name → error. Reconciler: update `newTestReconciler` to pass a `node.FakeResolver{"local": fakeNode}`; an existing single-service apply still works (resolves local); a `TestApplyRoutesToNode` — an app with `Node:"web1"` resolves to the web1 fake node (assert the container ran on that node's fake).
- [ ] **Step 2:** `go test ./internal/node/ ./internal/reconciler/ -v` → FAIL.
- [ ] **Step 3:** implement `resolver.go` (+ FakeResolver) and refactor the reconciler to resolve per-deploy. Keep the mutex + all existing flow; only the node source changes. Update `New`'s signature (resolver instead of node).
- [ ] **Step 4:** `go test ./...` → PASS (update call sites: cli daemon, api/client/web/mcp tests that construct `reconciler.New` — pass a resolver; the daemon builds a `RegistryResolver` over the store). `CGO_ENABLED=0 go build ./...`, vet, gofmt clean. Report call sites touched.
- [ ] **Step 5:** commit `feat: node resolver + reconciler placement (node=) via resolver`.

---

### Task 4: remote image transfer + mesh-address routing

**Files:** Modify `internal/reconciler/reconciler.go`, `internal/node/node.go` (RunSpec bind IP), `internal/router/*` (upstream host), `internal/reconciler/reconciler_test.go`.

**Interfaces:**
- `node.PortMapping` gains `HostIP string` (bind address; default `127.0.0.1` for local publishes, `0.0.0.0` for 80/443, the node's mesh addr for remote surfaces). `Local.RunContainer` binds `HostIP` when set.
- Reconciler `ensureImageOnNode(ctx, target node.Node, ref string) error`: if `target.ImagePresent(ref)` → done; else try `target.EnsureImage(ref)` (registry pull); if still `!ImagePresent` → transfer: `rc := localNode.SaveImage(ref)` → `target.LoadImage(rc)`. (The reconciler holds the local node via the resolver; add a `localNode()` helper = `resolver.Resolve("local")`.)
- Remote surface deploy: when the resolved node is NOT local (the app's `Node` is a registered node), run the surface with a `Publish` of an ephemeral host port bound to the node's **MeshAddr** (looked up from the registry; the resolver/daemon provides the mesh addr for a node), and set the Caddy upstream to `<meshAddr>:<hostPort>` instead of `<containerName>:<port>`. Local surfaces unchanged. The Router's `Route.Upstream` already is a host:port-ish string — for remote it's `meshAddr:hostPort`, for local `containerName` (Caddy resolves the container name on the shared network). Ensure `GenerateCaddyfile` emits `reverse_proxy <upstream>:<port>` correctly for both (for remote, port is baked into upstream — adjust so upstream can carry host:port and the Route.Port is the container port for local only; simplest: `Route.Upstream` is the full `host` and `Route.Port` the port; for remote set Upstream=meshAddr, Port=hostPort; for local Upstream=containerName, Port=containerPort — same shape).

- [ ] **Step 1: failing tests** (fake nodes + fake router): `TestEnsureImageTransfersWhenAbsent` — target fake reports image absent + EnsureImage (pull) leaves it absent → reconciler calls localNode.SaveImage + target.LoadImage; if present, no transfer. `TestRemoteSurfaceRoutesToMeshAddr` — an app on `web1` (mesh addr `100.64.0.2`) → the surface Publishes an ephemeral port bound to `100.64.0.2` and `FakeRouter.Routes[domain].Upstream == "100.64.0.2"` with the published port; a local app still routes to the container name. Keep existing local tests green.
- [ ] **Step 2:** `go test ./internal/reconciler/ -v` → FAIL.
- [ ] **Step 3:** implement `HostIP` in RunSpec/Local, `ensureImageOnNode` transfer, remote publish-on-mesh + upstream. Thread the node's mesh addr from the registry (the resolver or a `NodeMeta(name)` lookup the daemon supplies). Blue-green: the new remote container gets a fresh ephemeral port; probe through Caddy (staging route → the new meshAddr:port); flip; retire old.
- [ ] **Step 4:** `go test ./...` PASS; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: remote image save/load transfer + mesh-address Caddy routing`.

---

### Task 5: CLI + API — `lwd node add/ls/rm`; daemon wiring

**Files:** Modify `internal/api/api.go`, `internal/api/api_test.go`, `internal/client/client.go`, `internal/cli/cli.go`, `internal/cli/cli.go` (daemon builds the RegistryResolver).

**Interfaces:**
- API: `POST /nodes` (body {name, ssh_host, mesh_addr} → store.AddNode → 204; 400 on missing fields), `GET /nodes` (→ JSON []store.Node), `DELETE /nodes/{name}` (→ 204).
- Client: `AddNode(ctx, name, sshHost, meshAddr) error`, `Nodes(ctx) ([]store.Node, error)`, `RemoveNode(ctx, name) error`.
- CLI: `lwd node add <name> <ssh-host> <mesh-addr>` (or flags), `lwd node ls`, `lwd node rm <name>`. Dispatch a `node` subcommand.
- Daemon (`runDaemon`): build `node.NewRegistryResolver(localNode, lookup)` where `lookup` reads the store's nodes; pass the resolver to `reconciler.New`. (Replaces the single local node passed today.)

- [ ] **Step 1: failing api tests** — POST /nodes then GET /nodes shows it; DELETE removes; POST missing ssh_host → 400.
- [ ] **Step 2:** `go test ./internal/api/ -v` → FAIL.
- [ ] **Step 3:** implement API routes + client methods + CLI `node` subcommands + daemon resolver wiring.
- [ ] **Step 4:** `CGO_ENABLED=0 go build -o /tmp/lwd ./cmd/lwd && go test ./... && go vet ./... && gofmt -l . && /tmp/lwd version` all green.
- [ ] **Step 5:** commit `feat: lwd node add/ls/rm (API+CLI); daemon resolver wiring`.

---

### Task 6: e2e + README + VISION progress

**Files:** Modify `test/e2e_test.go`, `README.md`, `docs/VISION.md`.

- [ ] **Step 1: e2e** (guarded by `LWD_DOCKER_TEST`; also SKIP if no usable second Docker endpoint). Use `ssh://localhost` as a "remote" node IF key-based ssh to localhost + Docker is reachable (probe: `docker -H ssh://localhost version`); else SKIP with a clear message. Register the node (mesh addr = `127.0.0.1` for the loopback case), deploy a single-service app pinned to it with a built/loaded image, assert it's reachable through Caddy via the node addr; a `local` app still works. Clean up. (If ssh-localhost isn't viable in the environment, keep the e2e as a SKIP-scaffold that documents how to run it against a real node — do not fake a pass.)
- [ ] **Step 2:** `go test ./...` (no env) → PASS, e2e SKIPs.
- [ ] **Step 3:** `LWD_DOCKER_TEST=1 go test ./test/ -run TestEndToEndNode -v` → PASS or a clear SKIP (report which).
- [ ] **Step 4:** `CGO_ENABLED=0 go build ./... && ...-o /tmp/lwd ./cmd/lwd && ...-o /tmp/lwd-web ./cmd/lwd-web && ...-o /tmp/lwd-mcp ./cmd/lwd-mcp && go vet ./... && gofmt -l .` clean.
- [ ] **Step 5: README** — add "## Multi-node (federation)": `lwd node add/ls/rm`, `node = "..."` placement, docker-over-ssh + WireGuard mesh requirement, save/load image movement, that build stays on the controller. Note the single-node path is unchanged. Update `docs/VISION.md`'s roadmap: mark **P9a** done, P9b next. **Full README pass for Phases 1–9a.**
- [ ] **Step 6:** commit `test: e2e multi-node deploy; docs: federation README + VISION P9a progress`.

---

## Self-Review

**Spec coverage (P9a slice):** node registry (T1); image save/load + remote-ssh Node (T2); resolver + placement refactor (T3); image transfer + mesh routing (T4); node CLI/API + daemon wiring (T5); e2e + README + VISION (T6). ✓
**Deferred to 9b (by design):** dumb `lwd-agent` transport, capacity/health reporting, web/MCP node UX, richer cross-node e2e. **Deferred to P10+:** continuous reconciler, scheduler, replicas, multi-edge, resource drivers.
**Single-node non-regression:** the resolver returns the local node for `""`/`"local"`; all existing local flow/tests unchanged (T3 keeps them green).
**Placeholder scan:** store/resolver/routing logic have concrete tests; ssh-docker + save/load real paths carry behavior contracts + SDK adapt-notes (matching prior infra tasks). No TBD.
**Type consistency:** `node.Resolver`/`Resolve` used by the reconciler + faked in tests + real `RegistryResolver` in the daemon. `reconciler.New` gains a `node.Resolver` (replacing the `node.Node` arg) — all call sites (cli/api-tests/client-tests/web/mcp) updated in T3. `store.Node`, `node.PortMapping.HostIP`, `router.Route.Upstream` (host) + `.Port` consistent across T1/T2/T4/T5. Image methods `ImagePresent/SaveImage/LoadImage` consistent across node.go/local.go/fake.go/reconciler.
**Cross-task note:** T3 changes `reconciler.New`; T5 finishes the daemon wiring. All tasks keep `go test ./...` green without Docker/ssh; the remote path is exercised by T6's guarded e2e.
