# lwd Phase 2 Implementation Plan — router, TLS, blue-green, rollback

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every app gets an automatic-HTTPS URL at its domain, deploys are zero-downtime blue-green, and `lwd rollback` restores the prior version.

**Architecture:** Drop host-port publishing. Surfaces join a private `lwd` Docker network and are fronted by a Caddy container that lwd manages (binds :80/:443, automatic TLS). Deploy = run new container → health-check through Caddy → flip Caddy's upstream → retire old. A new `router` package owns the Caddyfile + Caddy container; the reconciler orchestrates node + router + store. Rollback re-runs the deploy path against a stored per-deployment spec snapshot.

**Tech Stack:** Go 1.25+, Docker SDK (`github.com/docker/docker/client`), Caddy (run as the official `caddy` image, configured via generated Caddyfile + admin API on `127.0.0.1:2019`), `modernc.org/sqlite`, `github.com/BurntSushi/toml`.

## Global Constraints

- Go floor **1.25** (already raised by a transitive dep in Phase 1). **No cgo** — everything builds under `CGO_ENABLED=0`.
- Module path `lwd`; import paths `lwd/internal/<pkg>`.
- Design spec: `docs/superpowers/specs/2026-07-03-lwd-phase2-router-bluegreen-design.md`. It governs; if a task conflicts with it, stop and surface it.
- **Surfaces publish NO host ports.** Only the Caddy container publishes host ports (80, 443). Surfaces are reachable by container name on the `lwd` network. This supersedes Phase 1's `port==hostport` model.
- Network name: **`lwd`**. Caddy container name: **`lwd-caddy`**. Caddy image: **`caddy:2`**. Caddy admin endpoint: **`127.0.0.1:2019`** (published from the Caddy container to the host loopback only). Caddyfile path: **`<LWD_DATA_DIR>/Caddyfile`** (default `/var/lib/lwd/Caddyfile`).
- Surface container naming changes to **`lwd-<app>-<deployid>`** (old + new coexist during a swap). App label `lwd.app=<name>` is retained; add label `lwd.role=surface` (vs `lwd.role=system` for Caddy) and `lwd.deploy=<deployid>`.
- Health check is layered and probed through Caddy: `[health] path` → 2xx; else honor Docker `HEALTHCHECK` if present; else liveness (still `running` after a settle window) + listening (a request through Caddy returns non-502/503). Never require a Docker `HEALTHCHECK` to exist.
- TLS: Let's Encrypt for public/resolvable domains; Caddy `internal` certs for local/`.localhost`/non-resolvable domains. Chosen per-domain at Caddyfile generation.
- Tests must use a temp data dir; Docker-dependent tests are guarded by `LWD_DOCKER_TEST` and SKIP otherwise, keeping `go test ./...` green with no Docker.

---

### Task 1: Node — private network, network-attached run (no host ports), container health inspect

**Files:**
- Modify: `internal/node/node.go` (interface + types)
- Modify: `internal/node/local.go` (Docker impl)
- Modify: `internal/node/fake.go` (fake impl)
- Modify: `internal/node/fake_test.go` (tests)

**Interfaces:**
- Consumes: existing `Node`, `RunSpec`, `Container`, `HealthSpec`.
- Produces (add to `Node`):
  - `EnsureNetwork(ctx context.Context, name string) error`
  - `ContainerHealth(ctx context.Context, id string) (state string, dockerHealth string, err error)` — `state` is Docker container state (`running`/`exited`/…); `dockerHealth` is `""` if the image declares no HEALTHCHECK, else `starting`/`healthy`/`unhealthy`.
  - `RunSpec` gains: `Network string` (network to attach; "" = default), `Publish []PortMapping` (host↔container ports; nil = publish nothing), and keep `Port int` (the app's primary container port, exposed on the network but NOT auto-published anymore).
  - `type PortMapping struct { HostPort, ContainerPort int }`
  - `Container` gains: `IP string` (address on the primary network, when known).
- **Behavior change:** `RunContainer` no longer auto-publishes `Port` to the host. It exposes `Port`, attaches to `Network` (if set), and publishes only entries in `Publish` (bound to `127.0.0.1` unless host port is 80/443). The fake records `Network`/`Publish` on the stored `Container` for assertions.

- [ ] **Step 1: Write the failing fake tests**

Add to `internal/node/fake_test.go`:
```go
func TestFakeEnsureNetwork(t *testing.T) {
	f := NewFake()
	if err := f.EnsureNetwork(context.Background(), "lwd"); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if !contains(f.Calls, "EnsureNetwork:lwd") {
		t.Errorf("calls = %v", f.Calls)
	}
}

func TestFakeRunRecordsNetworkAndNoPublish(t *testing.T) {
	f := NewFake()
	c, err := f.RunContainer(context.Background(), RunSpec{
		Name: "lwd-blog-1", Image: "img:1", Network: "lwd", Port: 8080,
		Labels: map[string]string{"lwd.app": "blog"},
	})
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}
	if c.Name != "lwd-blog-1" {
		t.Errorf("name = %q", c.Name)
	}
}

func TestFakeContainerHealth(t *testing.T) {
	f := NewFake()
	c, _ := f.RunContainer(context.Background(), RunSpec{Name: "x", Image: "i", Labels: map[string]string{"lwd.app": "x"}})
	f.HealthState = "running"
	f.DockerHealth = "healthy"
	state, dh, err := f.ContainerHealth(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("ContainerHealth: %v", err)
	}
	if state != "running" || dh != "healthy" {
		t.Errorf("state=%q dockerHealth=%q", state, dh)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
```
(If a `contains` helper already exists in the package's test files, do not redeclare it — reuse it.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/node/ -v`
Expected: FAIL — `EnsureNetwork`/`ContainerHealth` undefined; `RunSpec` has no `Network`.

- [ ] **Step 3: Extend the interface and types**

In `internal/node/node.go`: add `PortMapping`, the new `RunSpec` fields (`Network`, `Publish`), `Container.IP`, and the two new interface methods. Update the doc comment on `RunContainer` to state it no longer auto-publishes `Port`.

- [ ] **Step 4: Implement in the fake**

In `internal/node/fake.go`: add fields `HealthState string`, `DockerHealth string`; implement `EnsureNetwork` (record call, return nil) and `ContainerHealth` (return `f.HealthState`, `f.DockerHealth`, nil; default `HealthState` to `"running"` for created containers). Keep recording `Network`/`Publish` if useful. Ensure `var _ Node = (*Fake)(nil)` still holds.

- [ ] **Step 5: Implement in local.go (Docker) — adapt to the real SDK**

Update `internal/node/local.go`:
- `EnsureNetwork`: `NetworkInspect`; if not found, `NetworkCreate` (bridge driver). Idempotent.
- `RunContainer`: build `container.Config` (expose `Port/tcp`), `HostConfig.PortBindings` only from `spec.Publish` (bind host ports 80/443 to `0.0.0.0`, everything else to `127.0.0.1`), and attach to `spec.Network` via `NetworkingConfig.EndpointsConfig[spec.Network]`. Populate `Container.IP` from the network settings after start (best-effort).
- `ContainerHealth`: `ContainerInspect`; return `State.Status`, and `State.Health.Status` if `State.Health != nil` else `""`.

**SDK-drift note (as in Phase 1 Task 6):** exact type/field names (`network.CreateOptions`, `network.Inspect`, `container.NetworkingConfig`, `network.EndpointSettings`, `nat.PortMap`) may differ from any snippet here across SDK versions. Produce a WORKING implementation against the resolved SDK; keep imports clean; converge with `go build`/`go vet`. Do not keep dead import-silencing hacks.

- [ ] **Step 6: Run node unit tests**

Run: `go test ./internal/node/ -v`
Expected: PASS (Docker integration test still SKIPs).

- [ ] **Step 7: Keep the build green across callers**

The reconciler (Phase 1) calls `RunContainer` with `Port: app.Port` expecting host publish. It still compiles (fields unchanged in name), but behavior differs. Do NOT fix the reconciler here — Task 4 reworks it. Just confirm `CGO_ENABLED=0 go build ./...` and `go test ./...` pass (the Phase 1 host-port e2e is `LWD_DOCKER_TEST`-gated and is replaced in Task 7; note this in your report).

- [ ] **Step 8: Commit**

```bash
git add internal/node/
git commit -m "feat: node private network, network-attached run, container health inspect"
```

---

### Task 2: Router package — Caddyfile generation + Caddy container + admin reload

**Files:**
- Create: `internal/router/caddyfile.go` (pure generation)
- Create: `internal/router/caddyfile_test.go`
- Create: `internal/router/router.go` (Router interface + CaddyRouter impl)
- Create: `internal/router/fake.go` (FakeRouter for reconciler tests)
- Create: `internal/router/router_test.go`

**Interfaces:**
- Consumes: `node.Node` (to run/inspect the `lwd-caddy` container), `node.RunSpec`, `config`.
- Produces:
  - `type Route struct { Domain, Upstream string; Port int; TLSInternal bool }` — one active route (Upstream is a container name on the `lwd` network).
  - `func GenerateCaddyfile(adminAddr string, routes []Route) string` — pure; deterministic ordering (sort by Domain).
  - `type Router interface {`
      `EnsureUp(ctx) error;`
      `SetRoute(ctx, r Route) error;` `RemoveRoute(ctx, domain string) error;`
      `SetStaging(ctx, host, upstream string, port int) error;` `RemoveStaging(ctx, host string) error;`
      `ProbeThroughCaddy(ctx, host, path string) (status int, err error);`
      `Reload(ctx) error }`
  - `func NewCaddyRouter(n node.Node, dataDir string) *CaddyRouter` implementing `Router`.
  - `type FakeRouter struct { ... }` with `NewFakeRouter() *FakeRouter` implementing `Router`, knobs `ProbeStatus int`, `ProbeErr error`, `EnsureErr error`, and inspectable `Routes map[string]Route`, `Staging map[string]bool`, `Calls []string`.

**Design:** `CaddyRouter` keeps the active route set in memory (rebuilt from a generated Caddyfile on disk). `EnsureUp` ensures the `lwd` network and the `lwd-caddy` container (image `caddy:2`, publish 80→80, 443→443, admin 2019→127.0.0.1:2019, attach to `lwd`, label `lwd.role=system`) are running. Config changes regenerate the Caddyfile and reload Caddy via `POST http://127.0.0.1:2019/load` (Content-Type `text/caddyfile`) — validate before committing; on reload error, restore the previous Caddyfile in memory. `ProbeThroughCaddy` does `GET http://127.0.0.1:80<path>` with `Host: <host>`, returning the status code (used by the reconciler's layered health logic).

- [ ] **Step 1: Write the failing Caddyfile-generation test**

Create `internal/router/caddyfile_test.go`:
```go
package router

import (
	"strings"
	"testing"
)

func TestGenerateCaddyfileSortedWithTLS(t *testing.T) {
	out := GenerateCaddyfile("127.0.0.1:2019", []Route{
		{Domain: "b.example.com", Upstream: "lwd-b-2", Port: 8080},
		{Domain: "a.localhost", Upstream: "lwd-a-1", Port: 3000, TLSInternal: true},
	})
	if !strings.Contains(out, "admin 127.0.0.1:2019") {
		t.Error("missing admin directive")
	}
	// deterministic order: a.localhost block appears before b.example.com
	ai := strings.Index(out, "a.localhost")
	bi := strings.Index(out, "b.example.com")
	if ai == -1 || bi == -1 || ai > bi {
		t.Fatalf("blocks not sorted: a=%d b=%d\n%s", ai, bi, out)
	}
	if !strings.Contains(out, "reverse_proxy lwd-b-2:8080") {
		t.Error("missing reverse_proxy for b")
	}
	if !strings.Contains(out, "tls internal") {
		t.Error("a.localhost should use internal TLS")
	}
}

func TestGenerateCaddyfileEmpty(t *testing.T) {
	out := GenerateCaddyfile("127.0.0.1:2019", nil)
	if !strings.Contains(out, "admin 127.0.0.1:2019") {
		t.Error("empty config must still set admin")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/router/ -run TestGenerateCaddyfile -v`
Expected: FAIL — undefined `GenerateCaddyfile`, `Route`.

- [ ] **Step 3: Implement pure Caddyfile generation**

Create `internal/router/caddyfile.go`:
```go
// Package router owns lwd's reverse proxy: it generates the Caddyfile and manages
// the Caddy container that fronts all apps (TLS + domain routing). It holds no app
// logic beyond translating routes into Caddy config.
package router

import (
	"fmt"
	"sort"
	"strings"
)

// Route is one active domain -> container mapping.
type Route struct {
	Domain      string
	Upstream    string // container name on the lwd network
	Port        int
	TLSInternal bool // use Caddy self-signed certs (local/non-public domains)
}

// GenerateCaddyfile renders a deterministic Caddyfile for the given routes.
func GenerateCaddyfile(adminAddr string, routes []Route) string {
	var b strings.Builder
	fmt.Fprintf(&b, "{\n\tadmin %s\n}\n\n", adminAddr)

	sorted := append([]Route(nil), routes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Domain < sorted[j].Domain })

	for _, r := range sorted {
		fmt.Fprintf(&b, "%s {\n", r.Domain)
		if r.TLSInternal {
			b.WriteString("\ttls internal\n")
		}
		fmt.Fprintf(&b, "\treverse_proxy %s:%d\n", r.Upstream, r.Port)
		b.WriteString("}\n\n")
	}
	return b.String()
}

// UseInternalTLS reports whether a domain should use Caddy's self-signed certs
// rather than public ACME (local dev, .localhost, or bare hostnames).
func UseInternalTLS(domain string) bool {
	if domain == "" {
		return true
	}
	if strings.HasSuffix(domain, ".localhost") || domain == "localhost" {
		return true
	}
	// No dot => not a public FQDN (e.g. "myapp"); treat as internal.
	return !strings.Contains(domain, ".")
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/router/ -run TestGenerateCaddyfile -v`
Expected: PASS.

- [ ] **Step 5: Write the FakeRouter + its test**

Create `internal/router/fake.go` (implements `Router` in-memory; records calls; `ProbeThroughCaddy` returns `ProbeStatus`/`ProbeErr`; `SetRoute`/`RemoveRoute` mutate `Routes`; `SetStaging`/`RemoveStaging` mutate `Staging`). Create `internal/router/router_test.go` with a test that sets a route, sets/removes staging, and checks `ProbeThroughCaddy` returns the configured status. Include `var _ Router = (*FakeRouter)(nil)`.

- [ ] **Step 6: Implement CaddyRouter (real) — adapt to SDK/Caddy reality**

Create `internal/router/router.go`: the `Router` interface, `CaddyRouter` struct (holds `node.Node`, `dataDir`, in-memory `routes map[string]Route`, `staging map[string]Route`, a mutex, and `adminAddr`/`caddyBaseURL` constants). Implement:
- `EnsureUp`: `node.EnsureNetwork("lwd")`; if `lwd-caddy` not running, `node.EnsureImage("caddy:2")` + `node.RunContainer(RunSpec{Name:"lwd-caddy", Image:"caddy:2", Network:"lwd", Publish:[{80,80},{443,443},{2019,2019}], Labels:{"lwd.role":"system"}})`. (Admin 2019 must bind 127.0.0.1 — rely on the node's non-80/443 loopback binding rule from Task 1.) Then `Reload` to install the current routes.
- `SetRoute`/`RemoveRoute`/`SetStaging`/`RemoveStaging`: mutate maps, then `Reload`.
- `Reload`: build routes+staging into `[]Route`, `GenerateCaddyfile`, write to `<dataDir>/Caddyfile`, POST it to `http://127.0.0.1:2019/load` with `Content-Type: text/caddyfile`; on non-2xx, keep the prior file/state and return an error.
- `ProbeThroughCaddy(host, path)`: `GET http://127.0.0.1:80<path>` with `Host: host`; return status code.

Verify build + router unit tests: `CGO_ENABLED=0 go build ./... && go test ./internal/router/ -v`.

- [ ] **Step 7: Commit**

```bash
git add internal/router/
git commit -m "feat: router package — Caddyfile generation, Caddy container, admin reload, fake"
```

---

### Task 3: Store — per-deployment spec snapshot + history queries

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

**Interfaces:**
- Produces:
  - `Deployment` gains `Spec string` (JSON snapshot of the resolved `spec.App`) and keeps existing fields.
  - `RecordDeployment` persists `Spec`.
  - `func (s *Store) PreviousDeployment(app string) (*Deployment, error)` — the most recent **retired** deployment with status that was previously running (i.e. the last successful non-current one), or nil.
  - `func (s *Store) DeploymentsForApp(app string) ([]Deployment, error)` — newest first.
- **Migration:** add a `spec TEXT NOT NULL DEFAULT ''` column via `ALTER TABLE` guarded so re-running is safe (check `PRAGMA table_info` or catch "duplicate column").

- [ ] **Step 1: Write failing tests**

Add to `internal/store/store_test.go` tests for: `RecordDeployment` round-trips `Spec`; `PreviousDeployment` returns the prior successful deployment after two records where the first is retired; `DeploymentsForApp` returns newest-first. Use `t.TempDir()`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store/ -v` → FAIL (no `Spec` field / methods).

- [ ] **Step 3: Implement**

Add the `spec` column + safe migration in `Open`; add `Spec` to the struct, INSERT, and all SELECT column lists; implement `PreviousDeployment` (query `status=StatusRetired ORDER BY id DESC LIMIT 1`) and `DeploymentsForApp`.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/store/ -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/
git commit -m "feat: store per-deployment spec snapshot and history queries"
```

---

### Task 4: Reconciler — blue-green swap with layered health

**Files:**
- Modify: `internal/reconciler/reconciler.go`
- Modify: `internal/reconciler/reconciler_test.go`

**Interfaces:**
- Consumes: `node.Node` (incl. `EnsureNetwork`, `ContainerHealth`, network-attached `RunContainer`), `router.Router` (incl. `EnsureUp`, `SetStaging`/`RemoveStaging`, `ProbeThroughCaddy`, `SetRoute`), `store.Store` (incl. `Spec` snapshot), `spec.App`.
- Produces:
  - `func New(n node.Node, r router.Router, s *store.Store) *Reconciler` (router is a new dependency).
  - `Apply(ctx, *spec.App) (*store.Deployment, error)` — now blue-green.
  - Deploy id generation: monotonic, derived from the store (e.g. next deployment id) or a counter; container name `lwd-<app>-<deployid>`.
  - Layered health helper `checkHealth(ctx, app, stagingHost) error`.

**Blue-green flow (idempotent, atomic):**
1. `r.mu.Lock()` (keep the Phase 1 serialize-all mutex).
2. `app.Validate()`.
3. `router.EnsureUp`; `node.EnsureNetwork("lwd")`.
4. `node.EnsureImage(app.Image)`.
5. Determine `deployid` (unique, increasing). Container name `lwd-<app>-<deployid>`.
6. `node.RunContainer` on network `lwd`, no publish, labels `{lwd.app, lwd.role=surface, lwd.deploy=<deployid>}`.
7. `router.SetStaging("stage-<deployid>.lwd.internal", containerName, app.Port)`.
8. **Layered health** (`checkHealth`):
   - if `app.Health.Path != ""`: poll `router.ProbeThroughCaddy(stagingHost, path)` until 2xx or `app.Health.Timeout`.
   - else: inspect `node.ContainerHealth`; if `dockerHealth != ""`, poll until `healthy`/timeout; else liveness fallback — after a short settle, require `state == running` AND `router.ProbeThroughCaddy(stagingHost, "/")` returns a status that is NOT 502/503 (i.e. Caddy reached the app).
9. On health failure: `router.RemoveStaging`, `node.RemoveContainer(new)`, retire any prior running deployment row, record `StatusFailed`, return error (live domain untouched).
10. On success: `router.SetRoute({Domain: app.Domain, Upstream: containerName, Port: app.Port, TLSInternal: router.UseInternalTLS(app.Domain)})`; `router.RemoveStaging`; retire prior running deployment + `node.RemoveContainer(old surface)`; record new `StatusRunning` with `Spec` = JSON(app).
11. Unlock via defer.

- [ ] **Step 1: Write failing tests (fake node + fake router)**

Rewrite/extend `internal/reconciler/reconciler_test.go`. `newTestReconciler` now wires `node.NewFake()`, `router.NewFakeRouter()`, temp store. Tests:
- `TestApplyStagesProbesFlips`: happy path — asserts call ordering (RunContainer → SetStaging → ProbeThroughCaddy → SetRoute → RemoveStaging), a `StatusRunning` deployment recorded with non-empty `Spec`, and the route set for the app's domain.
- `TestApplyHealthFailLeavesDomainUntouched`: `FakeRouter.ProbeStatus = 502` (or `ProbeErr`) with a health path set → Apply errors; new container removed; staging removed; the real domain route was never set; no running deployment remains.
- `TestApplyLivenessFallbackNoPath`: no `health.path`, fake `DockerHealth=""`, `ProbeStatus=404` → treated healthy (app listening); route set.
- `TestApplyBlueGreenRetiresOld`: two applies → old surface container removed on the second, new route points at the new container.
- Keep `TestApplyRejectsInvalidSpec` and the redeploy-failure semantics.

Write these with concrete assertions against `FakeRouter.Routes`/`Staging`/`Calls` and `node.Fake.Calls`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/reconciler/ -v` → FAIL (`New` signature changed; new behavior absent).

- [ ] **Step 3: Implement the blue-green reconciler**

Rework `Apply` per the flow above; add `checkHealth`. Keep the mutex. Derive `deployid` from `store` (e.g. a `NextDeployID()` or reuse the inserted row id — if using the row id, insert a `StatusRunning` row first is circular; instead add a small monotonic counter or a `store.NextDeployID()` helper — pick one and implement it; document the choice). Snapshot the spec with `encoding/json`.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/reconciler/ -v` and `go test ./...` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/reconciler/ internal/store/
git commit -m "feat: blue-green reconciler with layered health through Caddy"
```

---

### Task 5: Rollback — reconciler.Rollback + API + CLI

**Files:**
- Modify: `internal/reconciler/reconciler.go`
- Modify: `internal/reconciler/reconciler_test.go`
- Modify: `internal/api/api.go`, `internal/api/api_test.go`
- Modify: `internal/client/client.go`, `internal/cli/cli.go`

**Interfaces:**
- Produces:
  - `func (r *Reconciler) Rollback(ctx, app string) (*store.Deployment, error)` — loads `store.PreviousDeployment(app)`, unmarshals its `Spec` snapshot into a `spec.App`, sets `Image` to the snapshot's recorded image digest, and runs `Apply` against it. Errors clearly if there is no previous deployment.
  - API: `POST /apps/{name}/rollback` → 200 `store.Deployment` or 404/500.
  - Client: `Rollback(ctx, name) (*store.Deployment, error)`.
  - CLI: `lwd rollback <app>`.

- [ ] **Step 1: Write failing reconciler + api tests**

Reconciler: `TestRollbackRedeploysPrevious` — apply v1 (image a), apply v2 (image b), rollback → a new running deployment whose image is `a` and route points at the rolled-back container. `TestRollbackNoHistory` — rollback with no prior deployment errors. API: `TestRollbackEndpoint` (fake stack) — 200 with a deployment; 404 when unknown app.

- [ ] **Step 2: Run to verify failure** → FAIL.

- [ ] **Step 3: Implement** `Rollback` in the reconciler, the API route, client method, and CLI command (mirror the `apply` wiring).

- [ ] **Step 4: Run to verify pass** — `go test ./...` PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/reconciler/ internal/api/ internal/client/ internal/cli/
git commit -m "feat: rollback to previous deployment via stored spec snapshot"
```

---

### Task 6: Daemon wiring + history + ls columns

**Files:**
- Modify: `internal/cli/cli.go` (construct `router.NewCaddyRouter`, pass to `reconciler.New`; `EnsureUp` at daemon start)
- Modify: `internal/api/api.go`, `internal/api/api_test.go` (`GET /apps/{name}/history`)
- Modify: `internal/client/client.go` (`History`)
- Modify: `internal/cli/cli.go` (`lwd history <app>`; `lwd ls` shows domain + status)
- Modify: `internal/config/config.go` (`CaddyfilePath()` helper)

**Interfaces:**
- `GET /apps/{name}/history` → `[]store.Deployment` (newest first).
- `lwd ls` output gains a DOMAIN column (from the current deployment's spec snapshot).
- Daemon: build `CaddyRouter(node, config.DataDir())`, `reconciler.New(node, router, store)`, and call `router.EnsureUp(ctx)` on startup (log if Caddy can't be ensured, but a failing EnsureUp at boot should abort with a clear error).

- [ ] **Step 1: Write failing api history test** — `TestHistoryEndpoint` (fake stack): after an apply, `GET /apps/blog/history` returns ≥1 deployment newest-first.

- [ ] **Step 2: Run to verify failure** → FAIL.

- [ ] **Step 3: Implement** the history endpoint + client `History` + CLI `history`/`ls` columns + daemon wiring + `config.CaddyfilePath()`.

- [ ] **Step 4: Verify** `CGO_ENABLED=0 go build -o /tmp/lwd ./cmd/lwd && go test ./... && /tmp/lwd version`.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/ internal/api/ internal/client/ internal/config/
git commit -m "feat: wire router into daemon; add history endpoint, CLI history, ls domain column"
```

---

### Task 7: End-to-end — real Caddy + blue-green + rollback; README update

**Files:**
- Replace: `test/e2e_test.go` (drop the Phase 1 host-port assumption)
- Modify: `README.md`

**Interfaces:** consumes the full stack.

**e2e (guarded by `LWD_DOCKER_TEST`):** build the real stack (real `node.NewLocal`, `router.NewCaddyRouter`, temp store), `EnsureUp` Caddy, deploy `traefik/whoami:latest` at a `.localhost` domain (internal TLS) with a health path, then:
1. Assert the app is reachable through Caddy (`GET http://127.0.0.1:80/` with `Host: <domain>` → 200).
2. Deploy a second time (new image tag or same) and assert the endpoint stays 200 across the swap (poll during) and the old surface container is gone.
3. `Rollback` and assert the endpoint still serves and the rolled-back deployment is recorded.
4. Clean up: remove app containers, the `lwd-caddy` container, and the `lwd` network. Assert no stray `lwd.role=surface`/`lwd-caddy` containers remain.

**Note:** `traefik/whoami` listens on :80; set the app `port = 80` (Caddy reaches it on the network at `<container>:80`) — no `WHOAMI_PORT_NUMBER` hack needed now, since we route by container port on the network rather than publishing to a host port.

- [ ] **Step 1: Write the e2e test** per the above.

- [ ] **Step 2: Run unit suite** — `go test ./...` (e2e SKIPs) → PASS.

- [ ] **Step 3: Run the real e2e** — `LWD_DOCKER_TEST=1 go test ./test/ -v`. Must PASS against real Docker; confirm no stray containers/network afterward. If it fails, investigate and report exactly why (do not paper over).

- [ ] **Step 4: Update README** — replace the Phase-1 recreate/host-port sections with: HTTPS via Caddy, zero-downtime blue-green, `lwd rollback`/`lwd history`, the `lwd` network + Caddy container model, and remove the now-fixed host-port-collision and stale-recreate limitations. Note `domain` is now live; `secrets`/compose/surfaces still deferred.

- [ ] **Step 5: Commit**

```bash
git add test/ README.md
git commit -m "test: end-to-end blue-green + rollback through real Caddy; docs: Phase 2 README"
```

---

## Self-Review

**Spec coverage:**
- Private `lwd` network + no host ports on surfaces → Task 1. ✓
- Managed Caddy container + Caddyfile + admin reload → Task 2. ✓
- Per-domain TLS (ACME vs internal) → Task 2 (`UseInternalTLS`) + Task 4 (route). ✓
- Blue-green swap (stage → probe → flip → retire) → Task 4. ✓
- Layered health (path 2xx → Docker HEALTHCHECK → liveness+listening), probed through Caddy → Task 4 (`checkHealth`) using Task 1 `ContainerHealth` + Task 2 `ProbeThroughCaddy`. ✓
- Spec-snapshot rollback + history → Tasks 3, 5, 6. ✓
- Daemon wiring + CLI (rollback, history, ls domain) → Tasks 5, 6. ✓
- Real e2e (zero-downtime + rollback) + README → Task 7. ✓

**Deferred (by design):** secrets (Phase 3), compose + surfaces/pinned (Phase 4), web UI (Phase 5), multi-node.

**Placeholder scan:** Infra tasks (1, 2, 6, 7) intentionally give behavior contracts + adapt-notes rather than verbatim final Docker/Caddy code, matching the Phase 1 Task 6 pattern that succeeded; pure/logic units (Caddyfile gen, store, reconciler-against-fakes) have complete code and tests. No TBD/"handle edge cases" left as the sole instruction.

**Type consistency:** `Router` interface method set identical across `router.go`, `fake.go`, and reconciler use. `Node` additions (`EnsureNetwork`, `ContainerHealth`, `RunSpec.Network/Publish`, `PortMapping`, `Container.IP`) consistent across `node.go`/`local.go`/`fake.go`/reconciler. `store.Deployment.Spec` consistent across store/reconciler/api/client. Container naming `lwd-<app>-<deployid>` and labels (`lwd.app`, `lwd.role`, `lwd.deploy`) consistent across node/reconciler/router/e2e.

**Known cross-task sequencing:** Task 1 changes `RunContainer` semantics; the reconciler is only reworked in Task 4, so the Phase 1 `LWD_DOCKER_TEST` e2e is temporarily inconsistent between Tasks 1–6 and is replaced in Task 7. Non-Docker `go test ./...` stays green throughout.
