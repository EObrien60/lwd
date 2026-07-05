# lwd Phase 11a — Capacity + requirements + scheduler + pools

**Status:** Design (decisions resolved)
**Date:** 2026-07-05
**Builds on:** P1–P10 (merged). North star: `docs/VISION.md`.
**Splits:** P11 → **P11a** (this: schedule NEW deploys onto a valid node) + **P11b**
(drain/evacuate + automatic node-loss failover, later).

## Goal

Let lwd place a surface on a suitable node automatically instead of only on a
hard-pinned `node=`. Nodes report **live** capacity; apps optionally declare a
**pool** and **requirements**; a small **placement engine** picks the node in
the pool with the most free capacity that meets the requirements. Single-node
stays first-class and zero-config.

Per VISION: the scheduler is a placement engine, **not** Kubernetes — it
understands CPU/RAM/storage/pools, never applications, and never elects database
primaries. Resources are never scheduled (only surfaces). Preserve
Node/Router/Reconciler/Store; extend, don't replace.

## Resolved decisions (2026-07-05)

1. **Split** P11 into P11a (this) + P11b (drain/evacuate/failover).
2. **Capacity = live usage.** Nodes report current utilization (not
   reservations). Placement reads live free capacity.
3. **App declares (all optional): `pool` + `[requirements]` (cpu, memory).**
   Both optional with sane defaults; the existing hard `node=` pin is unchanged
   and wins. Node labels / label-selectors are deferred (pools are the grouping).
4. **Placement = most-free (spread).** Among fitting candidates, pick the node
   with the most free memory (tie-break: most free CPU, then node name).

## Capacity model

`node.Capacity` (new type in package `node`):
```go
type Capacity struct {
    CPUCores     int     `json:"cpu_cores"`      // total logical CPUs
    CPUUsed      float64 `json:"cpu_used"`       // load-derived busy cores (loadavg1); 0 if unknown
    MemTotal     int64   `json:"mem_total"`      // bytes
    MemAvailable int64   `json:"mem_available"`  // bytes free-ish (MemAvailable)
    DiskTotal    int64   `json:"disk_total"`     // bytes, of the data/docker dir filesystem
    DiskFree     int64   `json:"disk_free"`      // bytes
    Known        bool    `json:"known"`          // false when a transport can't report live usage
}
```
`node.Node` gains `Capacity(ctx context.Context) (Capacity, error)`.

Per-transport sources:
- **Local** (`internal/node/local.go`): read `/proc/meminfo` (MemTotal,
  MemAvailable), `/proc/loadavg` (1-min load → CPUUsed), `runtime.NumCPU()` (or
  `/proc/cpuinfo`) for CPUCores, `syscall.Statfs` on the lwd data dir for disk.
  Pure Go, no cgo, Linux `/proc`. `Known=true`.
- **Agent** (`AgentNode` + `internal/agent`): authed `GET /capacity` returns the
  same `node.Capacity` JSON; the agent computes it exactly as Local does (it runs
  on the node). `AgentNode.Capacity` GETs it (bearer, bounded ctx). `Known=true`.
- **docker-over-ssh** (`NewRemoteSSH`, no agent): fall back to `docker info`
  (`NCPU`, `MemTotal`) via the Docker client for totals; live CPU/mem usage is
  unavailable over the Docker API, so report `MemAvailable=MemTotal`,
  `CPUUsed=0`, `Known=false`. The scheduler treats `Known=false` as "assume
  fully free" so ssh nodes remain schedulable (best-effort; the agent is the
  precise path — documented).

Non-Linux dev hosts (e.g. the maintainer's macOS) have no `/proc`: `Local.Capacity`
returns `Known=false` with a best-effort `runtime.NumCPU()` and zeroed mem/disk
rather than erroring, so `go test` and a single-node dev daemon never break.

## App declaration (spec.App)

```toml
pool = "web"            # optional; default "default"
[requirements]
cpu = 0.5               # optional; cores (float); 0/unset = no cpu floor
memory = "512M"         # optional; parsed to bytes; "" / 0 = no memory floor
```
- `spec.App` gains `Pool string \`toml:"pool"\`` and `Requirements *Requirements`
  where `type Requirements struct { CPU float64 \`toml:"cpu"\`; Memory string
  \`toml:"memory"\` }`. Memory accepts a size string (`"512M"`, `"2G"`, or plain
  bytes) parsed by a small helper `parseSize(string) (int64, error)`.
- Validation (`spec.App.Validate`): if `Requirements` present, `CPU >= 0` and
  `Memory` parses (or empty). `Pool` matches a safe name pattern (same shape as
  a node name: `^[A-Za-z0-9][A-Za-z0-9_-]*$`) when non-empty. `node=` and `pool`
  may both be set — an explicit `node=` pin WINS and the scheduler is skipped
  (documented; pool is then ignored with a note, not an error).
- **Docker enforcement:** the declared requirements are also applied to the
  surface container as limits (`--cpus` / `--memory`) so an app can't exceed what
  it asked for — extend `node.RunSpec` with optional `CPUs float64` / `MemoryBytes
  int64` and have `Local.RunContainer` set the corresponding Docker HostConfig
  fields (`NanoCPUs`, `Memory`). Unset (0) → no limit (unchanged behavior).

## Pools

- `store.Node` gains `Pool string` (`pool` column, `TEXT NOT NULL DEFAULT
  'default'`; guarded migration mirroring the P9b `agent_url` migration). A node
  with no pool set is in `"default"`.
- `node add <name> <ssh> <mesh> [--agent <url>] [--pool <name>]`; `POST /nodes`
  accepts `pool` (validated like the app pool name); `node ls` shows a POOL
  column.
- `lwd pool ls` → distinct pool names + node count + (optionally) aggregate
  capacity per pool. **No `pool create`**: a pool exists exactly when a node
  joins it (implicit membership — simpler than a stateful pool object, which
  would carry nothing but a name). The implicit `local` node belongs to
  `"default"`.

## Scheduler (placement engine)

New package `internal/scheduler` (small, pure, testable with no Docker):
```go
type NodeInfo struct {
    Name     string
    Pool     string
    Reachable bool
    Cap      node.Capacity
}
type Requirements struct { CPUCores float64; MemBytes int64 }
// Place returns the chosen node name, or an error naming why none fit.
func Place(candidates []NodeInfo, pool string, req Requirements) (string, error)
```
Algorithm (deterministic):
1. Filter to `Reachable` nodes whose `Pool == pool` (pool defaults to
   `"default"`).
2. Filter to nodes that FIT: `!Cap.Known` (assume free) OR
   (`req.MemBytes == 0 || Cap.MemAvailable >= req.MemBytes`) AND
   (`req.CPUCores == 0 || (Cap.CPUCores - Cap.CPUUsed) >= req.CPUCores`).
3. Rank the fitting nodes by **most free memory** (`Cap.MemAvailable`, with a
   `!Known` node valued at `MemTotal`, or if that's 0, treated as most-free);
   tie-break by most free CPU, then by name ascending.
4. Return the top node, or `error` "no node in pool %q has capacity for
   cpu=%.2g memory=%s" if the fitting set is empty.

The scheduler is pure data-in/data-out; the reconciler gathers `candidates`
(from the store's nodes + `node.Capacity` per node + reachability from the P9b
resolver) and calls `Place`.

## Reconciler integration

At the top of `applyImage`/`applyGit` (and the shared surface path), BEFORE
`ResolveMeta`:
- If `app.Node != "" && app.Node != "local"` → **pinned**: behave exactly as
  today (no scheduling).
- Else (unpinned, or `"local"`): if there is exactly one candidate node (the
  local node — i.e. a single-node install) → place local (non-regression, no
  probing needed). Otherwise gather candidates (nodes in the app's pool +
  local), fetch each reachable node's `Capacity`, call `scheduler.Place`, and set
  the resolved concrete node name for the rest of the deploy. Record the chosen
  node in the spec snapshot's `Node` field (so P10 heal and P11b failover
  redeploy to the same node, and `history`/`inspect` show where it ran).
- A scheduling failure records a `StatusFailed` deployment (like a node-resolve
  failure) and returns a clear error; the live route/old deployment are
  untouched.
- Compose apps: pinned-only for scheduling in P11a (they're already local-only
  guarded from P9a); unpinned compose stays local. (Scheduling compose apps is
  out of scope.)

**`node="local"` note:** today `""` and `"local"` both mean the local node. To
allow scheduling, an UNSET `node` (`""`) means "schedule me", while an explicit
`node="local"` remains a hard pin to local. `spec.Parse` currently normalizes
`""`→`"local"`; this changes so `""` is preserved as "unpinned/schedule" and
only an explicit `"local"` pins local. Single-node: `""` schedules among one
candidate = local, so the result is identical. (This is the one behavioral
subtlety; covered by tests + documented.)

## Capacity in the health snapshot

Extend P10's `reconciler.NodeHealth` with a `Capacity node.Capacity` field; the
control loop's `probeNodes` fetches `node.Capacity` alongside reachability into
the snapshot. `GET /health`, `lwd health`, and the web Health panel show
per-node capacity (mem free/total, cpu, disk). New CLI: `lwd node capacity`
(table of nodes × capacity) and `lwd node inspect <name>` (capacity + pool +
the surfaces currently placed there, derived from current deployments' spec
snapshots).

## API / CLI / web / MCP / skill

- API: `POST /nodes` gains `pool`; `GET /nodes` includes `pool` + `capacity`
  (fetched like reachability); `GET /pools` (name, node count). 
- client + CLI: `node add --pool`, `node ls` POOL column, `node capacity`,
  `node inspect <name>`, `pool ls`.
- web: node views show pool + capacity; the deploy modal gains an optional pool
  select + requirements inputs that emit `pool`/`[requirements]` into the
  generated lwd.toml.
- MCP: `lwd_node_add` gains `pool`; `lwd_apply`/`lwd_deploy_git` already take a
  `node` arg — add optional `pool` + `requirements` passthrough.
- skill (`skills/lwd-toml`): document `pool` + `[requirements]`.

## Security

Capacity carries no secrets (CPU/mem/disk numbers only). The agent `/capacity`
endpoint is authed like every other agent route (bearer + mesh-bound). No new
secret surface.

## Testing

All with fakes, no Docker:
- `scheduler.Place`: pool filter; requirements fit/no-fit (mem + cpu); most-free
  ranking + tie-breaks; `!Known` treated as free; empty-fit error; single
  candidate.
- `node.Capacity`: `node.Fake` gets a settable `Cap node.Capacity` + `CapErr`;
  Local `/proc` parsing tested against fixture strings (inject readers) where
  practical, else a `Known=false` fallback path on a non-Linux test host;
  AgentNode↔agent `/capacity` round-trip through the real handler + a fake local
  node (mirrors the P9b agent tests); RemoteSSH `docker info` fallback → `Known=false`.
- `parseSize`: "512M"/"2G"/"1024"/invalid.
- spec: `Pool`/`Requirements` parse + validate; `""` vs `"local"` node semantics.
- reconciler: unpinned app on a multi-candidate set → scheduled to the most-free
  node (fake capacities); pinned `node=` skips the scheduler; single-node `""` →
  local (non-regression); scheduling failure → StatusFailed + clear error; the
  chosen node is recorded in the snapshot.
- RunContainer applies `--cpus`/`--memory` when requirements set (assert via
  `node.Fake.LastRunSpec` carrying CPUs/MemoryBytes).
- api/cli/web/mcp: pool on node add; capacity in GET /nodes + /health; pool ls;
  node capacity/inspect.
- **Zero regression:** existing single-node + pinned-node + compose paths
  unchanged; `go test ./...` green without Docker.

## Out of scope (P11b and later)

- `node drain` / `node evacuate` (mark unschedulable + move surfaces off) — P11b.
- Automatic cross-node **failover** (reschedule surfaces when a node is lost),
  building on P10's loop + this scheduler — P11b.
- Node labels / label-selectors; reservation-based capacity; bin-packing;
  autoscaling — not planned (YAGNI unless a need appears).
- Scheduling stateful **resources** — never (resources are pinned + fail over via
  their driver, P14/P15).

## Guardrail check (VISION)

A placement engine that understands CPU/RAM/storage/pools and nothing about
applications; it never moves resources and never elects primaries. It extends
the reconciler (one scheduler package + a `Capacity` Node method + a spec field)
and makes the UX simpler: "I have three machines, deploy this" just works,
without the user choosing a box. Single-node is untouched. Not a k8s frontend.
