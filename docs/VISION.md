# lwd v2 — Vision & Roadmap

**Status:** North-star (agreed 2026-07-05). Defines the end state; guides every phase.

## What lwd is

lwd is an **opinionated deployment platform for people who own their infrastructure**.
Not another Kubernetes, Docker wrapper, or PaaS.

> Buy three machines. Install Linux. Install lwd. Deploy software with the UX of
> Vercel, the resilience of Fargate, and the operational simplicity of Docker Compose.

The philosophy is Unix: each component does one thing well; lwd **composes** battle-
tested software rather than replacing it. Docker stays Docker, Caddy stays Caddy,
Postgres stays Postgres, Git stays Git. **lwd is the glue.**

## Design principles

- **Simplicity beats flexibility.** If a feature can't be explained in a paragraph, it
  probably doesn't belong. Users must not need to understand distributed systems to
  deploy a website.
- **Infrastructure disappears.** Users think about applications, databases, storage,
  domains — never containers, overlay networks, orchestration, or scheduler internals.
- **Opinionated over configurable.** One ingress, one networking model, one deploy
  workflow, one rollback workflow. Every option has a maintenance cost.
- **Build on standards.** OCI images; WireGuard networking; Caddy reverse proxy; TOML
  config; env-var secrets; Docker runtime (containerd later — runtime differences never
  exposed to the user).
- **Product feels like Vercel; infra feels like VMware.** The stack stays readable top
  to bottom, every layer independently replaceable. No magic, no hidden state, no giant
  control plane.

## The guardrail (governs all v2 work)

> Do not evolve lwd into a frontend for Kubernetes. Evolve it into a **distributed
> version of its current reconciler**. Preserve the existing abstractions — **Node,
> Router, Reconciler, Store** — extending them for federation rather than replacing
> them. Every new subsystem must justify its existence by making the user experience
> **simpler**, not by increasing theoretical capability.

## Mental model — three kinds of things

1. **Surfaces** — stateless. Scale horizontally, move between machines, blue/green,
   roll back instantly, disposable. (APIs, sites, workers, cron.)
2. **Resources** — stateful. Have identity, own storage, **never** blue/green, **never**
   auto-migrate. They **fail over** via their own driver. (Postgres, Valkey, MinIO,
   queues, volumes, backups.)
3. **Nodes** — dumb compute (CPU/RAM/storage/networking). Nodes don't know about
   applications; applications are scheduled onto nodes.

## Architecture (intentionally boring)

```
Git → lwd Controller ─┬─ Fleet Reconciler (continuous)
                      ├─ Scheduler (placement engine, not k8s)
                      ├─ API
                      ├─ Secret Store
                      ├─ Resource Drivers
                      └─ Router Manager ──► Edge Nodes (Caddy)
Workers └─ lwd-agent (dumb) └─ Docker
Everything on plain Linux. WireGuard mesh. No custom kernel/init/fs, no service mesh.
```

- **Controller** owns desired state and is **not on the request path**. If it crashes:
  apps keep serving; deployments pause; nothing else breaks.
- **Agent** (per node) is **intentionally dumb**: execute Docker ops, report health +
  capacity, stream logs, execute deployments. Nothing more.
- **Scheduler** is a placement engine: given desired replicas + available capacity +
  requirements/labels, find a valid placement. It understands CPU/RAM/storage/labels —
  **not** applications. It never elects database primaries.
- **Networking**: all nodes join a WireGuard mesh; apps talk over private addresses;
  edges run Caddy and route to healthy replicas. No overlay/service-mesh/sidecars.
- **Storage**: volumes belong to resources; resources own volumes; volumes don't move
  automatically. If a node dies, the **database layer** decides failover, not the
  scheduler.

## Resolved v2 decisions (2026-07-05)

1. **Ordering:** resilience infrastructure first, first-class resources last (roadmap
   below).
2. **Reconciler:** a **continuous** control loop. A dead surface replica/node → the
   surface is **automatically rescheduled** elsewhere. **Resources never auto-migrate**
   — their driver (e.g. Patroni) handles failover.
3. **Edge/ingress:** **N Caddy edges** each fed identical controller-pushed route
   config; the domain's DNS round-robins across edge IPs; a dead edge is dropped by
   DNS/health. (A single central Caddy is the starting point in P9.)
4. **Resources:** a **driver model** (postgres/valkey/minio/volume/backup), evolving
   today's generated-compose backing. **Single-mode first** (one node, local disk,
   backups); **HA later via Patroni** (streaming replication + auto-promotion).

## Roadmap (each phase ships working software; each extends the reconciler)

- **P1–P8 — DONE (merged):** core deploy; HTTPS/blue-green/rollback; secrets; compose
  apps; web UI (lwd-web); git deploy + build-from-source + backing services; lwd.toml
  authoring skill; local MCP (lwd-mcp).
- **P9a — Federation foundation — DONE (merged):** node registry (`lwd node
  add/ls/rm`) + docker-over-ssh transport (Docker SDK ssh conn-helper, no custom
  agent yet) + `docker save|load` image movement (registry pull on the target
  tried first) + explicit `node=` placement, resolved per-deploy via
  `node.Resolver` + WireGuard-mesh-address routing (central Caddy's upstream
  becomes `<meshAddr>:<port>` for a remote surface; unchanged container-name
  routing for local). `image`/`[git]` apps (with or without `[[services]]`,
  backing services also targeted at the node's own daemon) are remote-capable;
  `compose=` apps are guarded local-only for now (`applyCompose` doesn't yet
  thread a resolved node's `DOCKER_HOST` through). Single-node path fully
  unchanged. See `README.md`'s [Multi-node](../README.md#multi-node-federation)
  section.
- **P9b — next:** the dumb `lwd-agent` (replacing raw docker-over-ssh transport),
  node capacity/health reporting, and web/MCP node UX (node choice + node list
  surfaced in `lwd-web`/`lwd-mcp`, currently CLI/API-only).
- **P10 — Continuous reconciler:** apply-time → control loop; self-heal crashed
  replicas/containers; observe node/edge health.
- **P11 — Scheduler + capacity + pools:** nodes report capacity; apps declare
  requirements; placement across nodes/pools; `node inspect/capacity/drain/evacuate`,
  `pool create`. **Surface failover reschedules here** on node loss.
- **P12 — Surface replicas + LB:** `lwd scale api N`; Caddy load-balances across healthy
  replicas; blue/green across the replica set. Human scaling only (no autoscaler).
- **P13 — Multi-edge routing:** N Caddy edges + DNS round-robin; edge-failure resilience.
- **P14 — Resource drivers (single-mode):** postgres/valkey/minio/volume/backup as
  first-class drivers with lifecycle; single node, local disk.
- **P15 — Resource HA + backups:** Patroni Postgres, driver-level failover, scheduled
  backups + restore.

Single-node stays a first-class, zero-mesh path throughout (the "one box" experience
must never regress as federation lands).

## Success criteria

Someone buys three second-hand mini PCs, installs Linux + lwd, and within an hour has
HA applications, blue/green deploys, automatic HTTPS, rolling upgrades, node
maintenance, capacity management, PostgreSQL, object storage, and backups — **without
ever learning Kubernetes**.

## Stack (every layer independently replaceable)

```
Linux → Docker → WireGuard → Caddy → lwd-agent → lwd Controller → Applications
```
