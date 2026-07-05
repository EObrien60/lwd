# lwd Phase 12 Implementation Plan — surface replicas + load balancing

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`).

**Goal:** Run N load-balanced, node-spread, self-healing replicas of a surface (`replicas = N` / `lwd scale api 3`), deployed blue-green as a set — with `replicas=1` (default) preserving today's exact single-container behavior.

**Architecture:** One deployment generation = one `store.Deployment` row carrying a `Replicas []Replica` set. Caddy `reverse_proxy` fans out to all replicas (round-robin + passive health). The scheduler spreads replicas across nodes; the deploy path health-gates the whole new set before flipping; P10 heal + P11b failover operate per-replica. N=1 degrades to the current path everywhere.

**Tech Stack:** Go 1.25+, no cgo, module `lwd`; `internal/{router,spec,store,scheduler,reconciler,api,client,cli,web,mcp}`.

## Global Constraints

- Go 1.25+; no cgo. Module `lwd`. Spec: `docs/superpowers/specs/2026-07-05-lwd-phase12-replicas-design.md`; north star `docs/VISION.md`.
- **`replicas=1` is the default and MUST be byte-for-byte the current behavior** — the non-regression contract. Every existing test passes unchanged; the N=1 Caddyfile is identical (no `lb_policy`/`fail_duration`).
- One deployment row per generation carries `Replicas []Replica{ContainerID, Node, Upstream string, Port int}`; `store.Deployment.ContainerID` retained = `Replicas[0].ContainerID`.
- **Replicas are surfaces only** — `replicas>1` invalid with `compose`. Validate `1 <= replicas <= 50`.
- **Caddy LB:** N>1 → `reverse_proxy h1:p1 h2:p2 …` + `lb_policy round_robin` + `fail_duration 30s` (passive health drops a dead replica at the edge). N=1 → exactly today's single `reverse_proxy host:port`.
- **Placement spreads** replicas across distinct schedulable nodes (fall back to stacking only when nodes < replicas); a hard `node=` pin puts ALL replicas on the pin.
- **Set blue-green:** deploy N, ALL must pass health, flip route to the set, retire the OLD set; any failure → old set + route untouched.
- Human scaling only (no autoscaler). Resources never replicated.
- **Zero regression:** single-node, compose, pinned, P10 heal, P11 scheduling/failover all intact for N=1. Tests fakes/no Docker; `go test ./... -race` green.
- README + skill + VISION updated in the final task.

---

### Task 1: router multi-upstream (`Route.Upstreams`) + Caddyfile LB (N=1 identical)

**Files:** Modify `internal/router/caddyfile.go` (+test), `internal/router/router.go` (`CaddyRouter` SetRoute/SetStaging/SeedRoutes), `internal/router/fake.go`, all `internal/router/*_test.go`; audit every `router.Route{...}`/`SetStaging` constructor repo-wide.

**Interfaces produced (consumed by T4–T7):**
- `type router.Upstream struct { Host string; Port int }`.
- `router.Route`: REPLACE `Upstream string; Port int` with `Upstreams []Upstream` (keep `Domain`, `TLSInternal`). A 1-element `Upstreams` reproduces today.
- `GenerateCaddyfile`: render `reverse_proxy` with all upstreams joined by spaces (`h:p` each); if `len(Upstreams) > 1` add, inside a `reverse_proxy { … }` block, `to h1:p1 h2:p2 …` + `lb_policy round_robin` + `fail_duration 30s`; if `len == 1` emit the EXACT current one-line form `reverse_proxy host:port` (no block, no lb directives) — byte-identical to today.
- `SetStaging(ctx, host string, upstreams []Upstream) error` (was `host, upstream string, port int`) — stages the whole set.
- `SetRoute`/`SeedRoutes` operate on `Route.Upstreams`. Update `FakeRouter` to match.

- [ ] **Step 1: failing tests** — caddyfile: `TestGenerateCaddyfileSingleUpstreamUnchanged` (a 1-upstream route renders the SAME string as the current golden — copy the existing golden assertion; this LOCKS non-regression), `TestGenerateCaddyfileMultiUpstreamRoundRobin` (3 upstreams → `lb_policy round_robin` + `fail_duration 30s` + all three `h:p` present), `TestRouteUpstreamsRoundTrip`. Update existing caddyfile/router tests to the new `Upstreams` shape (a single-element slice), asserting the N=1 output is unchanged.
- [ ] **Step 2:** `go test ./internal/router/ -v` → FAIL.
- [ ] **Step 3:** implement the `Upstream` type, `Route.Upstreams`, `GenerateCaddyfile` (N=1 identical / N>1 LB), `SetRoute`/`SetStaging`/`SeedRoutes`, fake. Fix ALL constructors that the compiler flags (reconciler, cli buildInitialRoutes, tests) to build a 1-element `Upstreams` — this is a compile-driven sweep; the reconciler's real multi-upstream use comes in T4, so here just make each existing single-upstream site build `[]Upstream{{Host, Port}}`.
- [ ] **Step 4:** `go test ./... -race` PASS (the whole repo compiles + N=1 behavior unchanged); build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: router multi-upstream routes + Caddy round-robin LB (N=1 unchanged)`.

---

### Task 2: `spec.Replicas` + `store.Deployment.Replicas` (model)

**Files:** Modify `internal/spec/spec.go` (+test), `internal/store/store.go` (+test).

**Interfaces produced (consumed by T3–T7):**
- `spec.App.Replicas int \`toml:"replicas"\``; `spec.Parse` defaults `0 → 1`; `Validate`: `Replicas >= 1` (error if `< 1`), `Replicas <= 50` (error above), and `Replicas > 1` is invalid when `Compose != ""` (error "replicas not supported for compose apps"). Doc the field.
- `store.Replica struct { ContainerID string \`json:"container_id"\`; Node string \`json:"node"\`; Upstream string \`json:"upstream"\`; Port int \`json:"port"\` }`.
- `store.Deployment.Replicas []Replica` — column `replicas TEXT NOT NULL DEFAULT ''` (JSON-encoded slice; empty string → nil) via guarded migration `migrateAddReplicasColumn` (mirror `migrateAddScheduledColumn`); `RecordDeployment` JSON-encodes it; `CurrentDeployment`/`PreviousDeployment`/`DeploymentsForApp` decode it (empty → nil). `ContainerID` stays a column; callers keep it in sync (T4).
- Helper `func (d *Deployment) replicasJSON() (string, error)` / decode on scan — keep it local + tested.

- [ ] **Step 1: failing tests** — spec: `TestParseDefaultsReplicasToOne` (unset → 1), `TestParseReplicas` (`replicas=3` → 3), `TestValidateReplicasMin` (0 → error), `TestValidateReplicasMax` (51 → error), `TestValidateReplicasCompose` (replicas=2 + compose → error). store: `TestDeploymentReplicasRoundTrip` (RecordDeployment with a 3-Replica slice → CurrentDeployment decodes all 3; empty → nil), pre-12 migration (old deployments table → replicas column added, defaults nil).
- [ ] **Step 2:** `go test ./internal/spec/ ./internal/store/ -run 'Replicas' -v` → FAIL.
- [ ] **Step 3:** implement the spec field+parse default+validate, the `Replica` type + column + migration + encode/decode in Record/Current/Previous/DeploymentsForApp.
- [ ] **Step 4:** `go test ./internal/spec/ ./internal/store/ -race -v` PASS; `go test ./...`; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: spec.Replicas + store.Deployment.Replicas set (model + migration)`.

---

### Task 3: replica placement (spread across nodes)

**Files:** Modify `internal/reconciler/schedule.go` (+test).

**Interfaces produced (consumed by T4):**
- `func (r *Reconciler) placeReplicas(ctx context.Context, app *spec.App, n int) (nodes []string, scheduled bool, err error)` — decide the node for each of `n` replicas:
  - If pinned (`app.Node != "" && != "local"`) → all `n` on `app.Node`; `scheduled=false`.
  - Else schedule with SPREAD: pick node for replica 0 via the normal scheduler; for replica k, call `placeExcludingSet(ctx, app, chosenSoFar)` (extend `placeExcluding` to an exclude SET) — pick the most-free node NOT already used; if it errors because all remaining nodes are excluded/full (fewer schedulable nodes than replicas), FALL BACK to the most-free node overall (allow stacking) rather than failing. Only fail if NO node can host even one replica. `scheduled=true`.
  - Returns the per-replica node list (length n).
- `func (r *Reconciler) placeExcludingSet(ctx, app, exclude []string) (string, error)` (generalize `placeExcluding`; `placeExcluding(x)` becomes `placeExcludingSet([]string{x})`).

- [ ] **Step 1: failing tests** — `TestPlaceReplicasSpreadsAcrossNodes` (3 replicas, 3 schedulable nodes → 3 distinct nodes), `TestPlaceReplicasStacksWhenFewerNodes` (3 replicas, 2 nodes → 2 distinct + 1 reused, no error), `TestPlaceReplicasPinnedAllOnPin` (`node=web1`, 3 → [web1,web1,web1], scheduled=false), `TestPlaceReplicasSingleNode` (3 replicas, only local → [local,local,local]), `TestPlaceExcludingSetDropsAll`.
- [ ] **Step 2:** `go test ./internal/reconciler/ -run 'PlaceReplicas|ExcludingSet' -v` → FAIL.
- [ ] **Step 3:** implement `placeExcludingSet` + `placeReplicas`.
- [ ] **Step 4:** `go test ./internal/reconciler/ -race -v` PASS; `go test ./...`; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: replica placement (spread across nodes, stack fallback, pin all)`.

---

### Task 4: set-based blue-green deploy (`deployReplicaSet`)

**Files:** Modify `internal/reconciler/reconciler.go` (+tests).

**Interfaces produced (consumed by T5–T7):**
- Generalize the surface deploy back-half to N replicas. `deployReplicaSet(ctx, app, image, env, backingNetwork, composeContent, specJSON, scheduled)`:
  1. `nodes, scheduled, err := r.placeReplicas(ctx, app, app.Replicas)` (or accept the already-resolved nodes — keep the provenance from the existing resolvePlacement for N=1 compatibility; simplest: placeReplicas is the single placement entry for the set).
  2. For each replica i: resolve its node (ResolveMeta), ensure image on it, run container `lwd-<app>-<deployID>-<i>` (mesh publish if remote), collect `store.Replica{ContainerID, Node: nodes[i], Upstream, Port}` and a `router.Upstream{Host, Port}`.
  3. Stage the whole set behind `stagingHost(deployID)` via `SetStaging(ctx, stageHost, upstreams)`; health-gate EACH replica (reuse `checkHealth` per replica container) — ALL must pass or it's a failed deploy (tear down the new set, record StatusFailed, leave old set + route).
  4. `SetRoute` with the set's `Upstreams`; remove staging; retire+remove the OLD set (every container in the prior current deployment's `Replicas`, each on its own node); record `StatusRunning` with `Replicas` + `ContainerID=Replicas[0].ContainerID`.
- **N=1 path:** `deployReplicaSet` with `app.Replicas==1` must produce exactly today's single-surface result (one container, one upstream, single-element Replicas). Keep `deployBlueGreenSurface`'s existing callers (applyImage/applyGit/rollback/heal) routing through `deployReplicaSet` (or have deployBlueGreenSurface delegate) so the whole surface path becomes set-based with N defaulting to 1.
- Retiring the OLD set must remove each old container ON ITS OWN NODE (per-replica node, like P11b's cross-node awareness) — not all on the new node.

- [ ] **Step 1: failing tests** — `TestDeploySingleReplicaUnchanged` (replicas=1 → one container, single-upstream route, Replicas len 1; behaves as the existing surface test), `TestDeployThreeReplicasAllHealthy` (replicas=3, 3 nodes → 3 containers across nodes, route has 3 upstreams, deployment.Replicas len 3), `TestDeployReplicaSetPartialUnhealthyFails` (one replica fails health → whole deploy StatusFailed, new containers removed, old set + route untouched), `TestDeployReplicaSetRetiresOldSet` (redeploy → all old replica containers removed on their nodes, new set live).
- [ ] **Step 2:** `go test ./internal/reconciler/ -run 'Replica|Deploy' -v` → FAIL.
- [ ] **Step 3:** implement `deployReplicaSet` + route existing surface deploys through it (N defaults to 1).
- [ ] **Step 4:** `go test ./internal/reconciler/ -race -v` PASS (ALL existing surface/apply/git/rollback tests still green — N=1 non-regression); `go test ./...`; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: set-based blue-green deploy (N replicas, all-healthy gate, multi-upstream route)`.

---

### Task 5: per-replica heal + remove-all + rollback set

**Files:** Modify `internal/reconciler/reconcile.go` (heal), `internal/reconciler/reconciler.go` (Remove/rollback) (+tests).

**Interfaces produced (consumed by T6):**
- `reconcileApp` (heal): iterate the current deployment's `Replicas`; for each replica whose container is dead (missing/not-running on `replica.Node`, when that node is reachable), recreate THAT replica on its node (run a fresh container, update the replica entry + the route's upstream set + the stored deployment's Replicas) under the per-app backoff. A replica on an unreachable node is left to failover. If the deployment has an empty/legacy `Replicas` (pre-12 rows), fall back to the single-`ContainerID` heal (non-regression). Update the stored `Replicas` (and `ContainerID`) after a heal.
- `Remove`: remove EVERY container in the current deployment's `Replicas` (each on its node) — not just `ContainerID`; then backing/route/retire as today. Legacy row (no Replicas) → the existing single-container remove.
- `Rollback`: restore the previous generation's `Replicas` count via the set deploy (redeploy the prior spec with its recorded `Replicas` length; preserve `Scheduled`). For an image/git app the set-based deploy handles it.

- [ ] **Step 1: failing tests** — `TestHealRecreatesOneDeadReplica` (3-replica app, replica 1's container dead → only replica 1 recreated, others untouched, route updated), `TestHealAllReplicasDead` (→ full set recreated), `TestHealLegacySingleContainer` (a deployment with empty Replicas but a ContainerID → old single heal path), `TestRemoveAllReplicas` (3 replicas → all 3 containers removed), `TestRollbackRestoresReplicaCount` (rollback a 1→3 scale → prior generation's count restored).
- [ ] **Step 2:** `go test ./internal/reconciler/ -run 'Heal|Remove|Rollback' -v` → FAIL.
- [ ] **Step 3:** implement per-replica heal, remove-all, rollback set.
- [ ] **Step 4:** `go test ./internal/reconciler/ -race -v` PASS (existing heal/remove/rollback for N=1 unchanged); `go test ./...`; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: per-replica heal + remove-all-replicas + rollback replica set`.

---

### Task 6: per-replica failover

**Files:** Modify `internal/reconciler/evacuate.go` (+tests).

**Interfaces produced:**
- `EvacuateNode`/`rescheduleSurfaceLocked` operate per-replica: for a SCHEDULED surface with replicas on the target node, reschedule ONLY those replicas to other nodes (excluding the dead node AND the app's other live replica nodes, to preserve spread — reuse `placeExcludingSet`), update the deployment's `Replicas` + the route's upstream set, retire the moved replicas' old entries (old-container removal skipped when the node is unreachable, as today). Replicas of the app on OTHER nodes are untouched. Pinned surfaces skipped. A single-replica app → exactly today's P11b whole-surface move.
- Auto-failover (the loop) already routes through `EvacuateNode`; no loop change needed beyond it now moving per-replica.

- [ ] **Step 1: failing tests** — `TestEvacuateMovesOnlyNodesReplicas` (3-replica app spread on A,B,C; evacuate B → only the B replica moves to a 4th node D (or stacks), route updated, A/C replicas untouched), `TestFailoverReschedulesReplicaPreservingSpread` (node loss → that node's replica rescheduled excluding the surviving replicas' nodes), `TestEvacuateSingleReplicaUnchanged` (1-replica app → today's behavior).
- [ ] **Step 2:** `go test ./internal/reconciler/ -run 'Evacuate|Failover' -v` → FAIL.
- [ ] **Step 3:** implement per-replica evacuate/reschedule.
- [ ] **Step 4:** `go test ./internal/reconciler/ -race -v` PASS (P11b single-replica tests unchanged); `go test ./...`; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: per-replica node-loss failover + evacuate (preserve spread)`.

---

### Task 7: `lwd scale` — API + client + CLI + status

**Files:** Modify `internal/api/api.go` (+test), `internal/client/client.go`, `internal/cli/cli.go`, `internal/api/api.go` AppStatus.

**Interfaces produced (consumed by T8):**
- API `POST /apps/{name}/scale` body `{replicas int}` → read the app's current deployment spec snapshot, set `Replicas`, validate (`1..50`, not compose), redeploy via the reconciler (set-based) → 200 + the new deployment. (Unknown app → 404; invalid count → 400.)
- `GET /apps`/`AppStatus` gains `replicas int` (from the current deployment's `len(Replicas)`) so `lwd ls`/`status` can show it.
- client: `Scale(ctx, name string, replicas int) (*store.Deployment, error)`.
- CLI: `lwd scale <app> <N>` → `client.Scale` (print the result: "scaled <app> to N replicas"); `lwd status <app>` shows per-replica node + the count; `lwd ls` shows a REPLICAS column.

- [ ] **Step 1: failing api tests** — `TestScaleUp` (1→3 → current deployment has 3 replicas), `TestScaleDown` (3→1), `TestScaleInvalidCount` (0 → 400), `TestScaleComposeRejected` (400), `TestScaleUnknownApp` (404), `TestAppStatusIncludesReplicas`.
- [ ] **Step 2:** `go test ./internal/api/ -run 'Scale|Replicas' -v` → FAIL.
- [ ] **Step 3:** implement the scale endpoint + client + CLI + AppStatus.replicas + status/ls display.
- [ ] **Step 4:** `CGO_ENABLED=0 go build -o /tmp/lwd ./cmd/lwd && go test ./... && go vet ./... && gofmt -l .` green.
- [ ] **Step 5:** commit `feat: lwd scale (API/client/CLI) + replica count in status`.

---

### Task 8: web + MCP + skill + docs + e2e

**Files:** Modify `internal/web/*` (assets + handler + fake), `internal/mcp/tools.go` + tests + fake, `skills/lwd-toml/*`, `test/e2e_test.go`, `README.md`, `docs/VISION.md`.

- [ ] **Step 1: web** (frontend-design skill) — the app view shows the replica count + per-replica node/health and a scale control (scale up/down → `POST /api/apps/{name}/scale`); the deploy modal gains an optional replicas input → `replicas = N` in the generated lwd.toml. Add `Scale` to the web `DaemonClient` + `fakeDaemon` + a `POST /api/apps/{name}/scale` proxy (authed). Buildless. Web tests: scale route requires auth + calls client; replicas shown.
- [ ] **Step 2: MCP** — `lwd_scale` tool {app, replicas} (destructiveHint) calling `client.Scale`; `replicas` optional arg on `lwd_apply`/`lwd_deploy_git` (set app.Replicas before Validate); `lwd_status`/`lwd_list` include replica count. Tests assert the flow.
- [ ] **Step 3: skill** — document `replicas` in `skills/lwd-toml/references/schema.md` + an example (a 3-replica app); keep `examples_test.go` green.
- [ ] **Step 4: e2e** (fakes, no Docker, unconditional) — `TestEndToEndReplicas`: deploy a 3-replica unpinned app → 3 containers across nodes + a 3-upstream route; scale to 1 → 1 container; kill one replica of a 3-set → heal recreates it. Reuse the reconciler-with-fakes helpers.
- [ ] **Step 5: verify** — `go test ./... -race`; `CGO_ENABLED=0 go build ./...` + four binaries; vet/gofmt clean. Screenshot the web scale UI if tooling allows.
- [ ] **Step 6: docs** — README "Replicas & load balancing" section (`replicas`, `lwd scale`, round-robin + passive health, spread placement, per-replica heal/failover, N=1 = single surface, human-only). `docs/VISION.md`: mark **P12 done**, note **P13 (multi-edge routing: N Caddy + DNS round-robin)** next. Full README pass P1–P12.
- [ ] **Step 7: commit** `feat: web+MCP+skill replica/scale UX; replicas e2e; docs P12`.

---

## Self-Review

**Spec coverage:** multi-upstream router+LB (T1); spec+store model (T2); spread placement (T3); set blue-green (T4); per-replica heal/remove/rollback (T5); per-replica failover (T6); scale API/CLI (T7); web/MCP/skill/docs/e2e (T8). ✓
**Non-regression (the contract):** N=1 Caddyfile byte-identical (T1 golden test); N=1 deploy = today (T4 test); N=1 heal/remove/rollback/failover unchanged (T5/T6 tests); legacy rows without Replicas fall back to ContainerID paths (T5). Router type change is compile-swept in T1 with single-element slices. ✓
**Decisions honored:** replicas surfaces-only + 1..50 (T2 validate); round-robin+fail_duration only for N>1 (T1); spread+stack-fallback+pin-all (T3); all-healthy set gate (T4); human-only scale (T7); resources never replicated (never touched). ✓
**Type consistency:** `router.Upstream`/`Route.Upstreams` (T1) used by T4/T5/T6/route updates; `store.Replica`/`Deployment.Replicas` (T2) used by T4–T7; `placeReplicas`/`placeExcludingSet` (T3) used by T4/T6; `deployReplicaSet` (T4) used by T5(rollback)/T7(scale); `client.Scale`/`AppStatus.replicas` (T7) used by T8. ✓
**Placeholder scan:** each task has concrete signatures + tests; the T1 constructor sweep is compile-driven (explicit). ✓
**Cross-task risks:** (a) T1's `Route` type change breaks every constructor — a compile sweep, done in T1 with 1-element slices so the repo stays green before T4 adds real multi-upstream. (b) The per-replica node awareness (old-set removal on each replica's own node) reuses P11b's cross-node-removal lesson — T4/T5/T6 must remove/heal each replica against ITS node, never the new/local node. (c) Legacy pre-12 deployment rows (empty Replicas) must not break heal/remove/failover — explicit fallback in T5.
