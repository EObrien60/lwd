// Package e2e exercises the full lwd stack — node, router, store, reconciler
// — against a real Docker daemon and a real Caddy container. It is gated by
// LWD_DOCKER_TEST so `go test ./...` stays green without Docker.
package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lwd/internal/agent"
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

// remoteNodeName, remoteNodeAppLabel, and remoteNodeDomain are the node
// name/app label/domain used by TestEndToEndRemoteNode for the app placed on
// the registered "remote" node. remoteNodeLocalAppLabel/Domain are a second,
// distinct app deployed with node="local" in the same test, to prove the
// single-node path still works unchanged alongside a multi-node one.
const remoteNodeName = "e2e-remote"
const remoteNodeAppLabel = "e2e-remote-whoami"
const remoteNodeDomain = "remote-whoami.localhost"
const remoteNodeLocalAppLabel = "e2e-remote-local-whoami"
const remoteNodeLocalDomain = "remote-node-local.localhost"

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

// TestEndToEndRemoteNode drives the full Phase 9a federation path — node
// registry, docker-over-ssh transport, image save|load transfer, node=
// placement, mesh-address Caddy routing — against a real Docker daemon,
// using ssh://localhost as the "remote" node.
//
// This only works in an environment where the host's own sshd is running,
// key-based ssh to localhost works non-interactively, and that ssh session
// can reach the same Docker daemon (i.e. `docker -H ssh://localhost version`
// succeeds). That is NOT assumed to be true generally — most CI/sandboxed
// environments have no sshd at all — so this test probes for it first and
// SKIPs with a clear message if unusable. It never fakes a pass.
//
// Because ssh://localhost loops back to the exact same physical Docker
// daemon as node.NewLocal(), this exercises the real resolver/RegistryResolver
// -> node.NewRemoteSSH code path and the mesh-address routing/publish logic
// end to end, but it can't distinguish "the image was pulled directly by the
// target" from "the image was save|load transferred from the controller" —
// both leave the same observable state (the image present on the one
// underlying daemon). A genuinely separate second Docker host would be
// needed to exercise the save|load byte-transfer branch specifically; that
// is out of scope for a single-machine e2e harness.
//
// Run with: LWD_DOCKER_TEST=1 go test ./test/ -run TestEndToEndRemoteNode -v
func TestEndToEndRemoteNode(t *testing.T) {
	if os.Getenv("LWD_DOCKER_TEST") == "" {
		t.Skip("set LWD_DOCKER_TEST=1 to run the end-to-end test against real Docker")
	}

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	probeOut, probeErr := exec.CommandContext(probeCtx, "docker", "-H", "ssh://localhost", "version").CombinedOutput()
	probeCancel()
	if probeErr != nil {
		t.Skipf("ssh://localhost Docker is not usable in this environment (key-based ssh to localhost + a reachable Docker daemon are required): docker -H ssh://localhost version failed: %v: %s", probeErr, probeOut)
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

	// A real RegistryResolver over the store, exactly like the daemon builds
	// in cli.runDaemon — the thing actually under test here, not a
	// node.FakeResolver.
	resolver := node.NewRegistryResolver(n, "", func(name string) (string, string, string, bool, error) {
		rec, err := s.GetNode(name)
		if err != nil {
			return "", "", "", false, err
		}
		if rec == nil {
			return "", "", "", false, nil
		}
		return rec.SSHHost, rec.MeshAddr, rec.AgentURL, true, nil
	})
	rec := reconciler.New(resolver, rtr, s, secStore, compose.NewFake(), source.NewFake(), build.NewFake())

	if err := s.AddNode(store.Node{Name: remoteNodeName, SSHHost: "localhost", MeshAddr: "127.0.0.1", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	t.Cleanup(func() {
		cleanupRemoteNodeResources(t, rec, s)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := rtr.EnsureUp(ctx); err != nil {
		if portsInUse(t) {
			t.Skipf("ports 80/443 appear to be in use and EnsureUp failed: %v", err)
		}
		t.Fatalf("EnsureUp: %v", err)
	}

	client := &http.Client{Timeout: 3 * time.Second}

	// --- Deploy pinned to the registered "remote" node ---
	remoteApp := &spec.App{
		Name:   remoteNodeAppLabel,
		Image:  "traefik/whoami:latest",
		Domain: remoteNodeDomain,
		Port:   80,
		Node:   remoteNodeName,
	}
	remoteApp.Health.Path = "/"
	remoteApp.Health.Timeout = 30 * time.Second

	applyCtx, applyCancel := context.WithTimeout(context.Background(), 90*time.Second)
	dep, err := rec.Apply(applyCtx, remoteApp)
	applyCancel()
	if err != nil {
		t.Fatalf("remote Apply: %v", err)
	}
	if dep.Status != store.StatusRunning {
		t.Fatalf("remote deploy status = %q, want running", dep.Status)
	}

	assertReachableDomain(t, client, remoteNodeDomain, "remote node deploy")

	// The image must be present and the surface container running as seen
	// THROUGH the node's own docker-over-ssh endpoint — i.e. resolved via the
	// exact ssh://localhost client the reconciler used, not just the plain
	// local daemon view (which happens to be the same underlying daemon in
	// this loopback setup, but asserting through the ssh endpoint proves the
	// node= placement path actually ran, rather than relying on that
	// coincidence).
	if out, err := exec.Command("docker", "-H", "ssh://localhost", "image", "inspect", "traefik/whoami:latest").CombinedOutput(); err != nil {
		t.Fatalf("docker -H ssh://localhost image inspect traefik/whoami:latest: %v: %s", err, out)
	}
	psOut, err := exec.Command("docker", "-H", "ssh://localhost", "ps", "--filter", "label=lwd.app="+remoteNodeAppLabel, "--filter", "status=running", "--format", "{{.ID}}").CombinedOutput()
	if err != nil {
		t.Fatalf("docker -H ssh://localhost ps (lwd.app=%s): %v: %s", remoteNodeAppLabel, err, psOut)
	}
	if len(splitLines(string(psOut))) == 0 {
		t.Fatalf("no running container labeled lwd.app=%s found on ssh://localhost — the surface did not run on the remote node", remoteNodeAppLabel)
	}

	// --- A LOCAL app deployed in the same run still works unchanged ---
	localApp := &spec.App{
		Name:   remoteNodeLocalAppLabel,
		Image:  "traefik/whoami:latest",
		Domain: remoteNodeLocalDomain,
		Port:   80,
		Node:   "local",
	}
	localApp.Health.Path = "/"
	localApp.Health.Timeout = 30 * time.Second

	applyCtx2, applyCancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	dep2, err := rec.Apply(applyCtx2, localApp)
	applyCancel2()
	if err != nil {
		t.Fatalf("local Apply: %v", err)
	}
	if dep2.Status != store.StatusRunning {
		t.Fatalf("local deploy status = %q, want running", dep2.Status)
	}

	assertReachableDomain(t, client, remoteNodeLocalDomain, "local app alongside a remote-node deploy")
}

// cleanupRemoteNodeResources is TestEndToEndRemoteNode's defensive teardown.
// It asks the reconciler to remove both apps it deployed (the product path:
// Remove resolves each app's own node from its stored spec snapshot, so the
// remote app is torn down via the same ssh endpoint it was deployed to),
// removes the registered node from the store, falls back to a direct
// docker-over-ssh cleanup of any stray remote-app containers in case Remove
// never ran, then reuses the shared lwd-caddy/lwd-network cleanup+verification
// from the other e2e tests (which also catches the remote app's container,
// since ssh://localhost loops back to the same underlying daemon in this
// harness). Each step is best-effort (logged, not fatal).
func cleanupRemoteNodeResources(t *testing.T, rec *reconciler.Reconciler, s *store.Store) {
	t.Helper()

	if err := rec.Remove(context.Background(), remoteNodeAppLabel); err != nil {
		t.Logf("cleanup: reconciler.Remove(%s): %v", remoteNodeAppLabel, err)
	}
	if err := rec.Remove(context.Background(), remoteNodeLocalAppLabel); err != nil {
		t.Logf("cleanup: reconciler.Remove(%s): %v", remoteNodeLocalAppLabel, err)
	}
	if err := s.DeleteNode(remoteNodeName); err != nil {
		t.Logf("cleanup: DeleteNode(%s): %v", remoteNodeName, err)
	}

	// Fallback: directly remove any stray remote-app containers via the same
	// ssh endpoint, in case Remove never ran (e.g. the test failed before any
	// deploy succeeded).
	out, err := exec.Command("docker", "-H", "ssh://localhost", "ps", "-aq", "--filter", "label=lwd.app="+remoteNodeAppLabel).CombinedOutput()
	if err != nil {
		t.Logf("cleanup: docker -H ssh://localhost ps (lwd.app=%s) failed: %v: %s", remoteNodeAppLabel, err, out)
	} else {
		for _, id := range splitLines(string(out)) {
			if rmOut, rmErr := exec.Command("docker", "-H", "ssh://localhost", "rm", "-f", id).CombinedOutput(); rmErr != nil {
				t.Logf("cleanup: docker -H ssh://localhost rm -f %s failed: %v: %s", id, rmErr, rmOut)
			}
		}
	}

	cleanupLWDResources(t, remoteNodeAppLabel, remoteNodeLocalAppLabel)
}

// agentSelectionNodeName is the node name used by
// TestEndToEndAgentTransportSelection.
const agentSelectionNodeName = "e2e-agent-selection"

// TestEndToEndAgentTransportSelection proves the P9b agent-transport
// selection path end to end WITHOUT requiring Docker: a real lwd-agent HTTP
// server (internal/agent.Server) — backed by a fake node.Node, so its own
// authenticated /ready endpoint answers without any Docker daemon — is
// started on loopback via httptest.NewServer, a node is registered in a real
// store.Store with that server's URL as its agent_url, and a real
// node.RegistryResolver (the exact type the daemon wires in cli.runDaemon,
// driven by the same store-backed lookup closure) is asked to resolve and
// report reachability for that node.
//
// This is deliberately NOT gated by LWD_DOCKER_TEST: it is the "at minimum"
// case called out in the P9b spec — the resolver actually dials the real
// agent's authenticated /ready endpoint over real HTTP and prefers it over
// ssh (see
// RegistryResolver.buildTransport) — and it must stay part of the plain `go
// test ./...` run. It never fakes the ssh_host as reachable: sshHost below is
// a syntactically valid but never-dialed value, so if the resolver ever
// silently fell back to ssh instead of using the agent, the assertions below
// would fail rather than pass by coincidence.
//
// Deploying an actual container through the agent's other endpoints
// (EnsureImage, RunContainer, ...) requires a real Docker daemon and is
// exercised separately by TestEndToEndAgentNodeDeploy, guarded behind
// LWD_DOCKER_TEST.
func TestEndToEndAgentTransportSelection(t *testing.T) {
	dir := t.TempDir()

	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// Registered before the DeleteNode cleanup below so it runs LAST (t.Cleanup
	// is LIFO) — the store must still be open when that cleanup runs.
	t.Cleanup(func() { s.Close() })

	backing := node.NewFake()
	agentSrv := agent.NewServer(backing, "test-agent-token")
	ts := httptest.NewServer(agentSrv.Handler())
	defer ts.Close()

	if err := s.AddNode(store.Node{
		Name:      agentSelectionNodeName,
		SSHHost:   "deploy@127.0.0.1", // never dialed: the agent transport answers and is preferred
		MeshAddr:  "127.0.0.1",
		AgentURL:  ts.URL,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	t.Cleanup(func() {
		if err := s.DeleteNode(agentSelectionNodeName); err != nil {
			t.Logf("cleanup: DeleteNode(%s): %v", agentSelectionNodeName, err)
		}
	})

	local := node.NewFake()
	resolver := node.NewRegistryResolver(local, "test-agent-token", func(name string) (string, string, string, bool, error) {
		rec, err := s.GetNode(name)
		if err != nil {
			return "", "", "", false, err
		}
		if rec == nil {
			return "", "", "", false, nil
		}
		return rec.SSHHost, rec.MeshAddr, rec.AgentURL, true, nil
	})

	transport, reachable := resolver.Reachable(context.Background(), agentSelectionNodeName)
	if transport != "agent" {
		t.Fatalf("Reachable transport = %q, want %q", transport, "agent")
	}
	if !reachable {
		t.Fatal("Reachable = false, want true")
	}

	// ResolveMeta is the actual path a deploy takes: it must select the same
	// agent transport, and the resolved Node must be a real *node.AgentNode
	// that can reach the (fake-backed) agent for a trivial primitive.
	n, meshAddr, dockerHost, isLocal, err := resolver.ResolveMeta(agentSelectionNodeName)
	if err != nil {
		t.Fatalf("ResolveMeta: %v", err)
	}
	if isLocal {
		t.Fatalf("ResolveMeta(%q).isLocal = true, want false", agentSelectionNodeName)
	}
	if meshAddr != "127.0.0.1" {
		t.Fatalf("meshAddr = %q, want %q", meshAddr, "127.0.0.1")
	}
	if dockerHost != "ssh://deploy@127.0.0.1" {
		t.Fatalf("dockerHost = %q, want %q (dockerHost always mirrors the registry row's ssh_host, independent of the selected transport)", dockerHost, "ssh://deploy@127.0.0.1")
	}
	if _, ok := n.(*node.AgentNode); !ok {
		t.Fatalf("resolved Node is %T, want *node.AgentNode", n)
	}
	if err := n.Ping(context.Background()); err != nil {
		t.Fatalf("resolved agent Node Ping: %v", err)
	}
}

// agentDeployNodeName, agentDeployAppLabel, and agentDeployDomain are the
// node name/app label/domain used by TestEndToEndAgentNodeDeploy.
const agentDeployNodeName = "e2e-agent-node"
const agentDeployAppLabel = "e2e-agent-whoami"
const agentDeployDomain = "agent-whoami.localhost"

// TestEndToEndAgentNodeDeploy drives a full deploy through the P9b agent
// transport against a real Docker daemon: a real lwd-agent HTTP server
// (internal/agent.Server, backed by a real node.NewLocal()) is started on
// loopback, a node is registered pointing at it, and a real
// node.RegistryResolver deploys traefik/whoami to it — proving the resolver
// selects "agent" (not "ssh") and that a full
// EnsureImage/EnsureNetwork/RunContainer/Health round-trip over real HTTP
// through internal/agent.Server actually produces a reachable container.
//
// Because the agent's backing Docker client loops back to the exact same
// physical daemon as the "controller" side (node.NewLocal()) — there being no
// second machine available in this harness — this can't distinguish "the
// container ran on a genuinely separate node" from "it ran locally via the
// agent"; that caveat is identical to TestEndToEndRemoteNode's use of
// ssh://localhost. What it DOES prove end to end, honestly: transport
// selection picks the agent (sshHost below is deliberately a value with no
// real ssh session available, so a silent ssh fallback would fail this test
// rather than pass by coincidence), and the agent's HTTP surface performs a
// real, working deploy.
//
// Run with: LWD_DOCKER_TEST=1 go test ./test/ -run TestEndToEndAgentNodeDeploy -v
func TestEndToEndAgentNodeDeploy(t *testing.T) {
	if os.Getenv("LWD_DOCKER_TEST") == "" {
		t.Skip("set LWD_DOCKER_TEST=1 to run the end-to-end test against real Docker")
	}

	n, err := node.NewLocal()
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if err := n.Ping(context.Background()); err != nil {
		t.Skipf("no local Docker daemon reachable, skipping: %v", err)
	}
	if !agentDeployMeshLoopbackUsable(t) {
		t.Skip("this Docker setup does not let a sibling container reach a host-published 127.0.0.1 port (a real WireGuard mesh address would not have this restriction) — skipping, since this test stands in for a mesh address with loopback in the absence of a second machine")
	}

	dir := t.TempDir()

	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// Registered before the cleanupAgentNodeResources below so it runs LAST
	// (t.Cleanup is LIFO) — the store must still be open when that cleanup's
	// rec.Remove/s.DeleteNode calls run.
	t.Cleanup(func() { s.Close() })

	const agentToken = "e2e-agent-token"
	agentBacking, err := node.NewLocal()
	if err != nil {
		t.Fatalf("NewLocal (agent backing): %v", err)
	}
	agentSrv := agent.NewServer(agentBacking, agentToken)
	ts := httptest.NewServer(agentSrv.Handler())
	defer ts.Close()

	rtr := router.NewCaddyRouter(n, dir)
	cipher, err := secrets.NewCipher(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("secrets.NewCipher: %v", err)
	}
	secStore := secrets.NewStore(cipher, s)

	resolver := node.NewRegistryResolver(n, agentToken, func(name string) (string, string, string, bool, error) {
		rec, err := s.GetNode(name)
		if err != nil {
			return "", "", "", false, err
		}
		if rec == nil {
			return "", "", "", false, nil
		}
		return rec.SSHHost, rec.MeshAddr, rec.AgentURL, true, nil
	})
	rec := reconciler.New(resolver, rtr, s, secStore, compose.NewFake(), source.NewFake(), build.NewFake())

	if err := s.AddNode(store.Node{
		Name:      agentDeployNodeName,
		SSHHost:   "localhost", // never dialed: the agent transport is preferred and reachable
		MeshAddr:  "127.0.0.1",
		AgentURL:  ts.URL,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	t.Cleanup(func() {
		cleanupAgentNodeResources(t, rec, s)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := rtr.EnsureUp(ctx); err != nil {
		if portsInUse(t) {
			t.Skipf("ports 80/443 appear to be in use and EnsureUp failed: %v", err)
		}
		t.Fatalf("EnsureUp: %v", err)
	}

	// Prove transport selection BEFORE deploying, so a silent ssh fallback
	// (which would fail below since sshHost="localhost" here has no
	// guaranteed working ssh session in a sandboxed environment) is caught
	// explicitly rather than surfacing as a confusing Apply failure.
	if transport, reachable := resolver.Reachable(ctx, agentDeployNodeName); transport != "agent" || !reachable {
		t.Fatalf("Reachable(%q) = (%q, %v), want (\"agent\", true)", agentDeployNodeName, transport, reachable)
	}

	app := &spec.App{
		Name:   agentDeployAppLabel,
		Image:  "traefik/whoami:latest",
		Domain: agentDeployDomain,
		Port:   80,
		Node:   agentDeployNodeName,
	}
	app.Health.Path = "/"
	app.Health.Timeout = 30 * time.Second

	applyCtx, applyCancel := context.WithTimeout(context.Background(), 90*time.Second)
	dep, err := rec.Apply(applyCtx, app)
	applyCancel()
	if err != nil {
		t.Fatalf("agent-node Apply: %v", err)
	}
	if dep.Status != store.StatusRunning {
		t.Fatalf("agent-node deploy status = %q, want running", dep.Status)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	assertReachableDomain(t, client, agentDeployDomain, "agent-node deploy")
}

// cleanupAgentNodeResources is TestEndToEndAgentNodeDeploy's defensive
// teardown: it asks the reconciler to remove the app (torn down through the
// same agent transport it was deployed with), deregisters the node, then
// reuses the shared lwd-caddy/lwd-network cleanup+verification. Each step is
// best-effort (logged, not fatal).
func cleanupAgentNodeResources(t *testing.T, rec *reconciler.Reconciler, s *store.Store) {
	t.Helper()

	if err := rec.Remove(context.Background(), agentDeployAppLabel); err != nil {
		t.Logf("cleanup: reconciler.Remove(%s): %v", agentDeployAppLabel, err)
	}
	if err := s.DeleteNode(agentDeployNodeName); err != nil {
		t.Logf("cleanup: DeleteNode(%s): %v", agentDeployNodeName, err)
	}

	cleanupLWDResources(t, agentDeployAppLabel)
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

// agentMeshProbeContainerName is the throwaway container
// agentDeployMeshLoopbackUsable creates to probe loopback-as-mesh-address
// reachability.
const agentMeshProbeContainerName = "e2e-agent-mesh-probe"

// agentDeployMeshLoopbackUsable probes whether a container-published port
// bound to 127.0.0.1 — the stand-in this test uses for a node's WireGuard
// mesh address, in the absence of a second real machine — is reachable from
// a SIBLING container, exactly the path lwd-caddy needs for a remote-node
// route (see reconciler.deployBlueGreenSurface's meshAddr upstream). Some
// Docker setups (notably Docker Desktop's Linux VM) bind a 127.0.0.1 host
// publish to the VM's own loopback only, unreachable from any container
// including lwd-caddy; a real WireGuard mesh address would not have this
// restriction, so this is a property of the sandbox, not of lwd. It never
// fakes success: any probe failure returns false so the caller SKIPs with a
// clear message instead of failing on an environment limitation unrelated to
// the agent transport itself.
func agentDeployMeshLoopbackUsable(t *testing.T) bool {
	t.Helper()

	_ = exec.Command("docker", "rm", "-f", agentMeshProbeContainerName).Run()
	runOut, err := exec.Command("docker", "run", "-d", "--rm", "--name", agentMeshProbeContainerName, "-p", "127.0.0.1:0:80", "traefik/whoami:latest").CombinedOutput()
	if err != nil {
		t.Logf("mesh-loopback probe: docker run failed: %v: %s", err, runOut)
		return false
	}
	defer exec.Command("docker", "rm", "-f", agentMeshProbeContainerName).Run()

	portOut, err := exec.Command("docker", "port", agentMeshProbeContainerName, "80/tcp").CombinedOutput()
	if err != nil {
		t.Logf("mesh-loopback probe: docker port failed: %v: %s", err, portOut)
		return false
	}
	addr := strings.TrimSpace(strings.Split(string(portOut), "\n")[0])
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		t.Logf("mesh-loopback probe: unexpected docker port output %q", addr)
		return false
	}
	port := addr[idx+1:]

	// Give the probe container a moment to start listening before a sibling
	// container tries to reach it.
	var lastErr error
	var lastOut []byte
	for i := 0; i < 10; i++ {
		out, err := exec.Command("docker", "run", "--rm", "curlimages/curl:latest",
			"curl", "-sf", "-o", "/dev/null", "-m", "3", "http://127.0.0.1:"+port+"/").CombinedOutput()
		if err == nil {
			return true
		}
		lastErr, lastOut = err, out
		time.Sleep(300 * time.Millisecond)
	}
	t.Logf("mesh-loopback probe: a sibling container could not reach 127.0.0.1:%s (this is a known Docker-Desktop-style host-loopback-publish limitation, not an lwd bug): %v: %s", port, lastErr, lastOut)
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

// selfHealAppLabel and selfHealDomain are the app name and domain used by
// TestEndToEndSelfHeal.
const selfHealAppLabel = "e2e-selfheal"
const selfHealDomain = "selfheal.localhost"

// TestEndToEndSelfHeal drives Phase 10's continuous reconciler through the
// full self-heal path — deploy, surface dies, Reconcile heals it — entirely
// against fakes (node.Fake, router.FakeRouter, a real store.Store): no Docker
// daemon involved anywhere, so unlike every other test in this file it is NOT
// gated by LWD_DOCKER_TEST and always runs as part of `go test ./...`.
//
// It mirrors internal/reconciler's own TestReconcileHealsDeadSurface (the
// unit-level proof the heal path works) but from the outside: it builds the
// reconciler exactly as cli.runDaemon does (reconciler.New with a real
// store.Store) and asserts on the same store/router/node surface an operator
// (or the web Health panel, or `lwd health`) would observe, rather than
// reaching into the Reconciler's internals.
func TestEndToEndSelfHeal(t *testing.T) {
	dir := t.TempDir()

	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	fn := node.NewFake()
	fr := router.NewFakeRouter()
	fr.ProbeStatus = http.StatusOK
	cipher, err := secrets.NewCipher(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("secrets.NewCipher: %v", err)
	}
	secStore := secrets.NewStore(cipher, s)

	rec := reconciler.New(node.FakeResolver{"local": fn}, fr, s, secStore, compose.NewFake(), source.NewFake(), build.NewFake())

	app := &spec.App{
		Name:   selfHealAppLabel,
		Image:  "whoami:fake",
		Domain: selfHealDomain,
		Port:   8080,
		Node:   "local",
	}
	app.Health.Timeout = 150 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dep, err := rec.Apply(ctx, app)
	if err != nil {
		t.Fatalf("initial Apply: %v", err)
	}
	if dep.Status != store.StatusRunning {
		t.Fatalf("initial deployment status = %q, want %q", dep.Status, store.StatusRunning)
	}
	if dep.ContainerID == "" {
		t.Fatalf("initial deployment has no ContainerID")
	}

	// Simulate the surface dying: remove its container from the fake node
	// entirely, so a subsequent ContainerHealth reports it absent — the same
	// "dead" signal reconciler.surfaceIsDead treats a removed/exited real
	// container as (see node.Fake.ContainerHealth: HealthState defaults to
	// "running" only for containers still tracked in its items map).
	if err := fn.RemoveContainer(ctx, dep.ContainerID); err != nil {
		t.Fatalf("RemoveContainer (simulate surface death): %v", err)
	}

	if err := rec.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// A brand-new surface must now be current, distinct from the dead one.
	cur, err := s.CurrentDeployment(app.Name)
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil {
		t.Fatalf("want a current deployment after self-heal, got none")
	}
	if cur.ContainerID == "" || cur.ContainerID == dep.ContainerID {
		t.Errorf("want a new surface container after self-heal, got ContainerID=%q (original was %q)", cur.ContainerID, dep.ContainerID)
	}
	if cur.Status != store.StatusRunning {
		t.Errorf("healed deployment status = %q, want %q", cur.Status, store.StatusRunning)
	}

	// The router's live route must point at the healed surface, not the dead
	// one.
	route, ok := fr.Routes[app.Domain]
	if !ok {
		t.Fatalf("want a live route for %q after self-heal", app.Domain)
	}
	if route.Upstream == "" {
		t.Errorf("want a live route upstream set after self-heal, got %+v", route)
	}

	// The health snapshot (what GET /health, `lwd health`, and the web Health
	// panel all read) must report the app healthy again post-heal.
	snap := rec.HealthSnapshot()
	var found bool
	for _, ah := range snap.Apps {
		if ah.App == app.Name {
			found = true
			if ah.State != reconciler.SurfaceHealthy {
				t.Errorf("app health state = %q, want %q", ah.State, reconciler.SurfaceHealthy)
			}
		}
	}
	if !found {
		t.Fatalf("want %q present in health snapshot Apps, got %+v", app.Name, snap.Apps)
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
