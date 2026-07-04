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
	"testing"
	"time"

	"lwd/internal/compose"
	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/router"
	"lwd/internal/secrets"
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
	rec := reconciler.New(n, rtr, s, secStore, compose.NewFake())

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
	rec := reconciler.New(n, rtr, s, secStore, compose.NewFake())

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
	var lastStatus int
	var lastErr error
	for i := 0; i < 20; i++ {
		lastStatus, lastErr = probe(client)
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
