package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"lwd/internal/node"
	"lwd/internal/router"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// newSchedulingReconciler builds a Reconciler (via newTestReconciler, so
// every non-node dependency is the usual fake) and then swaps in a
// FakeResolver mapping "local" (the original local fake) plus every name in
// extra to its own fresh *node.Fake — letting scheduling tests register
// store.Node rows (web1, web2, ...) and give each fake independently
// settable Capacity. Both r.resolver and the reconciler's internal
// localNode()-reachable resolver are the same map, since resolvePlacement
// and applyImage/applyGit both go through r.resolver.
func newSchedulingReconciler(t *testing.T, extra ...string) (*Reconciler, node.FakeResolver, *node.Fake, *router.FakeRouter, *store.Store) {
	t.Helper()
	r, localFake, fr, s := newTestReconciler(t)
	resolver := node.FakeResolver{"local": localFake}
	for _, name := range extra {
		resolver[name] = node.NewFake()
	}
	r.resolver = resolver
	return r, resolver, localFake, fr, s
}

// unpinnedApp returns a valid single-service image app with Node left unset
// ("") so it is subject to scheduling, unlike testApp() (which pins
// Node:"local").
func unpinnedApp(name string) *spec.App {
	return &spec.App{Name: name, Image: "img:1", Domain: name + ".example.com", Port: 8080, Replicas: 1}
}

// specNode unmarshals a deployment's JSON Spec snapshot and returns its Node
// field.
func specNode(t *testing.T, specJSON string) string {
	t.Helper()
	var a spec.App
	if err := json.Unmarshal([]byte(specJSON), &a); err != nil {
		t.Fatalf("unmarshal spec snapshot: %v", err)
	}
	return a.Node
}

func TestScheduleUnpinnedPicksMostFree(t *testing.T) {
	r, resolver, _, fr, _ := newSchedulingReconciler(t, "web1", "web2")
	ctx := context.Background()
	fr.ProbeStatus = 200

	resolver["web1"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 1000}
	resolver["web2"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 5000}

	s := r.store
	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}
	if err := s.AddNode(store.Node{Name: "web2", SSHHost: "deploy@web2", MeshAddr: "100.64.0.3", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web2: %v", err)
	}

	app := unpinnedApp("blog")
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout

	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := specNode(t, dep.Spec); got != "web2" {
		t.Errorf("recorded deployment Node = %q, want web2 (most free mem)", got)
	}
}

func TestPinnedNodeSkipsScheduler(t *testing.T) {
	r, resolver, _, fr, _ := newSchedulingReconciler(t, "web1", "web2")
	ctx := context.Background()
	fr.ProbeStatus = 200

	// web1 is "fuller" than web2, but the app pins Node explicitly, so
	// scheduling must be bypassed entirely.
	resolver["web1"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 500}
	resolver["web2"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 5000}

	s := r.store
	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}
	if err := s.AddNode(store.Node{Name: "web2", SSHHost: "deploy@web2", MeshAddr: "100.64.0.3", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web2: %v", err)
	}

	app := unpinnedApp("blog")
	app.Node = "web1"
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout

	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := specNode(t, dep.Spec); got != "web1" {
		t.Errorf("recorded deployment Node = %q, want web1 (pinned)", got)
	}
}

func TestSingleNodeUnpinnedGoesLocal(t *testing.T) {
	r, _, fr, _ := newTestReconciler(t)
	ctx := context.Background()
	fr.ProbeStatus = 200

	app := unpinnedApp("blog")
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout

	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := specNode(t, dep.Spec); got != "local" {
		t.Errorf("recorded deployment Node = %q, want local (non-regression, single node)", got)
	}
}

func TestScheduleNoCapacityFails(t *testing.T) {
	r, resolver, localFake, fr, _ := newSchedulingReconciler(t, "web1")
	ctx := context.Background()
	fr.ProbeStatus = 200

	// Both candidates (local and web1) must have a KNOWN, insufficient
	// capacity: an unknown (zero-value) Capacity is optimistically treated as
	// "fits anything" by the scheduler, which would defeat this test's intent.
	localFake.Cap = node.Capacity{Known: true, CPUCores: 1, MemAvailable: 100}
	resolver["web1"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 1, MemAvailable: 100}
	s := r.store
	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}

	app := unpinnedApp("blog")
	app.Requirements = &spec.Requirements{Memory: "1000000000"} // far more than any node's MemAvailable
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout

	_, err := r.Apply(ctx, app)
	if err == nil {
		t.Fatal("Apply: want error, got nil")
	}

	cur, cerr := s.CurrentDeployment(app.Name)
	if cerr != nil {
		t.Fatalf("CurrentDeployment: %v", cerr)
	}
	if cur != nil {
		t.Fatalf("want no current (running) deployment after failed scheduling, got %+v", cur)
	}
}

func TestRequirementsAppliedToRunSpec(t *testing.T) {
	r, f, fr, _ := newTestReconciler(t)
	ctx := context.Background()

	app := testApp() // pinned to local, so scheduling is bypassed; only RunSpec matters here
	app.Requirements = &spec.Requirements{CPU: 0.5, Memory: "256M"}
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	if _, err := r.Apply(ctx, app); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if f.LastRunSpec.CPUs != 0.5 {
		t.Errorf("LastRunSpec.CPUs = %v, want 0.5", f.LastRunSpec.CPUs)
	}
	want := int64(256 * 1024 * 1024)
	if f.LastRunSpec.MemoryBytes != want {
		t.Errorf("LastRunSpec.MemoryBytes = %d, want %d", f.LastRunSpec.MemoryBytes, want)
	}
}

// TestUnpinnedRecordsScheduled covers Phase 11b Task 1: an unpinned app
// (Node == "") is placed by the scheduler, and the resulting deployment must
// record Scheduled == true — this is the placement provenance later tasks
// use to decide which surfaces may be evacuated/failed-over.
func TestUnpinnedRecordsScheduled(t *testing.T) {
	r, _, fr, s := newTestReconciler(t)
	ctx := context.Background()
	fr.ProbeStatus = 200

	app := unpinnedApp("blog")
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout

	if _, err := r.Apply(ctx, app); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	cur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil || !cur.Scheduled {
		t.Fatalf("CurrentDeployment.Scheduled = %+v, want true for unpinned deploy", cur)
	}
}

// TestPinnedRecordsNotScheduled covers Phase 11b Task 1: an explicitly pinned
// app (Node set to a concrete node) bypasses the scheduler, and the resulting
// deployment must record Scheduled == false.
func TestPinnedRecordsNotScheduled(t *testing.T) {
	r, resolver, _, fr, s := newSchedulingReconciler(t, "web1")
	ctx := context.Background()
	fr.ProbeStatus = 200
	resolver["web1"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 5000}

	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}

	app := unpinnedApp("blog")
	app.Node = "web1"
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout

	if _, err := r.Apply(ctx, app); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	cur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil || cur.Scheduled {
		t.Fatalf("CurrentDeployment.Scheduled = %+v, want false for pinned deploy", cur)
	}
}

// TestResolvePlacementSkipsCordoned covers Phase 11b Task 2: a cordoned
// store node (Schedulable: false) must never be chosen by resolvePlacement,
// even when it is the most-free candidate — the other, uncordoned node must
// be picked instead.
func TestResolvePlacementSkipsCordoned(t *testing.T) {
	r, resolver, _, fr, _ := newSchedulingReconciler(t, "web1", "web2")
	ctx := context.Background()
	fr.ProbeStatus = 200

	// web2 is the most-free node by capacity, but it's cordoned, so web1 must
	// be chosen instead.
	resolver["web1"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 1000}
	resolver["web2"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 5000}

	s := r.store
	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}
	if err := s.AddNode(store.Node{Name: "web2", SSHHost: "deploy@web2", MeshAddr: "100.64.0.3", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web2: %v", err)
	}
	if err := s.SetSchedulable("web2", false); err != nil {
		t.Fatalf("SetSchedulable(web2, false): %v", err)
	}

	app := unpinnedApp("blog")
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout

	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := specNode(t, dep.Spec); got != "web1" {
		t.Errorf("recorded deployment Node = %q, want web1 (web2 is cordoned)", got)
	}
}

// TestPlaceExcludingDropsNode covers Phase 11b Task 2's placeExcluding
// helper, used by T3's reschedule: the excluded node must never be returned
// even when it is the most-free candidate — some other fitting node must be
// picked instead.
func TestPlaceExcludingDropsNode(t *testing.T) {
	r, resolver, _, _, _ := newSchedulingReconciler(t, "web1", "web2")
	ctx := context.Background()

	// web1 is the most-free node, but it's the exclude target, so web2 (the
	// only other fitting candidate) must be chosen.
	resolver["web1"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 9000}
	resolver["web2"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 1000}

	s := r.store
	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}
	if err := s.AddNode(store.Node{Name: "web2", SSHHost: "deploy@web2", MeshAddr: "100.64.0.3", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web2: %v", err)
	}

	app := unpinnedApp("blog")

	got, err := r.placeExcluding(ctx, app, "web1")
	if err != nil {
		t.Fatalf("placeExcluding: %v", err)
	}
	if got != "web2" {
		t.Fatalf("placeExcluding(app, %q) = %q, want web2 (web1 excluded)", "web1", got)
	}
	if got == "web1" {
		t.Fatalf("placeExcluding must never return the excluded node")
	}
}

// TestPlaceExcludingDropsLocal covers placeExcluding("local"): the local
// node must be droppable exactly like a named store node, so a surface
// scheduled onto local can still be evacuated off it.
func TestPlaceExcludingDropsLocal(t *testing.T) {
	r, resolver, localFake, _, _ := newSchedulingReconciler(t, "web1")
	ctx := context.Background()

	// local is the most-free node, but it's the exclude target.
	localFake.Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 9000}
	resolver["web1"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 1000}

	s := r.store
	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}

	app := unpinnedApp("blog")

	got, err := r.placeExcluding(ctx, app, "local")
	if err != nil {
		t.Fatalf("placeExcluding: %v", err)
	}
	if got != "web1" {
		t.Fatalf("placeExcluding(app, %q) = %q, want web1 (local excluded)", "local", got)
	}
}

// TestPlaceExcludingSetDropsAll covers Phase 12 Task 3's generalization of
// placeExcluding to an exclude SET: every name in the set must be dropped
// from the candidate pool, not just the first one.
func TestPlaceExcludingSetDropsAll(t *testing.T) {
	r, resolver, _, _, _ := newSchedulingReconciler(t, "web1", "web2", "web3")
	ctx := context.Background()

	// web1 and web2 are both more free than web3, but both are excluded, so
	// web3 (the only remaining candidate) must be chosen.
	resolver["web1"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 9000}
	resolver["web2"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 8000}
	resolver["web3"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 1000}

	s := r.store
	for i, n := range []string{"web1", "web2", "web3"} {
		if err := s.AddNode(store.Node{Name: n, SSHHost: "deploy@" + n, MeshAddr: fmt.Sprintf("100.64.0.%d", i+2), Pool: "default"}); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}

	app := unpinnedApp("blog")

	got, err := r.placeExcludingSet(ctx, app, []string{"local", "web1", "web2"})
	if err != nil {
		t.Fatalf("placeExcludingSet: %v", err)
	}
	if got != "web3" {
		t.Fatalf("placeExcludingSet(app, [local, web1, web2]) = %q, want web3", got)
	}
}

// TestPlaceReplicasSpreadsAcrossNodes covers Phase 12 Task 3: with 3
// schedulable nodes and 3 replicas, each replica must land on its own
// distinct node.
func TestPlaceReplicasSpreadsAcrossNodes(t *testing.T) {
	r, resolver, _, _, _ := newSchedulingReconciler(t, "web1", "web2", "web3")
	ctx := context.Background()

	resolver["web1"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 3000}
	resolver["web2"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 2000}
	resolver["web3"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 1000}

	s := r.store
	for i, n := range []string{"web1", "web2", "web3"} {
		if err := s.AddNode(store.Node{Name: n, SSHHost: "deploy@" + n, MeshAddr: fmt.Sprintf("100.64.0.%d", i+2), Pool: "default"}); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}

	app := unpinnedApp("blog")

	nodes, scheduled, err := r.placeReplicas(ctx, app, 3)
	if err != nil {
		t.Fatalf("placeReplicas: %v", err)
	}
	if !scheduled {
		t.Errorf("scheduled = false, want true (unpinned app)")
	}
	if len(nodes) != 3 {
		t.Fatalf("len(nodes) = %d, want 3", len(nodes))
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if seen[n] {
			t.Fatalf("nodes = %v, want 3 distinct nodes, got duplicate %q", nodes, n)
		}
		seen[n] = true
	}
}

// TestPlaceReplicasStacksWhenFewerNodes covers Phase 12 Task 3's
// stack-fallback: with only 2 schedulable nodes (local + web1) and 3
// replicas, placement must not fail — it spreads across the 2 distinct
// nodes then reuses the most-free one for the 3rd replica.
func TestPlaceReplicasStacksWhenFewerNodes(t *testing.T) {
	r, resolver, _, _, _ := newSchedulingReconciler(t, "web1")
	ctx := context.Background()

	resolver["web1"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 9000}

	s := r.store
	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}

	app := unpinnedApp("blog")

	nodes, scheduled, err := r.placeReplicas(ctx, app, 3)
	if err != nil {
		t.Fatalf("placeReplicas: %v", err)
	}
	if !scheduled {
		t.Errorf("scheduled = false, want true (unpinned app)")
	}
	if len(nodes) != 3 {
		t.Fatalf("len(nodes) = %d, want 3", len(nodes))
	}

	counts := map[string]int{}
	for _, n := range nodes {
		counts[n]++
	}
	if len(counts) != 2 {
		t.Fatalf("nodes = %v, want exactly 2 distinct nodes used (stack fallback)", nodes)
	}
	total := 0
	for _, c := range counts {
		total += c
	}
	if total != 3 {
		t.Fatalf("nodes = %v, want 3 total placements", nodes)
	}
	// web1 is the most-free node, so it must be the one reused.
	if counts["web1"] != 2 {
		t.Fatalf("counts = %v, want web1 reused twice (most-free stack target)", counts)
	}
}

// TestPlaceReplicasPinnedAllOnPin covers Phase 12 Task 3: a pinned app
// (Node set to a concrete node) places every replica on that same node,
// bypassing the scheduler entirely (scheduled=false, no spread).
func TestPlaceReplicasPinnedAllOnPin(t *testing.T) {
	r, _, _, _, _ := newSchedulingReconciler(t, "web1")
	ctx := context.Background()

	app := unpinnedApp("blog")
	app.Node = "web1"

	nodes, scheduled, err := r.placeReplicas(ctx, app, 3)
	if err != nil {
		t.Fatalf("placeReplicas: %v", err)
	}
	if scheduled {
		t.Errorf("scheduled = true, want false (pinned app)")
	}
	want := []string{"web1", "web1", "web1"}
	if len(nodes) != len(want) {
		t.Fatalf("nodes = %v, want %v", nodes, want)
	}
	for i, n := range nodes {
		if n != want[i] {
			t.Fatalf("nodes = %v, want %v", nodes, want)
		}
	}
}

// TestPlaceReplicasSingleNode covers Phase 12 Task 3's non-regression case:
// an unpinned app with no registered nodes places every replica on local
// (via stack fallback), never failing just because there's only one node.
func TestPlaceReplicasSingleNode(t *testing.T) {
	r, _, _, _ := newTestReconciler(t)
	ctx := context.Background()

	app := unpinnedApp("blog")

	nodes, scheduled, err := r.placeReplicas(ctx, app, 3)
	if err != nil {
		t.Fatalf("placeReplicas: %v", err)
	}
	if !scheduled {
		t.Errorf("scheduled = false, want true (unpinned app)")
	}
	want := []string{"local", "local", "local"}
	if len(nodes) != len(want) {
		t.Fatalf("nodes = %v, want %v", nodes, want)
	}
	for i, n := range nodes {
		if n != want[i] {
			t.Fatalf("nodes = %v, want %v", nodes, want)
		}
	}
}

// TestPlaceReplicasZeroTreatedAsOne covers Phase 12 Task 3's
// belt-and-suspenders n<=0 guard: an old snapshot could carry Replicas: 0,
// which must be treated as 1 rather than producing an empty/invalid node
// list.
func TestPlaceReplicasZeroTreatedAsOne(t *testing.T) {
	r, _, _, _ := newTestReconciler(t)
	ctx := context.Background()

	app := unpinnedApp("blog")

	nodes, _, err := r.placeReplicas(ctx, app, 0)
	if err != nil {
		t.Fatalf("placeReplicas: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1 (n<=0 treated as 1)", len(nodes))
	}
	if nodes[0] != "local" {
		t.Fatalf("nodes[0] = %q, want local", nodes[0])
	}
}

func TestProbeNodesIncludesCapacity(t *testing.T) {
	r, resolver, localFake, _, s := newSchedulingReconciler(t, "web1")
	ctx := context.Background()

	localFake.Cap = node.Capacity{Known: true, CPUCores: 8, MemAvailable: 12345}
	resolver["web1"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 2, MemAvailable: 999}

	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", Pool: "default"}); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}

	reach := newFakeReach()
	r.SetReachability(reach)

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	snap := r.HealthSnapshot()
	byName := map[string]NodeHealth{}
	for _, n := range snap.Nodes {
		byName[n.Name] = n
	}
	local, ok := byName["local"]
	if !ok {
		t.Fatalf("want local in snapshot, got %+v", snap.Nodes)
	}
	if local.Capacity.MemAvailable != 12345 {
		t.Errorf("local Capacity.MemAvailable = %d, want 12345", local.Capacity.MemAvailable)
	}
	web1, ok := byName["web1"]
	if !ok {
		t.Fatalf("want web1 in snapshot, got %+v", snap.Nodes)
	}
	if web1.Capacity.MemAvailable != 999 {
		t.Errorf("web1 Capacity.MemAvailable = %d, want 999", web1.Capacity.MemAvailable)
	}
}
