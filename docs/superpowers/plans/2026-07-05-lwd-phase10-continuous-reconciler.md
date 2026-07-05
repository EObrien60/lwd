# lwd Phase 10 Implementation Plan — continuous reconciler (self-heal + health observation)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`).

**Goal:** Add a background control loop to the daemon that periodically self-heals dead **surface** containers (recreate via the existing blue-green path, no re-clone/rebuild) and observes node + edge health, exposed via `GET /health` → CLI + web — controller stays off the request path.

**Architecture:** One new method `Reconciler.Reconcile(ctx)` (reuses `deployBlueGreenSurface`/`applyImage`/a refactored `rollbackGitLocked` for healing), guarded by the existing `r.mu` so it serializes with `Apply`/`Remove`/`Rollback`. A thin loop goroutine in `runDaemon` drives it on a ticker + post-Apply nudge. An in-memory `Health` snapshot (owned by the Reconciler) is read by a new `/health` endpoint. New capability reaches the Reconciler via **setters** (not `New` params — it has 14 call sites).

**Tech Stack:** Go 1.25+, no cgo, module `lwd`; stdlib (`time`, `context`, `sync`); `internal/{reconciler,config,router,api,client,cli,web}`.

## Global Constraints

- Go 1.25+; no cgo. Module `lwd`. Spec: `docs/superpowers/specs/2026-07-05-lwd-phase10-continuous-reconciler-design.md`; north star `docs/VISION.md`.
- **Heal strategy = recreate via blue-green** from the CURRENT deployment's recorded spec + already-built `Image`. Git apps reuse the recorded tag — **no re-clone, no rebuild** (like `Rollback`). Never a plain `docker start`.
- **Heal scope = surfaces only** (`lwd.role=surface`, i.e. image/git single-service apps). **Compose apps and `[[services]]` backing are never healed** (skipped by the heal path). 
- **Node/edge health = observe + report only.** No cross-node action; a surface on an unreachable node is marked `degraded` and left. (Reschedule is P11.)
- **Heal trigger = container missing OR not `running`.** A `running`-but-Docker-`unhealthy` container is `degraded`, **not** recreated.
- **Trigger cadence = interval ticker (`LWD_RECONCILE_INTERVAL`, default `15s`) + post-Apply nudge.**
- **Crash-loop protection:** per-app exponential backoff, give-up after `LWD_HEAL_MAX_ATTEMPTS` (default `5`); a successful heal or healthy check resets it; a manual successful deploy resets it.
- **Concurrency invariant:** no two mutating deploy operations interleave — heal work holds `r.mu`; health probes do NOT hold `r.mu`; the `Health` snapshot has its own `sync.RWMutex`, never held across a Docker/router call.
- **Zero regression:** single-node path and `Apply`/`Remove`/`Rollback`/compose/backing behavior unchanged. All tests use fakes; `go test ./...` passes with **no Docker**.
- **No secret value** appears in the health snapshot or any new endpoint/CLI/web output.
- **README + VISION** updated in the final task; verified in final review.

---

### Task 1: config knobs + health types + Reconciler snapshot state

**Files:** Modify `internal/config/config.go` (+ its test if present), `internal/reconciler/reconciler.go`; Create `internal/reconciler/health.go`, `internal/reconciler/health_test.go`.

**Interfaces produced (consumed by Tasks 3–6):**
- `config.ReconcileInterval() time.Duration` (env `LWD_RECONCILE_INTERVAL`, default `15*time.Second`; unparseable/empty → default).
- `config.HealMaxAttempts() int` (env `LWD_HEAL_MAX_ATTEMPTS`, default `5`; unparseable/≤0 → default).
- Types in package `reconciler`:
  ```go
  type SurfaceState string
  const (
      SurfaceHealthy  SurfaceState = "healthy"
      SurfaceDegraded SurfaceState = "degraded"
      SurfaceHealing  SurfaceState = "healing"
      SurfaceFailed   SurfaceState = "failed"
  )
  type AppHealth struct {
      App          string       `json:"app"`
      State        SurfaceState `json:"state"`
      LastError    string       `json:"last_error,omitempty"`
      HealAttempts int          `json:"heal_attempts"`
      UpdatedAt    time.Time    `json:"updated_at"`
  }
  type NodeHealth struct {
      Name      string    `json:"name"`
      Transport string    `json:"transport"`
      Reachable bool      `json:"reachable"`
      UpdatedAt time.Time `json:"updated_at"`
  }
  type EdgeHealth struct {
      Reachable bool      `json:"reachable"`
      UpdatedAt time.Time `json:"updated_at"`
  }
  type Health struct {
      Nodes []NodeHealth `json:"nodes"`
      Edge  EdgeHealth   `json:"edge"`
      Apps  []AppHealth  `json:"apps"`
  }
  // Reachability is the subset of *node.RegistryResolver the reconciler uses to
  // observe node health; supplied via SetReachability so New's many call sites
  // don't change. *node.RegistryResolver satisfies it.
  type Reachability interface {
      Reachable(ctx context.Context, name string) (transport string, ok bool)
  }
  ```
- On `*Reconciler` (added fields + methods):
  - fields: `healthMu sync.RWMutex`, `health Health`, `reach Reachability`, `nudge chan<- struct{}`, `heal map[string]*healState` (init in `New`).
  - `type healState struct { attempts int; nextEligible time.Time }` (unexported).
  - `func (r *Reconciler) SetReachability(rr Reachability)` — sets `r.reach`.
  - `func (r *Reconciler) SetNudge(ch chan<- struct{})` — sets `r.nudge`.
  - `func (r *Reconciler) HealthSnapshot() Health` — returns a deep copy (new slices) under `healthMu.RLock`.
  - `func (r *Reconciler) setHealth(h Health)` — stores a deep copy under `healthMu.Lock` (unexported).
  - `func (r *Reconciler) signalNudge()` — non-blocking send on `r.nudge` if non-nil (`select { case r.nudge <- struct{}{}: default: }`).

- [ ] **Step 1: failing tests** — `health_test.go`:
  - `TestHealthSnapshotReturnsCopy`: `setHealth(Health{Nodes:[]NodeHealth{{Name:"a"}}, Apps:[]AppHealth{{App:"x",State:SurfaceHealthy}}})`; get via `HealthSnapshot()`; mutate the returned slice's element; get again → original unchanged (proves deep copy).
  - `TestHealthSnapshotConcurrent`: spawn N goroutines calling `setHealth` and M calling `HealthSnapshot()` concurrently; `go test -race` clean, no panic.
  - `TestSignalNudgeNonBlocking`: with `nudge` unset → `signalNudge()` no-op; with a buffered `chan struct{}` of cap 1 set → first `signalNudge()` delivers, second (buffer full) does not block.
  - Build a `*Reconciler` for these via `New(node.FakeResolver{"local": node.NewFake()}, <fakeRouter>, s, <fakeSecrets>, compose.NewFake(), source.NewFake(), build.NewFake())` — mirror how `reconciler` package tests already construct one (check `internal/reconciler/*_test.go` for the existing helper/fakes and reuse them; if the reconciler package has no test helper, construct inline with the same fakes `test/e2e_test.go:822` uses).
  - In `internal/config/config_test.go` (create if absent, package `config`): `TestReconcileIntervalDefault` (unset → 15s), `TestReconcileIntervalEnv` (`t.Setenv("LWD_RECONCILE_INTERVAL","5s")` → 5s), `TestReconcileIntervalInvalid` (`"garbage"` → 15s); `TestHealMaxAttemptsDefault/Env/Invalid` (unset→5, `"3"`→3, `"0"`/`"x"`→5).
- [ ] **Step 2:** `go test ./internal/reconciler/ ./internal/config/ -run 'Health|Nudge|Reconcile(Interval)|HealMax' -v` → FAIL (undefined).
- [ ] **Step 3:** add `config.ReconcileInterval`/`HealMaxAttempts` (use `time.ParseDuration` / `strconv.Atoi`); create `health.go` with the types; add the fields to the `Reconciler` struct, initialize `heal: map[string]*healState{}` in `New`, and add the accessor/setters. Deep copy in `HealthSnapshot`/`setHealth` = allocate fresh `Nodes`/`Apps` slices and copy elements.
- [ ] **Step 4:** `go test ./internal/reconciler/ ./internal/config/ -v` PASS; `CGO_ENABLED=0 go build ./...`; `go vet ./internal/reconciler/ ./internal/config/`; `gofmt -l internal/reconciler internal/config` empty.
- [ ] **Step 5:** commit `feat: reconciler health snapshot types + reconcile/heal config knobs`.

---

### Task 2: `router.Router.Healthy` (edge health probe)

**Files:** Modify `internal/router/router.go` (interface + `CaddyRouter`), `internal/router/*_test.go`; and EVERY other `router.Router` implementation — the test fakes. Grep first: `grep -rn "func.*ProbeThroughCaddy" --include='*.go' .` finds each fake router (they implement the full interface); each needs the new method to keep compiling.

**Interfaces produced:** `Router.Healthy(ctx context.Context) bool` — reports whether Caddy is reachable/administrable right now (no mutation). `CaddyRouter.Healthy` checks the Caddy **admin** endpoint the same way `Reload`/`EnsureUp` already reach it (reuse the existing admin base URL / http client in `CaddyRouter`; a `GET <admin>/config/` returning a 2xx is "healthy"). On any transport error or non-2xx → `false`. Fakes: add a settable `HealthyResult bool` field (default it to `true` in their constructor or treat zero-value with a `healthySet` flag — simplest: add `Healthy bool` field defaulting true via constructor, or a `HealthyFn func() bool`). Match each fake's existing style.

- [ ] **Step 1: failing test** — in `internal/router/` add `TestCaddyRouterHealthy` if the package has a testable admin seam (an httptest server standing in for the Caddy admin, mirroring however existing router tests exercise the admin client); assert 2xx→true, non-2xx/closed→false. If the router package's tests don't already mock the admin endpoint, instead add the method and cover `Healthy` via the fake in the reconciler tests (Task 3/4) — but STILL implement `CaddyRouter.Healthy` for real. (Reviewer note: don't ship an untested real method if the package has an existing admin-mock pattern; use it.)
- [ ] **Step 2:** `go test ./internal/router/ -run Healthy -v` → FAIL (or compile error until fakes updated).
- [ ] **Step 3:** add `Healthy(ctx) bool` to the `Router` interface; implement on `CaddyRouter`; add the method to every fake router found by the grep (default true).
- [ ] **Step 4:** `go test ./... -run Healthy` PASS; `CGO_ENABLED=0 go build ./...` (all fakes satisfy the interface again); `go vet ./...`; `gofmt -l .` empty.
- [ ] **Step 5:** commit `feat: router.Healthy edge reachability probe`.

---

### Task 3: `healSurface` + rollbackGit refactor (locked variant)

**Files:** Modify `internal/reconciler/reconciler.go`, `internal/reconciler/reconciler_test.go` (or a new `internal/reconciler/heal_test.go`).

**Context:** `Rollback`→`rollbackGit` already "redeploy this git app's recorded spec+built tag without re-clone/rebuild" and takes `r.mu` itself. The heal path runs while ALREADY holding `r.mu` (Task 4), so it needs a lock-free body. Extract it.

**Interfaces produced (consumed by Task 4):**
- Refactor: split `rollbackGit(ctx, app)` into `rollbackGit` (locks `r.mu`, then calls the body) and `func (r *Reconciler) rollbackGitLocked(ctx context.Context, app *spec.App) (*store.Deployment, error)` (the current body verbatim, WITHOUT the `r.mu.Lock()/Unlock()`). `rollbackGit` becomes `{ r.mu.Lock(); defer r.mu.Unlock(); return r.rollbackGitLocked(ctx, app) }`. Behavior identical (existing rollback tests must still pass).
- `func (r *Reconciler) healSurfaceLocked(ctx context.Context, cur *store.Deployment) (*store.Deployment, error)` — CALLER MUST HOLD `r.mu`. Reconstructs `spec.App` from `cur.Spec`, pins `.Image = cur.Image`, and redeploys via the existing back-half:
  ```go
  func (r *Reconciler) healSurfaceLocked(ctx context.Context, cur *store.Deployment) (*store.Deployment, error) {
      var restored spec.App
      if cur.Spec == "" {
          return nil, fmt.Errorf("no spec snapshot to heal %q", cur.App)
      }
      if err := json.Unmarshal([]byte(cur.Spec), &restored); err != nil {
          return nil, fmt.Errorf("unmarshal spec snapshot for %q: %w", cur.App, err)
      }
      restored.Image = cur.Image
      if restored.Git != nil {
          return r.rollbackGitLocked(ctx, &restored) // reuses built tag, no clone/build
      }
      if err := restored.Validate(); err != nil {
          return nil, fmt.Errorf("invalid spec snapshot for %q: %w", cur.App, err)
      }
      return r.applyImage(ctx, &restored) // applyImage assumes r.mu held; image already present
  }
  ```
- `func surfaceIsDead(state string, err error) bool` — classify a `node.ContainerHealth` result as needing heal: `return err != nil || (state != "running")`. (A container-not-found manifests as either an error or a non-running/empty state depending on the Node impl; both mean dead. `node.Fake.ContainerHealth` returns `HealthState`; a caller can set it to `"exited"` or leave a not-found error.)
- Reset hook: in `deployBlueGreenSurface`, on the SUCCESS path (right before returning the `StatusRunning` deployment), add `r.resetHeal(app.Name)`. Add `func (r *Reconciler) resetHeal(app string) { delete(r.heal, app) }` (caller holds `r.mu`). This makes EVERY successful surface deploy — manual Apply, Rollback, or heal — clear the app's backoff.

- [ ] **Step 1: failing tests** (reconciler package fakes, no Docker):
  - `TestHealSurfaceImageApp`: store has a current `StatusRunning` image-app deployment (Spec snapshot with `Image` set, `Git`/`Compose` nil); call `r.mu.Lock(); dep, err := r.healSurfaceLocked(ctx, cur); r.mu.Unlock()`; assert a NEW surface container was run on the fake node, the fake router got a live route, and a new `StatusRunning` row exists. Use the SAME fakes the existing reconciler apply tests use.
  - `TestHealSurfaceGitAppReusesTagNoBuild`: current deployment's Spec has `Git != nil`, `Image = "lwd-build/app:abc123"`; a `build.Fake` recording calls; assert `healSurfaceLocked` produces a running surface and the `source.Fake`'s Clone and `build.Fake`'s Build were **NOT** called (reused tag).
  - `TestRollbackStillWorks`: an existing rollback test (or a new equivalent) passes unchanged after the refactor.
  - `TestSurfaceIsDead`: `surfaceIsDead("running", nil)==false`; `surfaceIsDead("exited", nil)==true`; `surfaceIsDead("", errors.New("not found"))==true`.
- [ ] **Step 2:** `go test ./internal/reconciler/ -run 'Heal|Rollback|SurfaceIsDead' -v` → FAIL.
- [ ] **Step 3:** do the `rollbackGit` split; add `healSurfaceLocked`, `surfaceIsDead`, `resetHeal`; add the `resetHeal(app.Name)` call in `deployBlueGreenSurface`'s success path.
- [ ] **Step 4:** `go test ./internal/reconciler/ -v` PASS (incl. all pre-existing apply/rollback tests); build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: healSurface (recreate surface from recorded spec+image); rollbackGit locked split`.

---

### Task 4: `Reconciler.Reconcile(ctx)` — the reconciliation pass

**Files:** Modify `internal/reconciler/reconciler.go`, `internal/reconciler/reconciler_test.go` (or `heal_test.go`).

**Interfaces produced (consumed by Task 5 & 6):**
- `func (r *Reconciler) Reconcile(ctx context.Context) error` — one pass:
  ```go
  func (r *Reconciler) Reconcile(ctx context.Context) error {
      apps, err := r.store.ListApps()
      if err != nil {
          return fmt.Errorf("reconcile: list apps: %w", err)
      }
      appHealth := make([]AppHealth, 0, len(apps))
      for _, app := range apps {
          if ah := r.reconcileApp(ctx, app); ah != nil {
              appHealth = append(appHealth, *ah)
          }
      }
      r.setHealth(Health{
          Nodes: r.probeNodes(ctx),
          Edge:  r.probeEdge(ctx),
          Apps:  appHealth,
      })
      return nil
  }
  ```
- `reconcileApp(ctx, app) *AppHealth` (unexported) — does NOT hold `r.mu` for the read/probe; only `tryHeal` takes `r.mu`:
  1. `cur, err := r.store.CurrentDeployment(app)`; if `err != nil` → return a `SurfaceDegraded` AppHealth with the error; if `cur == nil` → return `nil` (nothing to reconcile).
  2. If `cur.ContainerID == "" || isComposeApp(cur.Spec)` → return `nil` (compose/backing not healed here; skipped).
  3. `nodeName := nodeFromSpec(cur.Spec)` (empty → "local"). If `r.reach != nil`: `_, ok := r.reach.Reachable(ctx, nodeName)`; if `!ok` → return `AppHealth{app, SurfaceDegraded, "node "+nodeName+" unreachable", healAttempts(app), now()}` (do NOT heal — node down).
  4. Resolve node: `n, _, _, _, rerr := r.resolver.ResolveMeta(nodeName)`; if `rerr != nil` → `SurfaceDegraded` with the error, no heal.
  5. `state, _, herr := n.ContainerHealth(ctx, cur.ContainerID)`. If `!surfaceIsDead(state, herr)` → `r.clearHealAttempts(app)` and return `AppHealth{app, SurfaceHealthy, "", 0, now()}`.
  6. Dead → `return r.tryHeal(ctx, app, cur)`.
- `tryHeal(ctx, app, cur) *AppHealth` — takes `r.mu` (mutating):
  ```go
  func (r *Reconciler) tryHeal(ctx context.Context, app string, cur *store.Deployment) *AppHealth {
      r.mu.Lock()
      defer r.mu.Unlock()
      hs := r.heal[app]
      if hs == nil { hs = &healState{}; r.heal[app] = hs }
      now := time.Now()
      if hs.attempts >= config.HealMaxAttempts() {
          return &AppHealth{App: app, State: SurfaceFailed, LastError: "gave up after max heal attempts", HealAttempts: hs.attempts, UpdatedAt: now}
      }
      if now.Before(hs.nextEligible) {
          return &AppHealth{App: app, State: SurfaceDegraded, LastError: "waiting for heal backoff", HealAttempts: hs.attempts, UpdatedAt: now}
      }
      hs.attempts++
      if _, err := r.healSurfaceLocked(ctx, cur); err != nil {
          hs.nextEligible = now.Add(healBackoff(hs.attempts))
          return &AppHealth{App: app, State: SurfaceFailed, LastError: err.Error(), HealAttempts: hs.attempts, UpdatedAt: now}
      }
      // success: deployBlueGreenSurface already called resetHeal(app), so hs is
      // gone from r.heal; report healthy.
      return &AppHealth{App: app, State: SurfaceHealthy, HealAttempts: 0, UpdatedAt: now}
  }
  ```
- Helpers (all under `r.mu` except `healBackoff`):
  - `func healBackoff(attempts int) time.Duration` — exponential from a base, capped: `d := healBackoffBase << (attempts-1)` clamped to `healBackoffMax`; consts `healBackoffBase = 15*time.Second`, `healBackoffMax = 5*time.Minute`.
  - `func (r *Reconciler) clearHealAttempts(app string)` — `r.mu.Lock(); delete(r.heal, app); r.mu.Unlock()` (called from reconcileApp which does NOT already hold the lock — so it locks internally; keep it distinct from `resetHeal`, which assumes the lock is held).
  - `func (r *Reconciler) healAttempts(app string) int` — read `r.heal[app].attempts` under `r.mu.RLock`? `r.mu` is a `sync.Mutex`, not RWMutex — use `Lock`. Keep it tiny.
  - `probeNodes(ctx) []NodeHealth` — if `r.reach == nil` return `nil`; else `nodes,_ := r.store.ListNodes()`; for each plus the implicit `"local"`, `transport, ok := r.reach.Reachable(ctx, name)`; collect `NodeHealth{Name, Transport, Reachable: ok, UpdatedAt: now}`. (Include `local` first.)
  - `probeEdge(ctx) EdgeHealth` — `EdgeHealth{Reachable: r.router.Healthy(ctx), UpdatedAt: now}`.

**Note on `now()`:** use `time.Now()` directly (the package already does). No fake clock needed.

- [ ] **Step 1: failing tests** (fakes, no Docker; a `fakeReach` implementing `Reachability`, and the fake router with `Healthy`):
  - `TestReconcileHealsDeadSurface`: current running image-app deployment; fake node reports its container `"exited"`; `fakeReach` returns `("ssh",true)` for the app's node; `Reconcile(ctx)` → a new surface is created + route set + old row retired + new `StatusRunning`; snapshot's app is `SurfaceHealthy`.
  - `TestReconcileHealthyNoop`: container `"running"` → no new container, no route change; snapshot app `SurfaceHealthy`, heal map empty.
  - `TestReconcileSkipsCompose`: current deployment is a compose app (`isComposeApp` true) → not in snapshot Apps, no heal attempted.
  - `TestReconcileNodeUnreachableDegraded`: `fakeReach` returns `ok=false` for the app's node → app `SurfaceDegraded`, no container heal attempted (assert the fake node's RunContainer NOT called).
  - `TestReconcileBackoffAndGiveUp`: heal always fails (fake node RunContainer errors, or router SetStaging errors); `t.Setenv("LWD_HEAL_MAX_ATTEMPTS","2")`; call `Reconcile` repeatedly — attempts increment, `nextEligible` gates intervening calls (advance by constructing state or asserting the `SurfaceDegraded` "backoff" state on the immediate 2nd call), and after 2 attempts state is `SurfaceFailed` "gave up"; then a successful manual `Apply` clears it (heal map empty afterward).
  - `TestReconcileSnapshotNodesAndEdge`: with `fakeReach` + a fake router whose `Healthy` returns false → snapshot `Edge.Reachable==false` and `Nodes` includes `local` + any registered node with the reported transport.
  - `TestReconcileConcurrentWithApply` (optional but recommended): run `Reconcile` and `Apply` from two goroutines under `-race`; no data race, no deadlock (proves the `r.mu` discipline).
- [ ] **Step 2:** `go test ./internal/reconciler/ -run Reconcile -v` → FAIL.
- [ ] **Step 3:** implement `Reconcile`, `reconcileApp`, `tryHeal`, `probeNodes`, `probeEdge`, `healBackoff`, `clearHealAttempts`, `healAttempts`.
- [ ] **Step 4:** `go test ./internal/reconciler/ -race -v` PASS; build/vet/gofmt clean; `go test ./...` green.
- [ ] **Step 5:** commit `feat: Reconciler.Reconcile pass — self-heal surfaces + node/edge health snapshot`.

---

### Task 5: control loop + daemon wiring (nudge, ticker, graceful shutdown)

**Files:** Create `internal/reconciler/loop.go`, `internal/reconciler/loop_test.go`; Modify `internal/cli/cli.go` (`runDaemon`), and `internal/reconciler/reconciler.go` (nudge on Apply/Rollback success).

**Interfaces produced:**
- `func RunLoop(ctx context.Context, rec Reconcilable, interval time.Duration, nudge <-chan struct{})` in package `reconciler` — loops until `ctx.Done()`, calling `rec.Reconcile(ctx)` on each ticker tick AND each nudge; recovers panics per pass (log + continue); an initial pass runs once immediately on start.
  ```go
  type Reconcilable interface{ Reconcile(ctx context.Context) error }
  func RunLoop(ctx context.Context, rec Reconcilable, interval time.Duration, nudge <-chan struct{}) {
      t := time.NewTicker(interval)
      defer t.Stop()
      pass := func() {
          defer func() { if p := recover(); p != nil { log.Printf("reconcile loop: recovered panic: %v", p) } }()
          if err := rec.Reconcile(ctx); err != nil { log.Printf("reconcile: %v", err) }
      }
      pass() // initial
      for {
          select {
          case <-ctx.Done():
              return
          case <-t.C:
              pass()
          case <-nudge:
              pass()
          }
      }
  }
  ```
- Nudge on manual deploys: at the SUCCESS return of `Apply` and `Rollback`, call `r.signalNudge()`. Do this at the single points where they return a non-error `*store.Deployment` (wrap: capture `dep, err`, if `err == nil { r.signalNudge() }`, return). Keep behavior otherwise identical.

**Daemon wiring in `runDaemon`:**
- After building `srv := api.New(reconciler.New(...), ...)`, keep a handle to the reconciler (construct it into a var first: `rec := reconciler.New(resolver, r, s, secStore, compose.NewCLI(), source.NewCLI(), build.NewCLI()); srv := api.New(rec, s, n, r, secStore, resolver)`).
- `rec.SetReachability(resolver)` (the `*RegistryResolver` satisfies `reconciler.Reachability`).
- `nudge := make(chan struct{}, 1); rec.SetNudge(nudge)`.
- Create a cancellable context tied to SIGINT/SIGTERM: `ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM); defer stop()`.
- `go reconciler.RunLoop(ctx, rec, config.ReconcileInterval(), nudge)`.
- Serve as today, but so shutdown is graceful: run `httpSrv.Serve(ln)` in a goroutine and select on `ctx.Done()` to `httpSrv.Shutdown(context.Background())`, OR (simpler, acceptable) keep `Serve` in the foreground and rely on process exit to stop the loop — BUT since we created `ctx` from `signal.NotifyContext`, prefer: run Serve in a goroutine, `<-ctx.Done()`, then `httpSrv.Shutdown`. Return 0 after shutdown. (Keep the existing socket setup/chmod unchanged.)

- [ ] **Step 1: failing tests** — `loop_test.go`:
  - `TestRunLoopInitialAndTick`: a fake `Reconcilable` counting calls; `ctx` with a short cancel; `interval` small (e.g. 20ms); assert ≥2 calls (initial + at least one tick) before cancel, and it returns after cancel.
  - `TestRunLoopNudgeTriggersPass`: large interval (so ticks don't fire); send on `nudge`; assert a pass ran; cancel returns.
  - `TestRunLoopRecoversPanic`: a `Reconcilable` that panics on the first call and succeeds after; assert the loop keeps running (a later call happens) and does not crash.
- [ ] **Step 2:** `go test ./internal/reconciler/ -run RunLoop -v` → FAIL.
- [ ] **Step 3:** create `loop.go`; add the nudge-on-success to `Apply`/`Rollback`; wire `runDaemon` (imports: `os/signal`, `syscall`).
- [ ] **Step 4:** `go test ./internal/reconciler/ -race -v` PASS; `CGO_ENABLED=0 go build -o /tmp/lwd ./cmd/lwd` (daemon compiles); `go vet ./...`; `gofmt -l .` empty; `go test ./...` green. (Do NOT start a real daemon.)
- [ ] **Step 5:** commit `feat: reconcile control loop + daemon wiring (ticker, nudge, graceful shutdown)`.

---

### Task 6: `GET /health` API + client + `lwd health` CLI

**Files:** Modify `internal/api/api.go` (route + handler), `internal/api/api_test.go`, `internal/client/client.go`, `internal/cli/cli.go`.

**Interfaces produced (consumed by Task 7):**
- API: register `mux.HandleFunc("GET /health", srv.handleHealth)`; handler returns `writeJSON(w, http.StatusOK, srv.rec.HealthSnapshot())`. (The `Server` already holds `rec *reconciler.Reconciler`.)
- client: `type Health = ...` — to avoid importing `internal/reconciler` into `client` (check for an import cycle: `reconciler` imports `client`? It does NOT today; `reconciler` imports node/router/store/etc. `client` imports `store`,`api`,`spec`. If `client` importing `reconciler` is clean, reuse `reconciler.Health` directly: `func (c *Client) Health(ctx) (reconciler.Health, error)`. If it introduces a cycle, define a mirror `client.Health`/`client.NodeHealth`/`client.AppHealth`/`client.EdgeHealth` with identical json tags. VERIFY with `go list -deps` / a build; prefer reusing `reconciler.Health` if no cycle.) Method: GET `/health`, decode, return.
- CLI: `lwd health` subcommand (`runHealth`) — GET via the client, print: a NODES section (name, transport, reachable), an EDGE line (Caddy reachable), and an APPS section (app, state, heal attempts, last error). Register in the command switch next to `node`/`status`.

- [ ] **Step 1: failing api test** — `TestHealthEndpoint`: build a `Server` whose reconciler has a known snapshot (call `rec.Reconcile` against fakes first, OR expose a test seam — simplest: run `Reconcile` on a fake-backed reconciler so `HealthSnapshot` is populated, then `GET /health` and assert the JSON contains the expected nodes/edge/apps). Assert 200 + JSON shape.
- [ ] **Step 2:** `go test ./internal/api/ -run Health -v` → FAIL.
- [ ] **Step 3:** add the route+handler; add `client.Health`; add the `lwd health` CLI command + `runHealth`.
- [ ] **Step 4:** `CGO_ENABLED=0 go build -o /tmp/lwd ./cmd/lwd && go test ./... && go vet ./... && gofmt -l .` all green.
- [ ] **Step 5:** commit `feat: GET /health endpoint + client.Health + lwd health CLI`.

---

### Task 7: web health panel + docs (README + VISION) + e2e

**Files:** Modify `internal/web/server.go` (route + DaemonClient), `internal/web/api.go` or new `internal/web/health.go`, `internal/web/fake_client_test.go`, `internal/web/assets/{index.html,app.js,app.css}`, `internal/web/*_test.go`, `test/e2e_test.go`, `README.md`, `docs/VISION.md`.

- [ ] **Step 1: web (invoke frontend-design skill for the panel)** — add `Health(ctx) (client.Health-or-reconciler.Health, error)` to the web `DaemonClient` interface (matching `*client.Client`); update `fakeDaemon` in `fake_client_test.go`. Add `GET /api/health` (authed, proxies `s.client.Health`) — register in `server.go` under `/api/`; add a handler. Frontend: a **Health panel** on the dashboard (nodes: name/transport/reachable; edge: Caddy up/down; apps: state badge + heal attempts + last error), reusing the existing buildless design system (hand CSS + Alpine, no npm/CDN). Tests (`internal/web`): `GET /api/health` returns the fake's snapshot; requires auth (401 without cookie).
- [ ] **Step 2: e2e** (`test/e2e_test.go`, fakes, no Docker) — `TestEndToEndSelfHeal`: build the real `reconciler.New(FakeResolver, fakeRouter, store, ...)`, deploy an image app (Apply), then make the fake node report the surface container as `"exited"`, call `rec.Reconcile(ctx)`, and assert a NEW running surface exists + the route points at it + a new `StatusRunning` deployment row — the full self-heal path end to end without Docker. (No Docker guard needed since it's fake-backed; if any part needs a real daemon, guard + skip like the existing e2e.)
- [ ] **Step 3: verify** — `go test ./... -race` PASS; `CGO_ENABLED=0 go build ./...` + all four binaries (`lwd`, `lwd-web`, `lwd-mcp`, `lwd-agent`); `go vet ./...`; `gofmt -l .` empty. Drive the web Health panel if tooling allows (screenshot); don't leave a server running.
- [ ] **Step 4: docs** — README: a "Self-healing & health" section (the control loop; `LWD_RECONCILE_INTERVAL` default 15s; `LWD_HEAL_MAX_ATTEMPTS` default 5; what self-heals = surfaces only; node/edge health is observed; `lwd health`; the web Health panel; controller-off-request-path guarantee). `docs/VISION.md`: mark **P10 done**, P11 (scheduler + capacity + pools; cross-node surface reschedule) next. Full README pass so P1–P10 read coherently.
- [ ] **Step 5: commit** `feat: web health panel; self-heal e2e; docs P10`.

---

## Self-Review

**Spec coverage:** continuous loop (T5); `Reconcile` pass (T4); recreate-via-blue-green heal reusing existing path + rollbackGit split (T3); surfaces-only + compose-skipped (T4 reconcileApp); node/edge observe-only snapshot (T1 types, T4 probes, T2 router.Healthy); backoff + give-up cap + config knobs (T1 config, T4 tryHeal); interval + post-Apply nudge (T5); `GET /health`→client→CLI (T6); web panel + docs + e2e (T7). ✓
**Decisions honored:** heal=recreate/no-rebuild (T3 reuses rollbackGitLocked/applyImage, git skips clone+build — asserted in T3 test); surfaces only (compose skipped, T4); observe-only node/edge (no reschedule anywhere); trigger interval+nudge (T5). ✓
**Non-regression:** `New` signature unchanged (setters used) → all 14 call sites compile untouched; `rollbackGit` split preserves behavior (existing rollback test in T3); `deployBlueGreenSurface` only GAINS a `resetHeal` call on success; Router gains one method (all fakes updated in T2); `Apply`/`Rollback` only gain a non-blocking nudge on success. ✓
**Concurrency:** heal holds `r.mu` (tryHeal); probes don't; `Health` guarded by `healthMu`; nudge non-blocking; loop panic-recovering + ctx-cancellable; a `-race` reconcile-vs-apply test (T4) and `-race` suite (T7). ✓
**Type consistency:** `Health`/`NodeHealth`/`EdgeHealth`/`AppHealth`/`SurfaceState` defined in T1, used identically in T4/T6/T7; `Reachability` (T1) satisfied by `*node.RegistryResolver` (has `Reachable(ctx,name)(string,bool)` — matches P9b); `Reconcilable` (T5) satisfied by `*Reconciler.Reconcile`; `router.Healthy(ctx) bool` (T2) used by `probeEdge` (T4); `healSurfaceLocked`/`rollbackGitLocked`/`surfaceIsDead`/`resetHeal` (T3) used by `tryHeal`/`reconcileApp` (T4); `SetReachability`/`SetNudge`/`HealthSnapshot`/`signalNudge` (T1) used by daemon (T5) + api (T6). ✓
**Placeholder scan:** each code step carries concrete code; the two spots that say "check for an import cycle" (client↔reconciler in T6) and "grep for fake routers" (T2) are explicit verification actions with a stated fallback, not deferred design. ✓
**Cross-task note:** T2's `Router.Healthy` change forces every router fake to update — T4/T6/T7 tests depend on it. T1's setter approach is what keeps the 14 `reconciler.New` call sites and the `api.New` call sites from churning. The client↔reconciler type reuse (T6) is the one place to verify no import cycle before choosing reuse-vs-mirror.
