# lwd Phase 11b Implementation Plan — drain/evacuate + node-loss failover

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`).

**Goal:** Move scheduled surfaces off a node on operator `drain`/`evacuate` and automatically reschedule them when a node is lost (after a grace period) — reusing the P11a scheduler + P10 loop; resources never move; explicit `node=` pins are honored.

**Architecture:** Record placement provenance (`Scheduled`) so only scheduler-placed surfaces move. A `rescheduleSurfaceLocked` helper redeploys a scheduled surface to a new node (excluding the source) via the existing blue-green path; `EvacuateNode` drives it per node. `node drain` = cordon (`schedulable=0`, scheduler excludes it) + evacuate. The P10 reconcile loop tracks per-node unreachable-since and evacuates a node's scheduled surfaces once it exceeds `LWD_FAILOVER_GRACE`.

**Tech Stack:** Go 1.25+, no cgo, module `lwd`; `internal/{store,scheduler,reconciler,api,client,cli,web,mcp}`.

## Global Constraints

- Go 1.25+; no cgo. Module `lwd`. Spec: `docs/superpowers/specs/2026-07-05-lwd-phase11b-failover-design.md`; north star `docs/VISION.md`.
- **Only SCHEDULED surfaces move** (deployment `Scheduled==true`, i.e. deployed from an unpinned `node=""` spec). A pinned surface on a lost/drained node is reported + left, never relocated.
- **Resources never move** — act only on `lwd.role=surface`; compose/backing untouched.
- **Auto-failover:** a registered node must be continuously unreachable ≥ `LWD_FAILOVER_GRACE` (default `60s`, `<=0`→default) before its scheduled surfaces are evacuated. **Never fail over `local`.**
- **Drain = cordon (`schedulable=0`) + evacuate; `uncordon` clears it.** A cordoned node accepts no NEW placements (scheduler excludes it); you cannot cordon `local`.
- **No rebalancing / no fail-back.** A recovered node's stale surface containers are cleaned up best-effort; moved surfaces stay put.
- **Concurrency:** evacuate/reschedule/failover hold `r.mu` (serialized with Apply/Remove/Rollback/heal); `unreachableSince` + cordon reads under `r.mu`; failover never panics the loop.
- **Zero regression:** single-node (no registered nodes → nothing to drain/fail over), pinned surfaces, compose, P10 heal, P11a scheduling unchanged. Tests fakes/no Docker; `go test ./... -race` green.
- README + VISION updated in the final task.

---

### Task 1: placement provenance — `Scheduled` on deployments + `schedulable` on nodes (store) + record provenance

**Files:** Modify `internal/store/store.go` (+test), `internal/reconciler/schedule.go`, `internal/reconciler/reconciler.go` (+tests).

**Interfaces produced (consumed by T2–T5):**
- `store.Node.Schedulable bool` — column `schedulable INTEGER NOT NULL DEFAULT 1` via `migrateAddSchedulableColumn` (mirror `migrateAddPoolColumn`, called in `Open`); `AddNode` (default true when adding), `GetNode`, `ListNodes` include it. Add `func (s *Store) SetSchedulable(name string, schedulable bool) error` (UPDATE).
- `store.Deployment.Scheduled bool` — column `scheduled INTEGER NOT NULL DEFAULT 0` via `migrateAddScheduledColumn` (called in `Open`); `RecordDeployment` writes it; `CurrentDeployment`/`PreviousDeployment`/`DeploymentsForApp` scan it.
- `resolvePlacement` (schedule.go) signature → `func (r *Reconciler) resolvePlacement(ctx context.Context, app *spec.App) (nodeName string, scheduled bool, err error)`: `scheduled` is `true` iff it invoked the scheduler (original `app.Node == ""`); `false` for a pinned/`"local"` app. (Cordon-exclusion is added in T2 — this task just adds the provenance return.)
- `deployBlueGreenSurface` gains a `scheduled bool` param, recorded as `Scheduled` on the `StatusRunning` deployment it writes. Thread it from every caller:
  - `applyImage`/`applyGit` (reconciler.go:323, 416): capture `chosen, scheduled, err := r.resolvePlacement(...)`; `app.Node = chosen`; pass `scheduled` down.
  - `rollbackGitLocked` and `healSurfaceLocked`: pass the PROVENANCE OF THE SNAPSHOT being restored — `cur.Scheduled` (heal) / `prev.Scheduled` (rollback) — so a healed/rolled-back surface keeps its original movability. (rollback: read `prev.Scheduled`; heal: `cur.Scheduled`.)

- [ ] **Step 1: failing tests** — store: `TestSchedulableRoundTrip` (default true; SetSchedulable false→GetNode false), pre-11b migration (old nodes table → schedulable defaults 1); `TestDeploymentScheduledRoundTrip` (RecordDeployment Scheduled true → CurrentDeployment reflects it; default false), pre-11b deployments migration. reconciler: `TestUnpinnedRecordsScheduled` (unpinned deploy → current deployment `Scheduled==true`), `TestPinnedRecordsNotScheduled` (`node="web1"` → `Scheduled==false`), `TestHealPreservesScheduled` (a scheduled surface healed → new deployment still `Scheduled==true`).
- [ ] **Step 2:** `go test ./internal/store/ ./internal/reconciler/ -run 'Schedulable|Scheduled|Provenance|Heal' -v` → FAIL.
- [ ] **Step 3:** implement the two columns + migrations + SetSchedulable; thread `scheduled` through resolvePlacement → applyImage/applyGit/deployBlueGreenSurface and rollbackGitLocked/healSurfaceLocked.
- [ ] **Step 4:** `go test ./internal/store/ ./internal/reconciler/ -race -v` PASS; `go test ./...`; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: placement provenance (deployment.Scheduled) + node.Schedulable (cordon flag)`.

---

### Task 2: scheduler cordon exclusion + resolvePlacement honors cordon + exclude support

**Files:** Modify `internal/scheduler/scheduler.go` (+test), `internal/reconciler/schedule.go` (+test).

**Interfaces produced (consumed by T3, T5):**
- `scheduler.NodeInfo` gains `Schedulable bool`. `Place` excludes candidates with `!Schedulable` (a cordoned node accepts no new placement) — apply this filter alongside the existing Reachable/pool filter. Update the "no fitting node" vs "no reachable nodes" messaging to still make sense (a cordoned-out candidate set is "no schedulable node in pool %q").
- `resolvePlacement` sets each store candidate's `Schedulable` from `store.Node.Schedulable`; the local node is always `Schedulable:true`.
- Add exclude support for rescheduling: `func (r *Reconciler) placeExcluding(ctx context.Context, app *spec.App, exclude string) (string, error)` — same as resolvePlacement's scheduling branch but drops `exclude` from the candidate set (and always schedules, since it's only called for a movable surface). resolvePlacement can delegate to it with `exclude=""`. (Keep resolvePlacement's pinned short-circuit + provenance return.)

- [ ] **Step 1: failing tests** — scheduler: `TestPlaceExcludesCordoned` (a `Schedulable:false` node in the pool is never chosen; if it's the only one → "no schedulable node" error). reconciler: `TestResolvePlacementSkipsCordoned` (two nodes, the most-free one cordoned → the other is chosen); `TestPlaceExcludingDropsNode` (`placeExcluding(app, "A")` never returns A even if A is most-free).
- [ ] **Step 2:** `go test ./internal/scheduler/ ./internal/reconciler/ -run 'Cordon|Exclud' -v` → FAIL.
- [ ] **Step 3:** add `NodeInfo.Schedulable` + Place filter; wire `Schedulable` into resolvePlacement candidates; add `placeExcluding`.
- [ ] **Step 4:** `go test ./internal/scheduler/ ./internal/reconciler/ -race -v` PASS; `go test ./...`; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: scheduler excludes cordoned nodes + placeExcluding for reschedule`.

---

### Task 3: `rescheduleSurfaceLocked` + `EvacuateNode`

**Files:** Modify `internal/reconciler/reconciler.go` (or a new `internal/reconciler/evacuate.go`), `internal/reconciler/*_test.go`.

**Interfaces produced (consumed by T4, T5):**
- `func (r *Reconciler) rescheduleSurfaceLocked(ctx context.Context, cur *store.Deployment, excludeNode string) (*store.Deployment, error)` — CALLER HOLDS `r.mu`. Reconstruct `spec.App` from `cur.Spec` (pin `Image=cur.Image`); `newNode, err := r.placeExcluding(ctx, &restored, excludeNode)`; on err return it (no capacity elsewhere; caller decides). Set `restored.Node = newNode`; deploy via the blue-green path (`deployBlueGreenSurface`, `scheduled=true`) → new `StatusRunning` on `newNode`. Then remove the OLD surface on `excludeNode` best-effort: if `excludeNode` resolves + is reachable, `RemoveContainer(cur.ContainerID)` + retire `cur`; if `excludeNode` is unreachable (node-loss), skip removal (log) — the container dies with its node; retire `cur` anyway (the new one serves). (Git apps: `restored.Git != nil` still redeploys the recorded tag via the same path as heal — no re-clone/rebuild; reuse the healSurfaceLocked shape.)
- `type EvacuateResult struct { Moved []string; Skipped []string; Failed []EvacFailure }` and `type EvacFailure struct { App, Err string }`.
- `func (r *Reconciler) EvacuateNode(ctx context.Context, name string) (EvacuateResult, error)` — takes `r.mu`. For each app (`store.ListApps`) whose `CurrentDeployment` is a surface (`ContainerID != "" && !isComposeApp && nodeFromSpec(cur.Spec) == name`): if `cur.Scheduled` → `rescheduleSurfaceLocked(ctx, cur, name)` → append to Moved (or Failed on err); else → append to Skipped (pinned). Never touches compose/backing.

- [ ] **Step 1: failing tests** — `TestRescheduleMovesToAnotherNode` (scheduled surface on A, node B has capacity → redeployed to B, new StatusRunning on B, old cur retired); `TestRescheduleNoOtherNodeFails` (only A fits → error, and — if A reachable — the original left running); `TestEvacuateSkipsPinned` (a pinned surface on A → Skipped, not moved); `TestEvacuateMovesScheduledLeavesCompose` (a scheduled surface moves; a compose app on A untouched); `TestEvacuateUnreachableNodeSkipsOldRemoval` (excludeNode unreachable → new surface created, old container removal skipped, cur retired).
- [ ] **Step 2:** `go test ./internal/reconciler/ -run 'Reschedule|Evacuate' -v` → FAIL.
- [ ] **Step 3:** implement `rescheduleSurfaceLocked`, `EvacuateResult`/`EvacFailure`, `EvacuateNode`.
- [ ] **Step 4:** `go test ./internal/reconciler/ -race -v` PASS; `go test ./...`; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: rescheduleSurfaceLocked + EvacuateNode (move scheduled surfaces off a node)`.

---

### Task 4: drain / evacuate / uncordon — API + client + CLI

**Files:** Modify `internal/api/api.go` (+test), `internal/client/client.go`, `internal/cli/cli.go`.

**Interfaces produced (consumed by T6):**
- API (the Server holds `rec *reconciler.Reconciler`, `store`, `resolver`):
  - `POST /nodes/{name}/uncordon` → `store.SetSchedulable(name, true)` → 204. (Guard: unknown node → 404; `name=="local"` → 400 "cannot cordon the local node".)
  - `POST /nodes/{name}/evacuate` → `rec.EvacuateNode(ctx, name)` → 200 + JSON EvacuateResult.
  - `POST /nodes/{name}/drain` → `store.SetSchedulable(name, false)` THEN `rec.EvacuateNode(ctx, name)` → 200 + JSON EvacuateResult. (local → 400.)
  - `GET /nodes` status DTO includes `schedulable` (rides via embedded store.Node).
- client: `Uncordon(ctx, name) error`; `Evacuate(ctx, name) (reconciler.EvacuateResult, error)`; `Drain(ctx, name) (reconciler.EvacuateResult, error)`; `NodeStatus` includes `Schedulable` (embedded). (Reuse `reconciler.EvacuateResult` — client already imports reconciler for Health; confirm no cycle.)
- CLI: `lwd node drain <name>` / `node evacuate <name>` / `node uncordon <name>` (print the moved/skipped/failed summary); `node ls` + `node capacity` add a SCHEDULABLE column (yes/no or "cordoned").

- [ ] **Step 1: failing api tests** — `TestNodeDrainCordonsAndEvacuates` (fake reconciler/evacuate returns a result; schedulable flips to 0 + result returned), `TestNodeUncordon` (schedulable→1), `TestNodeEvacuate` (result returned, schedulable unchanged), `TestDrainLocalRejected` (400), `TestNodeListIncludesSchedulable`. (Use the api test harness; the reconciler's EvacuateNode may need a real reconciler with fakes, or assert at the store+handler level — match how existing node tests are structured.)
- [ ] **Step 2:** `go test ./internal/api/ -run 'Drain|Uncordon|Evacuate|Schedulable' -v` → FAIL.
- [ ] **Step 3:** implement the API routes/handlers, client methods, CLI commands + SCHEDULABLE column.
- [ ] **Step 4:** `CGO_ENABLED=0 go build -o /tmp/lwd ./cmd/lwd && go test ./... && go vet ./... && gofmt -l .` green.
- [ ] **Step 5:** commit `feat: node drain/evacuate/uncordon (API/client/CLI)`.

---

### Task 5: automatic node-loss failover in the P10 loop

**Files:** Modify `internal/reconciler/reconcile.go` (the `Reconcile` pass), `internal/reconciler/reconciler.go` (field), `internal/reconciler/health.go` (`NodeHealth.Cordoned`), `internal/config/config.go` (+test), `internal/reconciler/*_test.go`.

**Interfaces produced:**
- `config.FailoverGrace() time.Duration` (env `LWD_FAILOVER_GRACE`, default `60s`, `<=0`/unparseable → default — mirror `ReconcileInterval`).
- `Reconciler` gains `unreachableSince map[string]time.Time` (init in `New`; guarded by `r.mu`).
- `NodeHealth` gains `Cordoned bool \`json:"cordoned"\`` (from `store.Node.Schedulable == false`); `probeNodes` sets it.
- In `Reconcile`, AFTER the per-app + node probes, add a failover step (`failoverLostNodes(ctx)`): for each registered node (`store.ListNodes()`, never `local`): determine reachability (reuse the reachability already probed this pass, or `r.reach.Reachable`); if reachable → `delete(r.unreachableSince, name)` (under r.mu); if unreachable → under r.mu set `unreachableSince[name]` if absent, and if `time.Since(unreachableSince[name]) >= config.FailoverGrace()` → call `r.EvacuateNode(ctx, name)` (which reschedules its scheduled surfaces, excluding the dead node; old-container removal is skipped since unreachable) and then `delete(r.unreachableSince, name)` (so it doesn't re-fire every pass; anything that couldn't move is picked up by the normal heal/degraded path). Log a clear line on failover. Wrap so a failover error never aborts the pass.
- NOTE the locking: `EvacuateNode` takes `r.mu` itself; do NOT hold `r.mu` across the call. Read/mutate `unreachableSince` under short `r.mu` sections, call `EvacuateNode` without the lock held (it locks internally), mirroring how `reconcileApp`/`tryHeal` split lock scope in P10.

- [ ] **Step 1: failing tests** — config: `TestFailoverGraceDefault/Env/NonPositive`. reconciler: `TestFailoverAfterGrace` (a registered node unreachable; first pass (within grace) → NOT evacuated; advance `unreachableSince` to > grace [inject the time by pre-seeding the map] → next pass evacuates its scheduled surface to a healthy node); `TestFailoverWithinGraceNoop` (unreachable < grace → no move); `TestFailoverNeverLocal` (local never in the failover set); `TestFailoverLeavesPinned` (a pinned surface on the dead node stays, reported degraded); `TestFailoverRecoveryClearsTimer` (node reachable again → `unreachableSince` cleared). (Seed `unreachableSince` directly in tests to simulate elapsed grace deterministically — don't sleep.)
- [ ] **Step 2:** `go test ./internal/reconciler/ ./internal/config/ -run 'Failover|Grace' -v` → FAIL.
- [ ] **Step 3:** implement `FailoverGrace`, `unreachableSince`, `NodeHealth.Cordoned`, `failoverLostNodes` + wire into `Reconcile`.
- [ ] **Step 4:** `go test ./internal/reconciler/ -race -v` PASS; `go test ./...`; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: automatic node-loss failover (grace-gated surface reschedule in the reconcile loop)`.

---

### Task 6: web + MCP node lifecycle UX + docs + e2e

**Files:** Modify `internal/web/*` (assets + a handler + fake), `internal/mcp/tools.go` + `server_test.go` (+ fake), `test/e2e_test.go`, `README.md`, `docs/VISION.md`.

- [ ] **Step 1: web** (frontend-design skill) — the Nodes view gains a SCHEDULABLE indicator (cordoned badge) + Drain / Evacuate / Uncordon actions (buttons → `POST /api/nodes/{name}/drain|evacuate|uncordon`, showing the moved/skipped/failed result); the Health panel shows `cordoned` per node. Add the proxy handlers + `Drain`/`Evacuate`/`Uncordon` to the web `DaemonClient` + `fakeDaemon`. Buildless (no npm/CDN). Web tests: the three routes require auth + call the client; the drain action renders the result. 
- [ ] **Step 2: MCP** — add `lwd_node_drain` / `lwd_node_evacuate` / `lwd_node_uncordon` tools (destructiveHint where they move/stop things) calling the client; `lwd_node_list` includes `schedulable`. Tests assert the tools call the right client methods.
- [ ] **Step 3: e2e** (`test/e2e_test.go`, fakes, no Docker) — `TestEndToEndNodeDrain`: two nodes; deploy an unpinned app (lands on one), `EvacuateNode`/drain that node → the app is redeployed on the other, old retired. `TestEndToEndNodeLossFailover`: seed `unreachableSince` past the grace for a node hosting a scheduled surface, run `Reconcile` → the surface is rescheduled to the healthy node; a pinned surface on it is left. (Unconditional, no Docker.)
- [ ] **Step 4: verify** — `go test ./... -race`; `CGO_ENABLED=0 go build ./...` + four binaries; vet/gofmt clean. Screenshot the web node-lifecycle UI if tooling allows.
- [ ] **Step 5: docs** — README "Node maintenance & failover" section (`node drain`/`evacuate`/`uncordon`; automatic failover after `LWD_FAILOVER_GRACE`, default 60s; only scheduled surfaces move, pinned + resources stay; single-node has nothing to fail over). `docs/VISION.md`: mark **P11b done** (and P11 complete), note **P12 (surface replicas + LB)** next. Full README pass P1–P11b.
- [ ] **Step 6: commit** `feat: web+MCP node lifecycle UX; drain/failover e2e; docs P11b`.

---

## Self-Review

**Spec coverage:** provenance + cordon columns (T1); scheduler cordon exclusion + exclude helper (T2); reschedule + EvacuateNode (T3); drain/evacuate/uncordon API+CLI (T4); auto-failover loop + grace (T5); web/MCP/docs/e2e (T6). ✓
**Decisions honored:** only Scheduled surfaces move (provenance in T1, honored in T3 EvacuateNode + T5 failover); resources never move (surface-only filter T3); grace + never-local failover (T5); drain=cordon+evacuate, uncordon (T4); no rebalancing/fail-back (nothing re-populates a recovered node). ✓
**Non-regression:** single-node has no registered nodes → EvacuateNode/failover no-op; pinned surfaces skipped everywhere; compose untouched; P10 heal + P11a scheduling preserved (deployBlueGreenSurface only GAINS a scheduled param; resolvePlacement gains a bool return; heal/rollback preserve provenance). ✓
**Concurrency:** evacuate/reschedule/failover hold r.mu via the deploy path; `unreachableSince` mutated under short r.mu sections, `EvacuateNode` called WITHOUT the lock held (it locks itself) — the P10 reconcileApp/tryHeal split pattern; -race in T1/T3/T5. ✓
**Type consistency:** `store.Node.Schedulable` + `SetSchedulable` (T1) used by T2 (candidate.Schedulable), T4 (drain/uncordon), T5 (Cordoned); `store.Deployment.Scheduled` (T1) used by T3 (EvacuateNode movable check) + T5; `resolvePlacement`→`(node,scheduled,err)` + `placeExcluding` (T1/T2) used by T3; `scheduler.NodeInfo.Schedulable` (T2); `EvacuateResult`/`EvacFailure` (T3) returned by T4 API/client + T6; `config.FailoverGrace` + `unreachableSince` + `NodeHealth.Cordoned` (T5). ✓
**Placeholder scan:** each code step has concrete signatures/algorithm; test time-elapsure is handled by seeding `unreachableSince` (no sleeps). ✓
**Cross-task risks:** (a) T1 threads `scheduled` through several deployBlueGreenSurface callers — miss one and provenance is wrong; the T1 tests (pinned vs unpinned vs heal) catch it. (b) T5's lock discipline — `EvacuateNode` must NOT be called while holding `r.mu` (it re-locks) — explicit in the task. (c) client reusing `reconciler.EvacuateResult` (T4) — verify no import cycle (client already imports reconciler for Health, so fine).
