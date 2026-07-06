package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"lwd/internal/node"
	"lwd/internal/spec"
	"lwd/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestBuildInitialRoutesMatchesCurrentDeployment verifies that a running
// surface container whose ID matches the store's recorded current deployment
// produces exactly one route, built from that deployment's spec snapshot.
func TestBuildInitialRoutesMatchesCurrentDeployment(t *testing.T) {
	ctx := context.Background()
	n := node.NewFake()
	s := openTestStore(t)

	c, err := n.RunContainer(ctx, node.RunSpec{
		Name:  "lwd-blog-1",
		Image: "img:1",
		Labels: map[string]string{
			"lwd.role": "surface",
			"lwd.app":  "blog",
		},
		Port: 8080,
	})
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}

	specJSON, err := json.Marshal(spec.App{Name: "blog", Image: "img:1", Domain: "blog.example.com", Port: 8080, Node: "local"})
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	if _, err := s.RecordDeployment(store.Deployment{
		App:         "blog",
		Image:       "img:1",
		ContainerID: c.ID,
		Status:      store.StatusRunning,
		CreatedAt:   time.Now(),
		Spec:        string(specJSON),
	}); err != nil {
		t.Fatalf("RecordDeployment: %v", err)
	}

	routes, err := buildInitialRoutes(ctx, n, s)
	if err != nil {
		t.Fatalf("buildInitialRoutes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("routes = %+v, want exactly 1", routes)
	}
	r := routes[0]
	if r.Domain != "blog.example.com" {
		t.Errorf("Domain = %q, want blog.example.com", r.Domain)
	}
	if len(r.Upstreams) != 1 || r.Upstreams[0].Host != "lwd-blog-1" {
		t.Errorf("Upstreams = %+v, want [{lwd-blog-1 8080}]", r.Upstreams)
	}
	if len(r.Upstreams) != 1 || r.Upstreams[0].Port != 8080 {
		t.Errorf("Port = %+v, want 8080", r.Upstreams)
	}
}

// TestBuildInitialRoutesSkipsStaleSurface verifies that a running surface
// container whose ID does NOT match the app's current recorded deployment
// (e.g. left over from an old, superseded deploy) is skipped rather than
// seeded as a route.
func TestBuildInitialRoutesSkipsStaleSurface(t *testing.T) {
	ctx := context.Background()
	n := node.NewFake()
	s := openTestStore(t)

	// Stale surface container: no matching current deployment references it.
	if _, err := n.RunContainer(ctx, node.RunSpec{
		Name:  "lwd-blog-0",
		Image: "img:0",
		Labels: map[string]string{
			"lwd.role": "surface",
			"lwd.app":  "blog",
		},
		Port: 8080,
	}); err != nil {
		t.Fatalf("RunContainer: %v", err)
	}

	// Record a current deployment that points at a different (non-existent)
	// container id, so the stale container above never matches it.
	specJSON, _ := json.Marshal(spec.App{Name: "blog", Image: "img:1", Domain: "blog.example.com", Port: 8080, Node: "local"})
	if _, err := s.RecordDeployment(store.Deployment{
		App:         "blog",
		Image:       "img:1",
		ContainerID: "some-other-container-id",
		Status:      store.StatusRunning,
		CreatedAt:   time.Now(),
		Spec:        string(specJSON),
	}); err != nil {
		t.Fatalf("RecordDeployment: %v", err)
	}

	routes, err := buildInitialRoutes(ctx, n, s)
	if err != nil {
		t.Fatalf("buildInitialRoutes: %v", err)
	}
	if len(routes) != 0 {
		t.Fatalf("routes = %+v, want none (stale surface should be skipped)", routes)
	}
}

// TestBuildInitialRoutesSkipsAppWithNoCurrentDeployment verifies that a
// surface container for an app with no recorded current (running) deployment
// at all is skipped.
func TestBuildInitialRoutesSkipsAppWithNoCurrentDeployment(t *testing.T) {
	ctx := context.Background()
	n := node.NewFake()
	s := openTestStore(t)

	if _, err := n.RunContainer(ctx, node.RunSpec{
		Name:  "lwd-orphan-1",
		Image: "img:1",
		Labels: map[string]string{
			"lwd.role": "surface",
			"lwd.app":  "orphan",
		},
		Port: 8080,
	}); err != nil {
		t.Fatalf("RunContainer: %v", err)
	}

	routes, err := buildInitialRoutes(ctx, n, s)
	if err != nil {
		t.Fatalf("buildInitialRoutes: %v", err)
	}
	if len(routes) != 0 {
		t.Fatalf("routes = %+v, want none", routes)
	}
}

// TestBuildInitialRoutesMultiReplica covers the P12 final-review FIX 2: a
// current deployment recorded with a multi-entry Replicas set (Phase 12)
// must seed a route carrying EVERY replica's upstream, not just the one
// whose container happens to match ContainerID on the local node — the old
// container-matching path can only ever produce a single upstream, and can
// never see a replica running on a remote node at all. It also verifies a
// legacy row (empty Replicas, ContainerID set) still falls back to the
// original single-upstream, container-matching behavior unchanged.
func TestBuildInitialRoutesMultiReplica(t *testing.T) {
	ctx := context.Background()
	n := node.NewFake()
	s := openTestStore(t)

	// Phase 12 app: 3 replicas recorded, spread across local + two remote
	// nodes. Only the anchor (replica 0) has a real local container; replica
	// 1/2's "containers" are remote and deliberately absent from n's
	// ListContainers — buildInitialRoutes must seed their upstreams from the
	// recorded Replicas anyway, not from the local container list.
	anchor, err := n.RunContainer(ctx, node.RunSpec{
		Name:  "lwd-blog-1",
		Image: "img:1",
		Labels: map[string]string{
			"lwd.role": "surface",
			"lwd.app":  "blog",
		},
		Port: 8080,
	})
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}

	specJSON, err := json.Marshal(spec.App{Name: "blog", Image: "img:1", Domain: "blog.example.com", Port: 8080, Replicas: 3})
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	replicas := []store.Replica{
		{ContainerID: anchor.ID, Node: "local", Upstream: "lwd-blog-1", Port: 8080},
		{ContainerID: "remote-web1-container", Node: "web1", Upstream: "100.64.0.2", Port: 41000},
		{ContainerID: "remote-web2-container", Node: "web2", Upstream: "100.64.0.3", Port: 41001},
	}
	if _, err := s.RecordDeployment(store.Deployment{
		App:         "blog",
		Image:       "img:1",
		ContainerID: anchor.ID,
		Status:      store.StatusRunning,
		CreatedAt:   time.Now(),
		Spec:        string(specJSON),
		Scheduled:   true,
		Replicas:    replicas,
	}); err != nil {
		t.Fatalf("RecordDeployment: %v", err)
	}

	routes, err := buildInitialRoutes(ctx, n, s)
	if err != nil {
		t.Fatalf("buildInitialRoutes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("routes = %+v, want exactly 1", routes)
	}
	r := routes[0]
	if r.Domain != "blog.example.com" {
		t.Errorf("Domain = %q, want blog.example.com", r.Domain)
	}
	if len(r.Upstreams) != 3 {
		t.Fatalf("Upstreams = %+v, want 3 (one per replica)", r.Upstreams)
	}
	want := map[string]int{"lwd-blog-1": 8080, "100.64.0.2": 41000, "100.64.0.3": 41001}
	for _, u := range r.Upstreams {
		wantPort, ok := want[u.Host]
		if !ok {
			t.Errorf("unexpected upstream %+v", u)
			continue
		}
		if u.Port != wantPort {
			t.Errorf("upstream %s port = %d, want %d", u.Host, u.Port, wantPort)
		}
		delete(want, u.Host)
	}
	if len(want) != 0 {
		t.Errorf("missing upstreams for: %+v", want)
	}
}

// TestBuildInitialRoutesLegacySingleReplica covers the fallback half of FIX
// 2: a legacy (pre-Phase-12) row with no recorded Replicas must still seed a
// single upstream from the matching running local container, unchanged.
func TestBuildInitialRoutesLegacySingleReplica(t *testing.T) {
	ctx := context.Background()
	n := node.NewFake()
	s := openTestStore(t)

	c, err := n.RunContainer(ctx, node.RunSpec{
		Name:  "lwd-blog-1",
		Image: "img:1",
		Labels: map[string]string{
			"lwd.role": "surface",
			"lwd.app":  "blog",
		},
		Port: 8080,
	})
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}

	specJSON, err := json.Marshal(spec.App{Name: "blog", Image: "img:1", Domain: "blog.example.com", Port: 8080, Node: "local"})
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	// No Replicas set: simulates a pre-Phase-12 row.
	if _, err := s.RecordDeployment(store.Deployment{
		App:         "blog",
		Image:       "img:1",
		ContainerID: c.ID,
		Status:      store.StatusRunning,
		CreatedAt:   time.Now(),
		Spec:        string(specJSON),
	}); err != nil {
		t.Fatalf("RecordDeployment: %v", err)
	}

	routes, err := buildInitialRoutes(ctx, n, s)
	if err != nil {
		t.Fatalf("buildInitialRoutes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("routes = %+v, want exactly 1", routes)
	}
	r := routes[0]
	if len(r.Upstreams) != 1 || r.Upstreams[0].Host != "lwd-blog-1" || r.Upstreams[0].Port != 8080 {
		t.Errorf("Upstreams = %+v, want [{lwd-blog-1 8080}]", r.Upstreams)
	}
}
