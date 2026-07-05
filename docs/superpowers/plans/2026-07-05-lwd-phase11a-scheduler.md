# lwd Phase 11a Implementation Plan — capacity + requirements + scheduler + pools

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`).

**Goal:** Place an unpinned surface on the most-free node in its pool that meets its declared requirements, using live per-node capacity — while single-node stays zero-config and pinned `node=` is unchanged.

**Architecture:** A new pure `internal/scheduler` package does placement math over node capacity snapshots. `node.Node` gains `Capacity(ctx)` (live `/proc` locally / agent `/capacity` / `docker info` fallback for ssh). The reconciler resolves an unpinned surface to a concrete node via the scheduler before deploying and records it in the deployment snapshot. Apps optionally declare `pool` + `[requirements]`; nodes join a pool via `--pool`.

**Tech Stack:** Go 1.25+, no cgo, module `lwd`; stdlib (`os`, `syscall`, `runtime`, `bufio`, `strconv`); Docker SDK (`container.HostConfig.Resources`, `client.Info`); `internal/{node,agent,scheduler,spec,store,reconciler,api,client,cli,web,mcp}`.

## Global Constraints

- Go 1.25+; no cgo. Module `lwd`. Spec: `docs/superpowers/specs/2026-07-05-lwd-phase11a-scheduler-design.md`; north star `docs/VISION.md`.
- **Capacity is LIVE usage** (not reservations). `node.Capacity{CPUCores int, CPUUsed float64, MemTotal int64, MemAvailable int64, DiskTotal int64, DiskFree int64, Known bool}`.
- **Scheduler is a placement engine, NOT k8s:** understands CPU/RAM/storage/pools, never applications, never elects primaries, never schedules resources — surfaces only.
- **Placement = most-free (spread):** among reachable in-pool nodes that FIT the requirements, pick most free memory; tie-break most free CPU, then node name ascending.
- **App declares (all optional):** `pool="..."` (default `"default"`) + `[requirements]` `cpu` (cores, float) / `memory` (size string like `"512M"`). A hard `node=` pin WINS (scheduler skipped; pool ignored, not an error).
- **`node=""` means "schedule"; `node="local"` means "pin local".** `spec.Parse` STOPS defaulting `""`→`"local"` (preserves `""`); `RegistryResolver.ResolveMeta("")` and the compose path still resolve `""`→local, so only the surface deploy path adds scheduling. Single-node: `""` schedules among one candidate = local → identical result (non-regression).
- **Pools are implicit by membership:** `node add … --pool <name>`; no `pool create`. `local` node ∈ `"default"`.
- **Docker enforcement:** declared requirements set `--cpus`/`--memory` on the surface container (`RunSpec.CPUs`/`MemoryBytes` → `HostConfig.Resources.NanoCPUs`/`Memory`); unset (0) → no limit.
- **Zero regression:** single-node, pinned-node, and compose paths unchanged. All tests use fakes; `go test ./...` passes with **no Docker**. Non-Linux dev host: `Local.Capacity` returns `Known=false` gracefully, never errors the daemon/tests.
- **No secret** in capacity/pool output. Agent `/capacity` is authed like every agent route.
- README + skill + VISION updated in the final task.

---

### Task 1: `node.Capacity` type + `Node.Capacity` (Local /proc + remote docker-info + Fake)

**Files:** Create `internal/node/capacity.go`, `internal/node/capacity_test.go`; Modify `internal/node/node.go` (interface), `internal/node/local.go` (impl + remote flag), `internal/node/fake.go`.

**Interfaces produced (consumed by T3, T6, T7):**
- `type node.Capacity struct { CPUCores int \`json:"cpu_cores"\`; CPUUsed float64 \`json:"cpu_used"\`; MemTotal int64 \`json:"mem_total"\`; MemAvailable int64 \`json:"mem_available"\`; DiskTotal int64 \`json:"disk_total"\`; DiskFree int64 \`json:"disk_free"\`; Known bool \`json:"known"\` }`
- `Node` interface gains `Capacity(ctx context.Context) (Capacity, error)`.
- `Local` gains an unexported `remote bool` field: `NewLocal()` → false; `NewRemoteSSH()` → true (set it in `newLocalWithClient` via a param, or set the field after構築 — check the exact constructor). `Local.Capacity`:
  - `remote == false` (controller/agent's own host): read live `/proc` — `readProcCapacity()` in capacity.go: parse `/proc/meminfo` (`MemTotal:`, `MemAvailable:` — kB → bytes), `/proc/loadavg` (first field → `CPUUsed`), `runtime.NumCPU()` → `CPUCores`, `syscall.Statfs("/", &st)` → `DiskTotal = st.Blocks*st.Bsize`, `DiskFree = st.Bavail*st.Bsize`. `Known=true`. If ANY read fails (non-Linux dev host: no `/proc`), return `Capacity{CPUCores: runtime.NumCPU(), Known: false}, nil` (NOT an error) so the daemon/tests never break.
  - `remote == true` (docker-over-ssh node): call `l.cli.Info(ctx)` → set `CPUCores = info.NCPU`, `MemTotal = info.MemTotal`, `MemAvailable = info.MemTotal` (live usage unavailable over the Docker API), `CPUUsed = 0`, `Known = false`. On `Info` error → return the error (the node is unreachable).
- `Fake` gains `Cap Capacity` + `CapErr error` fields; `Fake.Capacity` returns `(f.Cap, f.CapErr)` and appends `"Capacity"` to `Calls`.

- [ ] **Step 1: failing tests** (`capacity_test.go`): `TestParseProcMeminfo` — feed a fixture meminfo string to a helper `parseMeminfo(io.Reader) (total, avail int64, err error)` (factor the parse out so it's testable without a real `/proc`): `"MemTotal:  16384000 kB\nMemAvailable: 8192000 kB\n"` → total `16384000*1024`, avail `8192000*1024`. `TestParseLoadavg` — `parseLoadavg1(io.Reader) (float64, error)` on `"0.50 0.40 0.30 1/234 5678"` → `0.5`. `TestFakeCapacity` — `Fake{Cap: Capacity{CPUCores:4, MemAvailable: 1<<30, Known:true}}` returns it; `CapErr` returns the error. (Local.Capacity's live path is environment-dependent; assert only that on THIS test host it returns nil error and, if Linux, `Known==true && CPUCores>0`, else `Known==false` — a build-tag-free runtime check `runtime.GOOS`.)
- [ ] **Step 2:** `go test ./internal/node/ -run 'Capacity|Meminfo|Loadavg' -v` → FAIL.
- [ ] **Step 3:** implement capacity.go (`Capacity` type, `parseMeminfo`, `parseLoadavg1`, `readProcCapacity`), the interface method, `Local.remote` + `Local.Capacity`, `Fake.Capacity`. Wire `remote=true` in `NewRemoteSSH`.
- [ ] **Step 4:** `go test ./internal/node/ -v` PASS; `CGO_ENABLED=0 go build ./...`; `go vet ./internal/node/`; `gofmt -l internal/node` empty; `go test ./...` once (all Node implementers still compile — `AgentNode` gets its Capacity in T3, so THIS task will break the `var _ Node = (*AgentNode)(nil)` assertion → add a temporary `AgentNode.Capacity` stub returning `Capacity{}, fmt.Errorf("not implemented")` here, replaced in T3, OR sequence so the build stays green: add the stub now and note it). Add the stub to keep the build green.
- [ ] **Step 5:** commit `feat: node.Capacity (live /proc local, docker-info remote, fake)`.

---

### Task 2: `RunSpec` CPU/memory limits → Docker

**Files:** Modify `internal/node/node.go` (`RunSpec`), `internal/node/local.go` (`RunContainer`), `internal/node/local_test.go` (or the node test that covers RunContainer).

**Interfaces produced (consumed by T7):** `RunSpec` gains `CPUs float64` (cores; 0 = no limit) and `MemoryBytes int64` (0 = no limit). `Local.RunContainer` sets, on the `HostConfig`: `hostCfg.Resources.NanoCPUs = int64(spec.CPUs * 1e9)` when `spec.CPUs > 0`; `hostCfg.Resources.Memory = spec.MemoryBytes` when `spec.MemoryBytes > 0`. (`container.HostConfig` embeds `Resources`.) Unset → fields stay 0 → Docker applies no limit (unchanged).

- [ ] **Step 1: failing test** — a `Local`-level test is hard without Docker; instead assert the plumbing via a focused unit: extract a tiny pure helper `applyResourceLimits(hostCfg *container.HostConfig, cpus float64, memBytes int64)` and test it: `cpus=0.5` → `NanoCPUs==500000000`; `memBytes=536870912` → `Memory==536870912`; both 0 → both 0. Call the helper from `RunContainer`. (This keeps the Docker-limit logic testable without a daemon.) Also assert `node.Fake.LastRunSpec` carries `CPUs`/`MemoryBytes` (the Fake already records `LastRunSpec` — a test constructs a `RunSpec` with them and checks the fake preserved them; the real enforcement is the helper test).
- [ ] **Step 2:** `go test ./internal/node/ -run 'ResourceLimits|RunSpec' -v` → FAIL.
- [ ] **Step 3:** add the fields + `applyResourceLimits` + call it in `RunContainer`.
- [ ] **Step 4:** `go test ./internal/node/ -v` PASS; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: RunSpec CPU/memory limits applied to Docker HostConfig`.

---

### Task 3: agent `/capacity` endpoint + `AgentNode.Capacity`

**Files:** Modify `internal/node/wire.go` (path const), `internal/agent/server.go` (route+handler), `internal/agent/server_test.go`, `internal/node/agent.go` (`AgentNode.Capacity`, replacing the T1 stub), `internal/node/agent_test.go`.

**Interfaces produced:** `node.PathCapacity = "/capacity"`. Agent serves `GET /capacity` (AUTHENTICATED — like every route except `/healthz`) → `writeJSON(w, 200, cap)` where `cap, err := s.node.Capacity(ctx)` (the agent's `s.node` is `NewLocal()` → reads the node's own `/proc`; on err → 500 + ErrorResponse). `AgentNode.Capacity(ctx)` GETs `baseURL+PathCapacity` with the bearer + a bounded ctx, decodes `node.Capacity`. Replace T1's `AgentNode.Capacity` stub.

- [ ] **Step 1: failing tests** — agent: `TestCapacity_RequiresAuth` (no token → 401) and `TestCapacity_ReturnsNodeCapacity` (fake local node with `Cap: Capacity{CPUCores:8, MemAvailable: 2<<30, Known:true}` → `GET /capacity` with token → 200 + that JSON). node: `TestAgentNodeCapacity` — drive the REAL `agent.NewServer(fake, tok).Handler()` via `node.NewAgentNode(url, tok)`; `Capacity` round-trips the fake's `Cap`; wrong token → error.
- [ ] **Step 2:** `go test ./internal/agent/ ./internal/node/ -run Capacity -v` → FAIL.
- [ ] **Step 3:** add `PathCapacity`; register+implement the agent handler; implement `AgentNode.Capacity`.
- [ ] **Step 4:** `go test ./internal/agent/ ./internal/node/ -v` PASS; `go test ./...`; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: agent /capacity endpoint + AgentNode.Capacity`.

---

### Task 4: `store.nodes.pool` + node pool UX (API/client/CLI) + `pool ls`

**Files:** Modify `internal/store/store.go` (+test), `internal/api/api.go` (+test), `internal/client/client.go`, `internal/cli/cli.go`.

**Interfaces produced (consumed by T7, T8):**
- `store.Node` gains `Pool string \`json:"pool"\``; column `pool TEXT NOT NULL DEFAULT 'default'` via `migrateAddPoolColumn` (mirror `migrateAddAgentURLColumn`, called in `Open`); `AddNode`/`GetNode`/`ListNodes` include it. A node added with empty pool stores `"default"` (normalize `""`→`"default"` in `AddNode`).
- API: `nodeRequest` gains `Pool string \`json:"pool"\``; `handleNodeAdd` validates it (empty OK → default; else `^[A-Za-z0-9][A-Za-z0-9_-]*$`); the `GET /nodes` status DTO gains `pool`. `GET /pools` → `[]{name string, nodes int}` (aggregate `store.ListNodes` by pool; always include `"default"`).
- client: `AddNode(ctx, name, sshHost, meshAddr, agentURL, pool string) error` (add `pool` param — update the T… no, update ALL callers now); `NodeStatus` gains `Pool`; `Pools(ctx) ([]client.Pool, error)` where `type Pool struct{ Name string \`json:"name"\`; Nodes int \`json:"nodes"\` }`.
- CLI: `node add <name> <ssh> <mesh> [--agent <url>] [--pool <name>]`; `node ls` adds a POOL column; new `pool ls` (`runPoolLs`) → table of pool + node count; register `case "pool"` → `runPool`.

- [ ] **Step 1: failing tests** — store: `TestAddNodePoolRoundTrip` (pool persists; empty→"default"); pre-11a-schema migration test (old `nodes` table w/o pool → migrates, defaults "default"). api: `TestNodeAddPersistsPool`, `TestNodeListIncludesPool`, `TestNodeAddInvalidPool` (→400), `TestPoolsEndpoint` (two nodes in pool "web", one implicit-none → counts; "default" present).
- [ ] **Step 2:** `go test ./internal/store/ ./internal/api/ -run 'Pool|pool' -v` → FAIL.
- [ ] **Step 3:** implement store column+migration, api pool field+validation+`/pools`, client `--pool` param+`NodeStatus.Pool`+`Pools`, CLI `--pool`+POOL column+`pool ls`. Update the sole `client.AddNode` caller(s) (CLI) for the new param.
- [ ] **Step 4:** `CGO_ENABLED=0 go build -o /tmp/lwd ./cmd/lwd && go test ./... && go vet ./... && gofmt -l .` green.
- [ ] **Step 5:** commit `feat: node pools (store column, --pool, pool ls, /pools)`.

---

### Task 5: `spec.App` pool + requirements + `parseSize` + `""`-preserving Parse

**Files:** Modify `internal/spec/spec.go`, `internal/spec/spec_test.go`; check `internal/spec/examples_test.go` still passes.

**Interfaces produced (consumed by T7, T8):**
- `spec.App` gains `Pool string \`toml:"pool"\`` and `Requirements *Requirements` where `type Requirements struct { CPU float64 \`toml:"cpu"\`; Memory string \`toml:"memory"\` }`.
- `func spec.ParseSize(s string) (int64, error)` — `""`→`(0,nil)`; plain digits→bytes; suffixes `K/M/G/T` (and `Ki/Mi/Gi` optional) case-insensitive → ×1000 or ×1024 (pick ONE convention: use binary 1024 for `K/M/G/T`, document it); invalid→error. Exported for T7/T8 reuse.
- `App.Requirements` accessor helpers (optional): keep raw; T7 converts via `ParseSize`.
- **`Parse` change:** DELETE the `if a.Node == "" { a.Node = "local" }` default (spec.go:108-109) so `""` is preserved. Update the doc comment on `App.Node` to state `""` = schedule / `"local"` = pin local.
- **Validate:** if `Requirements != nil`: `CPU >= 0`, and `ParseSize(Memory)` succeeds. If `Pool != ""`: matches `^[A-Za-z0-9][A-Za-z0-9_-]*$`. `Node` may be `""` (schedule), `"local"`, or a registered name — unchanged otherwise. Compose guard at spec.go:230-232 (`a.Node != "" && a.Node != "local"`) still holds (compose stays local for `""`/`"local"`).

- [ ] **Step 1: failing tests** — `TestParseSize` (""→0; "1024"→1024; "512M"→512*1024*1024; "2G"→2*1024^3; "bad"→err); `TestParsePreservesEmptyNode` (lwd.toml without `node` → `App.Node == ""`, NOT "local"); `TestParseExplicitLocalNode` (`node="local"` → stays "local"); `TestParsePoolAndRequirements` (`pool="web"` + `[requirements] cpu=0.5 memory="512M"` → fields set); `TestValidateRequirements` (bad memory → error; negative cpu → error); `TestValidatePoolName` (bad pool → error).
- [ ] **Step 2:** `go test ./internal/spec/ -run 'ParseSize|Node|Pool|Requirements' -v` → FAIL.
- [ ] **Step 3:** implement the fields, `ParseSize`, the Parse change, and Validate rules.
- [ ] **Step 4:** `go test ./internal/spec/ -v` PASS (incl. `examples_test.go`); `go test ./...` — **expect some reconciler/other tests that assumed Parse gives `Node=="local"` to surface**; if any fail, they reveal real call sites the T7 integration must handle — note them in the report but DO NOT change reconciler logic here (only fix tests that literally asserted the old `""`→"local" default, updating them to expect `""`). Build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: spec pool + requirements + ParseSize; preserve unset node as schedule`.

---

### Task 6: `internal/scheduler` — pure placement engine

**Files:** Create `internal/scheduler/scheduler.go`, `internal/scheduler/scheduler_test.go`.

**Interfaces produced (consumed by T7):**
```go
package scheduler
import "lwd/internal/node"
type NodeInfo struct { Name, Pool string; Reachable bool; Cap node.Capacity }
type Requirements struct { CPUCores float64; MemBytes int64 }
// Place picks the node in `pool` best fitting req, or an error naming why none fit.
func Place(candidates []NodeInfo, pool string, req Requirements) (string, error)
```
Algorithm (deterministic, pure):
1. `if pool == "" { pool = "default" }`.
2. Candidates = `c.Reachable && c.Pool == pool`.
3. Fit filter: a node fits if `!c.Cap.Known` OR (`req.MemBytes == 0 || c.Cap.MemAvailable >= req.MemBytes`) AND (`req.CPUCores == 0 || float64(c.Cap.CPUCores) - c.Cap.CPUUsed >= req.CPUCores`).
4. Rank fitting nodes by rank key = `freeMem` desc where `freeMem = c.Cap.MemAvailable` if `Known` else `c.Cap.MemTotal` (if that's also 0, use 0); tie-break `freeCPU = CPUCores - CPUUsed` desc (Known) else `CPUCores`; final tie-break `Name` asc.
5. Return top name, or `error` `fmt.Errorf("no node in pool %q has capacity (need cpu=%.2g memory=%d bytes)", pool, req.CPUCores, req.MemBytes)` when the fitting set is empty (distinguish an empty CANDIDATE set — "no reachable nodes in pool %q" — from a non-empty-candidates-but-none-fit set for a clearer message).

- [ ] **Step 1: failing tests** — `TestPlacePicksMostFreeMemory` (3 nodes, same pool, all fit → most `MemAvailable` wins); `TestPlacePoolFilter` (nodes in other pools excluded); `TestPlaceRequirementsFilterMem`/`Cpu` (nodes below the floor excluded); `TestPlaceUnknownTreatedAsFree` (a `Known:false` node fits any requirement and ranks by `MemTotal`); `TestPlaceTieBreakByName` (equal mem+cpu → lexical smallest name); `TestPlaceNoReachableNodes` (empty → "no reachable nodes in pool" error); `TestPlaceNoneFit` (candidates exist but all below floor → "no node…has capacity" error); `TestPlaceDefaultPool` (`pool==""` → treated as "default").
- [ ] **Step 2:** `go test ./internal/scheduler/ -v` → FAIL.
- [ ] **Step 3:** implement `scheduler.go`.
- [ ] **Step 4:** `go test ./internal/scheduler/ -v` PASS; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: scheduler placement engine (most-free spread, pool + requirements)`.

---

### Task 7: reconciler integration — schedule unpinned surfaces + resource limits + capacity in snapshot

**Files:** Modify `internal/reconciler/reconciler.go`, `internal/reconciler/reconcile.go` (probeNodes), `internal/reconciler/health.go` (`NodeHealth.Capacity`), `internal/reconciler/*_test.go`.

**Interfaces produced (consumed by T8):**
- `reconciler.NodeHealth` gains `Capacity node.Capacity \`json:"capacity"\``.
- New unexported `func (r *Reconciler) resolvePlacement(ctx context.Context, app *spec.App) (string, error)` — decides the concrete node for a SURFACE deploy:
  - If `app.Node != "" && app.Node != "local"` → return `app.Node` (pinned; no scheduling).
  - Gather candidates: `nodes, _ := r.store.ListNodes()`; build `[]scheduler.NodeInfo` for the LOCAL node (`Name:"local", Pool:"default", Reachable:true, Cap:` from `r.resolver.Resolve("local").Capacity(ctx)`) plus each store node whose `Pool == poolOf(app)` — for each, reachability from `r.reach` (if `r.reach != nil`, `_, ok := r.reach.Reachable(ctx, n.Name)`, else assume reachable) and `Cap` from `r.resolver.Resolve(n.Name).Capacity(ctx)` (on error → `Reachable:false`). (Only include the local node when the app's pool is `"default"` or unset, since local ∈ default.)
  - If `app.Node == "local"` was already handled; for `""`: `req := scheduler.Requirements{CPUCores: reqCPU(app), MemBytes: reqMem(app)}` (from `app.Requirements` via `spec.ParseSize`); `return scheduler.Place(candidates, poolOf(app), req)`.
  - `poolOf(app)` = `app.Pool` or `"default"`.
- Integration point: at the TOP of `applyImage` and `applyGit` (BEFORE the `specJSON` marshal so the snapshot captures the concrete node), for a surface app: `chosen, err := r.resolvePlacement(ctx, app); if err != nil { record StatusFailed; return err }; app.Node = chosen`. Then the existing `ResolveMeta(app.Node)` resolves the concrete node and the rest is unchanged. (Compose path: leave as-is — `applyCompose` keeps `ResolveMeta(app.Node)` which maps `""`→local; compose isn't scheduled.)
- Pass requirements into the surface `RunSpec`: in `deployBlueGreenSurface`, set `runSpec.CPUs = reqCPU(app)` and `runSpec.MemoryBytes = reqMem(app)` (0 when no requirements) so Docker enforces them.
- `probeNodes` (reconcile.go): for each node, also fetch `Capacity` (via `r.resolver.Resolve(name).Capacity(ctx)`, best-effort; on error leave zero `Capacity{}`) into `NodeHealth.Capacity`.
- Helpers `reqCPU(app) float64` / `reqMem(app) int64`: return 0 when `app.Requirements == nil`; else `app.Requirements.CPU` and `ParseSize(app.Requirements.Memory)` (ignore parse error → 0, since Validate already gated it).

- [ ] **Step 1: failing tests** (fakes, no Docker; `node.Fake` with settable `Cap`; a `FakeResolver` mapping names→fakes; a `fakeReach`):
  - `TestScheduleUnpinnedPicksMostFree`: two registered nodes (pool "default") with different `Cap.MemAvailable` + local; unpinned app (`Node==""`) → deployed to the most-free node; the recorded deployment's spec snapshot `Node` == that node.
  - `TestPinnedNodeSkipsScheduler`: `app.Node="web1"` → deploys to web1 regardless of capacity (even if web1 is fuller).
  - `TestSingleNodeUnpinnedGoesLocal`: no registered nodes, unpinned app → local (non-regression); snapshot `Node=="local"`.
  - `TestScheduleNoCapacityFails`: app requires more mem than any node's `MemAvailable` → `StatusFailed` recorded + clear error; live route/old deployment untouched.
  - `TestRequirementsAppliedToRunSpec`: app with `[requirements] cpu=0.5 memory="256M"` → `node.Fake.LastRunSpec.CPUs==0.5` and `MemoryBytes==256*1024*1024`.
  - `TestProbeNodesIncludesCapacity`: after `Reconcile`, `NodeHealth.Capacity` reflects the fake node's `Cap`.
- [ ] **Step 2:** `go test ./internal/reconciler/ -run 'Schedule|Pinned|SingleNode|Requirements|ProbeNodesCapacity' -v` → FAIL.
- [ ] **Step 3:** implement `NodeHealth.Capacity`, `resolvePlacement`, the applyImage/applyGit integration, the RunSpec requirement plumbing, probeNodes capacity, and helpers.
- [ ] **Step 4:** `go test ./internal/reconciler/ -race -v` PASS; `go test ./...` green; build/vet/gofmt clean. Report any call-site changes.
- [ ] **Step 5:** commit `feat: reconciler schedules unpinned surfaces + records placement + applies limits + capacity snapshot`.

---

### Task 8: capacity/pool UX (CLI node capacity/inspect, /health, web, MCP, skill) + docs + e2e

**Files:** Modify `internal/cli/cli.go`, `internal/web/*` (assets + a nodes/health handler already exist), `internal/web/fake_client_test.go`, `internal/mcp/tools.go` + `server_test.go`, `skills/lwd-toml/*`, `test/e2e_test.go`, `README.md`, `docs/VISION.md`.

- [ ] **Step 1: CLI** — `lwd node capacity` (table: NODE, POOL, CPU (used/cores), MEM (avail/total), DISK (free/total), KNOWN) from `client.Nodes` (now carrying capacity via `/nodes`, or from `client.Health`); `lwd node inspect <name>` (that node's capacity + pool + the surfaces currently placed on it — derive by scanning current deployments' spec snapshots for `Node==name`). Tests where the CLI has them; else assert via the api/client layer.
- [ ] **Step 2: web** (frontend-design skill) — show pool + capacity in the Nodes/Health views (extend the P9b Nodes view + P10 Health panel with capacity bars/text + pool); the deploy modal gains an optional pool `<select>` (from `/api/pools` or the nodes list) + optional cpu/memory requirement inputs that emit `pool` / `[requirements]` into the generated lwd.toml. Add `Pools`/capacity to the web `DaemonClient` + `fakeDaemon` as needed; a `GET /api/pools` proxy. Web tests: pool/capacity present in the nodes response; deploy modal emits `pool`/requirements.
- [ ] **Step 3: MCP** — `lwd_node_add` gains `pool`; `lwd_apply`/`lwd_deploy_git` gain optional `pool` + `requirements` (cpu/memory) that set `app.Pool`/`app.Requirements` before Validate/Apply; `lwd_node_list` includes pool + capacity. Tests assert the fields flow into the applied spec / AddNode call.
- [ ] **Step 4: skill** — `skills/lwd-toml/references/schema.md` + an example: document `pool` and `[requirements]` (cpu cores, memory size string), and that unset `node` means "let lwd place it". Keep `examples_test.go` green.
- [ ] **Step 5: e2e** (`test/e2e_test.go`, fakes, no Docker) — `TestEndToEndScheduling`: real reconciler + FakeResolver with two nodes of differing capacity; deploy an unpinned app; assert it lands on the most-free node and the snapshot records it. (No Docker needed.)
- [ ] **Step 6: verify** — `go test ./... -race`; `CGO_ENABLED=0 go build ./...` + four binaries; vet/gofmt clean. Screenshot the web capacity view if tooling allows.
- [ ] **Step 7: docs** — README "Scheduling & pools" section (unset node = auto-place; `pool`; `[requirements]`; `node ls`/`node capacity`/`node inspect`/`pool ls`; live-usage most-free placement; single-node unchanged; agent = precise capacity, ssh = best-effort). `docs/VISION.md`: mark **P11a done**, note **P11b (drain/evacuate + node-loss failover)** next. Full README pass for P1–P11a.
- [ ] **Step 8: commit** `feat: capacity/pool UX (CLI/web/MCP/skill); scheduling e2e; docs P11a`.

---

## Self-Review

**Spec coverage:** Capacity type + live sources (T1) + agent endpoint (T3); Docker limits (T2); pools store+UX (T4); spec pool/requirements + `""`-schedule semantics (T5); scheduler engine (T6); reconciler scheduling + snapshot record + limits + capacity-in-health (T7); CLI/web/MCP/skill/docs/e2e (T8). ✓
**Decisions honored:** live-usage (T1 reads /proc live; ssh docker-info Known=false); optional pool+reqs, node= wins (T5 validate + T7 resolvePlacement); most-free spread (T6); pools implicit (T4, no pool create). ✓
**Non-regression:** `spec.Parse` `""` preserved but `RegistryResolver`/compose still map `""`→local (T5 note + T7 leaves compose alone); single-node unpinned → local (T7 test); pinned skips scheduler (T7 test); `AgentNode` build kept green via a T1 stub replaced in T3; `RunContainer` limits default to 0 = no limit. ✓
**Type consistency:** `node.Capacity` (T1) used by agent (T3), scheduler `NodeInfo.Cap` (T6), `NodeHealth.Capacity` (T7), `resolvePlacement` (T7); `RunSpec.CPUs/MemoryBytes` (T2) set by T7, applied by T2's helper; `spec.App.Pool/Requirements` + `ParseSize` (T5) consumed by T7 (`reqCPU/reqMem`) + T8; `store.Node.Pool` (T4) read by T7 candidate gathering + T8; `scheduler.Place`/`NodeInfo`/`Requirements` (T6) called by T7; `client.AddNode` gains `pool` (T4) — all callers updated in T4. ✓
**Placeholder scan:** every code step has concrete code/algorithm; the two "note the failing tests" spots (T5 step 4) are explicit expected-fallout with a bounded action (fix only literal old-default assertions), not deferred design. ✓
**Cross-task risks:** (a) T1 must add the `AgentNode.Capacity` stub or the build breaks until T3 — called out. (b) T5's Parse change is the behavioral seam; T5 fixes only tests asserting the old default, and T7 adds the actual scheduling — so between T5 and T7 an unpinned app still resolves `""`→local via RegistryResolver (safe intermediate state). (c) `Local.Capacity` MUST branch on `remote` or a remote ssh node reports the controller's capacity — the single most important correctness point in T1.
