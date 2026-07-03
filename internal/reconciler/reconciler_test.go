package reconciler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"lwd/internal/node"
	"lwd/internal/router"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// shortTimeout keeps failure-path tests (which must poll to a deadline) fast.
const shortTimeout = 150 * time.Millisecond

func newTestReconciler(t *testing.T) (*Reconciler, *node.Fake, *router.FakeRouter, *store.Store) {
	t.Helper()
	f := node.NewFake()
	fr := router.NewFakeRouter()
	s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return New(f, fr, s), f, fr, s
}

func testApp() *spec.App {
	return &spec.App{Name: "blog", Image: "img:1", Domain: "blog.example.com", Port: 8080, Node: "local"}
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

	if !containsInOrder(f.Calls, "RunContainer:lwd-blog-1", "RunContainer:lwd-blog-1") {
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
	r := New(f, fr, s)

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
	r := New(f, fr, s)

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
	return ai != -1 && bi != -1 && ai <= bi
}
