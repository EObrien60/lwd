package reconciler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lwd/internal/compose"
	"lwd/internal/node"
	"lwd/internal/router"
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
	r, f, fr, s, _ := newTestReconcilerFull(t, sec)
	return r, f, fr, s
}

// newTestReconcilerWithCompose is like newTestReconciler but also returns the
// compose.Fake, for tests that exercise the compose deploy path and need to
// assert on / configure its calls (Up/ServiceContainer/Down).
func newTestReconcilerWithCompose(t *testing.T) (*Reconciler, *node.Fake, *router.FakeRouter, *store.Store, *compose.Fake) {
	t.Helper()
	return newTestReconcilerFull(t, &fakeResolver{vals: map[string]string{}})
}

// newTestReconcilerFull builds a Reconciler wired to fresh fakes for every
// dependency (node, router, compose) plus a temp-file store, using sec as the
// secret resolver. It is the single place all the other constructors above
// funnel through, so New's dependency list only has to be listed once here.
func newTestReconcilerFull(t *testing.T, sec SecretResolver) (*Reconciler, *node.Fake, *router.FakeRouter, *store.Store, *compose.Fake) {
	t.Helper()
	f := node.NewFake()
	fr := router.NewFakeRouter()
	cf := compose.NewFake()
	s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return New(f, fr, s, sec, cf), f, fr, s, cf
}

func testApp() *spec.App {
	return &spec.App{Name: "blog", Image: "img:1", Domain: "blog.example.com", Port: 8080, Node: "local"}
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
	r := New(f, fr, s, &fakeResolver{vals: map[string]string{}}, compose.NewFake())

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
	r := New(f, fr, s, &fakeResolver{vals: map[string]string{}}, compose.NewFake())

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
	r := New(f, fr, s, &fakeResolver{vals: map[string]string{}}, compose.NewFake())

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
	r, f, fr, s, cf := newTestReconcilerFull(t, &fakeResolver{vals: map[string]string{"DB": "secretval"}})
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
	r, _, _, s, cf := newTestReconcilerFull(t, &fakeResolver{err: fmt.Errorf("boom")})
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
