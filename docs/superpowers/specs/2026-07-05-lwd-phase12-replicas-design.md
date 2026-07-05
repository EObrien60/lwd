# lwd Phase 12 — surface replicas + load balancing

**Status:** Design (decisions resolved autonomously — user away, "keep going till
it's done"; decisions documented here for later review)
**Date:** 2026-07-05
**Builds on:** P1–P11 (merged). North star: `docs/VISION.md`.

## Goal

Run **N replicas** of a surface, load-balanced by Caddy across healthy replicas,
with the replica set spread across nodes and deployed blue-green as a set. Human
scaling only (`lwd scale api 3` / `replicas = 3`) — **no autoscaler**. Per-replica
self-heal (P10) and node-loss failover (P11b) fall out of the set model. Resources
are never replicated by lwd (a resource's own driver owns HA — P14/P15).

Per VISION: surfaces scale horizontally and move between machines; the edge routes
to healthy replicas; the scheduler places, humans decide the count.

## The core model decision

Today: one app → one current `StatusRunning` deployment → one `ContainerID` → one
Caddy upstream. P12: one app → one current deployment **generation** (still ONE
`store.Deployment` row) that carries a **set of replicas**.

- `store.Deployment` gains `Replicas []Replica` (stored as a JSON column
  `replicas`), where `type Replica struct { ContainerID string; Node string;
  Upstream string; Port int }` (Upstream = the Caddy target: a container name for
  a local replica, or a node mesh address for a remote one; Port = the container
  port or the published mesh host port — exactly today's single-surface upstream
  rule, per replica). The existing `ContainerID` column is retained and set to
  `Replicas[0].ContainerID` for back-compat / older readers.
- **`replicas = 1` (the default) degrades to today's exact behavior**: a
  single-element set, one upstream, single-container blue-green — this is the
  non-regression contract and every existing test must keep passing.
- `spec.App` gains `Replicas int \`toml:"replicas"\`` (default 1; `spec.Parse`
  defaults 0→1; Validate: `>= 1`, and a reasonable cap e.g. `<= 50` to catch
  fat-finger; replicas only valid for a **surface** — image/git — not a compose
  app: Validate rejects `replicas > 1` with `compose`).

## Routing — multi-upstream + passive health

- `router.Route` gains `Upstreams []Upstream` (`type Upstream struct { Host
  string; Port int }`). The existing `Upstream string`/`Port int` fields are
  removed in favor of `Upstreams` (a 1-element slice reproduces today's route);
  `SetRoute`/`SetStaging`/`SeedRoutes`/`GenerateCaddyfile` updated to render a
  set. (This is an internal type change — audit all constructors.)
- `GenerateCaddyfile`: for a route with upstreams `[h1:p1, h2:p2, ...]` emit
  `reverse_proxy h1:p1 h2:p2 ...` plus, when `len > 1`, `lb_policy round_robin`
  and passive health (`fail_duration 30s` inside the reverse_proxy block) so
  Caddy **drops a failing replica** at the edge immediately — the edge-level
  resilience that makes replicas useful even before lwd's own heal runs. A single
  upstream renders exactly as today (no lb_policy/fail_duration → byte-identical
  Caddyfile for N=1, preserving the existing caddyfile golden tests).
- Staging (blue-green probe) stages the NEW replica set behind the throwaway host
  and probes THROUGH Caddy as today (the probe hits the set; round-robin means it
  reaches replicas).

## Placement — spread the set

- The reconciler places each of the N replicas via the P11a scheduler, **spreading
  across distinct nodes**: place replica 1 normally; place replica k excluding the
  nodes already chosen for replicas 1..k-1 (via `placeExcluding` extended to a set,
  or an exclude-set variant `placeExcludingSet(app, exclude []string)`); if fewer
  schedulable nodes than replicas, fall back to reusing the most-free node (stack
  is allowed only when unavoidable). A hard `node=` pin places ALL replicas on that
  node (pinned = explicit; no spread). Requirements apply per replica.
- Single-node: all replicas land on local (only candidate) — a 3-replica app on
  one box runs 3 containers on it (valid; the LB still balances locally).

## Deploy — set-based blue-green

`deployReplicaSet` (generalizes `deployBlueGreenSurface`):
1. Resolve placement for N replicas (spread). Ensure image on each target node.
2. Run N new containers (unique names `lwd-<app>-<deployID>-<i>`), each connected
   to the lwd network (+ backing network if any); for a remote replica publish the
   ephemeral mesh port (as today). Collect each replica's Upstream/Port.
3. Stage the whole new set behind the throwaway host; health-gate: each replica
   must pass the layered health check (path 2xx / Docker health / liveness).
   **All replicas must pass** for the generation to go live (a partially-healthy
   set is a failed deploy — the old set keeps serving). (Health probes per replica
   by container; the staged Caddy route points at the whole new set.)
4. Flip the live route to the new set's upstreams; retire + remove the OLD set's
   containers; record a `StatusRunning` deployment with `Replicas` = the new set.
5. On any failure: remove the new containers, drop staging, record `StatusFailed`;
   the old set + live route are untouched (blue-green isolation, now set-wide).

`replicas=1` makes this exactly the current single-surface flow.

## Scale

- `lwd scale <app> <N>` — reads the app's current deployment spec snapshot, sets
  `Replicas = N`, and redeploys (a normal set-based blue-green to the new count).
  Scale up adds replicas; scale down deploys a smaller set (extra old containers
  retired in step 4). Human-only; no autoscaler. `POST /apps/{name}/scale {replicas}`.
- `lwd status` / `lwd ls` show replica count + per-replica node/health.

## Heal (P10) — per replica

`reconcileApp` iterates the current deployment's `Replicas`: for each replica whose
container is dead (missing/not-running on its node), recreate THAT replica on its
node (same node — heal is in-place, like today) and update the route's upstream set;
backoff per app as today. A replica on an unreachable node is left to failover
(P11b), not healed. Caddy passive health already keeps traffic off the dead replica
meanwhile. (If ALL replicas are dead → same as today's full-app heal.)

## Failover (P11b) — per replica

`EvacuateNode`/auto-failover move only the replicas of a **scheduled** surface that
sit on the target node (not the whole app): reschedule those replicas to other
nodes (excluding the dead one AND the app's other live replica nodes, to preserve
spread), update the route's upstream set, retire the moved replicas' old entries.
Pinned surfaces' replicas are left (reported). A single-replica app failing over is
exactly today's P11b behavior.

## Remove / rollback

- `Remove`: remove ALL of the current deployment's replica containers (+ backing as
  today), drop the route, retire.
- `Rollback`: restore the previous generation — redeploy its recorded `Replicas`
  count + spec (the set-based deploy), preserving `Scheduled` provenance (P11b).

## CLI / API / web / MCP / skill

- `lwd scale <app> <N>`; `lwd status`/`ls` show replicas + per-replica placement;
  `POST /apps/{name}/scale`; `client.Scale`.
- web: a replicas control on the app view (scale up/down) + per-replica node/health;
  the deploy modal gets an optional replicas input → `replicas = N` in the toml.
- MCP: `lwd_scale` tool; `replicas` on `lwd_apply`/`lwd_deploy_git`.
- skill: document `replicas`.

## Config

None new (count is per-app in the spec; health/reconcile knobs already exist).

## Security

No new secret surface; replica info is names/nodes/ports only.

## Testing (fakes, no Docker)

- **Non-regression FIRST:** replicas=1 → single-element set, single upstream,
  Caddyfile byte-identical to today (existing golden tests pass unchanged);
  heal/failover/remove/rollback for a 1-replica app behave exactly as P10/P11.
- multi-upstream Caddyfile (N>1 → round_robin + fail_duration; N=1 → no LB
  directives); Route with Upstreams round-trips.
- placement spreads N replicas across distinct nodes; falls back to stacking when
  nodes < replicas; pinned → all on the pin.
- set blue-green: N new containers, all-must-be-healthy gate, route flips to the
  set, old set retired; a partially-unhealthy new set → StatusFailed, old set + route
  untouched.
- scale up (1→3 adds replicas) / scale down (3→1 retires extras); scale rejected on
  a compose app.
- heal recreates ONE dead replica (others untouched), route updated; failover moves
  only the dead node's replicas, preserving spread.
- remove kills all replicas; rollback restores the prior count.
- api/cli/web/mcp scale surfaces; Validate replicas>=1, <=cap, not-with-compose.
- Zero regression: `go test ./... -race` green without Docker; single-node,
  compose, pinned, P10 heal, P11 scheduling/failover all intact for N=1.

## Out of scope

- Autoscaling (human-only, by design).
- Session affinity / sticky sessions / weighted LB (round-robin only; revisit if
  needed).
- Active (endpoint) health checks at the edge (passive fail_duration only; lwd's own
  health-gate covers readiness at deploy time).
- Replicated **resources** — never (resources are single-writer; HA is the driver's
  job, P14/P15).

## Guardrail check (VISION)

Extends the existing surface/deploy/route/scheduler/heal/failover machinery to a set
of size N (N=1 = today), rather than adding a new subsystem. The user thinks
"`lwd scale api 3`" and gets three load-balanced, spread, self-healing replicas — no
new distributed-systems concepts. Not a k8s frontend: no ReplicaSets/Deployments
controllers, no pod abstraction, just "run N of this surface."
