package reconciler

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"lwd/internal/node"
	"lwd/internal/spec"
	"lwd/internal/store"
)

func newTestReconciler(t *testing.T) (*Reconciler, *node.Fake, *store.Store) {
	t.Helper()
	f := node.NewFake()
	s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return New(f, s), f, s
}

func testApp() *spec.App {
	return &spec.App{Name: "blog", Image: "img:1", Port: 8080, Node: "local"}
}

func TestApplyStartsContainerAndRecords(t *testing.T) {
	r, f, s := newTestReconciler(t)
	dep, err := r.Apply(context.Background(), testApp())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if dep.Status != store.StatusRunning {
		t.Errorf("status = %q, want running", dep.Status)
	}
	cur, _ := s.CurrentDeployment("blog")
	if cur == nil || cur.ContainerID != dep.ContainerID {
		t.Errorf("CurrentDeployment mismatch: %+v", cur)
	}
	// Image must be ensured before the container runs.
	if !containsInOrder(f.Calls, "EnsureImage:img:1", "RunContainer:lwd-blog") {
		t.Errorf("call order wrong: %v", f.Calls)
	}
}

func TestApplyFailsWhenUnhealthy(t *testing.T) {
	r, f, s := newTestReconciler(t)
	f.HealthErr = errors.New("unhealthy")
	_, err := r.Apply(context.Background(), testApp())
	if err == nil {
		t.Fatal("want error when health fails")
	}
	// New container must be removed on health failure.
	if !contains(f.Calls, "RemoveContainer:fake-1") {
		t.Errorf("expected new container removed, calls: %v", f.Calls)
	}
	// No running deployment should remain.
	if cur, _ := s.CurrentDeployment("blog"); cur != nil {
		t.Errorf("want no running deployment, got %+v", cur)
	}
}

func TestApplyRecreatesRetiringOld(t *testing.T) {
	r, f, _ := newTestReconciler(t)
	ctx := context.Background()
	first, _ := r.Apply(ctx, testApp())
	second, err := r.Apply(ctx, testApp())
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if first.ContainerID == second.ContainerID {
		t.Fatal("expected a new container on redeploy")
	}
	// The first container should have been removed during the second apply.
	if !contains(f.Calls, "RemoveContainer:"+first.ContainerID) {
		t.Errorf("expected old container removed, calls: %v", f.Calls)
	}
}

func TestApplyRedeployFailureLeavesNoRunning(t *testing.T) {
	r, f, s := newTestReconciler(t)
	ctx := context.Background()
	if _, err := r.Apply(ctx, testApp()); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	f.HealthErr = errors.New("unhealthy")
	if _, err := r.Apply(ctx, testApp()); err == nil {
		t.Fatal("want error on failed redeploy")
	}
	if cur, _ := s.CurrentDeployment("blog"); cur != nil {
		t.Fatalf("want no running deployment after failed redeploy, got %+v", cur)
	}
}

func TestApplyRejectsInvalidSpec(t *testing.T) {
	r, _, _ := newTestReconciler(t)
	_, err := r.Apply(context.Background(), &spec.App{Name: "x"}) // missing image/port
	if err == nil {
		t.Fatal("want validation error")
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
