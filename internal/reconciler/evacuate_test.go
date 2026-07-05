package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

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

// countCall counts how many entries in f.Calls exactly equal want.
func countCall(f *node.Fake, want string) int {
	n := 0
	for _, c := range f.Calls {
		if c == want {
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
	// web2 (the new node deployReplicaSet actually ran against) — and EXACTLY
	// ONCE. Phase 12 Task 4 made deployReplicaSet the single owner of old-set
	// retirement (per-replica, on each replica's own node), so
	// rescheduleSurfaceLocked must no longer do its own removal on top of it:
	// a second RemoveContainer here would be a wasteful already-removed error
	// on a real Local/AgentNode backend (masked only by node.Fake's tolerant
	// map delete). This count assertion is the regression guard the old
	// contains()-only checks lacked.
	if got := countCall(web1, "RemoveContainer:"+cur.ContainerID); got != 1 {
		t.Errorf("RemoveContainer for old container %q on web1 called %d times, want exactly 1 (no double-remove), calls: %v", cur.ContainerID, got, web1.Calls)
	}
	// And nothing removed that container on web2 (the new node).
	if got := countCall(web2, "RemoveContainer:"+cur.ContainerID); got != 0 {
		t.Errorf("old container %q must never be removed on web2 (the new node), got %d such calls: %v", cur.ContainerID, got, web2.Calls)
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

// threeReplicaSpreadApp returns a Replicas:3 unpinned app with a memory
// requirement small enough to fit every node used by the Phase 12 Task 6
// per-replica evacuate/failover tests below.
func threeReplicaSpreadApp(name string) *spec.App {
	app := unpinnedApp(name)
	app.Replicas = 3
	app.Requirements = &spec.Requirements{Memory: "2000"}
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout
	return app
}

// setDescendingCapacity gives local/web1/web2[/web3] strictly descending
// MemAvailable (local highest) so scheduler.Place's most-free-first rule
// deterministically places replica 0 on local, replica 1 on web1, replica 2
// on web2, in that exact order — leaving web3 (if present) as the only node
// with spare, unused capacity for a replaced replica to land on.
func setDescendingCapacity(localFake *node.Fake, resolver node.FakeResolver, names ...string) {
	localFake.Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 9000}
	mem := int64(8000)
	for _, name := range names {
		resolver[name].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: mem}
		mem -= 1000
	}
}

// TestEvacuateMovesOnlyNodesReplicas covers Phase 12 Task 6's core
// per-replica evacuate: a 3-replica app spread across local/web1/web2 has
// its web1 replica (and ONLY that replica) moved to web3 when web1 is
// evacuated — local and web2's replica containers, entries, and route
// upstreams are completely untouched, and the deployment row is updated IN
// PLACE (same id) rather than becoming a brand new generation.
func TestEvacuateMovesOnlyNodesReplicas(t *testing.T) {
	r, resolver, localFake, fr, s := newSchedulingReconciler(t, "web1", "web2", "web3")
	ctx := context.Background()
	fr.ProbeStatus = 200
	setDescendingCapacity(localFake, resolver, "web1", "web2", "web3")

	for i, n := range []string{"web1", "web2", "web3"} {
		if err := s.AddNode(store.Node{Name: n, SSHHost: "deploy@" + n, MeshAddr: fmt.Sprintf("100.64.0.%d", i+2), Pool: "default"}); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}

	app := threeReplicaSpreadApp("blog")
	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("initial Apply: %v", err)
	}
	if len(dep.Replicas) != 3 {
		t.Fatalf("setup: want 3 replicas, got %+v", dep.Replicas)
	}
	wantNodes := []string{"local", "web1", "web2"}
	for i, want := range wantNodes {
		if dep.Replicas[i].Node != want {
			t.Fatalf("setup: replica %d on %q, want %q (dep=%+v)", i, dep.Replicas[i].Node, want, dep.Replicas)
		}
	}

	web1 := resolver["web1"].(*node.Fake)
	web2 := resolver["web2"].(*node.Fake)
	web3 := resolver["web3"].(*node.Fake)
	preLocalRuns := runContainerCalls(localFake)
	preWeb2Runs := runContainerCalls(web2)
	preWeb3Runs := runContainerCalls(web3)
	oldWeb1ContainerID := dep.Replicas[1].ContainerID

	result, err := r.EvacuateNode(ctx, "web1")
	if err != nil {
		t.Fatalf("EvacuateNode: %v", err)
	}
	if !contains(result.Moved, "blog") {
		t.Fatalf("Moved = %v, want to contain blog", result.Moved)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("Failed = %+v, want empty", result.Failed)
	}

	newCur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if newCur == nil || newCur.ID != dep.ID {
		t.Fatalf("want the SAME deployment row updated in place (id %d), got %+v", dep.ID, newCur)
	}
	if len(newCur.Replicas) != 3 {
		t.Fatalf("Replicas = %+v, want 3", newCur.Replicas)
	}
	if newCur.Replicas[0].Node != "local" || newCur.Replicas[0].ContainerID != dep.Replicas[0].ContainerID {
		t.Errorf("replica 0 (local) must be untouched, got %+v want %+v", newCur.Replicas[0], dep.Replicas[0])
	}
	if newCur.Replicas[2].Node != "web2" || newCur.Replicas[2].ContainerID != dep.Replicas[2].ContainerID {
		t.Errorf("replica 2 (web2) must be untouched, got %+v want %+v", newCur.Replicas[2], dep.Replicas[2])
	}
	if newCur.Replicas[1].Node != "web3" {
		t.Errorf("replica 1 (was web1) moved to %q, want web3", newCur.Replicas[1].Node)
	}

	if got := runContainerCalls(localFake); got != preLocalRuns {
		t.Errorf("local (untouched replica) got a new RunContainer call, want none: %v", localFake.Calls)
	}
	if got := runContainerCalls(web2); got != preWeb2Runs {
		t.Errorf("web2 (untouched replica) got a new RunContainer call, want none: %v", web2.Calls)
	}
	if got := runContainerCalls(web3); got != preWeb3Runs+1 {
		t.Errorf("want exactly one new RunContainer call on web3, got delta %d: %v", got-preWeb3Runs, web3.Calls)
	}

	if got := countCall(web1, "RemoveContainer:"+oldWeb1ContainerID); got != 1 {
		t.Errorf("RemoveContainer for old web1 container called %d times, want 1: %v", got, web1.Calls)
	}

	route, ok := fr.Routes[app.Domain]
	if !ok {
		t.Fatalf("want a live route for %s", app.Domain)
	}
	if len(route.Upstreams) != 3 {
		t.Fatalf("route.Upstreams = %+v, want 3", route.Upstreams)
	}
	found := false
	for _, u := range route.Upstreams {
		if u.Host == newCur.Replicas[1].Upstream && u.Port == newCur.Replicas[1].Port {
			found = true
		}
	}
	if !found {
		t.Errorf("route.Upstreams %+v does not contain the moved replica's new upstream %+v", route.Upstreams, newCur.Replicas[1])
	}
}

// TestFailoverReschedulesReplicaPreservingSpread covers Phase 12 Task 6's
// automatic-failover path: node loss for web1 (holding one replica of a
// 3-way spread set) reschedules ONLY that replica, excluding the surviving
// replicas' nodes (local, web2) to preserve spread — landing it on web3, the
// only node not already running this app.
func TestFailoverReschedulesReplicaPreservingSpread(t *testing.T) {
	r, resolver, localFake, fr, s := newSchedulingReconciler(t, "web1", "web2", "web3")
	ctx := context.Background()
	fr.ProbeStatus = 200
	setDescendingCapacity(localFake, resolver, "web1", "web2", "web3")

	for i, n := range []string{"web1", "web2", "web3"} {
		if err := s.AddNode(store.Node{Name: n, SSHHost: "deploy@" + n, MeshAddr: fmt.Sprintf("100.64.0.%d", i+2), Pool: "default"}); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}

	reach := newFakeReach()
	r.SetReachability(reach)

	app := threeReplicaSpreadApp("blog")
	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("initial Apply: %v", err)
	}
	if len(dep.Replicas) != 3 || dep.Replicas[1].Node != "web1" {
		t.Fatalf("setup: want replica 1 on web1, got %+v", dep.Replicas)
	}
	web1 := resolver["web1"].(*node.Fake)
	oldWeb1ContainerID := dep.Replicas[1].ContainerID

	reach.Set("web1", "ssh", false)
	r.mu.Lock()
	r.unreachableSince["web1"] = time.Now().Add(-2 * time.Minute)
	r.mu.Unlock()

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	newCur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if newCur == nil || newCur.ID != dep.ID {
		t.Fatalf("want deployment row updated in place, got %+v", newCur)
	}
	if newCur.Replicas[0].Node != "local" || newCur.Replicas[0].ContainerID != dep.Replicas[0].ContainerID {
		t.Errorf("replica 0 (local) must be untouched: %+v", newCur.Replicas[0])
	}
	if newCur.Replicas[2].Node != "web2" || newCur.Replicas[2].ContainerID != dep.Replicas[2].ContainerID {
		t.Errorf("replica 2 (web2) must be untouched: %+v", newCur.Replicas[2])
	}
	if newCur.Replicas[1].Node != "web3" {
		t.Errorf("replica 1 (lost node web1) rescheduled to %q, want web3 (excluding survivors local/web2)", newCur.Replicas[1].Node)
	}
	if contains(web1.Calls, "RemoveContainer:"+oldWeb1ContainerID) {
		t.Errorf("old container on unreachable web1 must NOT be removed, calls: %v", web1.Calls)
	}

	r.mu.Lock()
	_, stillTracked := r.unreachableSince["web1"]
	r.mu.Unlock()
	if stillTracked {
		t.Errorf("want unreachableSince[\"web1\"] cleared after failover")
	}
}

// TestEvacuateSingleReplicaUnchanged covers Phase 12 Task 6's non-regression
// contract via the public EvacuateNode entry point (TestRescheduleMovesToAnotherNode
// covers the same contract at the lower rescheduleSurfaceLocked level): a
// Replicas:1 app's evacuate is still exactly P11b's whole-surface move — a
// brand new deployment ROW, not the existing row updated in place.
func TestEvacuateSingleReplicaUnchanged(t *testing.T) {
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

	app := unpinnedApp("blog") // Replicas: 1
	app.Requirements = &spec.Requirements{Memory: "2000"}
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout

	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("initial Apply: %v", err)
	}
	if len(dep.Replicas) != 1 || dep.Replicas[0].Node != "web1" {
		t.Fatalf("setup: want single replica on web1, got %+v", dep.Replicas)
	}

	result, err := r.EvacuateNode(ctx, "web1")
	if err != nil {
		t.Fatalf("EvacuateNode: %v", err)
	}
	if !contains(result.Moved, "blog") {
		t.Fatalf("Moved = %v, want to contain blog", result.Moved)
	}

	newCur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if newCur == nil || newCur.ID == dep.ID {
		t.Fatalf("want a NEW deployment row (whole-surface move), got %+v (old id %d)", newCur, dep.ID)
	}
	if got := specNode(t, newCur.Spec); got != "web2" {
		t.Errorf("moved to %q, want web2", got)
	}

	oldRow := depByID(t, s, "blog", dep.ID)
	if oldRow == nil || oldRow.Status != store.StatusRetired {
		t.Errorf("old deployment %+v, want status retired", oldRow)
	}
}

// TestEvacuateReplicaNoCapacityFails covers the no-capacity-elsewhere case
// for a per-replica move: a 3-replica app fills every registered node
// (local/web1/web2), so evacuating web1's replica has nowhere else to go.
// That replica's move must be reported Failed (app NOT reported Moved, since
// nothing actually moved), and every replica (including the one on web1)
// must be left completely untouched.
func TestEvacuateReplicaNoCapacityFails(t *testing.T) {
	r, resolver, localFake, fr, s := newSchedulingReconciler(t, "web1", "web2")
	ctx := context.Background()
	fr.ProbeStatus = 200
	setDescendingCapacity(localFake, resolver, "web1", "web2")

	for i, n := range []string{"web1", "web2"} {
		if err := s.AddNode(store.Node{Name: n, SSHHost: "deploy@" + n, MeshAddr: fmt.Sprintf("100.64.0.%d", i+2), Pool: "default"}); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}

	app := threeReplicaSpreadApp("blog")
	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("initial Apply: %v", err)
	}
	wantNodes := []string{"local", "web1", "web2"}
	for i, want := range wantNodes {
		if dep.Replicas[i].Node != want {
			t.Fatalf("setup: replica %d on %q, want %q", i, dep.Replicas[i].Node, want)
		}
	}
	web1 := resolver["web1"].(*node.Fake)
	oldWeb1ContainerID := dep.Replicas[1].ContainerID

	result, err := r.EvacuateNode(ctx, "web1")
	if err != nil {
		t.Fatalf("EvacuateNode: %v", err)
	}
	if contains(result.Moved, "blog") {
		t.Errorf("Moved = %v, want NOT to contain blog (no capacity for the affected replica)", result.Moved)
	}
	if len(result.Failed) != 1 || result.Failed[0].App != "blog" {
		t.Fatalf("Failed = %+v, want exactly one entry for blog", result.Failed)
	}

	after, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if after == nil || after.ID != dep.ID {
		t.Fatalf("want same deployment row untouched, got %+v", after)
	}
	for i, want := range wantNodes {
		if after.Replicas[i].Node != want || after.Replicas[i].ContainerID != dep.Replicas[i].ContainerID {
			t.Errorf("replica %d = %+v, want untouched (node %s, container %s)", i, after.Replicas[i], want, dep.Replicas[i].ContainerID)
		}
	}
	if contains(web1.Calls, "RemoveContainer:"+oldWeb1ContainerID) {
		t.Errorf("failed move must not remove the old container: %v", web1.Calls)
	}
}

// TestEvacuateMultipleReplicasSameNode covers the multi-mover case: a
// scheduled 4-replica surface with TWO replicas stacked on the node being
// evacuated (web-b) and the other two on distinct nodes (web-a, web-c). Both
// web-b replicas must move, and — critically for spread — they must land on
// TWO DIFFERENT new nodes (web-d, web-e), never stacked onto the same one,
// because the second mover's exclude set includes the FIRST mover's freshly
// chosen node (cur.Replicas is updated in place between movers). The two
// survivors on web-a/web-c stay byte-identical.
//
// The starting deployment's Replicas is hand-built via RecordDeployment so
// the "2 replicas on the same node" arrangement is deterministic rather than
// depending on placement luck.
func TestEvacuateMultipleReplicasSameNode(t *testing.T) {
	r, resolver, localFake, fr, s := newSchedulingReconciler(t, "web-a", "web-b", "web-c", "web-d", "web-e")
	ctx := context.Background()
	fr.ProbeStatus = 200

	// local starved so it never fits the 2000-byte requirement; the two
	// excluded survivor nodes (web-a/web-c) and the evacuated node (web-b)
	// have ample capacity but are never candidates for a mover; web-d
	// (4000) > web-e (3000) so the first mover deterministically picks web-d
	// and the second — with web-d now excluded — picks web-e.
	localFake.Cap = node.Capacity{Known: true, CPUCores: 1, MemAvailable: 1}
	caps := map[string]int64{"web-a": 5000, "web-b": 5000, "web-c": 5000, "web-d": 4000, "web-e": 3000}
	for i, name := range []string{"web-a", "web-b", "web-c", "web-d", "web-e"} {
		fk := resolver[name].(*node.Fake)
		fk.Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: caps[name]}
		fk.MeshAddr = fmt.Sprintf("100.64.0.%d", i+2)
		if err := s.AddNode(store.Node{Name: name, SSHHost: "deploy@" + name, MeshAddr: fk.MeshAddr, Pool: "default"}); err != nil {
			t.Fatalf("AddNode %s: %v", name, err)
		}
	}

	specApp := spec.App{
		Name:         "blog",
		Image:        "img:1",
		Domain:       "blog.example.com",
		Port:         8080,
		Replicas:     4,
		Requirements: &spec.Requirements{Memory: "2000"},
	}
	specApp.Health.Path = "/healthz"
	specApp.Health.Timeout = shortTimeout
	specJSON, err := json.Marshal(specApp)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}

	replicas := []store.Replica{
		{ContainerID: "ca", Node: "web-a", Upstream: "100.64.0.2", Port: 9001},
		{ContainerID: "cb1", Node: "web-b", Upstream: "100.64.0.3", Port: 9002},
		{ContainerID: "cc", Node: "web-c", Upstream: "100.64.0.4", Port: 9003},
		{ContainerID: "cb2", Node: "web-b", Upstream: "100.64.0.3", Port: 9004},
	}
	id, err := s.RecordDeployment(store.Deployment{
		App:         "blog",
		Image:       "img:1",
		ContainerID: replicas[0].ContainerID,
		Status:      store.StatusRunning,
		CreatedAt:   time.Now(),
		Spec:        string(specJSON),
		Scheduled:   true,
		Replicas:    replicas,
	})
	if err != nil {
		t.Fatalf("RecordDeployment: %v", err)
	}

	webA := resolver["web-a"].(*node.Fake)
	webB := resolver["web-b"].(*node.Fake)
	webC := resolver["web-c"].(*node.Fake)
	webD := resolver["web-d"].(*node.Fake)
	webE := resolver["web-e"].(*node.Fake)

	result, err := r.EvacuateNode(ctx, "web-b")
	if err != nil {
		t.Fatalf("EvacuateNode: %v", err)
	}
	if !contains(result.Moved, "blog") {
		t.Fatalf("Moved = %v, want to contain blog", result.Moved)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("Failed = %+v, want empty", result.Failed)
	}

	newCur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if newCur == nil || newCur.ID != id {
		t.Fatalf("want the SAME deployment row updated in place (id %d), got %+v", id, newCur)
	}
	if len(newCur.Replicas) != 4 {
		t.Fatalf("Replicas = %+v, want 4", newCur.Replicas)
	}

	// Survivors untouched (node, container, upstream, port all identical).
	if newCur.Replicas[0] != replicas[0] {
		t.Errorf("replica 0 (web-a survivor) = %+v, want untouched %+v", newCur.Replicas[0], replicas[0])
	}
	if newCur.Replicas[2] != replicas[2] {
		t.Errorf("replica 2 (web-c survivor) = %+v, want untouched %+v", newCur.Replicas[2], replicas[2])
	}

	// Both movers relocated off web-b, onto TWO DIFFERENT nodes.
	moved1, moved3 := newCur.Replicas[1], newCur.Replicas[3]
	if moved1.Node == "web-b" || moved3.Node == "web-b" {
		t.Errorf("a mover is still on web-b: [1]=%+v [3]=%+v", moved1, moved3)
	}
	if moved1.Node == moved3.Node {
		t.Errorf("both movers stacked onto the same node %q — spread NOT preserved (distinct capacity existed): [1]=%+v [3]=%+v", moved1.Node, moved1, moved3)
	}
	if moved1.Node != "web-d" {
		t.Errorf("first mover on %q, want web-d (most-free target)", moved1.Node)
	}
	if moved3.Node != "web-e" {
		t.Errorf("second mover on %q, want web-e (web-d excluded by first mover)", moved3.Node)
	}
	if moved1.ContainerID == "cb1" || moved3.ContainerID == "cb2" {
		t.Errorf("movers kept their old container ids, want fresh ones: [1]=%q [3]=%q", moved1.ContainerID, moved3.ContainerID)
	}

	// No replica of this app remains on web-b.
	for i, rep := range newCur.Replicas {
		if rep.Node == "web-b" {
			t.Errorf("replica %d still on evacuated web-b: %+v", i, rep)
		}
	}

	// Exactly one new container per target node; none on the survivors.
	if got := runContainerCalls(webA); got != 0 {
		t.Errorf("web-a (survivor) got RunContainer calls, want none: %v", webA.Calls)
	}
	if got := runContainerCalls(webC); got != 0 {
		t.Errorf("web-c (survivor) got RunContainer calls, want none: %v", webC.Calls)
	}
	if got := runContainerCalls(webD); got != 1 {
		t.Errorf("web-d RunContainer calls = %d, want 1: %v", got, webD.Calls)
	}
	if got := runContainerCalls(webE); got != 1 {
		t.Errorf("web-e RunContainer calls = %d, want 1: %v", got, webE.Calls)
	}

	// Both old containers removed on their own node (web-b, reachable — no
	// Reachability wired up, so removal is attempted).
	if !contains(webB.Calls, "RemoveContainer:cb1") {
		t.Errorf("want old container cb1 removed on web-b: %v", webB.Calls)
	}
	if !contains(webB.Calls, "RemoveContainer:cb2") {
		t.Errorf("want old container cb2 removed on web-b: %v", webB.Calls)
	}

	// Live route carries all 4 upstreams (2 untouched survivors + 2 movers).
	route, ok := fr.Routes["blog.example.com"]
	if !ok {
		t.Fatalf("want a live route for blog.example.com")
	}
	if len(route.Upstreams) != 4 {
		t.Fatalf("route.Upstreams = %+v, want 4", route.Upstreams)
	}
	for _, mov := range []store.Replica{moved1, moved3} {
		found := false
		for _, u := range route.Upstreams {
			if u.Host == mov.Upstream && u.Port == mov.Port {
				found = true
			}
		}
		if !found {
			t.Errorf("route.Upstreams %+v missing moved replica upstream %+v", route.Upstreams, mov)
		}
	}
}
