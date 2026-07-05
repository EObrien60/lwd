package reconciler

import (
	"context"
	"testing"
	"time"

	"lwd/internal/node"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// TestFailoverAfterGrace covers Phase 11b Task 5's core path: a registered
// node hosting a scheduler-placed surface goes unreachable, and once
// unreachableSince (seeded directly here to simulate an elapsed grace period
// deterministically, without sleeping) is past config.FailoverGrace(), a
// Reconcile pass evacuates its scheduled surface to a healthy node — exactly
// like a manual EvacuateNode call — and clears the timer so it doesn't
// re-fire every subsequent pass.
func TestFailoverAfterGrace(t *testing.T) {
	r, resolver, localFake, fr, s := newSchedulingReconciler(t, "web1", "web2")
	ctx := context.Background()
	fr.ProbeStatus = 200

	// Starve local so it never fits; web1 the initial pick, web2 the only
	// other node still big enough once web1 is excluded.
	localFake.Cap = node.Capacity{Known: true, CPUCores: 1, MemAvailable: 1}
	web1 := resolver["web1"].(*node.Fake)
	web2 := resolver["web2"].(*node.Fake)
	web1.Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 9000}
	web2.Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 3000}

	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}
	if err := s.AddNode(store.Node{Name: "web2", SSHHost: "deploy@web2", MeshAddr: "100.64.0.3", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web2: %v", err)
	}

	reach := newFakeReach()
	r.SetReachability(reach)

	app := unpinnedApp("blog")
	app.Requirements = &spec.Requirements{Memory: "2000"}
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout

	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("initial Apply: %v", err)
	}
	if got := specNode(t, dep.Spec); got != "web1" {
		t.Fatalf("setup: initial placement = %q, want web1", got)
	}
	cur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}

	// Simulate node loss, already past the (default 60s) grace period —
	// seeded directly rather than sleeping.
	reach.Set("web1", "ssh", false)
	r.mu.Lock()
	r.unreachableSince["web1"] = time.Now().Add(-2 * time.Minute)
	r.mu.Unlock()

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	newCur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment after Reconcile: %v", err)
	}
	if newCur == nil || newCur.ID == cur.ID {
		t.Fatalf("want blog moved to a new deployment row after failover, got %+v", newCur)
	}
	if got := specNode(t, newCur.Spec); got != "web2" {
		t.Errorf("failover moved to %q, want web2", got)
	}
	if !newCur.Scheduled {
		t.Errorf("failed-over deployment Scheduled = false, want true")
	}

	oldRow := depByID(t, s, "blog", cur.ID)
	if oldRow == nil || oldRow.Status != store.StatusRetired {
		t.Errorf("old deployment %+v, want status retired", oldRow)
	}

	r.mu.Lock()
	_, stillTracked := r.unreachableSince["web1"]
	r.mu.Unlock()
	if stillTracked {
		t.Errorf("want unreachableSince[\"web1\"] cleared after failover, so it doesn't re-fire every pass")
	}
}

// TestFailoverWithinGraceNoop covers the grace-gating itself: the FIRST pass
// that observes a node unreachable must only start the timer (no move yet) —
// unreachableSince didn't exist before this pass, so it can't possibly be
// past config.FailoverGrace() yet.
func TestFailoverWithinGraceNoop(t *testing.T) {
	r, resolver, localFake, fr, s := newSchedulingReconciler(t, "web1", "web2")
	ctx := context.Background()
	fr.ProbeStatus = 200

	localFake.Cap = node.Capacity{Known: true, CPUCores: 1, MemAvailable: 1}
	web1 := resolver["web1"].(*node.Fake)
	web2 := resolver["web2"].(*node.Fake)
	web1.Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 9000}
	web2.Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 3000}

	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}
	if err := s.AddNode(store.Node{Name: "web2", SSHHost: "deploy@web2", MeshAddr: "100.64.0.3", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web2: %v", err)
	}

	reach := newFakeReach()
	r.SetReachability(reach)

	app := unpinnedApp("blog")
	app.Requirements = &spec.Requirements{Memory: "2000"}
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout

	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("initial Apply: %v", err)
	}
	cur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}

	reach.Set("web1", "ssh", false)

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	after, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment after Reconcile: %v", err)
	}
	if after == nil || after.ID != cur.ID {
		t.Errorf("want no failover before grace elapses, got %+v (was %+v)", after, cur)
	}
	if after != nil && after.ContainerID != dep.ContainerID {
		t.Errorf("want original container %q still recorded, got %q", dep.ContainerID, after.ContainerID)
	}

	r.mu.Lock()
	since, tracked := r.unreachableSince["web1"]
	r.mu.Unlock()
	if !tracked {
		t.Fatalf("want unreachableSince[\"web1\"] set after the first unreachable pass")
	}
	if time.Since(since) > 5*time.Second {
		t.Errorf("unreachableSince[\"web1\"] = %v, want just now", since)
	}
}

// TestFailoverNeverLocal covers the hard "never local" guarantee:
// failoverLostNodes iterates store.ListNodes() (which never returns "local")
// and must never probe or evacuate it, even if "local" were somehow seeded
// into unreachableSince as long overdue.
func TestFailoverNeverLocal(t *testing.T) {
	r, _, _, _ := newTestReconciler(t)
	ctx := context.Background()

	reach := newFakeReach()
	reach.Set("local", "local", false)
	r.SetReachability(reach)

	r.mu.Lock()
	r.unreachableSince["local"] = time.Now().Add(-24 * time.Hour)
	r.mu.Unlock()

	r.failoverLostNodes(ctx)

	if contains(reach.Calls, "local") {
		t.Errorf("failoverLostNodes probed \"local\", want it skipped entirely: calls=%v", reach.Calls)
	}
	r.mu.Lock()
	_, stillSeeded := r.unreachableSince["local"]
	r.mu.Unlock()
	if !stillSeeded {
		t.Errorf("want unreachableSince[\"local\"] left untouched (never visited)")
	}
}

// TestFailoverLeavesPinned covers that a surface explicitly pinned to a
// now-unreachable node (Scheduled == false) is left running, reported
// degraded, and never moved by automatic failover — EvacuateNode's own
// pinned-skip logic applies exactly the same way here as it does to a manual
// call.
func TestFailoverLeavesPinned(t *testing.T) {
	r, resolver, _, fr, s := newSchedulingReconciler(t, "web1")
	ctx := context.Background()
	fr.ProbeStatus = 200
	resolver["web1"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 9000}

	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}

	reach := newFakeReach()
	r.SetReachability(reach)

	app := unpinnedApp("pinned")
	app.Node = "web1" // explicit pin
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout

	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	reach.Set("web1", "ssh", false)
	r.mu.Lock()
	r.unreachableSince["web1"] = time.Now().Add(-2 * time.Minute)
	r.mu.Unlock()

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	after, err := s.CurrentDeployment("pinned")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if after == nil || after.ID != dep.ID || after.Status != store.StatusRunning {
		t.Errorf("want pinned deployment untouched by failover, got %+v", after)
	}

	snap := r.HealthSnapshot()
	ah := findAppHealth(t, snap, "pinned")
	if ah.State != SurfaceDegraded {
		t.Errorf("pinned app on unreachable node: state = %q, want degraded", ah.State)
	}
}

// TestFailoverRecoveryClearsTimer covers recovery: once a node reports
// reachable again, its unreachableSince entry must be cleared immediately —
// even if it had accumulated a long-overdue value — so a later blip starts a
// fresh grace window rather than firing instantly off a stale timestamp.
func TestFailoverRecoveryClearsTimer(t *testing.T) {
	r, _, _, _, s := newSchedulingReconciler(t, "web1")
	ctx := context.Background()

	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}

	reach := newFakeReach()
	reach.Set("web1", "ssh", true) // reachable again
	r.SetReachability(reach)

	r.mu.Lock()
	r.unreachableSince["web1"] = time.Now().Add(-2 * time.Minute)
	r.mu.Unlock()

	r.failoverLostNodes(ctx)

	r.mu.Lock()
	_, tracked := r.unreachableSince["web1"]
	r.mu.Unlock()
	if tracked {
		t.Errorf("want unreachableSince[\"web1\"] cleared once the node is reachable again")
	}
}
