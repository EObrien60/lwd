package reconciler

import (
	"context"
	"testing"

	"lwd/internal/compose"
	"lwd/internal/node"
	"lwd/internal/router"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// newSchedulingReconcilerWithCompose is newSchedulingReconciler plus the
// compose.Fake, for evacuate tests that need a compose app placed alongside
// scheduled surfaces to prove EvacuateNode never touches it.
func newSchedulingReconcilerWithCompose(t *testing.T, extra ...string) (*Reconciler, node.FakeResolver, *node.Fake, *router.FakeRouter, *store.Store, *compose.Fake) {
	t.Helper()
	r, localFake, fr, s, cf := newTestReconcilerWithCompose(t)
	resolver := node.FakeResolver{"local": localFake}
	for _, name := range extra {
		resolver[name] = node.NewFake()
	}
	r.resolver = resolver
	return r, resolver, localFake, fr, s, cf
}

// depByID returns the deployment with id from app's full history, or nil.
func depByID(t *testing.T, s *store.Store, app string, id int64) *store.Deployment {
	t.Helper()
	deps, err := s.DeploymentsForApp(app)
	if err != nil {
		t.Fatalf("DeploymentsForApp(%q): %v", app, err)
	}
	for i := range deps {
		if deps[i].ID == id {
			return &deps[i]
		}
	}
	return nil
}

// runContainerCalls counts RunContainer: calls recorded on f.
func runContainerCalls(f *node.Fake) int {
	n := 0
	for _, c := range f.Calls {
		if hasPrefix(c, "RunContainer:") {
			n++
		}
	}
	return n
}

// TestRescheduleMovesToAnotherNode covers Phase 11b Task 3's core reschedule
// path: a scheduler-placed surface currently on web1 is moved to web2 (the
// only other node with enough capacity — local is deliberately starved so it
// can never be chosen), producing a brand new StatusRunning, Scheduled
// deployment on web2, an actual RunContainer call on web2's fake, and the old
// deployment row retired with its OLD container explicitly removed from
// web1's fake (not web2's — the wrong-node hazard the brief calls out).
func TestRescheduleMovesToAnotherNode(t *testing.T) {
	r, resolver, localFake, fr, s := newSchedulingReconciler(t, "web1", "web2")
	ctx := context.Background()
	fr.ProbeStatus = 200

	// Starve local so it never fits; web1 is the most-free fitting candidate
	// initially, web2 the only other one that still has enough (3000) to fit
	// the 2000-byte requirement once web1 is excluded.
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

	app := unpinnedApp("blog")
	app.Requirements = &spec.Requirements{Memory: "2000"} // fits web1/web2 (9000/3000), not local (1)
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
	if cur == nil || !cur.Scheduled {
		t.Fatalf("setup: want a current Scheduled deployment, got %+v", cur)
	}

	preWeb2Runs := runContainerCalls(web2)

	r.mu.Lock()
	moved, err := r.rescheduleSurfaceLocked(ctx, cur, "web1")
	r.mu.Unlock()
	if err != nil {
		t.Fatalf("rescheduleSurfaceLocked: %v", err)
	}

	if moved.Status != store.StatusRunning {
		t.Errorf("moved.Status = %q, want running", moved.Status)
	}
	if !moved.Scheduled {
		t.Errorf("moved.Scheduled = false, want true")
	}
	if got := specNode(t, moved.Spec); got != "web2" {
		t.Errorf("moved to %q, want web2", got)
	}
	// moved.ContainerID is NOT compared against cur.ContainerID for
	// distinctness: web1 and web2 are independent *node.Fake instances, each
	// with its own local id sequence starting at "fake-1", so the same
	// string can legitimately name two different containers on two
	// different nodes. The RunContainer-call-count check below (on web2
	// specifically) and the RemoveContainer-on-web1 check further down are
	// what actually prove a new container was created on the new node.
	if runContainerCalls(web2) <= preWeb2Runs {
		t.Errorf("want a new RunContainer call on web2, calls: %v", web2.Calls)
	}

	// The old container must be removed from web1 (the excluded node) — NOT
	// web2 (the new node deployBlueGreenSurface actually ran against).
	if !contains(web1.Calls, "RemoveContainer:"+cur.ContainerID) {
		t.Errorf("want old container %q removed from web1, calls: %v", cur.ContainerID, web1.Calls)
	}

	oldRow := depByID(t, s, "blog", cur.ID)
	if oldRow == nil || oldRow.Status != store.StatusRetired {
		t.Errorf("old deployment %+v, want status retired", oldRow)
	}

	newCur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment after reschedule: %v", err)
	}
	if newCur == nil || newCur.ID != moved.ID {
		t.Fatalf("want current deployment to be the moved row, got %+v", newCur)
	}
}

// TestRescheduleNoOtherNodeFails covers the no-capacity-elsewhere case: only
// the excluded node (web1) fits, so rescheduleSurfaceLocked must fail and
// leave the original surface running untouched on web1.
func TestRescheduleNoOtherNodeFails(t *testing.T) {
	r, resolver, localFake, fr, s := newSchedulingReconciler(t, "web1")
	ctx := context.Background()
	fr.ProbeStatus = 200

	// Only web1 fits; local is starved so it can never be an alternative.
	localFake.Cap = node.Capacity{Known: true, CPUCores: 1, MemAvailable: 1}
	web1 := resolver["web1"].(*node.Fake)
	web1.Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 9000}

	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}

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

	r.mu.Lock()
	_, err = r.rescheduleSurfaceLocked(ctx, cur, "web1")
	r.mu.Unlock()
	if err == nil {
		t.Fatal("rescheduleSurfaceLocked: want error, got nil (no other node fits)")
	}

	// The original must be left completely untouched.
	after, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment after failed reschedule: %v", err)
	}
	if after == nil || after.ID != cur.ID || after.Status != store.StatusRunning {
		t.Errorf("want original deployment %d still running, got %+v", cur.ID, after)
	}
	if after != nil && after.ContainerID != dep.ContainerID {
		t.Errorf("want original container %q still recorded, got %q", dep.ContainerID, after.ContainerID)
	}
}

// TestEvacuateSkipsPinned covers EvacuateNode's pinned-surface path: a
// surface explicitly pinned to the target node (Scheduled == false) must be
// reported Skipped, never moved, and left running untouched.
func TestEvacuateSkipsPinned(t *testing.T) {
	r, resolver, _, fr, s := newSchedulingReconciler(t, "web1")
	ctx := context.Background()
	fr.ProbeStatus = 200
	resolver["web1"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 9000}

	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}

	app := unpinnedApp("pinned")
	app.Node = "web1" // explicit pin
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout

	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	result, err := r.EvacuateNode(ctx, "web1")
	if err != nil {
		t.Fatalf("EvacuateNode: %v", err)
	}

	if !contains(result.Skipped, "pinned") {
		t.Errorf("Skipped = %v, want to contain %q", result.Skipped, "pinned")
	}
	if contains(result.Moved, "pinned") {
		t.Errorf("Moved = %v, want NOT to contain %q (pinned surfaces must never move)", result.Moved, "pinned")
	}
	if len(result.Failed) != 0 {
		t.Errorf("Failed = %v, want empty", result.Failed)
	}

	after, err := s.CurrentDeployment("pinned")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if after == nil || after.ID != dep.ID || after.Status != store.StatusRunning {
		t.Errorf("want pinned deployment untouched, got %+v", after)
	}
}

// TestEvacuateMovesScheduledLeavesCompose covers EvacuateNode's mixed-fleet
// case: a scheduler-placed surface on the target node is moved, while a
// Phase-4 compose app pinned to the SAME node is never even considered (not
// Moved, Skipped, or Failed) and its stack is left running untouched. The
// node under evacuation here is "local": compose apps are only supported on
// local (remote compose isn't implemented), so that's the only node a
// compose app can coexist with a scheduled surface on.
func TestEvacuateMovesScheduledLeavesCompose(t *testing.T) {
	r, resolver, localFake, fr, s, cf := newSchedulingReconcilerWithCompose(t, "web1", "web2")
	ctx := context.Background()
	fr.ProbeStatus = 200

	// local is the most-free candidate initially (so the unpinned surface
	// lands there); web1 has enough (5000) to fit the 2000-byte requirement
	// once local is excluded, web2 does not (500).
	localFake.Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 9000}
	web1 := resolver["web1"].(*node.Fake)
	web2 := resolver["web2"].(*node.Fake)
	web1.Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 5000}
	web2.Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 500}

	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}
	if err := s.AddNode(store.Node{Name: "web2", SSHHost: "deploy@web2", MeshAddr: "100.64.0.3", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web2: %v", err)
	}

	scheduledApp := unpinnedApp("blog")
	scheduledApp.Requirements = &spec.Requirements{Memory: "2000"}
	scheduledApp.Health.Path = "/healthz"
	scheduledApp.Health.Timeout = shortTimeout
	if _, err := r.Apply(ctx, scheduledApp); err != nil {
		t.Fatalf("Apply scheduled: %v", err)
	}
	blogCur, err := s.CurrentDeployment("blog")
	if err != nil || blogCur == nil {
		t.Fatalf("CurrentDeployment(blog): %v / %+v", err, blogCur)
	}
	if got := specNode(t, blogCur.Spec); got != "local" {
		t.Fatalf("setup: initial placement = %q, want local", got)
	}

	composeApp := testComposeApp(t, "services:\n  web:\n    image: nginx\n") // Node defaults to "local"
	composeApp.Health.Path = "/healthz"
	composeApp.Health.Timeout = shortTimeout
	cf.ServiceID = "compose-container-1"
	cf.ServiceName = "lwd-webapp-web-1"
	if _, err := r.Apply(ctx, composeApp); err != nil {
		t.Fatalf("Apply compose: %v", err)
	}
	composeCur, err := s.CurrentDeployment("webapp")
	if err != nil || composeCur == nil {
		t.Fatalf("CurrentDeployment(webapp): %v / %+v", err, composeCur)
	}

	result, err := r.EvacuateNode(ctx, "local")
	if err != nil {
		t.Fatalf("EvacuateNode: %v", err)
	}

	if !contains(result.Moved, "blog") {
		t.Errorf("Moved = %v, want to contain %q", result.Moved, "blog")
	}
	if contains(result.Skipped, "webapp") || contains(result.Moved, "webapp") {
		t.Errorf("compose app must never be considered at all, got Skipped=%v Moved=%v", result.Skipped, result.Moved)
	}

	newBlog, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment(blog) after evacuate: %v", err)
	}
	if newBlog == nil || newBlog.ID == blogCur.ID {
		t.Errorf("want blog moved to a new deployment row, got %+v", newBlog)
	}
	if got := specNode(t, newBlog.Spec); got != "web1" {
		t.Errorf("blog moved to %q, want web1", got)
	}

	afterCompose, err := s.CurrentDeployment("webapp")
	if err != nil {
		t.Fatalf("CurrentDeployment(webapp) after evacuate: %v", err)
	}
	if afterCompose == nil || afterCompose.ID != composeCur.ID || afterCompose.Status != store.StatusRunning {
		t.Errorf("want compose app untouched, got %+v", afterCompose)
	}
	if contains(cf.Calls, "Down:lwd-webapp") {
		t.Errorf("compose Down called, want compose stack never touched: %v", cf.Calls)
	}
}

// TestEvacuateUnreachableNodeSkipsOldRemoval covers node-loss failover: when
// excludeNode is unreachable (per Reachability), the new surface is still
// created on a fitting node and the old row is retired, but the old
// container removal against the (unreachable, presumably dead) node is
// skipped rather than attempted.
func TestEvacuateUnreachableNodeSkipsOldRemoval(t *testing.T) {
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
	if got := specNode(t, dep.Spec); got != "web1" {
		t.Fatalf("setup: initial placement = %q, want web1", got)
	}
	cur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}

	// Simulate node loss: web1 is now unreachable.
	reach.Set("web1", "ssh", false)

	result, err := r.EvacuateNode(ctx, "web1")
	if err != nil {
		t.Fatalf("EvacuateNode: %v", err)
	}
	if !contains(result.Moved, "blog") {
		t.Fatalf("Moved = %v, want to contain %q", result.Moved, "blog")
	}

	newCur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment after evacuate: %v", err)
	}
	if newCur == nil || newCur.ID == cur.ID {
		t.Fatalf("want a new current deployment, got %+v", newCur)
	}
	if got := specNode(t, newCur.Spec); got != "web2" {
		t.Errorf("moved to %q, want web2", got)
	}

	// The old container must NOT have been removed from web1 (unreachable).
	if contains(web1.Calls, "RemoveContainer:"+cur.ContainerID) {
		t.Errorf("want old container removal SKIPPED on unreachable web1, calls: %v", web1.Calls)
	}

	oldRow := depByID(t, s, "blog", cur.ID)
	if oldRow == nil || oldRow.Status != store.StatusRetired {
		t.Errorf("old deployment %+v, want status retired even though node unreachable", oldRow)
	}
}
