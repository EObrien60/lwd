package reconciler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lwd/internal/build"
	"lwd/internal/compose"
	"lwd/internal/node"
	"lwd/internal/router"
	"lwd/internal/source"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// shortTimeout keeps failure-path tests (which must poll to a deadline) fast.
const shortTimeout = 150 * time.Millisecond

// fakeResolver is a test double for SecretResolver: it either returns a
// canned error (simulating a fail-closed resolve failure) or looks up each
// requested name in vals (defaulting to "" for names not present, matching
// the brief's fake).
type fakeResolver struct {
	vals map[string]string
	err  error
}

func (f *fakeResolver) Resolve(app string, names []string) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]string{}
	for _, n := range names {
		out[n] = f.vals[n]
	}
	return out, nil
}

func newTestReconciler(t *testing.T) (*Reconciler, *node.Fake, *router.FakeRouter, *store.Store) {
	t.Helper()
	return newTestReconcilerWithResolver(t, &fakeResolver{vals: map[string]string{}})
}

func newTestReconcilerWithResolver(t *testing.T, sec SecretResolver) (*Reconciler, *node.Fake, *router.FakeRouter, *store.Store) {
	t.Helper()
	r, f, fr, s, _, _, _ := newTestReconcilerFull(t, sec)
	return r, f, fr, s
}

// newTestReconcilerWithCompose is like newTestReconciler but also returns the
// compose.Fake, for tests that exercise the compose deploy path and need to
// assert on / configure its calls (Up/ServiceContainer/Down).
func newTestReconcilerWithCompose(t *testing.T) (*Reconciler, *node.Fake, *router.FakeRouter, *store.Store, *compose.Fake) {
	t.Helper()
	r, f, fr, s, cf, _, _ := newTestReconcilerFull(t, &fakeResolver{vals: map[string]string{}})
	return r, f, fr, s, cf
}

// newTestReconcilerWithGit is like newTestReconciler but also returns the
// source.Fake and build.Fake, for tests that exercise the git-deploy path and
// need to assert on / configure its calls (Clone/Build/ImageExists).
func newTestReconcilerWithGit(t *testing.T) (*Reconciler, *node.Fake, *router.FakeRouter, *store.Store, *source.Fake, *build.Fake) {
	t.Helper()
	return newTestReconcilerWithGitAndResolver(t, &fakeResolver{vals: map[string]string{}})
}

// newTestReconcilerWithGitAndResolver is newTestReconcilerWithGit with an
// explicit secret resolver, for git-path fail-closed-secret tests.
func newTestReconcilerWithGitAndResolver(t *testing.T, sec SecretResolver) (*Reconciler, *node.Fake, *router.FakeRouter, *store.Store, *source.Fake, *build.Fake) {
	t.Helper()
	r, f, fr, s, _, sf, bf := newTestReconcilerFull(t, sec)
	return r, f, fr, s, sf, bf
}

// newTestReconcilerFull builds a Reconciler wired to fresh fakes for every
// dependency (node, router, compose, source, build) plus a temp-file store,
// using sec as the secret resolver. It is the single place all the other
// constructors above funnel through, so New's dependency list only has to be
// listed once here.
func newTestReconcilerFull(t *testing.T, sec SecretResolver) (*Reconciler, *node.Fake, *router.FakeRouter, *store.Store, *compose.Fake, *source.Fake, *build.Fake) {
	t.Helper()
	f := node.NewFake()
	fr := router.NewFakeRouter()
	cf := compose.NewFake()
	sf := source.NewFake()
	bf := build.NewFake()
	s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	resolver := node.FakeResolver{"local": f}
	return New(resolver, fr, s, sec, cf, sf, bf), f, fr, s, cf, sf, bf
}

func testApp() *spec.App {
	return &spec.App{Name: "blog", Image: "img:1", Domain: "blog.example.com", Port: 8080, Node: "local"}
}

// testGitApp returns a valid git-built app spec with no backing services: a
// single-service surface built from `[git]` + `[build]`, per the Phase 6
// design (docs/superpowers/specs/2026-07-04-lwd-phase6-git-deploy-design.md).
func testGitApp() *spec.App {
	return &spec.App{
		Name:   "gitapp",
		Domain: "gitapp.example.com",
		Port:   8080,
		Node:   "local",
		Git:    &spec.Git{URL: "https://example.com/repo.git", Ref: "main"},
		Build:  &spec.Build{Dockerfile: "Dockerfile"},
	}
}

// testComposeApp writes content to a temp compose file and returns a compose
// spec.App pointing at it (Compose already resolved to an absolute path, as
// spec.Load is expected to do at parse time).
func testComposeApp(t *testing.T, content string) *spec.App {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker-compose.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	return &spec.App{
		Name:    "webapp",
		Compose: path,
		Service: "web",
		Domain:  "webapp.example.com",
		Port:    8080,
		Node:    "local",
	}
}

func TestApplyStagesProbesFlips(t *testing.T) {
	r, f, fr, s := newTestReconciler(t)
	app := testApp()
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	dep, err := r.Apply(context.Background(), app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !contains(f.Calls, "RunContainer:lwd-blog-1") {
		t.Fatalf("sanity: RunContainer not called, calls: %v", f.Calls)
	}
	if !containsInOrder(fr.Calls, "SetStaging:stage-1.lwd.internal", "ProbeThroughCaddy:stage-1.lwd.internal") {
		t.Errorf("expected SetStaging before ProbeThroughCaddy, calls: %v", fr.Calls)
	}
	if !containsInOrder(fr.Calls, "ProbeThroughCaddy:stage-1.lwd.internal", "SetRoute:blog.example.com") {
		t.Errorf("expected ProbeThroughCaddy before SetRoute, calls: %v", fr.Calls)
	}
	if !containsInOrder(fr.Calls, "SetRoute:blog.example.com", "RemoveStaging:stage-1.lwd.internal") {
		t.Errorf("expected SetRoute before RemoveStaging, calls: %v", fr.Calls)
	}

	if dep.Status != store.StatusRunning {
		t.Errorf("status = %q, want running", dep.Status)
	}
	if dep.Spec == "" {
		t.Errorf("want non-empty Spec snapshot")
	}

	route, ok := fr.Routes["blog.example.com"]
	if !ok {
		t.Fatalf("Routes[blog.example.com] not set, routes: %+v", fr.Routes)
	}
	if route.Upstream != dep.ContainerID && route.Upstream != "lwd-blog-1" {
		t.Errorf("route.Upstream = %q, want the new container name", route.Upstream)
	}
	if fr.Staging["stage-1.lwd.internal"] {
		t.Errorf("staging route should have been removed after cutover")
	}

	cur, _ := s.CurrentDeployment("blog")
	if cur == nil || cur.ContainerID != dep.ContainerID {
		t.Errorf("CurrentDeployment mismatch: %+v", cur)
	}
}

func TestApplyHealthFailLeavesDomainUntouched(t *testing.T) {
	r, f, fr, s := newTestReconciler(t)
	app := testApp()
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 502

	_, err := r.Apply(context.Background(), app)
	if err == nil {
		t.Fatal("want error when health probe never succeeds")
	}

	if !contains(f.Calls, "RemoveContainer:fake-1") {
		t.Errorf("expected new container removed, calls: %v", f.Calls)
	}
	if fr.Staging["stage-1.lwd.internal"] {
		t.Errorf("staging route should have been removed on failure")
	}
	if _, ok := fr.Routes["blog.example.com"]; ok {
		t.Errorf("live domain route must never be set on health failure, routes: %+v", fr.Routes)
	}
	if cur, _ := s.CurrentDeployment("blog"); cur != nil {
		t.Errorf("want no running deployment, got %+v", cur)
	}
}

func TestApplyHealthFailWithProbeErrLeavesDomainUntouched(t *testing.T) {
	r, _, fr, s := newTestReconciler(t)
	app := testApp()
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout
	fr.ProbeErr = context.DeadlineExceeded

	_, err := r.Apply(context.Background(), app)
	if err == nil {
		t.Fatal("want error when the probe transport fails")
	}
	if _, ok := fr.Routes["blog.example.com"]; ok {
		t.Errorf("live domain route must never be set on health failure")
	}
	if cur, _ := s.CurrentDeployment("blog"); cur != nil {
		t.Errorf("want no running deployment, got %+v", cur)
	}
}

func TestApplyLivenessFallbackNoPath(t *testing.T) {
	t.Run("healthy when Caddy reaches the app", func(t *testing.T) {
		r, _, fr, _ := newTestReconciler(t)
		app := testApp()
		app.Health.Timeout = shortTimeout
		fr.ProbeStatus = 404 // not 502/503: Caddy reached the app.

		dep, err := r.Apply(context.Background(), app)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if _, ok := fr.Routes["blog.example.com"]; !ok {
			t.Errorf("want route set for successful liveness fallback")
		}
		if dep.Status != store.StatusRunning {
			t.Errorf("status = %q, want running", dep.Status)
		}
	})

	t.Run("fails when Caddy cannot reach the app", func(t *testing.T) {
		r, _, fr, s := newTestReconciler(t)
		app := testApp()
		app.Health.Timeout = shortTimeout
		fr.ProbeStatus = 502 // Caddy-generated bad gateway.

		_, err := r.Apply(context.Background(), app)
		if err == nil {
			t.Fatal("want error when Caddy never reaches the app")
		}
		if _, ok := fr.Routes["blog.example.com"]; ok {
			t.Errorf("live domain route must never be set on health failure")
		}
		if cur, _ := s.CurrentDeployment("blog"); cur != nil {
			t.Errorf("want no running deployment, got %+v", cur)
		}
	})
}

func TestApplyDockerHealthcheck(t *testing.T) {
	f := node.NewFake()
	f.DockerHealth = "healthy"
	fr := router.NewFakeRouter()
	s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	r := New(node.FakeResolver{"local": f}, fr, s, &fakeResolver{vals: map[string]string{}}, compose.NewFake(), source.NewFake(), build.NewFake())

	app := testApp()
	app.Health.Timeout = shortTimeout

	dep, err := r.Apply(context.Background(), app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if dep.Status != store.StatusRunning {
		t.Errorf("status = %q, want running", dep.Status)
	}
	if contains(fr.Calls, "ProbeThroughCaddy:stage-1.lwd.internal") {
		t.Errorf("docker healthcheck path should not need ProbeThroughCaddy for readiness, calls: %v", fr.Calls)
	}
}

func TestApplyDockerHealthcheckUnhealthyFails(t *testing.T) {
	f := node.NewFake()
	f.DockerHealth = "unhealthy"
	fr := router.NewFakeRouter()
	s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	r := New(node.FakeResolver{"local": f}, fr, s, &fakeResolver{vals: map[string]string{}}, compose.NewFake(), source.NewFake(), build.NewFake())

	app := testApp()
	app.Health.Timeout = shortTimeout

	_, err = r.Apply(context.Background(), app)
	if err == nil {
		t.Fatal("want error when Docker reports unhealthy")
	}
	if _, ok := fr.Routes["blog.example.com"]; ok {
		t.Errorf("live domain route must never be set on health failure")
	}
}

func TestApplyBlueGreenRetiresOld(t *testing.T) {
	r, f, fr, _ := newTestReconciler(t)
	ctx := context.Background()
	app := testApp()
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	first, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	second, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	if first.ContainerID == second.ContainerID {
		t.Fatal("expected a new container on redeploy")
	}
	if !contains(f.Calls, "RemoveContainer:"+first.ContainerID) {
		t.Errorf("expected old container removed, calls: %v", f.Calls)
	}
	route, ok := fr.Routes["blog.example.com"]
	if !ok || route.Upstream != "lwd-blog-2" {
		t.Errorf("route should point at the second container, got %+v", route)
	}
}

func TestApplyRedeployHealthFailKeepsOldServing(t *testing.T) {
	r, f, fr, s := newTestReconciler(t)
	ctx := context.Background()
	app := testApp()
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	v1, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("v1 Apply: %v", err)
	}

	routeBefore, ok := fr.Routes["blog.example.com"]
	if !ok {
		t.Fatalf("Routes[blog.example.com] not set after v1, routes: %+v", fr.Routes)
	}

	fr.ProbeStatus = 502
	v2, err := r.Apply(ctx, app)
	if err == nil {
		t.Fatal("want error when v2 health probe never succeeds")
	}
	if v2 != nil {
		t.Errorf("want nil deployment on failure, got %+v", v2)
	}

	cur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil || cur.ID != v1.ID || cur.ContainerID != v1.ContainerID || cur.Status != store.StatusRunning {
		t.Fatalf("want v1 still the current running deployment, got %+v (v1=%+v)", cur, v1)
	}

	if contains(f.Calls, "RemoveContainer:"+v1.ContainerID) {
		t.Errorf("v1 container must not be removed on v2 health failure, calls: %v", f.Calls)
	}
	if !contains(f.Calls, "RemoveContainer:fake-2") {
		t.Errorf("expected the new (v2) container to be removed, calls: %v", f.Calls)
	}

	routeAfter, ok := fr.Routes["blog.example.com"]
	if !ok {
		t.Fatalf("Routes[blog.example.com] must remain set after failed redeploy, routes: %+v", fr.Routes)
	}
	if routeAfter.Upstream != routeBefore.Upstream {
		t.Errorf("route.Upstream changed on failed redeploy: before=%q after=%q", routeBefore.Upstream, routeAfter.Upstream)
	}
	if routeAfter.Upstream != "lwd-blog-1" {
		t.Errorf("route.Upstream = %q, want still pointing at v1's container name", routeAfter.Upstream)
	}
}

func TestApplySetRouteFailureRecordsFailedAndKeepsOld(t *testing.T) {
	r, f, fr, s := newTestReconciler(t)
	ctx := context.Background()
	app := testApp()
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	v1, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("v1 Apply: %v", err)
	}

	fr.SetRouteErr = fmt.Errorf("boom")
	v2, err := r.Apply(ctx, app)
	if err == nil {
		t.Fatal("want error when SetRoute fails")
	}
	if v2 != nil {
		t.Errorf("want nil deployment on failure, got %+v", v2)
	}

	// The prior running deployment must still be current: SetRoute failing
	// after a passing health check must not touch the old, live deployment.
	cur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil || cur.ID != v1.ID || cur.ContainerID != v1.ContainerID || cur.Status != store.StatusRunning {
		t.Fatalf("want v1 still the current running deployment, got %+v (v1=%+v)", cur, v1)
	}

	// The v2 container must have been removed.
	if !contains(f.Calls, "RemoveContainer:fake-2") {
		t.Errorf("expected the new (v2) container to be removed, calls: %v", f.Calls)
	}
	if contains(f.Calls, "RemoveContainer:"+v1.ContainerID) {
		t.Errorf("v1 container must not be removed on v2 SetRoute failure, calls: %v", f.Calls)
	}

	// A StatusFailed row must have been recorded for the v2 attempt — unlike
	// every other failure path, this one used to vanish from history.
	history, err := s.DeploymentsForApp(app.Name)
	if err != nil {
		t.Fatalf("DeploymentsForApp: %v", err)
	}
	var sawFailed bool
	for _, d := range history {
		if d.Status == store.StatusFailed && d.ContainerID == "fake-2" {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Errorf("want a StatusFailed deployment recorded for the failed SetRoute attempt, history: %+v", history)
	}
}

func TestApplyDockerHealthStartingThenHealthy(t *testing.T) {
	f := node.NewFake()
	f.DockerHealthSeq = []string{"starting", "starting", "healthy"}
	fr := router.NewFakeRouter()
	s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	r := New(node.FakeResolver{"local": f}, fr, s, &fakeResolver{vals: map[string]string{}}, compose.NewFake(), source.NewFake(), build.NewFake())

	app := testApp()
	app.Health.Timeout = shortTimeout

	dep, err := r.Apply(context.Background(), app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if dep.Status != store.StatusRunning {
		t.Errorf("status = %q, want running", dep.Status)
	}
	if _, ok := fr.Routes["blog.example.com"]; !ok {
		t.Errorf("want route set once docker health flips to healthy")
	}
}

// TestApplyPlacesOnNode is the pivotal federation-placement test: an app
// declaring node = "web1" must have its container run on the node resolved
// for "web1", not on the local node, even though both are wired into the
// same Reconciler via a single FakeResolver.
func TestApplyPlacesOnNode(t *testing.T) {
	n0 := node.NewFake()
	n1 := node.NewFake()
	fr := router.NewFakeRouter()
	s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	resolver := node.FakeResolver{"local": n0, "web1": n1}
	r := New(resolver, fr, s, &fakeResolver{vals: map[string]string{}}, compose.NewFake(), source.NewFake(), build.NewFake())

	app := testApp()
	app.Node = "web1"
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	dep, err := r.Apply(context.Background(), app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if dep.Status != store.StatusRunning {
		t.Errorf("status = %q, want running", dep.Status)
	}

	if !contains(n1.Calls, "RunContainer:lwd-blog-1") {
		t.Errorf("want the container run on the resolved node (web1), calls: %v", n1.Calls)
	}
	for _, c := range n0.Calls {
		if strings.HasPrefix(c, "RunContainer:") {
			t.Errorf("want no RunContainer call on the local node when app.Node=web1, calls: %v", n0.Calls)
		}
	}

	// Remove must also resolve back to web1 (from the stored spec snapshot's
	// Node field), not the local node.
	if err := r.Remove(context.Background(), "blog"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !contains(n1.Calls, "RemoveContainer:"+dep.ContainerID) {
		t.Errorf("want Remove to remove the container on web1, calls: %v", n1.Calls)
	}
}

// TestApplyUnknownNodeRecordsFailed ensures a bad `node = "..."` fails the
// deploy closed (no container ever started) and is recorded in history,
// rather than silently falling back to the local node.
func TestApplyUnknownNodeRecordsFailed(t *testing.T) {
	r, f, _, s := newTestReconciler(t)
	app := testApp()
	app.Node = "ghost"

	_, err := r.Apply(context.Background(), app)
	if err == nil {
		t.Fatal("want error for an unregistered node")
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "RunContainer:") {
			t.Errorf("want no RunContainer call when the node can't be resolved, calls: %v", f.Calls)
		}
	}
	history, err := s.DeploymentsForApp(app.Name)
	if err != nil {
		t.Fatalf("DeploymentsForApp: %v", err)
	}
	var sawFailed bool
	for _, d := range history {
		if d.Status == store.StatusFailed {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Errorf("want a StatusFailed deployment recorded, history: %+v", history)
	}
}

func TestApplyRejectsInvalidSpec(t *testing.T) {
	r, f, fr, _ := newTestReconciler(t)
	_, err := r.Apply(context.Background(), &spec.App{Name: "x"}) // missing image/port
	if err == nil {
		t.Fatal("want validation error")
	}
	if len(f.Calls) != 0 {
		t.Errorf("want no node calls before validation, got %v", f.Calls)
	}
	if len(fr.Calls) != 0 {
		t.Errorf("want no router calls before validation, got %v", fr.Calls)
	}
}

func TestRollbackRedeploysPrevious(t *testing.T) {
	r, _, fr, s := newTestReconciler(t)
	ctx := context.Background()
	app := testApp()
	app.Image = "img:a"
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	v1, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("v1 Apply: %v", err)
	}

	app2 := testApp()
	app2.Image = "img:b"
	app2.Health.Timeout = shortTimeout
	v2, err := r.Apply(ctx, app2)
	if err != nil {
		t.Fatalf("v2 Apply: %v", err)
	}
	if v2.Image != "img:b" {
		t.Fatalf("sanity: v2.Image = %q, want img:b", v2.Image)
	}

	back, err := r.Rollback(ctx, "blog")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if back.Image != "img:a" {
		t.Errorf("Rollback image = %q, want img:a", back.Image)
	}
	if back.Status != store.StatusRunning {
		t.Errorf("Rollback status = %q, want running", back.Status)
	}
	if back.ContainerID == v1.ContainerID || back.ContainerID == v2.ContainerID {
		t.Errorf("Rollback should start a fresh container, got %q (v1=%q v2=%q)", back.ContainerID, v1.ContainerID, v2.ContainerID)
	}

	route, ok := fr.Routes["blog.example.com"]
	if !ok {
		t.Fatalf("Routes[blog.example.com] not set after rollback")
	}
	if route.Upstream != back.ContainerID && route.Upstream != containerName(app, 3) {
		t.Errorf("route.Upstream = %q, want it to point at the rolled-back container", route.Upstream)
	}

	cur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil || cur.ID != back.ID || cur.Image != "img:a" {
		t.Fatalf("want current deployment to be the rollback, got %+v", cur)
	}
}

func TestRollbackNoHistory(t *testing.T) {
	r, _, _, _ := newTestReconciler(t)
	_, err := r.Rollback(context.Background(), "blog")
	if err == nil {
		t.Fatal("want error when there is no previous deployment")
	}
}

func TestApplyInjectsSecrets(t *testing.T) {
	r, f, fr, _ := newTestReconcilerWithResolver(t, &fakeResolver{vals: map[string]string{"DB": "secret"}})
	app := testApp()
	app.Env = map[string]string{"A": "1"}
	app.Secrets = []string{"DB"}
	fr.ProbeStatus = 200

	_, err := r.Apply(context.Background(), app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	env := f.LastRunSpec.Env
	if env["A"] != "1" {
		t.Errorf("env[A] = %q, want 1", env["A"])
	}
	if env["DB"] != "secret" {
		t.Errorf("env[DB] = %q, want secret", env["DB"])
	}
}

func TestSecretOverridesEnv(t *testing.T) {
	r, f, fr, _ := newTestReconcilerWithResolver(t, &fakeResolver{vals: map[string]string{"K": "secret"}})
	app := testApp()
	app.Env = map[string]string{"K": "plain"}
	app.Secrets = []string{"K"}
	fr.ProbeStatus = 200

	_, err := r.Apply(context.Background(), app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := f.LastRunSpec.Env["K"]; got != "secret" {
		t.Errorf("env[K] = %q, want secret (secrets must win over plain env)", got)
	}
}

func TestApplyFailsClosedOnResolveError(t *testing.T) {
	r, f, _, s := newTestReconcilerWithResolver(t, &fakeResolver{err: fmt.Errorf("boom")})
	app := testApp()
	app.Secrets = []string{"DB"}

	_, err := r.Apply(context.Background(), app)
	if err == nil {
		t.Fatal("want error when secret resolution fails")
	}

	for _, c := range f.Calls {
		if strings.HasPrefix(c, "RunContainer:") {
			t.Errorf("want no RunContainer call when secrets fail closed, calls: %v", f.Calls)
		}
	}

	history, err := s.DeploymentsForApp(app.Name)
	if err != nil {
		t.Fatalf("DeploymentsForApp: %v", err)
	}
	var sawFailed bool
	for _, d := range history {
		if d.Status == store.StatusFailed {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Errorf("want a StatusFailed deployment recorded, history: %+v", history)
	}

	if cur, _ := s.CurrentDeployment("blog"); cur != nil {
		t.Errorf("want no running deployment, got %+v", cur)
	}
}

func TestApplyComposeUpConnectsRoutesVerifies(t *testing.T) {
	r, f, fr, s, cf, _, _ := newTestReconcilerFull(t, &fakeResolver{vals: map[string]string{"DB": "secretval"}})
	app := testComposeApp(t, "services:\n  web:\n    image: nginx\n")
	app.Health.Timeout = shortTimeout
	app.Env = map[string]string{"A": "1"}
	app.Secrets = []string{"DB"}
	cf.ServiceID = "cid-1"
	cf.ServiceName = "lwd-webapp-web-1"
	fr.ProbeStatus = 200

	dep, err := r.Apply(context.Background(), app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !containsInOrder(cf.Calls, "Up:lwd-webapp", "ServiceContainer:lwd-webapp:web") {
		t.Errorf("expected Up before ServiceContainer, calls: %v", cf.Calls)
	}
	if !contains(f.Calls, "ConnectContainerToNetwork:cid-1:lwd") {
		t.Errorf("expected ConnectContainerToNetwork:cid-1:lwd, calls: %v", f.Calls)
	}
	if !containsInOrder(fr.Calls, "SetRoute:webapp.example.com", "ProbeThroughCaddy:webapp.example.com") {
		t.Errorf("expected SetRoute before the health probe, calls: %v", fr.Calls)
	}

	if dep.Status != store.StatusRunning {
		t.Errorf("status = %q, want running", dep.Status)
	}
	if dep.Compose == "" {
		t.Errorf("want non-empty Compose snapshot")
	}
	if dep.ContainerID != "cid-1" {
		t.Errorf("ContainerID = %q, want cid-1", dep.ContainerID)
	}

	route, ok := fr.Routes["webapp.example.com"]
	if !ok {
		t.Fatalf("Routes[webapp.example.com] not set, routes: %+v", fr.Routes)
	}
	if route.Upstream != "lwd-webapp-web-1" {
		t.Errorf("route.Upstream = %q, want lwd-webapp-web-1", route.Upstream)
	}

	if cf.LastUp.Env["A"] != "1" {
		t.Errorf("LastUp.Env[A] = %q, want 1", cf.LastUp.Env["A"])
	}
	if cf.LastUp.Env["DB"] != "secretval" {
		t.Errorf("LastUp.Env[DB] = %q, want secretval", cf.LastUp.Env["DB"])
	}

	cur, _ := s.CurrentDeployment("webapp")
	if cur == nil || cur.ContainerID != "cid-1" {
		t.Errorf("CurrentDeployment mismatch: %+v", cur)
	}
}

func TestApplyComposeFailClosedSecret(t *testing.T) {
	r, _, _, s, cf, _, _ := newTestReconcilerFull(t, &fakeResolver{err: fmt.Errorf("boom")})
	app := testComposeApp(t, "services:\n  web:\n    image: nginx\n")
	app.Secrets = []string{"DB"}

	_, err := r.Apply(context.Background(), app)
	if err == nil {
		t.Fatal("want error when secret resolution fails")
	}

	if len(cf.Calls) != 0 {
		t.Errorf("want no compose calls when secrets fail closed, calls: %v", cf.Calls)
	}

	history, err := s.DeploymentsForApp(app.Name)
	if err != nil {
		t.Fatalf("DeploymentsForApp: %v", err)
	}
	var sawFailed bool
	for _, d := range history {
		if d.Status == store.StatusFailed {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Errorf("want a StatusFailed deployment recorded, history: %+v", history)
	}
}

func TestApplyComposeHealthFailRecordsFailed(t *testing.T) {
	r, _, fr, s, cf := newTestReconcilerWithCompose(t)
	app := testComposeApp(t, "services:\n  web:\n    image: nginx\n")
	app.Health.Timeout = shortTimeout
	cf.ServiceID = "cid-2"
	cf.ServiceName = "lwd-webapp-web-1"
	fr.ProbeStatus = 502

	_, err := r.Apply(context.Background(), app)
	if err == nil {
		t.Fatal("want error when the health probe never succeeds")
	}

	if contains(cf.Calls, "Down:lwd-webapp") {
		t.Errorf("compose down must never be called on a failed health check, calls: %v", cf.Calls)
	}
	// The stack is left live (in-place delegate model): the route set before
	// health-gating must remain, unlike blue-green's isolation guarantee.
	if _, ok := fr.Routes["webapp.example.com"]; !ok {
		t.Errorf("want the route to remain set (stack stays live) after a failed compose health check")
	}

	history, err := s.DeploymentsForApp(app.Name)
	if err != nil {
		t.Fatalf("DeploymentsForApp: %v", err)
	}
	var sawFailed bool
	for _, d := range history {
		if d.Status == store.StatusFailed && d.ContainerID == "cid-2" {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Errorf("want a StatusFailed deployment recorded, history: %+v", history)
	}
}

func TestRollbackComposeReappliesStored(t *testing.T) {
	r, _, fr, s, cf := newTestReconcilerWithCompose(t)
	fr.ProbeStatus = 200

	app1 := testComposeApp(t, "content-A")
	app1.Health.Timeout = shortTimeout
	cf.ServiceID = "cid-1"
	cf.ServiceName = "lwd-webapp-web-1"
	if _, err := r.Apply(context.Background(), app1); err != nil {
		t.Fatalf("v1 Apply: %v", err)
	}

	app2 := testComposeApp(t, "content-B")
	app2.Health.Timeout = shortTimeout
	cf.ServiceID = "cid-2"
	cf.ServiceName = "lwd-webapp-web-2"
	if _, err := r.Apply(context.Background(), app2); err != nil {
		t.Fatalf("v2 Apply: %v", err)
	}

	cf.ServiceID = "cid-3"
	cf.ServiceName = "lwd-webapp-web-3"
	back, err := r.Rollback(context.Background(), "webapp")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if back.Compose != "content-A" {
		t.Errorf("Rollback Compose = %q, want content-A", back.Compose)
	}
	if back.Status != store.StatusRunning {
		t.Errorf("Rollback status = %q, want running", back.Status)
	}

	cur, err := s.CurrentDeployment("webapp")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil || cur.Compose != "content-A" {
		t.Fatalf("want current deployment restored to content-A, got %+v", cur)
	}
}

func TestRemoveComposeCallsDown(t *testing.T) {
	r, _, fr, s, cf := newTestReconcilerWithCompose(t)
	app := testComposeApp(t, "services:\n  web:\n    image: nginx\n")
	app.Health.Timeout = shortTimeout
	cf.ServiceID = "cid-1"
	cf.ServiceName = "lwd-webapp-web-1"
	fr.ProbeStatus = 200

	if _, err := r.Apply(context.Background(), app); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if err := r.Remove(context.Background(), "webapp"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if !contains(cf.Calls, "Down:lwd-webapp") {
		t.Errorf("expected compose Down call, calls: %v", cf.Calls)
	}
	if _, ok := fr.Routes["webapp.example.com"]; ok {
		t.Errorf("want route removed after Remove")
	}
	cur, err := s.CurrentDeployment("webapp")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur != nil {
		t.Errorf("want no current deployment after Remove, got %+v", cur)
	}
}

func TestRemoveSingleServiceRemovesContainersAndRoute(t *testing.T) {
	r, f, fr, s := newTestReconciler(t)
	app := testApp()
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	dep, err := r.Apply(context.Background(), app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if err := r.Remove(context.Background(), "blog"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if !contains(f.Calls, "RemoveContainer:"+dep.ContainerID) {
		t.Errorf("expected container removed, calls: %v", f.Calls)
	}
	if _, ok := fr.Routes["blog.example.com"]; ok {
		t.Errorf("want route removed after Remove")
	}
	cur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur != nil {
		t.Errorf("want no current deployment after Remove, got %+v", cur)
	}
}

func TestApplyGitClonesBuildsDeploys(t *testing.T) {
	r, f, fr, _, sf, bf := newTestReconcilerWithGit(t)
	app := testGitApp()
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200
	sf.SHA = "deadbeefcafe0123456789abcdef0123456789"

	dep, err := r.Apply(context.Background(), app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	wantTag := "lwd-build/gitapp:deadbeefcafe"
	if dep.Image != wantTag {
		t.Errorf("Image = %q, want %q", dep.Image, wantTag)
	}

	if len(sf.Calls) != 1 {
		t.Fatalf("want exactly one Clone call, calls: %v", sf.Calls)
	}
	if sf.LastURL != app.Git.URL || sf.LastRef != app.Git.Ref {
		t.Errorf("Clone(url=%q, ref=%q), want (%q, %q)", sf.LastURL, sf.LastRef, app.Git.URL, app.Git.Ref)
	}

	if !contains(bf.Calls, "Build") {
		t.Errorf("want Build called, calls: %v", bf.Calls)
	}
	if bf.LastTag != wantTag {
		t.Errorf("Build tag = %q, want %q", bf.LastTag, wantTag)
	}
	if bf.LastDockerfile != "Dockerfile" {
		t.Errorf("Build dockerfile = %q, want Dockerfile", bf.LastDockerfile)
	}
	// The build context must be rooted at the just-cloned directory (proving
	// clone ran, and its result was used, before build).
	if bf.LastContext != sf.LastDir {
		t.Errorf("Build context = %q, want cloned dir %q", bf.LastContext, sf.LastDir)
	}

	if !contains(f.Calls, "RunContainer:lwd-gitapp-1") {
		t.Errorf("want surface container run, calls: %v", f.Calls)
	}
	if f.LastRunSpec.Image != wantTag {
		t.Errorf("RunContainer image = %q, want %q", f.LastRunSpec.Image, wantTag)
	}

	// The recorded deployment's ContainerID must be the SURFACE container
	// (the one that actually received the live route), not e.g. left empty
	// or pointed at some other id — this is what `lwd logs`/`lwd ls` rely on
	// to find the right container for a git-built app.
	if dep.ContainerID == "" {
		t.Fatal("dep.ContainerID is empty, want the surface container id")
	}
	surfaces, err := f.ListContainers(context.Background(), map[string]string{"lwd.app": "gitapp", "lwd.role": "surface"})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(surfaces) != 1 || surfaces[0].ID != dep.ContainerID {
		t.Errorf("dep.ContainerID = %q, want the sole surface container's id, found: %+v", dep.ContainerID, surfaces)
	}

	if !containsInOrder(fr.Calls, "SetStaging:stage-1.lwd.internal", "ProbeThroughCaddy:stage-1.lwd.internal") {
		t.Errorf("expected SetStaging before the health probe, calls: %v", fr.Calls)
	}
	if !containsInOrder(fr.Calls, "ProbeThroughCaddy:stage-1.lwd.internal", "SetRoute:gitapp.example.com") {
		t.Errorf("expected the health probe before SetRoute, calls: %v", fr.Calls)
	}
}

func TestApplyGitSkipsBuildIfImageExists(t *testing.T) {
	r, _, fr, _, sf, bf := newTestReconcilerWithGit(t)
	app := testGitApp()
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200
	sf.SHA = "cafebabecafe0123456789abcdef0123456789"

	wantTag := "lwd-build/gitapp:" + shortSHA(sf.SHA)
	bf.Exists = map[string]bool{wantTag: true}

	dep, err := r.Apply(context.Background(), app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if contains(bf.Calls, "Build") {
		t.Errorf("want no Build call when the image already exists, calls: %v", bf.Calls)
	}
	if !contains(bf.Calls, "ImageExists") {
		t.Errorf("want ImageExists checked, calls: %v", bf.Calls)
	}
	if dep.Image != wantTag {
		t.Errorf("Image = %q, want %q", dep.Image, wantTag)
	}
}

func TestApplyGitWithBacking(t *testing.T) {
	r, f, fr, _, cf, _, _ := newTestReconcilerFull(t, &fakeResolver{vals: map[string]string{"DB_PASS": "s3cr3t"}})
	app := testGitApp()
	app.Health.Timeout = shortTimeout
	app.Secrets = []string{"DB_PASS"}
	app.Services = []spec.Service{
		{Name: "db", Image: "postgres:16", Secrets: []string{"DB_PASS"}},
	}
	fr.ProbeStatus = 200

	dep, err := r.Apply(context.Background(), app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !contains(cf.Calls, "Up:lwd-gitapp") {
		t.Errorf("want backing compose Up for project lwd-gitapp, calls: %v", cf.Calls)
	}
	// The persisted compose snapshot must never contain the resolved secret
	// value: it is stored verbatim, plaintext, in store.Deployment.Compose
	// and served back over the API, so a leaked value there defeats
	// encryption-at-rest for secrets. It should instead hold a ${...}
	// compose-interpolation reference.
	if strings.Contains(dep.Compose, "s3cr3t") {
		t.Errorf("want rendered backing compose to NOT include the resolved secret value, got %q", dep.Compose)
	}
	if !strings.Contains(dep.Compose, `"${DB_PASS}"`) {
		t.Errorf("want rendered backing compose to include a ${DB_PASS} reference, got %q", dep.Compose)
	}
	// The resolved value must still reach the compose-up process env, so
	// docker compose's own interpolation can resolve the ${DB_PASS} ref.
	if cf.LastUp.Env["DB_PASS"] != "s3cr3t" {
		t.Errorf("want LastUp.Env[DB_PASS] = s3cr3t (resolved value handed to compose up, never persisted), got %q", cf.LastUp.Env["DB_PASS"])
	}

	if !containsInOrder(f.Calls, "EnsureNetwork:lwd-gitapp", "RunContainer:lwd-gitapp-1") {
		t.Errorf("want the backing network ensured before the surface starts, calls: %v", f.Calls)
	}
	if !containsInOrder(f.Calls, "RunContainer:lwd-gitapp-1", "ConnectContainerToNetwork:"+dep.ContainerID+":lwd-gitapp") {
		t.Errorf("want the surface connected to the backing network after it starts, calls: %v", f.Calls)
	}
}

func TestApplyGitFailClosedSecret(t *testing.T) {
	r, f, _, s, sf, bf := newTestReconcilerWithGitAndResolver(t, &fakeResolver{err: fmt.Errorf("boom")})
	app := testGitApp()
	app.Secrets = []string{"DB"}

	_, err := r.Apply(context.Background(), app)
	if err == nil {
		t.Fatal("want error when secret resolution fails")
	}

	if len(sf.Calls) != 0 {
		t.Errorf("want no Clone call when secrets fail closed, calls: %v", sf.Calls)
	}
	if len(bf.Calls) != 0 {
		t.Errorf("want no Build/ImageExists call when secrets fail closed, calls: %v", bf.Calls)
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "RunContainer:") {
			t.Errorf("want no RunContainer call when secrets fail closed, calls: %v", f.Calls)
		}
	}

	history, err := s.DeploymentsForApp(app.Name)
	if err != nil {
		t.Fatalf("DeploymentsForApp: %v", err)
	}
	var sawFailed bool
	for _, d := range history {
		if d.Status == store.StatusFailed {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Errorf("want a StatusFailed deployment recorded, history: %+v", history)
	}
}

func TestApplyImageAppWithBacking(t *testing.T) {
	r, f, fr, _, cf, _, _ := newTestReconcilerFull(t, &fakeResolver{vals: map[string]string{}})
	app := testApp()
	app.Services = []spec.Service{{Name: "db", Image: "postgres:16"}}
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	dep, err := r.Apply(context.Background(), app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !contains(cf.Calls, "Up:lwd-blog") {
		t.Errorf("want backing compose Up for project lwd-blog, calls: %v", cf.Calls)
	}
	if !contains(f.Calls, "ConnectContainerToNetwork:"+dep.ContainerID+":lwd-blog") {
		t.Errorf("want the surface connected to the backing network, calls: %v", f.Calls)
	}
}

// TestRollbackImageAppWithBacking covers the rollback compose-vs-backing
// guard for a plain image app that declares backing [[service]]s: Rollback
// must redeploy it through the image/backing surface path (a fresh
// RunContainer reconnected to the backing network, with backing compose
// idempotently re-ensured via Up), and must NOT misroute it into the Phase-4
// applyCompose branch. That branch is guarded on the app spec's own Compose
// field, which is always "" for a backing app (Compose is only ever set for
// a Phase-4 [compose] app), so it must never be taken here — in particular,
// compose.Down (part of the Phase-4 compose lifecycle) must never be called.
func TestRollbackImageAppWithBacking(t *testing.T) {
	r, f, fr, s, cf, _, _ := newTestReconcilerFull(t, &fakeResolver{vals: map[string]string{}})
	ctx := context.Background()

	app1 := testApp()
	app1.Image = "img:a"
	app1.Services = []spec.Service{{Name: "db", Image: "postgres:16"}}
	app1.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	v1, err := r.Apply(ctx, app1)
	if err != nil {
		t.Fatalf("v1 Apply: %v", err)
	}

	app2 := testApp()
	app2.Image = "img:b"
	app2.Services = []spec.Service{{Name: "db", Image: "postgres:16"}}
	app2.Health.Timeout = shortTimeout
	v2, err := r.Apply(ctx, app2)
	if err != nil {
		t.Fatalf("v2 Apply: %v", err)
	}
	if v2.Image != "img:b" {
		t.Fatalf("sanity: v2.Image = %q, want img:b", v2.Image)
	}

	back, err := r.Rollback(ctx, "blog")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if back.Image != "img:a" {
		t.Errorf("Rollback image = %q, want img:a", back.Image)
	}
	if back.Status != store.StatusRunning {
		t.Errorf("Rollback status = %q, want running", back.Status)
	}
	if back.ContainerID == v1.ContainerID || back.ContainerID == v2.ContainerID {
		t.Errorf("Rollback should start a fresh surface container, got %q (v1=%q v2=%q)", back.ContainerID, v1.ContainerID, v2.ContainerID)
	}

	// Redeployed via the image/backing surface path: a fresh RunContainer,
	// reconnected to the backing network.
	if !contains(f.Calls, "RunContainer:"+containerName(app1, 3)) {
		t.Errorf("want rollback to start a fresh surface container via RunContainer, calls: %v", f.Calls)
	}
	if !contains(f.Calls, "ConnectContainerToNetwork:"+back.ContainerID+":lwd-blog") {
		t.Errorf("want rollback surface reconnected to the backing network, calls: %v", f.Calls)
	}

	// Backing compose re-ensured (idempotent Up) on rollback too...
	if !contains(cf.Calls, "Up:lwd-blog") {
		t.Errorf("want backing compose re-ensured on rollback, calls: %v", cf.Calls)
	}
	// ...but never a Down: that would mean Rollback took the Phase-4
	// applyCompose branch instead of the image/backing surface path.
	for _, c := range cf.Calls {
		if strings.HasPrefix(c, "Down:") {
			t.Errorf("want no compose Down call on rollback of an image-with-backing app, calls: %v", cf.Calls)
		}
	}

	cur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil || cur.ID != back.ID || cur.Image != "img:a" {
		t.Fatalf("want current deployment to be the rollback, got %+v", cur)
	}
}

func TestRollbackGitRedeploysPriorTag(t *testing.T) {
	r, _, fr, s, sf, bf := newTestReconcilerWithGit(t)
	ctx := context.Background()
	app := testGitApp()
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	sf.SHA = "1111111111111111111111111111111111111a"
	v1, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("v1 Apply: %v", err)
	}

	sf.SHA = "2222222222222222222222222222222222222b"
	v2, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("v2 Apply: %v", err)
	}
	if v1.Image == v2.Image {
		t.Fatalf("sanity: v1/v2 should have built different tags, both = %q", v1.Image)
	}

	preCloneCalls := len(sf.Calls)
	preBuildCalls := len(bf.Calls)

	back, err := r.Rollback(ctx, "gitapp")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if back.Image != v1.Image {
		t.Errorf("Rollback image = %q, want %q (the prior tag)", back.Image, v1.Image)
	}
	if len(sf.Calls) != preCloneCalls {
		t.Errorf("want no additional Clone call on rollback, calls: %v", sf.Calls)
	}
	if len(bf.Calls) != preBuildCalls {
		t.Errorf("want no additional Build/ImageExists call on rollback, calls: %v", bf.Calls)
	}
	if back.Status != store.StatusRunning {
		t.Errorf("status = %q, want running", back.Status)
	}

	cur, err := s.CurrentDeployment("gitapp")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil || cur.Image != v1.Image {
		t.Fatalf("want current deployment restored to %q, got %+v", v1.Image, cur)
	}
}

func TestRemoveGitDownsBacking(t *testing.T) {
	r, f, fr, s, cf, _, _ := newTestReconcilerFull(t, &fakeResolver{vals: map[string]string{}})
	app := testGitApp()
	app.Health.Timeout = shortTimeout
	app.Services = []spec.Service{{Name: "db", Image: "postgres:16"}}
	fr.ProbeStatus = 200

	dep, err := r.Apply(context.Background(), app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if err := r.Remove(context.Background(), "gitapp"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if !contains(cf.Calls, "Down:lwd-gitapp") {
		t.Errorf("want backing compose Down for project lwd-gitapp, calls: %v", cf.Calls)
	}
	if !contains(f.Calls, "RemoveContainer:"+dep.ContainerID) {
		t.Errorf("want the surface container removed, calls: %v", f.Calls)
	}
	if _, ok := fr.Routes["gitapp.example.com"]; ok {
		t.Errorf("want route removed after Remove")
	}

	cur, err := s.CurrentDeployment("gitapp")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur != nil {
		t.Errorf("want no current deployment after Remove, got %+v", cur)
	}
}

// TestEnsureImageTransfersWhenAbsent covers the Phase 9a-Task 4 image
// transfer path used before running a surface on a non-local node:
// ensureImageOnNode should transfer (save on the local node, load on the
// target) only when the image is absent AND a registry pull on the target
// doesn't produce it; it must not transfer when the target already has it,
// and it must hard-fail (no transfer, no run) when ImagePresent itself
// errors on the target.
func TestEnsureImageTransfersWhenAbsent(t *testing.T) {
	t.Run("absent after pull triggers save|load transfer", func(t *testing.T) {
		local := node.NewFake()
		remote := node.NewFake()
		fr := router.NewFakeRouter()
		s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		resolver := node.FakeResolver{"local": local, "web1": remote}
		r := New(resolver, fr, s, &fakeResolver{vals: map[string]string{}}, compose.NewFake(), source.NewFake(), build.NewFake())

		app := testApp() // Image: "img:1"
		app.Node = "web1"
		app.Health.Timeout = shortTimeout
		fr.ProbeStatus = 200

		// remote.Images has no entry for "img:1", and Fake.EnsureImage never
		// marks an image present (it only records the call), so the
		// "registry pull on the target" step leaves it absent and the
		// transfer path must run.
		dep, err := r.Apply(context.Background(), app)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if dep.Status != store.StatusRunning {
			t.Errorf("status = %q, want running", dep.Status)
		}
		if !contains(local.Calls, "SaveImage:img:1") {
			t.Errorf("want SaveImage on the local node, calls: %v", local.Calls)
		}
		if !contains(remote.Calls, "LoadImage") {
			t.Errorf("want LoadImage on the target node, calls: %v", remote.Calls)
		}
		if !remote.Images["img:1"] {
			t.Errorf("want img:1 present on the target node after transfer")
		}
	})

	t.Run("already present on target skips transfer", func(t *testing.T) {
		local := node.NewFake()
		remote := node.NewFake()
		remote.Images = map[string]bool{"img:1": true}
		fr := router.NewFakeRouter()
		s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		resolver := node.FakeResolver{"local": local, "web1": remote}
		r := New(resolver, fr, s, &fakeResolver{vals: map[string]string{}}, compose.NewFake(), source.NewFake(), build.NewFake())

		app := testApp()
		app.Node = "web1"
		app.Health.Timeout = shortTimeout
		fr.ProbeStatus = 200

		dep, err := r.Apply(context.Background(), app)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if dep.Status != store.StatusRunning {
			t.Errorf("status = %q, want running", dep.Status)
		}
		for _, c := range local.Calls {
			if strings.HasPrefix(c, "SaveImage:") {
				t.Errorf("want no SaveImage call when already present on target, calls: %v", local.Calls)
			}
		}
		if contains(remote.Calls, "LoadImage") {
			t.Errorf("want no LoadImage call when already present on target, calls: %v", remote.Calls)
		}
	})

	t.Run("ImagePresent error fails deploy closed, no transfer, no run", func(t *testing.T) {
		local := node.NewFake()
		remote := node.NewFake()
		remote.ImagePresentErr = fmt.Errorf("docker unreachable")
		fr := router.NewFakeRouter()
		s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		resolver := node.FakeResolver{"local": local, "web1": remote}
		r := New(resolver, fr, s, &fakeResolver{vals: map[string]string{}}, compose.NewFake(), source.NewFake(), build.NewFake())

		app := testApp()
		app.Node = "web1"
		app.Health.Timeout = shortTimeout
		fr.ProbeStatus = 200

		if _, err := r.Apply(context.Background(), app); err == nil {
			t.Fatal("want Apply to fail when ImagePresent errors on the target")
		}
		for _, c := range remote.Calls {
			if strings.HasPrefix(c, "RunContainer:") {
				t.Errorf("want no RunContainer call when image presence can't be determined, calls: %v", remote.Calls)
			}
		}
		if contains(remote.Calls, "LoadImage") {
			t.Errorf("want no LoadImage call, calls: %v", remote.Calls)
		}
		for _, c := range local.Calls {
			if strings.HasPrefix(c, "SaveImage:") {
				t.Errorf("want no SaveImage call, calls: %v", local.Calls)
			}
		}
	})
}

// TestRemoteSurfaceRoutesToMeshAddr covers the Phase 9a-Task 4 mesh-address
// routing: a surface placed on a registered remote node must publish an
// ephemeral host port bound to that node's mesh address, and the live Caddy
// route must point at meshAddr:<published port> — never at the container
// name, which Caddy (running on the controller) cannot resolve on another
// node's Docker network. A local surface must be completely unaffected: no
// published port, and the route still targets the container name.
func TestRemoteSurfaceRoutesToMeshAddr(t *testing.T) {
	n0 := node.NewFake() // local
	n1 := node.NewFake() // web1
	n1.MeshAddr = "100.64.0.2"
	n1.Images = map[string]bool{"img:1": true} // skip the transfer path; routing is what's under test
	fr := router.NewFakeRouter()
	s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	resolver := node.FakeResolver{"local": n0, "web1": n1}
	r := New(resolver, fr, s, &fakeResolver{vals: map[string]string{}}, compose.NewFake(), source.NewFake(), build.NewFake())

	app := testApp()
	app.Node = "web1"
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	dep, err := r.Apply(context.Background(), app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if dep.Status != store.StatusRunning {
		t.Errorf("status = %q, want running", dep.Status)
	}

	if len(n1.LastRunSpec.Publish) != 1 {
		t.Fatalf("want exactly one published port for a remote surface, got %+v", n1.LastRunSpec.Publish)
	}
	pub := n1.LastRunSpec.Publish[0]
	if pub.HostIP != "100.64.0.2" {
		t.Errorf("published HostIP = %q, want the node's mesh address", pub.HostIP)
	}
	if pub.ContainerPort != app.Port {
		t.Errorf("published ContainerPort = %d, want %d", pub.ContainerPort, app.Port)
	}

	containers, err := n1.ListContainers(context.Background(), map[string]string{"lwd.app": app.Name})
	if err != nil || len(containers) != 1 {
		t.Fatalf("ListContainers on web1: %v, %+v", err, containers)
	}
	wantPort := containers[0].HostPort
	if wantPort == 0 {
		t.Fatal("want a non-zero published host port assigned")
	}

	route, ok := fr.Routes[app.Domain]
	if !ok {
		t.Fatalf("want a live route for %s", app.Domain)
	}
	if route.Upstream != "100.64.0.2" {
		t.Errorf("route.Upstream = %q, want the node's mesh address", route.Upstream)
	}
	if route.Port != wantPort {
		t.Errorf("route.Port = %d, want the published host port %d", route.Port, wantPort)
	}

	// A local app is completely unaffected: no publish, and the route still
	// targets the container name.
	localApp := testApp()
	localApp.Name = "localblog"
	localApp.Domain = "localblog.example.com"
	localApp.Health.Timeout = shortTimeout

	localDep, err := r.Apply(context.Background(), localApp)
	if err != nil {
		t.Fatalf("Apply (local): %v", err)
	}
	if len(n0.LastRunSpec.Publish) != 0 {
		t.Errorf("want no published ports for a local surface, got %+v", n0.LastRunSpec.Publish)
	}
	localRoute, ok := fr.Routes[localApp.Domain]
	if !ok {
		t.Fatalf("want a live route for %s", localApp.Domain)
	}
	if localRoute.Upstream != containerName(localApp, localDep.ID) {
		t.Errorf("route.Upstream = %q, want the surface container name", localRoute.Upstream)
	}
	if localRoute.Port != localApp.Port {
		t.Errorf("route.Port = %d, want the app's declared port %d", localRoute.Port, localApp.Port)
	}
}

// TestBackingRunsOnRemoteNode covers the Phase 9a-Task 6 fix: an app placed
// on a remote node (app.Node = "web1") that declares [[services]] must have
// its backing compose project brought up (and, on Remove, torn down) against
// that node's own Docker daemon — via DOCKER_HOST=ssh://<sshHost> in the
// process env `docker compose` runs with — never the controller's local
// Docker. Before this fix, ensureBacking/downBacking passed only the app's
// resolved env/secrets, so DOCKER_HOST was never set and the backing project
// (and a stray lwd-<app> network) landed on the controller instead of the
// remote node. A local app's backing must see no DOCKER_HOST at all.
func TestBackingRunsOnRemoteNode(t *testing.T) {
	local := node.NewFake()
	remote := node.NewFake() // web1
	remote.MeshAddr = "100.64.0.2"
	remote.DockerHost = "ssh://deploy@web1"
	remote.Images = map[string]bool{"img:1": true} // skip the image-transfer path; backing routing is what's under test
	fr := router.NewFakeRouter()
	cf := compose.NewFake()
	s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	resolver := node.FakeResolver{"local": local, "web1": remote}
	r := New(resolver, fr, s, &fakeResolver{vals: map[string]string{}}, cf, source.NewFake(), build.NewFake())

	app := testApp()
	app.Node = "web1"
	app.Services = []spec.Service{{Name: "db", Image: "postgres:16"}}
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	if _, err := r.Apply(context.Background(), app); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !contains(cf.Calls, "Up:lwd-blog") {
		t.Fatalf("want backing compose Up for project lwd-blog, calls: %v", cf.Calls)
	}
	if cf.LastUp.Env["DOCKER_HOST"] != "ssh://deploy@web1" {
		t.Errorf("LastUp.Env[DOCKER_HOST] = %q, want ssh://deploy@web1", cf.LastUp.Env["DOCKER_HOST"])
	}
	// The backing network is ensured on the remote node itself, not local.
	if !contains(remote.Calls, "EnsureNetwork:lwd-blog") {
		t.Errorf("want the backing network ensured on the remote node, calls: %v", remote.Calls)
	}
	for _, c := range local.Calls {
		if c == "EnsureNetwork:lwd-blog" {
			t.Errorf("want no stray backing network ensured on the local/controller node, calls: %v", local.Calls)
		}
	}

	if err := r.Remove(context.Background(), app.Name); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !contains(cf.Calls, "Down:lwd-blog") {
		t.Fatalf("want backing compose Down for project lwd-blog, calls: %v", cf.Calls)
	}
	if cf.LastDown.Env["DOCKER_HOST"] != "ssh://deploy@web1" {
		t.Errorf("LastDown.Env[DOCKER_HOST] = %q, want ssh://deploy@web1", cf.LastDown.Env["DOCKER_HOST"])
	}

	// A local app's backing must see no DOCKER_HOST at all.
	localApp := testApp()
	localApp.Name = "locblog"
	localApp.Domain = "locblog.example.com"
	localApp.Services = []spec.Service{{Name: "db", Image: "postgres:16"}}
	localApp.Health.Timeout = shortTimeout

	if _, err := r.Apply(context.Background(), localApp); err != nil {
		t.Fatalf("Apply (local): %v", err)
	}
	if _, ok := cf.LastUp.Env["DOCKER_HOST"]; ok {
		t.Errorf("want no DOCKER_HOST for a local app's backing Up, Env: %v", cf.LastUp.Env)
	}

	if err := r.Remove(context.Background(), localApp.Name); err != nil {
		t.Fatalf("Remove (local): %v", err)
	}
	if cf.LastDown.Env != nil {
		t.Errorf("want no env (no DOCKER_HOST) for a local app's backing Down, Env: %v", cf.LastDown.Env)
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

func containsInOrder(xs []string, a, b string) bool {
	ai, bi := -1, -1
	for i, x := range xs {
		if x == a && ai == -1 {
			ai = i
		}
		if x == b {
			bi = i
		}
	}
	return ai != -1 && bi != -1 && ai < bi
}
