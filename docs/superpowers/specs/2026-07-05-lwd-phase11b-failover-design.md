# lwd Phase 11b — node drain/evacuate + automatic node-loss failover

**Status:** Design (decisions resolved autonomously — user away, standing directive
"keep going till it's done"; decisions documented here for later review)
**Date:** 2026-07-05
**Builds on:** P1–P11a (merged). North star: `docs/VISION.md`.
**Completes:** P11 (P11a = scheduling new deploys; P11b = moving surfaces off a
node on drain or loss).

## Goal

Make surfaces resilient to node loss and node maintenance. An operator can
**drain** a node (stop scheduling to it + move its surfaces off) or **evacuate**
it; and when a node becomes unreachable, the continuous reconciler (P10)
**automatically reschedules** its surfaces onto healthy nodes (via the P11a
scheduler) after a grace period. **Resources never move** (they fail over via
their own driver — P14/P15). Single-node stays first-class.

Per VISION: surfaces are disposable and move between machines; resources have
identity and never auto-migrate; the scheduler places, it doesn't elect
primaries.

## Resolved decisions (2026-07-05)

1. **Only SCHEDULED surfaces move.** A surface deployed from an unpinned spec
   (`node=""`, placed by the scheduler) is movable. A surface with an explicit
   `node=` pin is a **contract** — on a lost/drained node it is reported
   (degraded / "pinned, not evacuated") and left in place, never silently
   relocated. This requires recording **placement provenance** (a `Scheduled`
   flag) on the deployment, because P11a rewrites the snapshot's `Node` to the
   concrete chosen node (so pinned and scheduled snapshots otherwise look
   identical).
2. **Resources never move.** Failover/evacuate act only on `lwd.role=surface`
   containers. Compose apps and `[[services]]` backing are never rescheduled.
3. **Auto-failover has a grace period.** A node must be continuously unreachable
   for `LWD_FAILOVER_GRACE` (default `60s`) before its scheduled surfaces are
   evacuated — avoids flapping on a transient blip. Recovery resets the timer.
4. **Auto-failover never targets the local node.** If the controller's own
   Docker is down, the daemon is effectively down; there is nowhere to fail over
   *to* from the controller's perspective, and a network partition must not make
   the controller evacuate everything. Only registered remote nodes fail over.
5. **Drain = cordon + evacuate.** `node drain` marks a node unschedulable
   (cordon) AND evacuates its scheduled surfaces. `node uncordon` clears the
   cordon. `node evacuate` moves scheduled surfaces off without cordoning.
6. **No rebalancing / no fail-back.** A recovered node is not automatically
   re-populated; its stale (superseded) surface containers are cleaned up
   best-effort, and it becomes available for future placements. Surfaces that
   moved stay where they were rescheduled. (A rebalancer is out of scope — YAGNI.)

## Data model

- `store.nodes` gains `schedulable INTEGER NOT NULL DEFAULT 1` (1 = schedulable,
  0 = cordoned). Guarded migration (mirror the P11a `pool` migration).
  `store.Node.Schedulable bool`.
- `store.deployments` gains `scheduled INTEGER NOT NULL DEFAULT 0` (1 = placed by
  the scheduler / movable; 0 = pinned or legacy). Guarded migration.
  `store.Deployment.Scheduled bool`. Set to 1 by the reconciler when
  `resolvePlacement` actually scheduled (the app's original `Node` was `""`).

## Scheduler change

- `scheduler.NodeInfo` gains `Schedulable bool`. `Place` excludes a candidate
  that is `!Schedulable` (a cordoned node accepts no NEW placements). The local
  node is always `Schedulable:true` (you cannot cordon local — it is the
  controller). This is the ONLY change to the pure engine; ranking/fit unchanged.
- `resolvePlacement` (P11a) now: (a) sets each store candidate's `Schedulable`
  from `store.Node.Schedulable`; (b) records provenance so the caller can mark
  the deployment `Scheduled`. Provenance: `resolvePlacement` returns
  `(nodeName string, scheduled bool, err error)` where `scheduled` is true iff it
  invoked the scheduler (original `app.Node == ""`). The deploy records
  `Scheduled: scheduled` on the `store.Deployment`.

## Rescheduling one surface

New reconciler method (holds `r.mu`, reuses the blue-green machinery):
`func (r *Reconciler) rescheduleSurfaceLocked(ctx, cur *store.Deployment, excludeNode string) (*store.Deployment, error)`:
- Reconstruct `spec.App` from `cur.Spec`; it is only called for **scheduled**
  surfaces (`cur.Scheduled`), so it re-runs placement with `excludeNode` removed
  from the candidate set, picks a new node, and deploys there via
  `deployBlueGreenSurface` (health-gated, route flipped) — a fresh
  `StatusRunning` on the new node, `Scheduled:true`.
- The OLD surface on `excludeNode` is removed best-effort (the old deployment row
  retired). If `excludeNode` is unreachable (the node-loss case), removal is
  skipped (logged) — the container dies with its node; the route already points
  at the new one.
- If placement finds no other fitting node → the reschedule fails; the surface
  stays reported degraded/failed (no healthy capacity elsewhere), live route
  untouched where possible.
- Implemented by extending `resolvePlacement` to accept an `exclude` set (or a
  small `placeExcluding(ctx, app, exclude string)` helper) so the target node is
  not chosen again.

## Evacuate + drain (operator-initiated)

New reconciler method `EvacuateNode(ctx, name string) (EvacuateResult, error)`
(takes `r.mu`): for each app whose current deployment is a **surface** placed on
`name`:
- if `cur.Scheduled` → `rescheduleSurfaceLocked(ctx, cur, name)`; collect
  moved/failed.
- else (pinned) → skip, record in `Skipped` (pinned surfaces are not moved).
Returns `EvacuateResult{ Moved []string, Skipped []string, Failed []struct{App,Err} }`.
`node evacuate <name>` calls it. `node drain <name>` = set `schedulable=0`
(cordon) THEN `EvacuateNode`. `node uncordon <name>` = set `schedulable=1`.

- API: `POST /nodes/{name}/drain`, `POST /nodes/{name}/evacuate`,
  `POST /nodes/{name}/uncordon` (return the EvacuateResult where applicable).
- client + CLI: `lwd node drain <name>`, `lwd node evacuate <name>`,
  `lwd node uncordon <name>`; `node ls`/`node capacity` show a SCHEDULABLE column
  (cordoned nodes flagged).

## Automatic node-loss failover (P10 loop)

The reconciler tracks, per registered node, when it was first observed
unreachable: `unreachableSince map[string]time.Time` (guarded by `r.mu`; updated
in the reconcile pass). Each pass, in `Reconcile` (after the per-app + node
probes):
- For each registered node (never `local`): if reachable now → clear its
  `unreachableSince`. If unreachable → set `unreachableSince[name]` if unset;
  if `now - unreachableSince[name] >= LWD_FAILOVER_GRACE` → **evacuate** its
  scheduled surfaces (`EvacuateNode`-style reschedule, excluding the dead node),
  then clear the timer (so it doesn't re-fire every pass; a still-present dead
  surface that couldn't move will be retried on the normal heal/backoff path).
- Failover reuses `rescheduleSurfaceLocked` (removal of the old container is
  skipped since the node is unreachable). It only moves `cur.Scheduled` surfaces
  on the dead node; pinned ones are left degraded (reported in the health
  snapshot).
- The health snapshot gains, per node, a `Cordoned bool` and (per app) the
  existing surface state already covers "healing/degraded"; a failed-over app
  becomes `SurfaceHealthy` on its new node.

Grace + local-exclusion together prevent both flapping and
partition-induced mass-evacuation. `LWD_FAILOVER_GRACE` is a Go duration
(default `60s`; `<=0` → default, mirroring `ReconcileInterval`).

## Config

- `LWD_FAILOVER_GRACE` — continuous-unreachable duration before auto-failover;
  default `60s`.

## Concurrency & safety

- `EvacuateNode`/`rescheduleSurfaceLocked`/the failover step all hold `r.mu`
  (they mutate deployments/routes) — serialized with Apply/Remove/Rollback/heal,
  same as P10. `unreachableSince` + the cordon reads happen under `r.mu`.
- A cordon set via the API invalidates nothing in the resolver (cordon only
  affects NEW placement, not the transport cache).
- Failover never runs for `local`; a scheduling failure during failover/evacuate
  is logged + surfaced (degraded), never panics the loop.

## Testing (fakes, no Docker)

- provenance: an unpinned deploy records `Scheduled:true`; a pinned deploy
  `Scheduled:false`.
- scheduler: a `Schedulable:false` candidate is excluded from `Place`.
- `rescheduleSurfaceLocked`: a scheduled surface on node A → redeployed to node B
  (excluding A), new StatusRunning on B, old retired; no other fitting node →
  error, live route untouched.
- `EvacuateNode`: moves scheduled surfaces, skips pinned (reported), returns the
  result; resources/compose untouched.
- drain = cordon + evacuate (schedulable flips to 0 + surfaces moved); uncordon
  flips back; a cordoned node gets no NEW placements (scheduler excludes it).
- auto-failover: a node unreachable < grace → NOT evacuated; ≥ grace → its
  scheduled surfaces rescheduled to a healthy node; `local` never failed over; a
  pinned surface on the dead node left degraded; recovery clears the timer.
- api/cli/web: drain/evacuate/uncordon endpoints + commands; SCHEDULABLE column;
  auth on the web routes.
- **Zero regression:** single-node (no registered nodes → nothing to fail over /
  drain), pinned surfaces, compose, P10 heal, P11a scheduling all unchanged;
  `go test ./... -race` green without Docker.

## Out of scope

- Rebalancing / fail-back to a recovered node (YAGNI).
- Resource failover / migration (P14/P15 driver model).
- Surface replicas (P12) — failover here moves the single surface; replica-aware
  failover comes with P12.
- Draining/evacuating the local node (cannot cordon the controller).

## Guardrail check (VISION)

Extends the reconciler (evacuate/reschedule reuse the existing blue-green deploy
+ P11a scheduler; failover is a few lines in the P10 loop) — no new control
plane. It makes the UX simpler: "a machine died, my sites moved themselves" and
"I need to reboot node2 — `lwd node drain node2`, do maintenance, `uncordon`."
Resources are never touched; explicit pins are honored; the scheduler still only
understands CPU/RAM/pools. Not a k8s frontend.
