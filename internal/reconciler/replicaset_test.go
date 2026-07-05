package reconciler

import (
	"context"
	"fmt"
	"testing"

	"lwd/internal/node"
	"lwd/internal/store"
)

// TestDeploySingleReplicaUnchanged covers Phase 12 Task 4's non-regression
// contract: an app with Replicas == 1 (the default) must behave EXACTLY like
// today's single-surface blue-green deploy — one container named
// containerName(app, deployID) with NO index suffix, a single-upstream
// route, and a Replicas slice of length 1 whose lone entry's ContainerID
// matches the deployment's own ContainerID.
func TestDeploySingleReplicaUnchanged(t *testing.T) {
	r, f, fr, s := newTestReconciler(t)
	app := testApp() // Replicas: 1, Node: "local"
	app.Health.Path = "/healthz"
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	dep, err := r.Apply(context.Background(), app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if dep.Status != store.StatusRunning {
		t.Fatalf("status = %q, want running", dep.Status)
	}
	if !contains(f.Calls, "RunContainer:lwd-blog-1") {
		t.Fatalf("want a container named lwd-blog-1 (no index suffix for a single replica), calls: %v", f.Calls)
	}
	if contains(f.Calls, "RunContainer:lwd-blog-1-0") {
		t.Fatalf("N=1 must NOT append an index suffix (would change container naming for a single-replica app), calls: %v", f.Calls)
	}
	if len(dep.Replicas) != 1 {
		t.Fatalf("len(Replicas) = %d, want 1", len(dep.Replicas))
	}
	if dep.Replicas[0].ContainerID != dep.ContainerID {
		t.Errorf("Replicas[0].ContainerID = %q, want dep.ContainerID %q", dep.Replicas[0].ContainerID, dep.ContainerID)
	}
	if dep.Replicas[0].Node != "local" {
		t.Errorf("Replicas[0].Node = %q, want local", dep.Replicas[0].Node)
	}

	route, ok := fr.Routes["blog.example.com"]
	if !ok {
		t.Fatalf("want a live route for blog.example.com")
	}
	if len(route.Upstreams) != 1 {
		t.Fatalf("route.Upstreams = %+v, want exactly 1 (N=1 byte-identical route)", route.Upstreams)
	}

	cur, _ := s.CurrentDeployment("blog")
	if cur == nil || len(cur.Replicas) != 1 || cur.ContainerID != dep.ContainerID {
		t.Fatalf("CurrentDeployment mismatch: %+v", cur)
	}
}

// TestDeployThreeReplicasAllHealthy covers Phase 12 Task 4's core set-based
// deploy: an unpinned app with Replicas == 3 and 3 schedulable nodes must
// place all 3 replicas on distinct nodes, run 3 containers, health-gate all
// of them, and flip the live route to a 3-upstream set.
func TestDeployThreeReplicasAllHealthy(t *testing.T) {
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
	if dep.Status != store.StatusRunning {
		t.Fatalf("status = %q, want running", dep.Status)
	}
	if len(dep.Replicas) != 3 {
		t.Fatalf("len(Replicas) = %d, want 3, got %+v", len(dep.Replicas), dep.Replicas)
	}
	if dep.ContainerID != dep.Replicas[0].ContainerID {
		t.Errorf("ContainerID = %q, want Replicas[0].ContainerID %q", dep.ContainerID, dep.Replicas[0].ContainerID)
	}

	// Container IDs are only unique per-node (each *node.Fake has its own
	// independent id sequence, mirroring how real container IDs are only
	// unique within a single Docker daemon) — so the meaningful invariant is
	// 3 distinct NODES, each contributing a non-empty container id.
	seenNodes := map[string]bool{}
	for _, rep := range dep.Replicas {
		if seenNodes[rep.Node] {
			t.Fatalf("replicas = %+v, want 3 distinct nodes, duplicate %q", dep.Replicas, rep.Node)
		}
		seenNodes[rep.Node] = true
		if rep.ContainerID == "" {
			t.Errorf("replica on %q has empty ContainerID: %+v", rep.Node, dep.Replicas)
		}
	}
	if len(seenNodes) != 3 {
		t.Fatalf("replica nodes = %+v, want 3 distinct nodes", dep.Replicas)
	}

	route, ok := fr.Routes[app.Domain]
	if !ok {
		t.Fatalf("want a live route for %s", app.Domain)
	}
	if len(route.Upstreams) != 3 {
		t.Fatalf("route.Upstreams = %+v, want 3", route.Upstreams)
	}

	cur, _ := s.CurrentDeployment("blog")
	if cur == nil || len(cur.Replicas) != 3 {
		t.Fatalf("CurrentDeployment mismatch: %+v", cur)
	}
}

// TestDeployReplicaSetPartialUnhealthyFails covers Phase 12 Task 4's
// all-replicas-must-be-healthy gate: if even one replica in a new set fails
// its health check, the WHOLE generation is a failed deploy — every new
// container is torn down (on its own node), no route is ever set, and the
// deployment is recorded StatusFailed.
func TestDeployReplicaSetPartialUnhealthyFails(t *testing.T) {
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
	// web3 (the third replica placed, per the same most-free ordering as
	// TestDeployThreeReplicasAllHealthy) reports an immediate Docker
	// "unhealthy" status — checkDockerHealth fails fast on that, no polling
	// needed, keeping this test deterministic and quick.
	resolver["web3"].(*node.Fake).DockerHealth = "unhealthy"

	app := unpinnedApp("blog")
	app.Replicas = 3
	app.Health.Timeout = shortTimeout

	_, err := r.Apply(ctx, app)
	if err == nil {
		t.Fatal("Apply: want error when one replica's health check fails")
	}

	for _, name := range []string{"web1", "web2", "web3"} {
		fake := resolver[name].(*node.Fake)
		containers, lerr := fake.ListContainers(ctx, nil)
		if lerr != nil {
			t.Fatalf("ListContainers on %s: %v", name, lerr)
		}
		if len(containers) != 0 {
			t.Errorf("node %s still has containers after a failed replica-set deploy: %+v", name, containers)
		}
	}

	if _, ok := fr.Routes[app.Domain]; ok {
		t.Errorf("want no live route for %s after a failed deploy", app.Domain)
	}
	if fr.Staging["stage-1.lwd.internal"] {
		t.Errorf("want staging route removed after a failed deploy")
	}

	cur, _ := s.CurrentDeployment(app.Name)
	if cur != nil {
		t.Fatalf("want no current (running) deployment after a failed deploy, got %+v", cur)
	}

	history, err := s.DeploymentsForApp(app.Name)
	if err != nil {
		t.Fatalf("DeploymentsForApp: %v", err)
	}
	found := false
	for _, d := range history {
		if d.Status == store.StatusFailed {
			found = true
		}
	}
	if !found {
		t.Errorf("want a StatusFailed deployment recorded, history: %+v", history)
	}
}

// TestDeployReplicaSetRetiresOldSet covers Phase 12 Task 4's old-set
// retirement: redeploying a multi-replica app must remove EVERY old
// replica's container on ITS OWN node (P11b's cross-node lesson generalized
// to a set) — never all on the new/local node — while the new set goes
// live.
func TestDeployReplicaSetRetiresOldSet(t *testing.T) {
	r, resolver, _, fr, s := newSchedulingReconciler(t, "web1", "web2")
	ctx := context.Background()
	fr.ProbeStatus = 200

	resolver["web1"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 3000}
	resolver["web2"].(*node.Fake).Cap = node.Capacity{Known: true, CPUCores: 4, MemAvailable: 2000}
	for i, n := range []string{"web1", "web2"} {
		if err := s.AddNode(store.Node{Name: n, SSHHost: "deploy@" + n, MeshAddr: fmt.Sprintf("100.64.0.%d", i+2), Pool: "default"}); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}

	app := unpinnedApp("blog")
	app.Replicas = 2
	app.Health.Timeout = shortTimeout

	first, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if len(first.Replicas) != 2 {
		t.Fatalf("first.Replicas = %+v, want 2", first.Replicas)
	}
	oldByNode := map[string]string{}
	for _, rep := range first.Replicas {
		oldByNode[rep.Node] = rep.ContainerID
	}
	if len(oldByNode) != 2 {
		t.Fatalf("want the first generation spread across 2 distinct nodes, got %+v", first.Replicas)
	}

	second, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(second.Replicas) != 2 {
		t.Fatalf("second.Replicas = %+v, want 2", second.Replicas)
	}

	for nodeName, oldID := range oldByNode {
		fake := resolver[nodeName].(*node.Fake)
		if !contains(fake.Calls, "RemoveContainer:"+oldID) {
			t.Errorf("want RemoveContainer:%s on node %s (old replica removed on ITS OWN node), calls: %v", oldID, nodeName, fake.Calls)
		}
		containers, lerr := fake.ListContainers(ctx, nil)
		if lerr != nil {
			t.Fatalf("ListContainers on %s: %v", nodeName, lerr)
		}
		for _, c := range containers {
			if c.ID == oldID {
				t.Errorf("old container %s still present on %s after redeploy", oldID, nodeName)
			}
		}
	}

	route, ok := fr.Routes[app.Domain]
	if !ok {
		t.Fatalf("want a live route for %s", app.Domain)
	}
	if len(route.Upstreams) != 2 {
		t.Fatalf("route.Upstreams = %+v, want 2 (the new set)", route.Upstreams)
	}

	cur, _ := s.CurrentDeployment(app.Name)
	if cur == nil || cur.ID != second.ID {
		t.Fatalf("CurrentDeployment = %+v, want the second generation (id %d)", cur, second.ID)
	}
}
