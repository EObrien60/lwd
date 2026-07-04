# lwd Phase 6 Implementation Plan — git deploy, build-from-source, backing services, UI authoring

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax. The UI task (Task 7) MUST use the frontend-design skill.

**Goal:** Deploy from a git repo (box's `git` clone + `docker build` → blue-green surface), let apps declare pinned backing services (Postgres/MinIO/…) via generated compose, tag-by-sha rollback, and add web-UI From-Git + Builder authoring.

**Architecture:** New `internal/source` (git clone, shell out) + `internal/build` (docker build, shell out), both behind interfaces with fakes. Backing services render (pure fn) to a compose YAML run pinned via the existing `internal/compose`. Reconciler gains `applyGit`; backing handling applies to image + git apps. The surface is built locally (`lwd-build/<app>:<sha>`), deployed blue-green, and connected to both the `lwd` network (Caddy) and a per-app backing network. Zero registry (EnsureImage local-fallback).

**Tech Stack:** Go 1.25+; box `git` + `docker build` (via os/exec); reuse `internal/{compose,node,router,store,secrets,reconciler,client,web}`.

## Global Constraints

- Go 1.25+; no cgo. Module `lwd`. Design spec governs: `docs/superpowers/specs/2026-07-04-lwd-phase6-git-deploy-design.md`.
- **Suckless:** box `git` (clone/fetch) + `docker build` only — NO GitHub/GitLab API/OAuth/webhooks. lwd manages no git credentials (box git auth applies).
- **Model A:** `lwd.toml` is the source of truth; `[git]` supplies the build context.
- **Backing services are PINNED** — never blue-greened, never torn down on surface redeploy/rollback (data safety); named volumes persist.
- Built images tagged `lwd-build/<app>:<shortsha>`, kept; rollback redeploys the prior tag (no rebuild).
- Requirements on host for git apps: `git` + `docker build` (+ compose plugin for backing) — error on first git deploy, never at daemon start.
- Fail-closed secrets before any clone/build/compose (as in Phases 3–4).
- **README is a required deliverable** (Task 8), verified in the final review.
- Tests use fakes + temp dirs; git/docker/compose-dependent tests guarded by `LWD_DOCKER_TEST`.

---

### Task 1: spec — `[git]`, `[build]`, `[[service]]` backing validation

**Files:** `internal/spec/spec.go`, `internal/spec/spec_test.go`.

**Interfaces:** Add to `App`: `Git *Git` (`toml:"git"`) where `Git{ URL, Ref, Path string }`; `Services []Service` (`toml:"service"`) where `Service{ Name, Image, Command string; Env map[string]string; Secrets []string; Volume string }`. `Build` already exists (`Context, Dockerfile`) — reuse. `Validate`:
- Git app: `Git != nil` requires `Git.URL != ""` and `Build != nil`; must NOT set `Image` or `Compose`. Requires `Domain` + `Port` (it's a web surface). Default `Git.Ref` to `"main"` if empty in `Parse`.
- `Services`: each requires `Name` (DNS-safe: `^[a-z0-9][a-z0-9-]*$`) + `Image`; names unique. Allowed on image apps and git apps; **rejected on compose apps** (`Compose != ""` + services → error).
- Existing image/compose/single-service rules unchanged.

- [ ] **Step 1: failing tests** — git app valid (git+build+domain+port); git without build → error; git+image → error; service without name/image → error; dup service names → error; bad service name → error; compose+service → error; service on image app → valid; existing tests still pass; parse round-trips `[git]`/`[[service]]` from TOML (+ Ref default).
- [ ] **Step 2:** `go test ./internal/spec/ -v` → FAIL.
- [ ] **Step 3:** implement structs + validation + Ref default.
- [ ] **Step 4:** PASS; build/vet/gofmt clean.
- [ ] **Step 5:** commit `feat: spec support for git source and backing services`.

---

### Task 2: source package — git clone (box git)

**Files:** Create `internal/source/git.go`, `internal/source/fake.go`, `internal/source/git_test.go`.

**Interfaces:** `type Git interface { Clone(ctx, url, ref, dir string) (sha string, err error) }`. `CLI` (real): shallow clone via `git`: `git clone --depth 1 --branch <ref> <url> <dir>` then `git -C <dir> rev-parse HEAD` for the sha; if the branch-clone fails (ref is a raw sha), fall back to `git clone <url> <dir>` + `git -C <dir> checkout <ref>`. Non-zero exit → error incl. stderr. `Fake`: knobs `SHA`, `Err`, records `Calls`/`LastClone`. `var _ Git` assertions.

- [ ] Steps: failing fake test (Clone records url/ref/dir, returns SHA/Err) → implement CLI + fake → `go test ./internal/source/ -v` PASS → build/vet/gofmt clean → commit `feat: source package (git clone via box git)`.
- [ ] Optional (git available): an `LWD_DOCKER_TEST`-guarded real clone of a tiny public repo into a temp dir, assert a sha comes back; clean up. Not required.

---

### Task 3: build package — docker build

**Files:** Create `internal/build/build.go`, `internal/build/fake.go`, `internal/build/build_test.go`.

**Interfaces:** `type Builder interface { Build(ctx, contextDir, dockerfile, tag string) error; ImageExists(ctx, tag string) (bool, error) }`. `CLI` (real): `docker build -t <tag> -f <contextDir>/<dockerfile> <contextDir>` (exec, stderr surfaced); `ImageExists` via `docker image inspect <tag>` exit code. `Fake`: knobs `BuildErr`, `Exists` (map/bool), records `Calls`/`LastTag`. `var _ Builder` assertion.

- [ ] Steps: failing fake test → implement CLI + fake → PASS → build/vet/gofmt clean → commit `feat: build package (docker build wrapper)`.

---

### Task 4: backing-services compose rendering (pure)

**Files:** Create `internal/reconciler/backing.go` (or `internal/compose/backing.go`), `_test.go`.

**Interfaces:** `func RenderBackingCompose(appName string, services []spec.Service, env map[string]string, secretVals map[string]string) (yaml string, network string)` — pure. Emits a compose doc: one service per `spec.Service` (image, optional `command`, merged env from service.Env + resolved service.Secrets, `volumes: [name:path]` from `Volume`, named top-level volumes), all on a network `lwd-<app>` (also emitted as a top-level network). Deterministic ordering. No ports published (backing is internal). Returns the yaml + the per-app network name.

- [ ] Steps: failing test — render 2 services (db with volume+env, minio with command+secret) → assert the yaml contains both services, the named volumes, the `lwd-<app>` network, injected env, and NO published ports; deterministic (sorted). Empty services → empty/nil yaml. → implement → PASS → build/vet/gofmt clean → commit `feat: render backing services to a compose project`.

---

### Task 5: reconciler — applyGit + backing + rollback + remove

**Files:** `internal/reconciler/reconciler.go`, `internal/reconciler/reconciler_test.go`.

**Interfaces:** `New` gains `source.Git` + `build.Builder` params: `New(n node.Node, r router.Router, s *store.Store, sec SecretResolver, comp compose.Composer, src source.Git, bld build.Builder)`. `Apply` branches: `app.Git != nil` → `applyGit`; else existing image/compose. **Backing services** (`len(app.Services) > 0`) are ensured for image + git apps (a shared helper `ensureBacking(ctx, app, secretVals) (network string, err error)` that renders → writes a temp compose file → `compose.Up(project="lwd-"+app.Name, file, env)` → returns the per-app network name).

**applyGit flow:** validate → EnsureUp + EnsureNetwork("lwd") → resolve secrets (fail-closed) → `ensureBacking` (if services) → `git.Clone(url, ref, tempdir)` → `sha` → `tag := "lwd-build/"+app.Name+":"+short(sha)` → if `!builder.ImageExists(tag)` then `builder.Build(context, dockerfile, tag)` → set the surface image to `tag` → run surface (blue-green via existing path) on network "lwd", then `node.ConnectContainerToNetwork(surfaceID, backingNetwork)` if backing → route → health → record (Image=tag, Compose=rendered backing yaml, Spec=snapshot). Remove temp clone.

**ensureBacking for image apps:** the existing single-service `Apply` path, when `len(app.Services)>0`, calls `ensureBacking` before starting the surface and connects the surface to the backing network.

**Rollback:** git/built app → redeploy the previous deployment's Image tag (already local; no clone/rebuild) via the surface path (+ re-ensure backing). Existing image/compose rollback unchanged.

**Remove:** if the app had backing services (deployment.Compose non-empty from a rendered backing project) → `compose.Down("lwd-"+app, tempfile)` for the backing project too. (Volumes remain — data not auto-destroyed.)

- [ ] **Step 1: failing tests** (fakes: source, build, compose, node, router, resolver; temp store). Update `newTestReconciler` for the 2 new deps.
  - `TestApplyGitClonesBuildsDeploys`: git app, fake clone returns sha, fake build records tag, ProbeStatus 200 → order Clone → Build → RunContainer(surface) → SetRoute → health; deployment Image == `lwd-build/<app>:<short sha>`.
  - `TestApplyGitSkipsBuildIfImageExists`: `builder.Exists[tag]=true` → no Build call, still deploys.
  - `TestApplyGitWithBacking`: services declared → `ensureBacking` renders + `compose.Up` called (project lwd-<app>) BEFORE the surface; surface connected to the backing network (assert ConnectContainerToNetwork:<id>:lwd-<app>); backing env includes resolved secrets.
  - `TestApplyGitFailClosedSecret`: resolver err → no clone/build/compose; StatusFailed.
  - `TestApplyImageAppWithBacking`: a plain image app + services → backing ensured + surface connected.
  - `TestRollbackGitRedeploysPriorTag`: apply sha1 (tag1), apply sha2 (tag2), rollback → redeploys tag1, NO new Build/Clone call.
  - `TestRemoveGitDownsBacking`: app with backing → Remove calls compose.Down for the backing project + RemoveRoute + retire.
  - Keep all existing reconciler tests green.
- [ ] **Step 2:** FAIL → **Step 3:** implement → **Step 4:** `go test ./...` PASS; build/vet/gofmt clean. Note ripple: `New` gains 2 args — update cli daemon (real source.NewCLI/build.NewCLI) + api_test/client_test/e2e/web (fakes). Report what you touched outside internal/reconciler.
- [ ] **Step 5:** commit `feat: git deploy + backing services + tag-by-sha rollback in reconciler`.

---

### Task 6: daemon/API wiring + store built-tag/sha

**Files:** `internal/cli/cli.go`, `internal/store/store.go` (+ test) if a git-sha column is wanted, `internal/api/*` only if needed.

- Daemon `runDaemon`: construct `source.NewCLI()` + `build.NewCLI()`, pass to `reconciler.New(...)`.
- Store: the built tag lives in `Deployment.Image` and the rendered backing compose in `Deployment.Compose` (both existing columns) — confirm rollback/remove read them. Add a `git_sha` column ONLY if needed for display; otherwise skip (the tag encodes the short sha). Prefer: no new column; `logs` uses the surface container id (ensure applyGit records the surface container id as ContainerID).
- API/CLI: `apply`/`rollback`/`rm`/`logs`/`history` already cover git apps (they're just apps). Verify `ls` domain + `logs` work for a git app (ContainerID = surface).

- [ ] Steps: failing test if a store/api change is made (else a small cli/daemon smoke); implement wiring; `CGO_ENABLED=0 go build -o /tmp/lwd ./cmd/lwd && go test ./... && go vet ./... && gofmt -l . && /tmp/lwd version` clean → commit `feat: wire git source + builder into the daemon`.

---

### Task 7: web UI — From-Git + Builder tabs (+ backing rows) — REQUIRED SKILL

**Files:** `internal/web/assets/{index.html,app.js,app.css}` (+ maybe a small `internal/web/api.go` helper if the UI needs an lwd.toml-generation endpoint — prefer generating the toml client-side in app.js and POSTing to the existing `/api/apply`).

**REQUIRED:** use the **frontend-design skill**. Extend the existing Deploy modal (currently paste-only) into tabs, matching the established design system:
- **From Git**: fields url, ref (default main), subdir, Dockerfile (default Dockerfile), name, domain, port, env (key/val rows), secrets (names), and a **Backing services** section (rows: name, image, command, volume, env) → builds an `lwd.toml` (with `[git]`, `[build]`, `[[service]]`) client-side → `POST /api/apply`. Inline 400 error.
- **Builder**: same but for an image app (image instead of git) + backing rows.
- **Paste**: existing raw-toml textarea.
- The generated toml must parse+validate server-side (the /api/apply path already validates). Show the generated toml (a "preview" pane) so it's transparent.

- [ ] **Step 1:** invoke frontend-design; build the tabs + backing-service row UI wired to `/api/apply`.
- [ ] **Step 2:** `go build`/`go test ./internal/web/...` pass (update any asset-marker test); drive the running server (browser tooling) — open the Deploy modal, fill the From-Git form incl. a backing service, confirm the generated toml preview is correct and apply posts it; screenshot if possible. Don't leave a server running.
- [ ] **Step 3:** commit `feat: web UI From-Git + Builder deploy tabs with backing services`.

---

### Task 8: e2e + README

**Files:** `test/e2e_test.go`, `README.md`.

- [ ] **Step 1: e2e** (guarded by `LWD_DOCKER_TEST`; also SKIP if `git`/`docker build`/compose absent). Create a tiny throwaway git repo in a temp dir: `git init`, a minimal `Dockerfile` (e.g. `FROM traefik/whoami` — no build steps needed, or a trivial static server) + commit, so `source.CLI.Clone` of `file://<tempdir>` works. Build a `spec.App{Git:{URL:file://..., Ref: <branch>}, Build:{Dockerfile:"Dockerfile"}, Domain:"git-whoami.localhost", Port:80, Services:[{Name:"cache", Image:"redis:7-alpine"}]}`. Deploy via the real reconciler (real source/build/compose/node/router). Assert: app reachable through Caddy (200); the `cache` (redis) backing container is running on the per-app network; redeploy → redis container id UNCHANGED (pinned) and a new/again surface; rollback → prior built image redeployed; rm → surface + backing down. Reuse cleanup helpers; SKIP cleanly if tools/ports unavailable.
- [ ] **Step 2:** `go test ./...` (no env) → PASS, e2e SKIPs.
- [ ] **Step 3:** `LWD_DOCKER_TEST=1 go test ./test/ -run TestEndToEndGit -v` → MUST pass against real git+docker+compose; confirm no strays.
- [ ] **Step 4: README** — add "## Deploy from Git" (the `[git]`/`[build]` fields, box-git/no-API stance, private-repo-via-box-auth, build-and-deploy flow, tag-by-sha rollback) and "## Backing services" (`[[service]]`, pinned, volumes persist, per-app network, works with image or git apps). Update Scope + Known limitations. **Full pass confirming README reflects Phases 1–6.**
- [ ] **Step 5:** commit `test: e2e git deploy + backing service; docs: git deploy + backing README`.

---

## Self-Review

**Spec coverage:** spec git/build/service validation (T1); git clone (T2); docker build (T3); backing render (T4); applyGit + backing + rollback + remove (T5); daemon wiring (T6); UI From-Git/Builder + backing (T7); e2e + README (T8). ✓

**Deferred (by design):** Model B (repo self-config); git+repo-compose; auto-redeploy on push; registry push; Phase 7 (skill), Phase 8 (MCP).

**README:** T8 explicit + Phases 1–6 accuracy pass; final review checks it.

**Placeholder scan:** logic/testable units (spec, source/build fakes, backing render pure fn, reconciler against fakes) have concrete tests; real git/docker/compose give behavior contracts + adapt-notes (matching prior infra-task pattern). UI is spec+frontend-design-driven with fixed endpoints. No TBD.

**Type consistency:** `reconciler.New` gains `source.Git` + `build.Builder` (all call sites noted: cli=real, tests/web=fakes). `source.Git`/`build.Builder`/interfaces mirrored in fakes with `var _` assertions. `spec.App.Git`/`Services` + `spec.Service`/`spec.Git` consistent across spec/reconciler/web-render. Built image tag `lwd-build/<app>:<shortsha>` and per-app backing network `lwd-<app>` consistent across reconciler/backing-render/e2e. Reuses `compose.Composer`, `node.ConnectContainerToNetwork`, `store.Deployment.{Image,Compose,ContainerID}` from prior phases.

**Cross-task note:** T5 changes `reconciler.New`; T6 finishes wiring. Backing services reuse Phase 4's compose path (pinned) + Phase 4's ConnectContainerToNetwork. All tasks keep `go test ./...` green without Docker; real git/build/compose only in T8's guarded e2e.
