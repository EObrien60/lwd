// Package e2e exercises the full lwd stack — node, router, store, reconciler
// — against a real Docker daemon and a real Caddy container. It is gated by
// LWD_DOCKER_TEST so `go test ./...` stays green without Docker.
package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lwd/internal/build"
	"lwd/internal/compose"
	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/router"
	"lwd/internal/secrets"
	"lwd/internal/source"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// appLabel is the lwd.app label value used for every surface container this
// test creates, so cleanup can find (and verify the absence of) exactly them.
const appLabel = "e2e-whoami"

// domain is a .localhost domain, which router.UseInternalTLS treats as
// internal — Caddy serves it with a self-signed cert, no ACME involved.
const domain = "whoami.localhost"

// secretAppLabel and failClosedAppLabel are the lwd.app label values used by
// TestEndToEndSecretInjection, kept distinct from appLabel so the two tests'
// cleanup and assertions never collide with each other.
const secretAppLabel = "e2e-secret-whoami"
const failClosedAppLabel = "e2e-failclosed-whoami"

// secretDomain and failClosedDomain are the .localhost domains used by
// TestEndToEndSecretInjection.
const secretDomain = "secret-whoami.localhost"
const failClosedDomain = "failclosed-whoami.localhost"

// composeAppLabel and composeDomain are the app name and domain used by
// TestEndToEndComposeApp. composeProject mirrors the reconciler's own
// "lwd-<app>" project-naming convention (see reconciler.applyCompose), so
// this test can inspect the project's containers directly via the compose
// CLI without going through lwd's own compose.Composer — that's the thing
// under test.
const composeAppLabel = "e2e-compose"
const composeDomain = "compose-whoami.localhost"
const composeProject = "lwd-" + composeAppLabel

// gitAppLabel, gitDomain, gitBackingProject, and gitBuiltImageRepo are the
// app name, domain, backing-compose project, and built-image repository used
// by TestEndToEndGitDeploy. gitBackingProject mirrors the reconciler's
// "lwd-<app>" project/network convention (see reconciler.ensureBacking /
// RenderBackingCompose); gitBuiltImageRepo mirrors its "lwd-build/<app>" tag
// convention (see reconciler.applyGit).
const gitAppLabel = "e2e-git"
const gitDomain = "git-whoami.localhost"
const gitBackingProject = "lwd-" + gitAppLabel
const gitBuiltImageRepo = "lwd-build/" + gitAppLabel

// caddyContainerName mirrors router.caddyContainerName (unexported), needed
// here only for best-effort cleanup via the docker CLI.
const caddyContainerName = "lwd-caddy"

// lwdNetwork mirrors the private network name used by router and reconciler.
const lwdNetwork = "lwd"

// TestEndToEndBlueGreenRollback drives the real stack — node.NewLocal,
// router.NewCaddyRouter, store.Open, reconciler.New — against a real Docker
// daemon: it brings up Caddy, deploys traefik/whoami twice (exercising the
// blue-green swap with a zero-downtime assertion), rolls back, and confirms
// cleanup leaves no stray lwd-labeled resources behind.
//
// Run with: LWD_DOCKER_TEST=1 go test ./test/ -v
func TestEndToEndBlueGreenRollback(t *testing.T) {
	if os.Getenv("LWD_DOCKER_TEST") == "" {
		t.Skip("set LWD_DOCKER_TEST=1 to run the end-to-end test against real Docker")
	}

	dir := t.TempDir()

	n, err := node.NewLocal()
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	rtr := router.NewCaddyRouter(n, dir)
	cipher, err := secrets.NewCipher(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("secrets.NewCipher: %v", err)
	}
	secStore := secrets.NewStore(cipher, s)
	rec := reconciler.New(node.FakeResolver{"local": n}, rtr, s, secStore, compose.NewFake(), source.NewFake(), build.NewFake())

	// Cleanup runs regardless of how the test ends (pass, fail, or panic via
	// t.Fatal) and is best-effort: each step's error is logged, not fatal, so
	// a failure partway through doesn't stop the rest of the teardown.
	t.Cleanup(func() {
		cleanupLWDResources(t, appLabel)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := rtr.EnsureUp(ctx); err != nil {
		if portsInUse(t) {
			t.Skipf("ports 80/443 appear to be in use and EnsureUp failed: %v", err)
		}
		t.Fatalf("EnsureUp: %v", err)
	}

	app := &spec.App{
		Name:   appLabel,
		Image:  "traefik/whoami:latest",
		Domain: domain,
		Port:   80, // whoami listens on :80; Caddy reaches it at <container>:80 on the lwd network
		Node:   "local",
	}
	app.Health.Path = "/"
	app.Health.Timeout = 30 * time.Second

	client := &http.Client{Timeout: 3 * time.Second}

	// --- First deploy ---
	applyCtx, applyCancel := context.WithTimeout(context.Background(), 60*time.Second)
	dep1, err := rec.Apply(applyCtx, app)
	applyCancel()
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if dep1.Status != store.StatusRunning {
		t.Fatalf("first deploy status = %q, want running", dep1.Status)
	}

	assertReachable(t, client, "first deploy")

	// --- Second deploy: exercises the blue-green swap ---
	// Poll the endpoint concurrently with the deploy to catch any downtime
	// window during the swap; the reconciler's own mutex serializes Apply, so
	// this poller and the deploy race safely against independent HTTP
	// connections.
	monitor := startZeroDowntimeMonitor(client)

	applyCtx2, applyCancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	dep2, err := rec.Apply(applyCtx2, app)
	applyCancel2()
	downtimeErrs := monitor.stop()

	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(downtimeErrs) > 0 {
		t.Errorf("zero-downtime violated during second deploy: %d failed probe(s), first: %v", len(downtimeErrs), downtimeErrs[0])
	}
	if dep2.Status != store.StatusRunning {
		t.Fatalf("second deploy status = %q, want running", dep2.Status)
	}
	if dep2.ContainerID == dep1.ContainerID {
		t.Fatalf("second deploy reused the same container id %q; blue-green should start a fresh surface", dep2.ContainerID)
	}

	assertReachable(t, client, "after second deploy")

	// The old surface must be gone: exactly one lwd.app=<appLabel> "surface"
	// container should remain.
	containers, err := n.ListContainers(context.Background(), map[string]string{"lwd.app": appLabel, "lwd.role": "surface"})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	var running []node.Container
	for _, c := range containers {
		if c.State == "running" {
			running = append(running, c)
		}
	}
	if len(running) != 1 {
		t.Fatalf("want exactly 1 running surface container labeled lwd.app=%s, got %d: %+v", appLabel, len(running), running)
	}
	if running[0].ID != dep2.ContainerID {
		t.Fatalf("remaining surface container id = %q, want the second deployment's %q", running[0].ID, dep2.ContainerID)
	}
	for _, c := range containers {
		if c.ID == dep1.ContainerID {
			t.Fatalf("old surface container %q (first deploy) still present: %+v", dep1.ContainerID, c)
		}
	}

	// --- Rollback ---
	rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 60*time.Second)
	dep3, err := rec.Rollback(rollbackCtx, appLabel)
	rollbackCancel()
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if dep3 == nil {
		t.Fatal("Rollback returned nil deployment")
	}
	if dep3.Status != store.StatusRunning {
		t.Fatalf("rollback deploy status = %q, want running", dep3.Status)
	}
	if dep3.Image != app.Image {
		t.Fatalf("rollback image = %q, want %q", dep3.Image, app.Image)
	}

	assertReachable(t, client, "after rollback")
}

// TestEndToEndSecretInjection drives the real stack with a real
// secrets.Store (secrets.NewCipher backed by a temp key file, wrapping the
// test's own store.Store) wired into the reconciler, and exercises two
// scenarios end to end against real Docker:
//
//  1. Injection: a secret is set via secStore.Set, the app declares it in
//     Secrets, and after a successful Apply the surface container's actual
//     environment (inspected via `docker inspect`, not the whoami HTTP
//     response) contains it.
//  2. Fail-closed: an app declares a secret that was never set; Apply must
//     return an error and must not leave any surface container running (in
//     fact, per the reconciler's contract, none should ever be created,
//     since secrets are resolved before the container is started).
//
// Run with: LWD_DOCKER_TEST=1 go test ./test/ -v
func TestEndToEndSecretInjection(t *testing.T) {
	if os.Getenv("LWD_DOCKER_TEST") == "" {
		t.Skip("set LWD_DOCKER_TEST=1 to run the end-to-end test against real Docker")
	}

	dir := t.TempDir()

	n, err := node.NewLocal()
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	rtr := router.NewCaddyRouter(n, dir)
	cipher, err := secrets.NewCipher(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("secrets.NewCipher: %v", err)
	}
	secStore := secrets.NewStore(cipher, s)
	rec := reconciler.New(node.FakeResolver{"local": n}, rtr, s, secStore, compose.NewFake(), source.NewFake(), build.NewFake())

	t.Cleanup(func() {
		cleanupLWDResources(t, secretAppLabel, failClosedAppLabel)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := rtr.EnsureUp(ctx); err != nil {
		if portsInUse(t) {
			t.Skipf("ports 80/443 appear to be in use and EnsureUp failed: %v", err)
		}
		t.Fatalf("EnsureUp: %v", err)
	}

	// --- Scenario 1: injection ---
	if err := secStore.Set(secretAppLabel, "LWD_TEST_SECRET", "s3cr3t"); err != nil {
		t.Fatalf("secStore.Set: %v", err)
	}

	app := &spec.App{
		Name:    secretAppLabel,
		Image:   "traefik/whoami:latest",
		Domain:  secretDomain,
		Port:    80,
		Node:    "local",
		Secrets: []string{"LWD_TEST_SECRET"},
	}
	app.Health.Path = "/"
	app.Health.Timeout = 30 * time.Second

	applyCtx, applyCancel := context.WithTimeout(context.Background(), 60*time.Second)
	dep, err := rec.Apply(applyCtx, app)
	applyCancel()
	if err != nil {
		t.Fatalf("Apply (injection): %v", err)
	}
	if dep.Status != store.StatusRunning {
		t.Fatalf("injection deploy status = %q, want running", dep.Status)
	}
	if dep.ContainerID == "" {
		t.Fatal("injection deploy has no ContainerID")
	}

	envOut, err := exec.Command("docker", "inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", dep.ContainerID).CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect %s: %v: %s", dep.ContainerID, err, envOut)
	}
	if !containsLine(splitLines(string(envOut)), "LWD_TEST_SECRET=s3cr3t") {
		t.Fatalf("container %s env does not contain LWD_TEST_SECRET=s3cr3t; got:\n%s", dep.ContainerID, envOut)
	}

	// --- Scenario 2: fail-closed on an unset secret ---
	failApp := &spec.App{
		Name:    failClosedAppLabel,
		Image:   "traefik/whoami:latest",
		Domain:  failClosedDomain,
		Port:    80,
		Node:    "local",
		Secrets: []string{"UNSET_SECRET"},
	}
	failApp.Health.Path = "/"
	failApp.Health.Timeout = 30 * time.Second

	failCtx, failCancel := context.WithTimeout(context.Background(), 60*time.Second)
	failDep, err := rec.Apply(failCtx, failApp)
	failCancel()
	if err == nil {
		t.Fatalf("Apply (fail-closed) unexpectedly succeeded: %+v", failDep)
	}

	failContainers, err := n.ListContainers(context.Background(), map[string]string{"lwd.app": failClosedAppLabel})
	if err != nil {
		t.Fatalf("ListContainers (fail-closed): %v", err)
	}
	if len(failContainers) != 0 {
		t.Fatalf("fail-closed deploy left %d container(s) labeled lwd.app=%s: %+v", len(failContainers), failClosedAppLabel, failContainers)
	}
}

// TestEndToEndComposeApp drives the real stack — including a real
// compose.CLI, not compose.NewFake() — against a real Docker daemon and the
// `docker compose` plugin. It deploys a two-service compose stack (a `web`
// service Caddy fronts, and a `cache` backing service that plays the role of
// a database), and proves the Phase 4 core guarantee: redeploying an
// unchanged compose file does NOT recreate the unchanged backing service —
// the `cache` container's ID survives across a redeploy, even though the
// whole point of the compose delegate model is that lwd does not manage that
// container directly.
//
// Run with: LWD_DOCKER_TEST=1 go test ./test/ -run TestEndToEndComposeApp -v
func TestEndToEndComposeApp(t *testing.T) {
	if os.Getenv("LWD_DOCKER_TEST") == "" {
		t.Skip("set LWD_DOCKER_TEST=1 to run the end-to-end test against real Docker")
	}
	if out, err := exec.Command("docker", "compose", "version").CombinedOutput(); err != nil {
		t.Skipf("docker compose plugin not available (docker compose version failed: %v: %s)", err, out)
	}

	dir := t.TempDir()

	composeFile := filepath.Join(dir, "docker-compose.yml")
	composeContent := "services:\n" +
		"  web:\n" +
		"    image: traefik/whoami:latest\n" +
		"  cache:\n" +
		"    image: redis:7-alpine\n"
	if err := os.WriteFile(composeFile, []byte(composeContent), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}

	n, err := node.NewLocal()
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// Registered via t.Cleanup (not a plain defer) so it runs after — not
	// before — the compose cleanup below: t.Cleanup callbacks run in LIFO
	// order among themselves, but a bare `defer` in the test body would run
	// as soon as the function returns, i.e. before any t.Cleanup callback,
	// closing the store out from under cleanupComposeProject's call to
	// rec.Remove.
	t.Cleanup(func() { s.Close() })

	rtr := router.NewCaddyRouter(n, dir)
	cipher, err := secrets.NewCipher(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("secrets.NewCipher: %v", err)
	}
	secStore := secrets.NewStore(cipher, s)
	rec := reconciler.New(node.FakeResolver{"local": n}, rtr, s, secStore, compose.NewCLI(), source.NewCLI(), build.NewCLI())

	// Cleanup runs regardless of how the test ends. It tears the compose
	// stack down (via the reconciler, exercising Remove/`compose down` as a
	// product path, plus a direct `docker compose down` fallback in case
	// Remove never ran), then reuses the shared lwd-caddy/lwd-network
	// cleanup+verification from the other e2e tests. Registered after the
	// store's own Cleanup above, so it runs first (LIFO) — the store is
	// still open when rec.Remove needs it.
	t.Cleanup(func() {
		cleanupComposeProject(t, rec, composeAppLabel, composeProject, composeFile)
		cleanupLWDResources(t, composeAppLabel)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := rtr.EnsureUp(ctx); err != nil {
		if portsInUse(t) {
			t.Skipf("ports 80/443 appear to be in use and EnsureUp failed: %v", err)
		}
		t.Fatalf("EnsureUp: %v", err)
	}

	app := &spec.App{
		Name:    composeAppLabel,
		Compose: composeFile,
		Service: "web",
		Domain:  composeDomain,
		Port:    80, // whoami listens on :80
		Node:    "local",
	}
	app.Health.Path = "/"
	app.Health.Timeout = 30 * time.Second

	client := &http.Client{Timeout: 3 * time.Second}

	// --- First deploy ---
	applyCtx, applyCancel := context.WithTimeout(context.Background(), 60*time.Second)
	dep1, err := rec.Apply(applyCtx, app)
	applyCancel()
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if dep1.Status != store.StatusRunning {
		t.Fatalf("first deploy status = %q, want running", dep1.Status)
	}

	assertReachableDomain(t, client, composeDomain, "first deploy")

	redisID1 := composeServiceContainerID(t, composeProject, "cache")

	// --- Redeploy: the core Phase 4 guarantee ---
	// The compose file and env are unchanged, so `docker compose up -d`
	// should leave the `cache` service's container completely untouched —
	// this is the whole point of delegating to compose instead of the
	// surfaces-outside-compose machinery: an unchanged backing service (here
	// standing in for a database) never goes down on redeploy.
	applyCtx2, applyCancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	dep2, err := rec.Apply(applyCtx2, app)
	applyCancel2()
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if dep2.Status != store.StatusRunning {
		t.Fatalf("second deploy status = %q, want running", dep2.Status)
	}

	assertReachableDomain(t, client, composeDomain, "after redeploy")

	redisID2 := composeServiceContainerID(t, composeProject, "cache")
	if redisID2 != redisID1 {
		t.Fatalf("cache (redis) container id changed across redeploy (%q -> %q): compose recreated an unchanged backing service, which should never happen", redisID1, redisID2)
	}

	// --- Remove ---
	if err := rec.Remove(context.Background(), composeAppLabel); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	assertNoComposeProjectContainers(t, composeProject)
}

// TestEndToEndGitDeploy drives the real stack — including real
// source.CLI (git) and build.CLI (`docker build`), not the fakes used by the
// reconciler's unit tests — against a real Docker daemon. It creates a
// throwaway local git repo (a one-line `FROM traefik/whoami` Dockerfile) and
// deploys it as a git-built app declaring a `cache` (redis) backing service,
// proving the full Phase 6 flow end to end:
//
//   - clone (via `file://<repo>`) + `docker build` actually produce a locally
//     tagged image (`lwd-build/e2e-git:<sha>`), which is then run via the
//     same zero-downtime blue-green surface path as an `image` app;
//   - the declared backing service comes up as a real pinned container on
//     the app's own compose project/network, reachable independent of
//     whoami actually using it;
//   - redeploying the same git ref reuses the built tag (no rebuild) while
//     still starting a fresh surface container (blue-green), and leaves the
//     pinned backing container completely untouched (same id);
//   - rollback redeploys the prior built tag without re-cloning or
//     rebuilding, and likewise never disturbs the backing container.
//
// Run with: LWD_DOCKER_TEST=1 go test ./test/ -run TestEndToEndGitDeploy -v
func TestEndToEndGitDeploy(t *testing.T) {
	if os.Getenv("LWD_DOCKER_TEST") == "" {
		t.Skip("set LWD_DOCKER_TEST=1 to run the end-to-end test against real Docker")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
	if out, err := exec.Command("docker", "compose", "version").CombinedOutput(); err != nil {
		t.Skipf("docker compose plugin not available (docker compose version failed: %v: %s)", err, out)
	}

	repoDir, branch := setupGitRepo(t)

	dir := t.TempDir()

	n, err := node.NewLocal()
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// Registered before the main cleanup below so it runs LAST (t.Cleanup is
	// LIFO) — the store must still be open when cleanupGitDeployResources
	// calls rec.Remove.
	t.Cleanup(func() { s.Close() })

	rtr := router.NewCaddyRouter(n, dir)
	cipher, err := secrets.NewCipher(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("secrets.NewCipher: %v", err)
	}
	secStore := secrets.NewStore(cipher, s)
	rec := reconciler.New(node.FakeResolver{"local": n}, rtr, s, secStore, compose.NewCLI(), source.NewCLI(), build.NewCLI())

	t.Cleanup(func() {
		cleanupGitDeployResources(t, rec)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := rtr.EnsureUp(ctx); err != nil {
		if portsInUse(t) {
			t.Skipf("ports 80/443 appear to be in use and EnsureUp failed: %v", err)
		}
		t.Fatalf("EnsureUp: %v", err)
	}

	app := &spec.App{
		Name:   gitAppLabel,
		Git:    &spec.Git{URL: "file://" + repoDir, Ref: branch},
		Build:  &spec.Build{Dockerfile: "Dockerfile"},
		Domain: gitDomain,
		Port:   80, // whoami listens on :80
		Node:   "local",
		Services: []spec.Service{
			{Name: "cache", Image: "redis:7-alpine"},
		},
	}
	app.Health.Path = "/"
	app.Health.Timeout = 30 * time.Second

	client := &http.Client{Timeout: 3 * time.Second}

	// --- First deploy: clone + build + backing + surface ---
	// A generous outer timeout: unlike the other e2e tests, this deploy also
	// clones a repo and runs `docker build` (which may need to pull the
	// traefik/whoami base layers on a cold cache).
	applyCtx, applyCancel := context.WithTimeout(context.Background(), 180*time.Second)
	dep1, err := rec.Apply(applyCtx, app)
	applyCancel()
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if dep1.Status != store.StatusRunning {
		t.Fatalf("first deploy status = %q, want running", dep1.Status)
	}
	if !strings.HasPrefix(dep1.Image, gitBuiltImageRepo+":") {
		t.Fatalf("first deploy image = %q, want prefix %q", dep1.Image, gitBuiltImageRepo+":")
	}

	assertReachableDomain(t, client, gitDomain, "first deploy")

	// The built image must actually exist locally — proves docker build ran
	// against the cloned repo, not just that the reconciler recorded a tag.
	if out, err := exec.Command("docker", "image", "inspect", dep1.Image).CombinedOutput(); err != nil {
		t.Fatalf("docker image inspect %s: %v: %s", dep1.Image, err, out)
	}

	// The cache (redis) backing container must be running, pinned, on the
	// app's own compose project/network — independent of whoami actually
	// talking to it.
	redisID1 := composeServiceContainerID(t, gitBackingProject, "cache")

	// --- Redeploy: same repo/ref, so the build should be skipped
	// (ImageExists short-circuits it), but a fresh surface is still started
	// (blue-green), and the pinned backing service must be left completely
	// untouched.
	applyCtx2, applyCancel2 := context.WithTimeout(context.Background(), 180*time.Second)
	dep2, err := rec.Apply(applyCtx2, app)
	applyCancel2()
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if dep2.Status != store.StatusRunning {
		t.Fatalf("second deploy status = %q, want running", dep2.Status)
	}
	if dep2.Image != dep1.Image {
		t.Fatalf("second deploy image = %q, want unchanged %q (same git ref should reuse the built tag)", dep2.Image, dep1.Image)
	}
	if dep2.ContainerID == dep1.ContainerID {
		t.Fatalf("second deploy reused the same container id %q; blue-green should start a fresh surface", dep2.ContainerID)
	}

	assertReachableDomain(t, client, gitDomain, "after redeploy")

	redisID2 := composeServiceContainerID(t, gitBackingProject, "cache")
	if redisID2 != redisID1 {
		t.Fatalf("cache (redis) backing container id changed across redeploy (%q -> %q): a pinned backing service must never be recreated by a surface redeploy", redisID1, redisID2)
	}

	// --- Rollback: redeploys the previous deployment's built image tag
	// (here, identical to the current one, since the git ref/sha never
	// changed) without re-cloning or rebuilding, and must not disturb the
	// pinned backing service.
	rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 180*time.Second)
	dep3, err := rec.Rollback(rollbackCtx, gitAppLabel)
	rollbackCancel()
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if dep3 == nil {
		t.Fatal("Rollback returned nil deployment")
	}
	if dep3.Status != store.StatusRunning {
		t.Fatalf("rollback deploy status = %q, want running", dep3.Status)
	}
	if dep3.Image != dep1.Image {
		t.Fatalf("rollback image = %q, want the prior built tag %q", dep3.Image, dep1.Image)
	}

	assertReachableDomain(t, client, gitDomain, "after rollback")

	redisID3 := composeServiceContainerID(t, gitBackingProject, "cache")
	if redisID3 != redisID1 {
		t.Fatalf("cache (redis) backing container id changed across rollback (%q -> %q): rollback must not disturb pinned backing services", redisID1, redisID3)
	}
}

// setupGitRepo creates a throwaway local git repository in a fresh temp dir
// containing a single-line Dockerfile (`FROM traefik/whoami` — the base
// image already listens on :80 and needs no build steps of its own), commits
// it under a local-only git identity, and returns the repo's directory and
// its current branch name (whatever the host's git default-branch config
// produces — deliberately not assumed to be "main", since that depends on
// the host's git version/config).
func setupGitRepo(t *testing.T) (dir, branch string) {
	t.Helper()
	dir = t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM traefik/whoami\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "lwd-e2e@example.com")
	runGit(t, dir, "config", "user.name", "lwd e2e")
	runGit(t, dir, "add", "Dockerfile")
	runGit(t, dir, "commit", "-m", "init")

	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s rev-parse --abbrev-ref HEAD: %v: %s", dir, err, out)
	}
	branch = strings.TrimSpace(string(out))
	if branch == "" {
		t.Fatalf("git -C %s rev-parse --abbrev-ref HEAD returned an empty branch name", dir)
	}
	return dir, branch
}

// runGit runs `git -C dir <args...>`, failing the test with the command's
// combined output on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	fullArgs := append([]string{"-C", dir}, args...)
	if out, err := exec.Command("git", fullArgs...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(fullArgs, " "), err, out)
	}
}

// cleanupGitDeployResources is TestEndToEndGitDeploy's defensive teardown.
// It asks the reconciler to remove the app (the product path: Remove ->
// removes the surface container(s) by label + `compose down`s the pinned
// backing project), falls back to direct docker commands in case Remove
// never got a chance to run (e.g. the test failed before any deploy
// succeeded), removes every built image tag for this app, then reuses the
// shared lwd-caddy/lwd-network cleanup+verification from the other e2e
// tests, and finally verifies no stray backing-project container remains.
// Each removal step is best-effort (logged, not fatal); the verification
// steps are real assertions.
func cleanupGitDeployResources(t *testing.T, rec *reconciler.Reconciler) {
	t.Helper()

	if err := rec.Remove(context.Background(), gitAppLabel); err != nil {
		t.Logf("cleanup: reconciler.Remove(%s): %v", gitAppLabel, err)
	}

	// Fallback: directly remove any stray backing-project containers and
	// network in case Remove never ran.
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+gitBackingProject).CombinedOutput()
	if err != nil {
		t.Logf("cleanup: docker ps (compose.project=%s) failed: %v: %s", gitBackingProject, err, out)
	} else {
		for _, id := range splitLines(string(out)) {
			if rmOut, rmErr := exec.Command("docker", "rm", "-f", id).CombinedOutput(); rmErr != nil {
				t.Logf("cleanup: docker rm -f %s failed: %v: %s", id, rmErr, rmOut)
			}
		}
	}
	if rmOut, rmErr := exec.Command("docker", "network", "rm", gitBackingProject).CombinedOutput(); rmErr != nil {
		t.Logf("cleanup: docker network rm %s: %v: %s", gitBackingProject, rmErr, rmOut)
	}

	// Remove every built image tag for this app (best-effort: an already-gone
	// tag from a failed early run, or an image still referenced by a
	// not-yet-removed container, fails removal harmlessly).
	imgOut, err := exec.Command("docker", "images", "--filter", "reference="+gitBuiltImageRepo, "-q").CombinedOutput()
	if err != nil {
		t.Logf("cleanup: docker images (reference=%s) failed: %v: %s", gitBuiltImageRepo, err, imgOut)
	} else {
		for _, id := range splitLines(string(imgOut)) {
			if rmOut, rmErr := exec.Command("docker", "rmi", "-f", id).CombinedOutput(); rmErr != nil {
				t.Logf("cleanup: docker rmi -f %s failed: %v: %s", id, rmErr, rmOut)
			}
		}
	}

	cleanupLWDResources(t, gitAppLabel)

	// Verify no stray backing-project container remains.
	verifyOut, err := exec.Command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+gitBackingProject).CombinedOutput()
	if err != nil {
		t.Errorf("cleanup verification: docker ps (compose.project=%s) failed: %v: %s", gitBackingProject, err, verifyOut)
	} else if remaining := splitLines(string(verifyOut)); len(remaining) > 0 {
		t.Errorf("cleanup verification: %d stray container(s) for backing project %s remain: %v", len(remaining), gitBackingProject, remaining)
	}
}

// composeServiceContainerID resolves the running container ID for service
// within project via the compose CLI directly (`docker compose -p <project>
// ps -q <service>`) — deliberately bypassing lwd's own compose.Composer,
// which is exactly the thing under test in TestEndToEndComposeApp.
func composeServiceContainerID(t *testing.T, project, service string) string {
	t.Helper()
	out, err := exec.Command("docker", "compose", "-p", project, "ps", "-q", service).CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose -p %s ps -q %s: %v: %s", project, service, err, out)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		t.Fatalf("no running container found for service %s in project %s", service, project)
	}
	return id
}

// assertNoComposeProjectContainers fails the test if any container labeled
// com.docker.compose.project=project remains — the label the compose CLI
// itself applies to every container it creates for a project, regardless of
// service.
func assertNoComposeProjectContainers(t *testing.T, project string) {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+project).CombinedOutput()
	if err != nil {
		t.Fatalf("docker ps (compose.project=%s) failed: %v: %s", project, err, out)
	}
	if remaining := splitLines(string(out)); len(remaining) > 0 {
		t.Fatalf("%d stray container(s) for compose project %s remain: %v", len(remaining), project, remaining)
	}
}

// cleanupComposeProject is TestEndToEndComposeApp's defensive teardown. It
// asks the reconciler to remove the app (the product path: Remove ->
// `compose down` for a compose app), then falls back to a direct `docker
// compose down` against composeFile in case Remove never got a chance to run
// (e.g. the test failed before the app was ever successfully deployed, so
// the store has no current deployment to key off of), and finally verifies
// no container labeled for this compose project remains. Each step is
// best-effort (logged, not fatal) except the final verification, matching
// the pattern established by cleanupLWDResources.
func cleanupComposeProject(t *testing.T, rec *reconciler.Reconciler, appName, project, composeFile string) {
	t.Helper()

	if err := rec.Remove(context.Background(), appName); err != nil {
		t.Logf("cleanup: reconciler.Remove(%s): %v", appName, err)
	}

	if out, err := exec.Command("docker", "compose", "-p", project, "-f", composeFile, "down").CombinedOutput(); err != nil {
		t.Logf("cleanup: docker compose -p %s -f %s down: %v: %s", project, composeFile, err, out)
	}

	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+project).CombinedOutput()
	if err != nil {
		t.Errorf("cleanup verification: docker ps (compose.project=%s) failed: %v: %s", project, err, out)
	} else if remaining := splitLines(string(out)); len(remaining) > 0 {
		t.Errorf("cleanup verification: %d stray container(s) for compose project %s remain: %v", len(remaining), project, remaining)
	}
}

// containsLine reports whether lines contains an exact match for target.
func containsLine(lines []string, target string) bool {
	for _, l := range lines {
		if l == target {
			return true
		}
	}
	return false
}

// zeroDowntimeMonitor repeatedly probes the app's endpoint in the background
// and records every non-200 result, so a caller can assert zero downtime
// across some operation (e.g. a blue-green swap) that runs concurrently.
type zeroDowntimeMonitor struct {
	stopCh chan struct{}
	doneCh chan []error
}

// startZeroDowntimeMonitor begins polling immediately in a background
// goroutine; call stop() to end polling and collect the failures observed.
func startZeroDowntimeMonitor(client *http.Client) *zeroDowntimeMonitor {
	m := &zeroDowntimeMonitor{
		stopCh: make(chan struct{}),
		doneCh: make(chan []error, 1),
	}
	go func() {
		var errs []error
		for {
			select {
			case <-m.stopCh:
				m.doneCh <- errs
				return
			default:
			}
			if status, err := probe(client); err != nil {
				errs = append(errs, fmt.Errorf("probe transport error: %w", err))
			} else if status != 200 {
				errs = append(errs, fmt.Errorf("probe returned status %d", status))
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	return m
}

// stop ends polling and returns every failure observed since start.
func (m *zeroDowntimeMonitor) stop() []error {
	close(m.stopCh)
	return <-m.doneCh
}

// probe issues one GET through Caddy's public HTTP listener with the
// domain's Host header, returning the response status code.
func probe(client *http.Client) (int, error) {
	return probeDomain(client, domain)
}

// probeDomain is probe generalized to an arbitrary Host header, used by
// tests (like TestEndToEndComposeApp) that route a domain other than the
// package-level whoami.localhost constant.
func probeDomain(client *http.Client, domain string) (int, error) {
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:80/", nil)
	if err != nil {
		return 0, err
	}
	req.Host = domain
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// assertReachable retries the probe a few times to allow Caddy/the container
// to settle, then fails the test if it never returns 200.
func assertReachable(t *testing.T, client *http.Client, when string) {
	t.Helper()
	assertReachableDomain(t, client, domain, when)
}

// assertReachableDomain is assertReachable generalized to an arbitrary
// domain.
func assertReachableDomain(t *testing.T, client *http.Client, domain, when string) {
	t.Helper()
	var lastStatus int
	var lastErr error
	for i := 0; i < 20; i++ {
		lastStatus, lastErr = probeDomain(client, domain)
		if lastErr == nil && lastStatus == 200 {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s: endpoint never returned 200 (last status=%d, err=%v)", when, lastStatus, lastErr)
}

// portsInUse does a best-effort check for something already listening on
// 80/443, used only to decide whether an EnsureUp failure should SKIP rather
// than FAIL the test.
func portsInUse(t *testing.T) bool {
	t.Helper()
	for _, port := range []string{"80", "443"} {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
	}
	return false
}

// cleanupLWDResources removes every container labeled lwd.app=<label> for
// each of appLabels, the lwd-caddy container, and the lwd network, then
// asserts none remain: no stray lwd.app=<label> surface containers for any
// label passed, no lwd-caddy container, and no lwd network. It shells out to
// the docker CLI directly (rather than node.Node, which has no
// remove-network/remove-caddy helpers) since this is test-only teardown, not
// product code.
//
// The removal steps are best-effort (errors are logged, not fatal, so a
// failure partway through doesn't stop the rest of the teardown), but the
// final verification steps are real assertions: each e2e test that calls
// this owns the shared lwd/lwd-caddy resources for the duration of its own
// run, so once its own cleanup has run, both must be gone.
func cleanupLWDResources(t *testing.T, appLabels ...string) {
	t.Helper()

	for _, label := range appLabels {
		out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=lwd.app="+label).CombinedOutput()
		if err != nil {
			t.Logf("cleanup: docker ps (lwd.app=%s) failed: %v: %s", label, err, out)
			continue
		}
		for _, id := range splitLines(string(out)) {
			if rmOut, rmErr := exec.Command("docker", "rm", "-f", id).CombinedOutput(); rmErr != nil {
				t.Logf("cleanup: docker rm -f %s failed: %v: %s", id, rmErr, rmOut)
			}
		}
	}

	if rmOut, rmErr := exec.Command("docker", "rm", "-f", caddyContainerName).CombinedOutput(); rmErr != nil {
		t.Logf("cleanup: docker rm -f %s: %v: %s", caddyContainerName, rmErr, rmOut)
	}

	if rmOut, rmErr := exec.Command("docker", "network", "rm", lwdNetwork).CombinedOutput(); rmErr != nil {
		t.Logf("cleanup: docker network rm %s: %v: %s", lwdNetwork, rmErr, rmOut)
	}

	// Verify no stray lwd.app=<label> surface containers remain for any
	// label this test owns.
	for _, label := range appLabels {
		verifyOut, err := exec.Command("docker", "ps", "-aq", "--filter", "label=lwd.app="+label).CombinedOutput()
		if err != nil {
			t.Errorf("cleanup verification: docker ps failed: %v: %s", err, verifyOut)
		} else if remaining := splitLines(string(verifyOut)); len(remaining) > 0 {
			t.Errorf("cleanup verification: %d stray container(s) labeled lwd.app=%s remain: %v", len(remaining), label, remaining)
		}
	}

	// Verify the lwd-caddy container is gone. This test is the only one that
	// manages lwd-caddy, so it must not survive its own teardown.
	caddyOut, err := exec.Command("docker", "ps", "-a", "--filter", "name=^/"+caddyContainerName+"$", "--format", "{{.Names}}").CombinedOutput()
	if err != nil {
		t.Errorf("cleanup verification: docker ps (name=%s) failed: %v: %s", caddyContainerName, err, caddyOut)
	} else if remaining := splitLines(string(caddyOut)); len(remaining) > 0 {
		t.Errorf("cleanup verification: %s container still present after cleanup: %v", caddyContainerName, remaining)
	}

	// Verify the lwd network is gone. Scoped to run only after this test's
	// own cleanup, since this is the only e2e test that creates/manages the
	// lwd network — no other test relies on it surviving.
	netOut, err := exec.Command("docker", "network", "ls", "--filter", "name=^"+lwdNetwork+"$", "--format", "{{.Name}}").CombinedOutput()
	if err != nil {
		t.Errorf("cleanup verification: docker network ls (name=%s) failed: %v: %s", lwdNetwork, err, netOut)
	} else if remaining := splitLines(string(netOut)); len(remaining) > 0 {
		t.Errorf("cleanup verification: %s network still present after cleanup: %v", lwdNetwork, remaining)
	}
}

// splitLines splits docker CLI output into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			line := s[start:i]
			// trim \r if present
			for len(line) > 0 && (line[len(line)-1] == '\r' || line[len(line)-1] == ' ') {
				line = line[:len(line)-1]
			}
			if line != "" {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	return out
}
