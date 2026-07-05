# lwd Phase 10 — Continuous reconciler (self-heal + health observation)

**Status:** Design (decisions resolved)
**Date:** 2026-07-05
**Builds on:** P1–P9b (merged). North star: `docs/VISION.md`.

## Goal

Evolve lwd from **apply-time-only** reconciliation into a **continuous control
loop**: the daemon runs a background reconciler that periodically drives reality
toward the desired state (the store's current `StatusRunning` deployments),
**self-heals dead surface containers in place**, and **observes** node + edge
(Caddy) health — all while the controller stays **off the request path** (if it
crashes, apps keep serving; only reconciliation pauses).

Per VISION: **resources never auto-migrate** and **surfaces are not rescheduled
across nodes here** — cross-node reschedule on node loss is P11 (it needs the
scheduler). P10 heals a surface **on the same node it already runs on**, and for
node/edge health it **observes and reports only**. Single-node stays
first-class.

## Resolved decisions (2026-07-05)

1. **Heal strategy = recreate via blue-green.** A dead surface is healed by
   redeploying the current deployment's **recorded spec + already-built image**
   through the existing blue-green surface path (health-gate → flip route →
   remove dead → record fresh `StatusRunning`). Git apps reuse the recorded
   built tag (`lwd-build/<app>:<sha>`) — **no re-clone, no rebuild** — exactly as
   `Rollback` already does. Not a plain `docker start` (a corrupt/crash-looping
   container would just restart into the same failure with no health gate).
2. **Heal scope = surfaces only.** The loop actively heals only lwd-managed
   surface containers (`lwd.role=surface`: image/git single-service apps).
   Compose apps and `[[services]]` backing projects rely on Docker restart
   policies / their own drivers (consistent with "resources fail over via their
   driver"; first-class resource drivers are P14). Their health is **observed**
   but never acted on by the heal path.
3. **Node/edge health = observe + report only.** Each pass probes every
   registered node (`resolver.Reachable`) and Caddy, records it in a live
   in-memory snapshot, logs transitions, and exposes it — but takes **no
   cross-node action**. A surface on a down/unreachable node is marked degraded
   and left. Node-loss failover/reschedule is P11.
4. **Trigger = interval polling + post-Apply nudge.** A periodic ticker
   (`LWD_RECONCILE_INTERVAL`, default 15s) plus an immediate pass requested after
   each successful `Apply`. No Docker event-stream dependency (that is a possible
   later refinement); the interval is the worst-case self-heal detection latency.

## What "unhealthy" means (heal trigger)

A surface's current deployment is considered **dead → heal** when, on its
resolved node, its container is:
- **missing** (not returned by `ListContainers`/`ContainerHealth` reports
  not-found), or
- **not running** (Docker state is `exited`/`dead`/`created`/anything ≠
  `running`).

A container that is **running but Docker-`unhealthy`** (its image HEALTHCHECK is
failing) is marked **degraded in the snapshot but NOT recreated** in P10:
recreating into the same image rarely fixes a config/dependency problem and
risks churning a flapping healthcheck. (Revisit if it proves necessary.)

## Architecture

Extends the existing abstractions — no replacement:

```
runDaemon
  ├─ api.Server (unix socket)        ── request path (unchanged)
  └─ controlloop goroutine (NEW) ── ticker + nudge ──► Reconciler.Reconcile(ctx)
                                                          ├─ per app: heal dead surface (blue-green)
                                                          └─ probe nodes + Caddy → HealthSnapshot
HealthSnapshot (in-memory) ──► GET /health ──► client ──► lwd health / web panel
```

### Components

- **`Reconciler.Reconcile(ctx) error`** (new method on the existing
  `*Reconciler`). One reconciliation pass. Takes `r.mu` for its heal work so it
  **serializes with `Apply`/`Remove`/`Rollback`** — the same mutex that already
  guards those, so a manual deploy and an auto-heal can never interleave. Steps:
  1. `store.ListApps()` → for each app, `store.CurrentDeployment(app)`.
  2. Skip apps whose current deployment is a **compose app**
     (`isComposeApp(spec)` — reuse the existing helper) or has no `ContainerID`/
     spec snapshot. (Backing `[[services]]` are not separate apps; they ride
     with their surface app and are not health-checked here.)
  3. Resolve the app's node from its spec snapshot (`nodeFromSpec`, reuse). If
     the node **does not resolve or is unreachable**, record the app as
     `degraded` (node down) in the snapshot and **skip** healing (no cross-node
     action).
  4. Check the surface container's health via `node.ContainerHealth`. Running →
     `healthy`, reset the app's backoff. Missing/not-running → **heal**.
  5. Heal = `healSurface(ctx, app, currentDeployment)` (see below), guarded by
     backoff (see Crash-loop protection). Record the outcome in the snapshot.
  6. After the per-app loop, probe node + edge health into the snapshot (see
     Health observation).
  A single app's error (resolve/heal/probe) is isolated: logged, recorded in the
  snapshot, and the pass continues to the next app. `Reconcile` returns an error
  only for a whole-pass failure (e.g. `ListApps` itself failing).

- **`healSurface`** (new unexported helper). Redeploys `cur`'s recorded state
  without re-clone/rebuild, reusing the existing back-half machinery
  (`deployBlueGreenSurface` and friends). It reconstructs the `spec.App` from
  `cur.Spec`, pins `Image` to `cur.Image` (the built/registry tag already
  present or transferable), re-resolves secrets + ensures backing (idempotent),
  and runs the blue-green surface deploy. On success: new `StatusRunning` row,
  dead one set `StatusRetired`, route flipped to the healthy container. On
  failure: `StatusFailed` recorded for the attempt, the live route left as-is
  (still pointing at the dead upstream — nothing worse than before), backoff
  advanced. This is deliberately the **same path** `rollbackGit`/`applyImage`
  already use, generalized to "redeploy the CURRENT deployment's spec+image".

- **Crash-loop protection.** The reconciler tracks, per app, the count of
  consecutive failed/attempted heals and the next-eligible time (exponential
  backoff: e.g. 15s, 30s, 60s, 120s, capped). Before healing an app the loop
  checks the backoff gate; if not yet eligible it skips (still marks degraded).
  After a configurable cap (`LWD_HEAL_MAX_ATTEMPTS`, default 5) it **gives up**:
  the surface is left dead, logged once, and shown `failed` in the snapshot until
  a **manual `Apply`** (which resets the app's heal state) or the container
  coming back on its own. A successful heal or a healthy check resets the
  counter. State is in-memory (recovery from a daemon restart just re-observes).

- **Control loop** (`internal/reconciler/loop.go`, or a small
  `internal/controlloop` package — a plan-level call; keep it thin). `Run(ctx,
  rec, interval, nudge <-chan struct{})` loops on a `time.Ticker` and on nudge,
  calling `rec.Reconcile(ctx)` each time, until `ctx` is cancelled. A panic in a
  pass is recovered (logged) so one bad pass never kills the daemon. Started as a
  goroutine in `runDaemon`; `ctx` is cancelled on SIGINT/SIGTERM for graceful
  shutdown (an in-flight pass is cancelled via ctx). The daemon exposes a
  buffered `nudge` channel; `api`/`Reconciler.Apply` signals it after a
  successful apply (non-blocking send).

- **HealthSnapshot** (new, in-memory, `internal/reconciler` or a small type).
  Live state — **not persisted** (no hidden state; a restart re-derives it):
  - per node: name, transport (`agent`/`ssh`/`local`), reachable bool, last
    checked.
  - edge: Caddy reachable bool, last checked.
  - per app (surface): state ∈ {`healthy`,`degraded`,`healing`,`failed`}, last
    error, consecutive heal attempts, last transition time.
  Guarded by its own `sync.RWMutex` (API handlers read it concurrently with the
  loop writing it). The reconciler owns and updates it each pass; the API reads a
  copy.

- **Health observation.** Node reachability comes from
  `resolver.Reachable(ctx, name)` (P9b) for each `store.ListNodes()` entry
  (plus the implicit `local`). Edge health is a cheap Caddy check — reuse an
  existing router capability (e.g. a `ProbeThroughCaddy` to a known/admin
  endpoint) or add a minimal `router.Healthy(ctx) bool`; the plan picks the
  least-invasive option. Transitions (up→down, down→up) are logged.

### API / client / CLI / web

- **`GET /health`** (api): returns the HealthSnapshot as JSON (nodes, edge,
  apps). Read-only; no secrets.
- **client**: `Health(ctx) (client.Health, error)`.
- **CLI**: `lwd health` prints nodes (transport/reachable), edge, and per-app
  surface health (state + last error + heal attempts). (Folded into a new
  subcommand; `lwd status` stays app-deployment focused.)
- **web**: a small health panel on the dashboard (nodes + edge + app health),
  reusing the existing buildless design system; the existing Nodes view can link
  to / show reachability from the same snapshot.

## Config

- `LWD_RECONCILE_INTERVAL` — reconcile ticker period; default `15s`
  (parsed as a Go duration; invalid/empty → default).
- `LWD_HEAL_MAX_ATTEMPTS` — consecutive heal give-up cap; default `5`.

## Concurrency & safety

- `Reconcile`'s heal work holds `r.mu` → serialized with `Apply`/`Remove`/
  `Rollback`. The health-probe portion may run without the deploy lock (it only
  reads + writes the snapshot), but keep the whole pass simple: acquire `r.mu`
  for the heal section, release before/around probing to avoid blocking manual
  deploys behind slow node probes. (Plan decides the exact locking split; the
  invariant is: **no two mutating deploy operations interleave**.)
- HealthSnapshot has its own lock; never held across a node/Docker call.
- The loop is cancellation-aware and panic-recovering.

## Testing

All tests run with fakes, **no Docker**:
- **Heal happy path**: `FakeResolver` + `node.Fake` reporting a surface as
  not-running + a fake router + store with a current `StatusRunning` git/image
  deployment → `Reconcile` creates a new surface, health-gates it (fake router
  probe OK), flips the route, retires the dead row, records a new
  `StatusRunning`. Assert the git case does **not** call clone/build.
- **Healthy no-op**: container running → no new container, no route change,
  backoff reset.
- **Compose/backing untouched**: a compose app's current deployment is skipped
  by the heal path (observed only).
- **Node unreachable**: app on a node whose `Reachable` is false → marked
  `degraded`, no heal attempted, no panic, pass continues.
- **Crash-loop backoff**: a surface whose heal keeps failing → attempts capped
  at `LWD_HEAL_MAX_ATTEMPTS`, backoff gates intervening passes, then `failed`;
  a subsequent manual `Apply` resets the counter.
- **Loop**: ticker drives `Reconcile`; `ctx` cancel stops it; a nudge triggers
  an immediate pass; a panic in one pass is recovered and the loop continues.
- **HealthSnapshot / API**: `GET /health` returns nodes + edge + app health;
  concurrent read while the loop writes is race-free (`go test -race`).
- **Zero regression**: existing reconciler/api/cli/web suites unchanged; single
  node path and `Apply`/`Remove`/`Rollback` behavior identical.

## Out of scope (later phases)

- Cross-node surface **reschedule** on node loss, scheduler, capacity, pools,
  drain/evacuate — **P11**.
- Surface **replicas** / load balancing — P12.
- Docker **event-stream**-driven reconciliation (instant heal) — possible later
  refinement; interval polling is the P10 baseline.
- First-class **resource drivers** (postgres/valkey/minio) + their failover —
  P14/P15. P10 does not heal or migrate resources.

## Guardrail check (VISION)

Extends the reconciler into a continuous one; preserves Node/Router/Reconciler/
Store. Adds exactly one method (`Reconcile`), a thin loop, and a live health
snapshot. Justifies itself by making the UX simpler: apps come back on their own
after a crash, and the operator can see node/edge/app health at a glance —
without any new distributed-systems concepts. It does **not** turn lwd into a
scheduler or a k8s frontend (no placement decisions, no cross-node motion).
