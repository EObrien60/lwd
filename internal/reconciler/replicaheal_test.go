package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"lwd/internal/node"
	"lwd/internal/store"
)

// threeNodeReplicaApp sets up a scheduling reconciler with three distinct,
// differently-sized nodes (so an unpinned 3-replica app spreads one replica
// per node, exactly like TestDeployThreeReplicasAllHealthy), applies it, and
// returns the reconciler plus the resulting deployment. Shared by the
// per-replica heal and remove-all-replicas tests below.
func threeNodeReplicaApp(t *testing.T) (*Reconciler, node.FakeResolver, *store.Store, *store.Deployment) {
	t.Helper()
	r, resolver, _, fr, s := newSchedulingReconciler(t, "web1", "web2", "web3")
	ctx := context.Background()
	fr.ProbeStatus = 200

	resolver["web1"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 3000}
	resolver["web2"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 2000}
	resolver["web3"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 1000}
	for i, n := range []string{"web1", "web2", "web3"} {
		if err := s.AddNode(store.Node{Name: n, SSHHost: "deploy@" + n, MeshAddr: fmt.Sprintf("100.64.0.%d", i+2), Pool: "default"}); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}

	app := unpinnedApp("blog")
	app.Replicas = 3
	app.Health.Timeout = shortTimeout

	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(dep.Replicas) != 3 {
		t.Fatalf("Replicas = %+v, want 3", dep.Replicas)
	}
	seen := map[string]bool{}
	for _, rep := range dep.Replicas {
		seen[rep.Node] = true
	}
	if len(seen) != 3 {
		t.Fatalf("want the 3 replicas spread across 3 distinct nodes, got %+v", dep.Replicas)
	}

	return r, resolver, s, dep
}

// TestHealRecreatesOneDeadReplica covers Phase 12 Task 5's core per-replica
// heal contract: of a healthy 3-replica app, killing ONE replica's container
// must recreate ONLY that replica (a fresh RunContainer on its own node) —
// the other two replicas' containers are never touched — and the live
// route's upstream set is updated to reflect the swap while keeping all 3
// entries.
func TestHealRecreatesOneDeadReplica(t *testing.T) {
	r, resolver, s, dep := threeNodeReplicaApp(t)
	ctx := context.Background()
	app := "blog"

	dead := dep.Replicas[1]
	deadNode := resolver[dead.Node].(*node.Fake)
	if err := deadNode.RemoveContainer(ctx, dead.ContainerID); err != nil {
		t.Fatalf("RemoveContainer (simulate death): %v", err)
	}

	preRun := map[string]int{}
	for name, n := range resolver {
		preRun[name] = countPrefix(n.(*node.Fake).Calls, "RunContainer:")
	}

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	for name, n := range resolver {
		post := countPrefix(n.(*node.Fake).Calls, "RunContainer:")
		if name == dead.Node {
			if post != preRun[name]+1 {
				t.Errorf("node %s: RunContainer calls = %d, want %d+1 (healed replica)", name, post, preRun[name])
			}
		} else if post != preRun[name] {
			t.Errorf("node %s: RunContainer calls = %d, want unchanged %d (healthy replica untouched)", name, post, preRun[name])
		}
	}

	cur, err := s.CurrentDeployment(app)
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil || len(cur.Replicas) != 3 {
		t.Fatalf("CurrentDeployment mismatch: %+v", cur)
	}
	if cur.ID != dep.ID {
		t.Errorf("want the SAME deployment row updated in place (id %d), got %d", dep.ID, cur.ID)
	}
	for i, rep := range cur.Replicas {
		if i == 1 {
			if rep.ContainerID == dead.ContainerID {
				t.Errorf("replica 1 = %+v, want a NEW container id", rep)
			}
			if rep.Node != dead.Node {
				t.Errorf("replica 1 node = %q, want unchanged %q (recreated IN PLACE)", rep.Node, dead.Node)
			}
		} else if rep != dep.Replicas[i] {
			t.Errorf("replica %d changed: got %+v, want untouched %+v", i, rep, dep.Replicas[i])
		}
	}

	snap := r.HealthSnapshot()
	ah := findAppHealth(t, snap, app)
	if ah.State != SurfaceHealthy {
		t.Errorf("app health state = %q, want healthy after heal", ah.State)
	}
}

// TestHealAllReplicasDead covers the other end of the per-replica heal
// contract: if EVERY replica dies at once, every one is still recreated
// individually (never via a whole-set blue-green redeploy) — each on its
// original node, with the deployment row updated in place.
func TestHealAllReplicasDead(t *testing.T) {
	r, resolver, s, dep := threeNodeReplicaApp(t)
	ctx := context.Background()

	for _, rep := range dep.Replicas {
		fake := resolver[rep.Node].(*node.Fake)
		if err := fake.RemoveContainer(ctx, rep.ContainerID); err != nil {
			t.Fatalf("RemoveContainer(%s) on %s: %v", rep.ContainerID, rep.Node, err)
		}
	}

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil || len(cur.Replicas) != 3 {
		t.Fatalf("CurrentDeployment mismatch: %+v", cur)
	}
	if cur.ID != dep.ID {
		t.Errorf("want the SAME deployment row updated in place (id %d), got %d", dep.ID, cur.ID)
	}
	for i, rep := range cur.Replicas {
		if rep.Node != dep.Replicas[i].Node {
			t.Errorf("replica %d node changed: got %q, want %q", i, rep.Node, dep.Replicas[i].Node)
		}
		if rep.ContainerID == dep.Replicas[i].ContainerID {
			t.Errorf("replica %d not recreated: still %q", i, rep.ContainerID)
		}
	}

	snap := r.HealthSnapshot()
	ah := findAppHealth(t, snap, "blog")
	if ah.State != SurfaceHealthy {
		t.Errorf("app health state = %q, want healthy after healing all replicas", ah.State)
	}
}

// TestHealLegacySingleContainer covers the non-regression fallback: a
// deployment row with an EMPTY Replicas (simulating a pre-Phase-12 row that
// predates deployReplicaSet always populating it) but a populated
// ContainerID must still be healed via the original single-container
// blue-green path — a brand-new deployment ROW recorded (not an in-place
// Replicas patch), exactly as it always has.
func TestHealLegacySingleContainer(t *testing.T) {
	r, f, fr, s := newTestReconciler(t)
	ctx := context.Background()
	app := testApp()
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	specJSON, err := json.Marshal(app)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}

	c, err := f.RunContainer(ctx, node.RunSpec{Name: "lwd-blog-legacy", Image: app.Image, Port: app.Port, Network: lwdNetwork})
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}

	legacyID, err := s.RecordDeployment(store.Deployment{
		App:         app.Name,
		Image:       app.Image,
		ContainerID: c.ID,
		Status:      store.StatusRunning,
		CreatedAt:   time.Now(),
		Spec:        string(specJSON),
		// Replicas intentionally left empty: simulates a pre-Phase-12 row.
	})
	if err != nil {
		t.Fatalf("RecordDeployment: %v", err)
	}

	reach := newFakeReach()
	reach.Set("local", "local", true)
	r.SetReachability(reach)

	if err := f.RemoveContainer(ctx, c.ID); err != nil {
		t.Fatalf("RemoveContainer (simulate death): %v", err)
	}

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cur, err := s.CurrentDeployment(app.Name)
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil {
		t.Fatalf("want a current deployment after heal")
	}
	if cur.ID == legacyID {
		t.Errorf("want a brand-new deployment ROW from the legacy blue-green heal path, got the same id %d back", cur.ID)
	}
	if cur.ContainerID == c.ID {
		t.Errorf("want a brand-new container after heal")
	}
	if cur.Status != store.StatusRunning {
		t.Errorf("status = %q, want running", cur.Status)
	}

	route, ok := fr.Routes[app.Domain]
	if !ok {
		t.Fatalf("want a live route for %q after heal", app.Domain)
	}
	if len(route.Upstreams) == 0 {
		t.Errorf("want a non-empty upstream set after heal")
	}
}

// TestRemoveAllReplicas covers Phase 12 Task 5's Remove fix: a 3-replica app
// spread across 3 distinct nodes must have EVERY replica's container removed
// on ITS OWN node, not just the anchor's — the pre-fix removeSingleService
// only removed by label on the single node named by the spec snapshot (the
// anchor), silently orphaning every other replica.
func TestRemoveAllReplicas(t *testing.T) {
	r, resolver, s, dep := threeNodeReplicaApp(t)
	ctx := context.Background()

	if err := r.Remove(ctx, "blog"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	for _, rep := range dep.Replicas {
		fake := resolver[rep.Node].(*node.Fake)
		if !contains(fake.Calls, "RemoveContainer:"+rep.ContainerID) {
			t.Errorf("want RemoveContainer:%s on node %s, calls: %v", rep.ContainerID, rep.Node, fake.Calls)
		}
		containers, err := fake.ListContainers(ctx, nil)
		if err != nil {
			t.Fatalf("ListContainers on %s: %v", rep.Node, err)
		}
		for _, c := range containers {
			if c.ID == rep.ContainerID {
				t.Errorf("container %s still present on %s after Remove", rep.ContainerID, rep.Node)
			}
		}
	}

	cur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur != nil {
		t.Errorf("want no current deployment after Remove, got %+v", cur)
	}
}

// TestRollbackRestoresReplicaCount covers Phase 12 Task 5's rollback
// contract: scaling an app from 1 replica to 3 and then rolling back must
// restore the PRIOR generation's replica count (1), not the just-superseded
// one (3) — this falls out of Rollback restoring the previous deployment's
// Spec snapshot (whose Replicas field is exactly what was recorded at that
// generation) and redeploying it through deployReplicaSet, which sizes the
// new set from app.Replicas.
func TestRollbackRestoresReplicaCount(t *testing.T) {
	r, _, fr, s := newTestReconciler(t)
	ctx := context.Background()
	app := testApp() // Replicas: 1, Node: "local"
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	v1, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("v1 Apply: %v", err)
	}
	if len(v1.Replicas) != 1 {
		t.Fatalf("sanity: v1.Replicas = %+v, want 1", v1.Replicas)
	}

	app.Replicas = 3
	v2, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("v2 Apply: %v", err)
	}
	if len(v2.Replicas) != 3 {
		t.Fatalf("sanity: v2.Replicas = %+v, want 3", v2.Replicas)
	}

	back, err := r.Rollback(ctx, "blog")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(back.Replicas) != 1 {
		t.Errorf("Rollback Replicas = %+v, want 1 (prior generation's count restored)", back.Replicas)
	}
	if back.Status != store.StatusRunning {
		t.Errorf("Rollback status = %q, want running", back.Status)
	}

	route, ok := fr.Routes[app.Domain]
	if !ok {
		t.Fatalf("want a live route for %s", app.Domain)
	}
	if len(route.Upstreams) != 1 {
		t.Errorf("route.Upstreams = %+v, want 1 (rolled back to a single replica)", route.Upstreams)
	}

	cur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil || len(cur.Replicas) != 1 {
		t.Fatalf("CurrentDeployment mismatch: %+v", cur)
	}
}
