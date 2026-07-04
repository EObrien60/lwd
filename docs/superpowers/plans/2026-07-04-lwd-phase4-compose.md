# lwd Phase 4 Implementation Plan — compose apps

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** An `lwd.toml` app can be a Docker Compose stack; lwd delegates orchestration to `docker compose`, routes the declared web service through Caddy, health-checks it, and passes `env`+resolved `secrets` to compose. Single-service `image` apps are unchanged.

**Architecture:** Reconciler `Apply` branches on `app.Compose != ""`. Compose path: `docker compose up -d` (in-place recreate; db stays up), connect the routed service container to the `lwd` network, set the Caddy route, health-check through Caddy. A `compose.Composer` interface (real CLI impl + fake) shells out to `docker compose`. Deployment records store the compose file content for rollback.

**Tech Stack:** Go 1.25+, `docker compose` CLI plugin (via os/exec), Docker SDK, `modernc.org/sqlite`.

## Global Constraints

- Go floor **1.25**; **no cgo** (CGO_ENABLED=0). Module `lwd`; imports `lwd/internal/<pkg>`.
- Design spec governs: `docs/superpowers/specs/2026-07-04-lwd-phase4-compose-design.md`.
- **Delegate to compose, in-place recreate** (NOT blue-green). Compose only recreates changed services, so the db stays up. A failed health check leaves the new stack live (documented tradeoff).
- Compose apps require the **`docker compose` CLI plugin** on the host; single-service apps do not. Error clearly on first compose deploy if absent — never at daemon start.
- **Env + secrets → compose process environment** (fail-closed on a missing declared secret, before `compose up`).
- Compose project name is **`lwd-<app>`**. Routed service container is connected to the **`lwd`** network so Caddy reaches it by container name.
- Single-service `image` apps and their blue-green path are **unchanged**.
- **README is a required deliverable** (Task 7) and its accuracy is checked in the final review.
- Tests use fakes + temp dirs; Docker/compose-dependent tests guarded by `LWD_DOCKER_TEST` and SKIP otherwise.

---

### Task 1: spec — support compose apps in validation

**Files:** Modify `internal/spec/spec.go`, `internal/spec/spec_test.go`.

**Interfaces:** `App` already has `Compose string`, `Service string`(add if missing — check current struct; the design uses `service`), `Domain`, `Port`, `Surfaces`. Add a `Service string` field with toml tag `service` if not present. `Validate` gains a compose branch.

**Behavior:** If `Compose != ""` → compose app: require `Service`, `Domain`, `Port`; reject if `Image != ""` or `Build != nil` (can't be both); `Surfaces` still rejected (unused). Else → single-service (existing rules unchanged: `Image` + `Port` required, name-regex, etc.). Name validation applies to both.

- [ ] **Step 1: Write failing tests** in `spec_test.go`:
  - `TestValidateComposeApp`: `{Name:"webapp", Compose:"docker-compose.yml", Service:"web", Domain:"x.example.com", Port:8080}` → valid.
  - `TestComposeRequiresService`: compose set but Service empty → error.
  - `TestComposeRejectsImageMix`: compose + image both set → error.
  - `TestComposeStillRejectsSurfaces`: compose + surfaces → error.
  - Existing single-service validation tests still pass (image app unaffected).
- [ ] **Step 2: Run** `go test ./internal/spec/ -v` → FAIL.
- [ ] **Step 3: Implement** the compose branch in `Validate` (and add the `Service` field if missing). Ensure the parse round-trips `service`/`compose` from TOML (add a quick parse test if `Service` is newly added).
- [ ] **Step 4: Run** `go test ./internal/spec/ -v` → PASS; `CGO_ENABLED=0 go build ./...` clean.
- [ ] **Step 5: Commit** `git add internal/spec/ && git commit -m "feat: validate compose apps in spec"`

---

### Task 2: node — ConnectContainerToNetwork

**Files:** Modify `internal/node/node.go`, `internal/node/local.go`, `internal/node/fake.go`, `internal/node/fake_test.go`.

**Interfaces:** Add to `Node`: `ConnectContainerToNetwork(ctx context.Context, containerID, network string) error`. Fake records the call (and remembers connections for assertion). Local uses the Docker SDK `NetworkConnect`; idempotent — if the container is already on the network, treat "already exists" as success.

- [ ] **Step 1: Write failing fake test** `TestFakeConnectContainerToNetwork`: after `ConnectContainerToNetwork(ctx,"c1","lwd")`, `f.Calls` contains `ConnectContainerToNetwork:c1:lwd`. Keep `var _ Node = (*Fake)(nil)` and `(*Local)(nil)`.
- [ ] **Step 2: Run** `go test ./internal/node/ -v` → FAIL.
- [ ] **Step 3: Implement** interface method + fake (record) + local (SDK `NetworkConnect(ctx, network, containerID, nil)`; ignore/treat-as-ok an "already exists in network" error — adapt to the resolved SDK error shape; keep imports clean).
- [ ] **Step 4: Run** `go test ./internal/node/ -v` → PASS; build clean.
- [ ] **Step 5: Commit** `git add internal/node/ && git commit -m "feat: node ConnectContainerToNetwork"`

---

### Task 3: compose package — Composer interface + CLI impl + fake

**Files:** Create `internal/compose/compose.go`, `internal/compose/fake.go`, `internal/compose/compose_test.go`.

**Interfaces:**
- `type UpSpec struct { Project, File string; Env map[string]string }`
- `type Composer interface { Up(ctx context.Context, spec UpSpec) error; Down(ctx context.Context, project, file string) error; ServiceContainer(ctx context.Context, project, service string) (id, name string, err error) }`
- `func NewCLI() *CLI` — real impl shelling out to `docker compose`. `Up` runs `docker compose -p <project> -f <file> up -d --remove-orphans` with `cmd.Env = os.Environ() + spec.Env` (KEY=VAL). `Down` runs `... down`. `ServiceContainer` runs `docker compose -p <project> -f <file> ps -q <service>` to get the container id, then `docker inspect -f '{{.Name}}' <id>` (strip leading `/`) for the name; error if no container. (Use the docker client for inspect if simpler; either is fine.) On non-zero exit, return an error including stderr.
- `type Fake struct { ... }` with `NewFake() *Fake`, knobs `UpErr`, `ServiceID`, `ServiceName`, `ServiceErr`, `DownErr`, inspectable `Calls []string` and `LastUp UpSpec`. `var _ Composer = (*Fake)(nil)`.

- [ ] **Step 1: Write failing test** `compose_test.go` for the FAKE: `Up` records `LastUp` (project/file/env); `ServiceContainer` returns configured id/name or `ServiceErr`; `Down` records the call; include the `var _ Composer` assertion.
- [ ] **Step 2: Run** `go test ./internal/compose/ -v` → FAIL.
- [ ] **Step 3: Implement** `compose.go` (CLI, adapt to real `docker compose` output — behavior is the contract) + `fake.go`.
- [ ] **Step 4: Run** `go test ./internal/compose/ -v` → PASS; build + vet clean. (No real-Docker test here; the e2e in Task 7 exercises the CLI.)
- [ ] **Step 5: Commit** `git add internal/compose/ && git commit -m "feat: compose package (Composer interface, docker compose CLI, fake)"`

---

### Task 4: store — compose content column

**Files:** Modify `internal/store/store.go`, `internal/store/store_test.go`.

**Interfaces:** Add `Compose string` to `Deployment`; add a `compose TEXT NOT NULL DEFAULT ''` column via a safe idempotent migration (same guarded `PRAGMA table_info` + `ALTER TABLE` pattern used for the `spec` column in Phase 2 — reuse that approach). `RecordDeployment` persists `Compose`; all `SELECT`s include it.

- [ ] **Step 1: Write failing tests**: `Compose` round-trips through `RecordDeployment` + `CurrentDeployment`; a pre-existing DB without the column migrates on `Open` (mirror the Phase-2 `TestMigrationFromPreSpecSchema` pattern — a table lacking `compose` gets it, data preserved). Use `t.TempDir()`.
- [ ] **Step 2: Run** `go test ./internal/store/ -run 'Compose|Migration' -v` → FAIL.
- [ ] **Step 3: Implement** the column + migration + field in all reads/writes.
- [ ] **Step 4: Run** `go test ./internal/store/ -v` → PASS; build clean.
- [ ] **Step 5: Commit** `git add internal/store/ && git commit -m "feat: store compose content per deployment"`

---

### Task 5: reconciler — compose deploy path + rollback + rm

**Files:** Modify `internal/reconciler/reconciler.go`, `internal/reconciler/reconciler_test.go`.

**Interfaces:** `New` gains a `compose.Composer` param: `func New(n node.Node, r router.Router, s *store.Store, sec SecretResolver, comp compose.Composer) *Reconciler`. Add `applyCompose(ctx, *spec.App) (*store.Deployment, error)`. `Apply` branches at the top: `if app.Compose != "" { return r.applyCompose(ctx, app) }` else existing blue-green. `Rollback` and the rm path (see below) handle both shapes.

**applyCompose flow** (hold the mutex; the existing `Apply` mutex must guard both branches — put the lock in `Apply` before the branch, and have `applyCompose` assume it's held, OR lock in each; pick one and be consistent — simplest: lock at the top of `Apply` before branching):
1. `app.Validate()` (already done if lock/validate is before the branch — ensure validate runs).
2. `router.EnsureUp`; `node.EnsureNetwork("lwd")`.
3. `secretVals, err := r.secrets.Resolve(app.Name, app.Secrets)` — fail-closed: on error record `StatusFailed` (spec snapshot; compose content if readable) and return, before running compose. Merge `app.Env` + `secretVals` → `env`.
4. Read compose file content from `<app dir>/<app.Compose>` — BUT the reconciler only gets the parsed `*spec.App`, not the dir. **Decision:** the compose file path in `app.Compose` is resolved by the CLI layer (which knows the dir) relative to the app dir; the reconciler needs the resolved absolute path + the content. Add a field to `spec.App` populated at load time: when `spec.Load(dir)` parses a compose app, resolve `Compose` to an absolute path (filepath.Join(dir, Compose)) and store it back in `App.Compose`; also the reconciler reads the file content itself for the snapshot. So `applyCompose` does `content, err := os.ReadFile(app.Compose)`. (Confirm `spec.Load` resolves the path to absolute — add that in Task 1 or here; document which.)
5. `project := "lwd-" + app.Name`. `compose.Up({Project: project, File: app.Compose, Env: env})`.
6. `id, name, err := compose.ServiceContainer(project, app.Service)` → error if none.
7. `node.ConnectContainerToNetwork(id, "lwd")`.
8. `router.SetRoute({Domain: app.Domain, Upstream: name, Port: app.Port, TLSInternal: router.UseInternalTLS(app.Domain)})` (new stack is live immediately).
9. Health-check through Caddy (staging probe against `name`, layered — reuse the existing `checkHealth`-style helper; for compose there's no separate staged container, so probe the routed service via a staging route to `name`). On failure: record `StatusFailed` (with spec + compose content), return error (stack stays live but flagged). On success: retire prior running deployment row (do NOT tear the stack down — compose owns it), record `StatusRunning` with `Spec` + `Compose` content.

**Rollback:** if the previous deployment has non-empty `Compose` content (compose app), write it to a temp file, set the unmarshaled spec's `Compose` to that temp path, and call `applyCompose`. Else (single-service) the existing rollback.

**Rm path:** the API currently removes containers by label + retires. Add reconciler support so the daemon can tear down a compose app via `compose down`. Simplest: add `func (r *Reconciler) Remove(ctx, app string) error` that looks up the current deployment; if it's a compose app (Compose content present / spec has Compose), `compose.Down("lwd-"+app, <temp file from stored content>)` + `router.RemoveRoute(domain)` + retire; else the existing single-service removal (remove containers by label + RemoveRoute + retire). Wire the API `removeApp` to call `reconciler.Remove` instead of doing it inline (moves the delete logic into the reconciler, which the Phase 2 review flagged as desirable). Update `api.New`/`Server` if needed so it can call the reconciler's Remove.

- [ ] **Step 1: Write failing tests** (fake composer + fake node + fake router + fake resolver + temp store). Update `newTestReconciler` to pass a `compose.NewFake()`.
  - `TestApplyComposeUpConnectsRoutesVerifies`: compose app, fake composer returns a service container `("cid","lwd-webapp-web-1")`, ProbeStatus 200 → asserts order Up → ServiceContainer → ConnectContainerToNetwork → SetRoute → probe; a `StatusRunning` deployment recorded with non-empty `Compose`; route points at `lwd-webapp-web-1`; `fakeComposer.LastUp.Env` contains merged env+secret.
  - `TestApplyComposeFailClosedSecret`: resolver error → no `Up` call, `StatusFailed` recorded, error returned.
  - `TestApplyComposeHealthFailRecordsFailed`: ProbeStatus 502 → error, `StatusFailed` recorded (stack left up — no Down called).
  - `TestRollbackComposeReappliesStored`: apply v1 (compose content A) success, apply v2 (content B) success, rollback → applyCompose runs with content A (assert the composer's LastUp file content equals A, or that a compose deploy occurred and the restored deployment references content A).
  - `TestRemoveComposeCallsDown`: current deployment is a compose app → `Remove` calls `compose.Down` + `RemoveRoute` + retires.
  - Keep all existing single-service reconciler tests green (the branch must not change them).
- [ ] **Step 2: Run** `go test ./internal/reconciler/ -v` → FAIL.
- [ ] **Step 3: Implement** the branch, `applyCompose`, `Rollback` compose case, `Remove`. Update `New` signature; update call sites minimally to compile (cli daemon passes real `compose.NewCLI()`; api/client/e2e tests pass `compose.NewFake()`).
- [ ] **Step 4: Run** `go test ./...` → PASS; `CGO_ENABLED=0 go build ./...`, `go vet ./...`, `gofmt -l .` clean. Report what you touched outside internal/reconciler.
- [ ] **Step 5: Commit** `git add -A && git commit -m "feat: compose deploy/rollback/remove in reconciler"`

---

### Task 6: daemon + API wiring; spec.Load path resolution; CLI unchanged surface

**Files:** Modify `internal/cli/cli.go`, `internal/api/api.go`, `internal/api/api_test.go`, `internal/spec/spec.go` (if path resolution not done in Task 1).

**Interfaces:**
- Daemon (`runDaemon`): construct `compose.NewCLI()` and pass to `reconciler.New(...)`.
- API `removeApp` → delegate to `reconciler.Remove(ctx, name)` (so compose apps are torn down via `compose down`). Give `Server` a reconciler reference if it doesn't have one (it constructs/holds `rec` already — use it).
- `spec.Load(dir)`: for a compose app, resolve `App.Compose` to `filepath.Join(dir, App.Compose)` (absolute) so the daemon-side reconciler can read the file. (If Task 1/5 already did this, just verify.) Single-service apps unaffected.
- CLI: no new commands — `apply`/`rm`/`rollback`/`ls`/`logs` already cover compose apps. `lwd ls` DOMAIN column already derives from the spec snapshot (works for compose too). Verify `logs` for a compose app streams the routed service's container logs (the current deployment's ContainerID — ensure applyCompose records the routed container id as `ContainerID` so `logs` works).

- [ ] **Step 1: Write failing api test**: `TestDeleteComposeAppCallsReconcilerRemove` or extend delete tests — with a fake stack, deleting a compose app routes through `reconciler.Remove` (assert via fake composer Down call or route removal). If wiring `removeApp`→`reconciler.Remove` changes behavior for single-service, keep the existing delete test green.
- [ ] **Step 2: Run** `go test ./internal/api/ -v` → FAIL.
- [ ] **Step 3: Implement** daemon wiring + `removeApp` delegation + `spec.Load` path resolution + ensure `applyCompose` records the routed container id as the deployment `ContainerID` (for `logs`).
- [ ] **Step 4: Verify** `CGO_ENABLED=0 go build -o /tmp/lwd ./cmd/lwd && go test ./... && go vet ./... && gofmt -l . && /tmp/lwd version` — all green.
- [ ] **Step 5: Commit** `git add -A && git commit -m "feat: wire compose into daemon; delete via reconciler.Remove; resolve compose path in spec.Load"`

---

### Task 7: e2e + README

**Files:** Modify `test/e2e_test.go`, `README.md`.

- [ ] **Step 1: Write the e2e** (guarded by `LWD_DOCKER_TEST`; also SKIP with a clear message if `docker compose version` fails — compose plugin absent). Build the real stack incl. `compose.NewCLI()`. Write a temp compose file with two services: a web service (`traefik/whoami`, listens on 80) and a backing service (e.g. `redis:7-alpine`). Create an `lwd.toml`-style `spec.App{Compose: <tempfile>, Service:"web", Domain:"compose-whoami.localhost", Port:80}`. Deploy via the reconciler:
  - assert the web service is reachable through Caddy (`GET http://127.0.0.1:80/` Host `compose-whoami.localhost` → 200);
  - assert the backing (redis) container is running (list project containers / `docker compose -p lwd-<app> ps`);
  - capture the redis container id, redeploy, assert the redis container id is UNCHANGED (compose didn't recreate the unchanged backing service — the core Phase 4 guarantee);
  - `reconciler.Remove` (or `compose down`) tears the stack down; assert no project containers remain.
  - Reuse the existing cleanup helpers (also remove `lwd-caddy` + `lwd` network); SKIP if 80/443 in use.
- [ ] **Step 2: Run** `go test ./...` (e2e SKIPs) → PASS.
- [ ] **Step 3: Run** `LWD_DOCKER_TEST=1 go test ./test/ -v` → MUST pass against real Docker + compose; confirm no stray containers/network. If compose plugin absent, it SKIPs — say so.
- [ ] **Step 4: README** — add a "Compose apps" section: the `compose`/`service` fields, the in-place-recreate model (db stays up; not zero-downtime; failed health leaves the new stack live — rollback to recover), the `docker compose` plugin requirement, how env/secrets reach the stack. Update the Scope section to list compose as supported and adjust Known limitations. **Verify the whole README still accurately reflects Phases 1–4** (build/run/apply/secrets/networking all current).
- [ ] **Step 5: Commit** `git add -A && git commit -m "test: e2e compose app (db survives redeploy); docs: compose apps README"`

---

## Self-Review

**Spec coverage:** compose validation (T1); ConnectContainerToNetwork (T2); Composer interface+CLI+fake (T3); compose content persistence (T4); applyCompose + rollback + remove (T5); daemon/API wiring + path resolution + logs (T6); e2e proving db-survives-redeploy + README (T7). ✓

**Deferred (by design):** surfaces-outside-compose blue-green; web UI (Phase 5); lwd.toml skill (Phase 6); MCP (Phase 7).

**README:** Task 7 Step 4 makes it an explicit, verified deliverable (final review checks accuracy across Phases 1–4).

**Placeholder scan:** logic/testable units (spec validation, node method, compose fake, store column) have concrete tests; compose CLI + reconciler + e2e give behavior contracts + adapt-notes (compose output parsing), matching the pattern used for Docker/Caddy infra in prior phases. No TBD.

**Type consistency:** `reconciler.New` gains `compose.Composer` (all call sites noted: cli daemon = real CLI, api/client/e2e tests = fake). `Composer` method set identical across `compose.go`/`fake.go`/reconciler use. `store.Deployment.Compose` consistent across store/reconciler/rollback. `node.ConnectContainerToNetwork` consistent across node.go/local.go/fake.go. `spec.App.Service` added + resolved-to-absolute `Compose` path in `spec.Load`.

**Cross-task note:** T5 changes `reconciler.New`; T6 finishes wiring. Both keep `go test ./...` green; the real compose CLI is only exercised by T7's guarded e2e.
