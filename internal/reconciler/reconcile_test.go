package reconciler

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"lwd/internal/store"
)

// errBoom is a canned error used by tests that need RunContainer (or another
// fake dependency) to fail deterministically, e.g. to exercise the heal
// backoff/give-up path.
var errBoom = errors.New("boom")

// fakeReach is a test double for Reachability: it returns a per-name
// canned (transport, ok) result, defaulting to ("local", true) for any name
// not explicitly configured via Set, so tests that don't care about a
// particular node's reachability don't need to configure every name Reconcile
// happens to probe. Calls are recorded so tests can assert which names were
// probed.
type fakeReach struct {
	mu      sync.Mutex
	results map[string]reachResult
	Calls   []string
}

type reachResult struct {
	transport string
	ok        bool
}

func newFakeReach() *fakeReach {
	return &fakeReach{results: map[string]reachResult{}}
}

// Set configures the (transport, ok) result Reachable returns for name.
func (f *fakeReach) Set(name, transport string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[name] = reachResult{transport: transport, ok: ok}
}

func (f *fakeReach) Reachable(ctx context.Context, name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, name)
	if r, ok := f.results[name]; ok {
		return r.transport, r.ok
	}
	return "local", true
}

// countPrefix counts how many entries in calls start with prefix.
func countPrefix(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if hasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// TestReconcileHealsDeadSurface covers the happy self-heal path: a currently
// running image-app deployment whose container the fake node now reports
// "exited" (dead) is healed by Reconcile into a brand-new surface — a new
// container, the live route re-set, the old row retired, and a fresh
// StatusRunning row recorded — and the resulting health snapshot marks the
// app SurfaceHealthy.
func TestReconcileHealsDeadSurface(t *testing.T) {
	r, f, fr, s := newTestReconciler(t)
	ctx := context.Background()
	app := testApp()
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("initial Apply: %v", err)
	}

	reach := newFakeReach()
	reach.Set("local", "local", true)
	r.SetReachability(reach)

	// Simulate the current container having died: remove it from the fake
	// node's tracked items so ContainerHealth reports it absent (dead).
	// node.Fake.HealthState, if set, applies globally to every container —
	// including the brand-new one heal creates — so removing the specific
	// dead container is used instead, leaving the healed container's own
	// (default "running") health observation untouched.
	if err := f.RemoveContainer(ctx, dep.ContainerID); err != nil {
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
	if cur.ContainerID == dep.ContainerID {
		t.Errorf("want a brand-new surface container after heal, got same id %q", cur.ContainerID)
	}
	if cur.Status != "running" {
		t.Errorf("current deployment status = %q, want running", cur.Status)
	}

	route, ok := fr.Routes[app.Domain]
	if !ok {
		t.Fatalf("want a live route for %q after heal", app.Domain)
	}
	if len(route.Upstreams) == 0 || route.Upstreams[0].Host == "" {
		t.Errorf("want live route upstream set, got %+v", route)
	}

	snap := r.HealthSnapshot()
	var found bool
	for _, ah := range snap.Apps {
		if ah.App == app.Name {
			found = true
			if ah.State != SurfaceHealthy {
				t.Errorf("app health state = %q, want healthy", ah.State)
			}
		}
	}
	if !found {
		t.Fatalf("want %q present in health snapshot Apps, got %+v", app.Name, snap.Apps)
	}
}

// TestReconcileHealthyNoop covers the no-op path: a running, healthy
// container must not be touched — no new container run, no route changed —
// and the snapshot reports the app healthy with an empty heal map.
func TestReconcileHealthyNoop(t *testing.T) {
	r, f, fr, s := newTestReconciler(t)
	ctx := context.Background()
	app := testApp()
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	if _, err := r.Apply(ctx, app); err != nil {
		t.Fatalf("initial Apply: %v", err)
	}

	reach := newFakeReach()
	r.SetReachability(reach)
	f.HealthState = "running"

	preRun := countPrefix(f.Calls, "RunContainer:")
	preRoute := fr.Routes[app.Domain]

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	postRun := countPrefix(f.Calls, "RunContainer:")
	if postRun != preRun {
		t.Errorf("want no new RunContainer call on healthy noop, pre=%d post=%d calls=%v", preRun, postRun, f.Calls)
	}
	if !reflect.DeepEqual(fr.Routes[app.Domain], preRoute) {
		t.Errorf("want route unchanged on healthy noop, got %+v want %+v", fr.Routes[app.Domain], preRoute)
	}

	snap := r.HealthSnapshot()
	var found bool
	for _, ah := range snap.Apps {
		if ah.App == app.Name {
			found = true
			if ah.State != SurfaceHealthy {
				t.Errorf("app health state = %q, want healthy", ah.State)
			}
			if ah.HealAttempts != 0 {
				t.Errorf("heal attempts = %d, want 0", ah.HealAttempts)
			}
		}
	}
	if !found {
		t.Fatalf("want %q present in health snapshot Apps", app.Name)
	}

	r.mu.Lock()
	_, exists := r.heal[app.Name]
	r.mu.Unlock()
	if exists {
		t.Errorf("want heal map empty for healthy app")
	}

	_ = s
}

// TestReconcileSkipsCompose covers that a compose app's current deployment is
// entirely skipped by the surface reconciler: no AppHealth entry is produced
// for it, and no heal is attempted.
func TestReconcileSkipsCompose(t *testing.T) {
	r, _, fr, _, cf := newTestReconcilerWithCompose(t)
	ctx := context.Background()
	app := testComposeApp(t, "services:\n  web:\n    image: nginx\n")
	fr.ProbeStatus = 200
	cf.ServiceID = "compose-container-1"
	cf.ServiceName = "webapp-web-1"

	if _, err := r.Apply(ctx, app); err != nil {
		t.Fatalf("initial Apply: %v", err)
	}

	reach := newFakeReach()
	r.SetReachability(reach)

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	snap := r.HealthSnapshot()
	for _, ah := range snap.Apps {
		if ah.App == app.Name {
			t.Fatalf("want compose app %q absent from health snapshot Apps, got %+v", app.Name, ah)
		}
	}
}

// TestReconcileNodeUnreachableDegraded covers that when the app's node is
// reported unreachable, Reconcile marks the app degraded WITHOUT attempting a
// container heal (no RunContainer call).
func TestReconcileNodeUnreachableDegraded(t *testing.T) {
	r, f, fr, _ := newTestReconciler(t)
	ctx := context.Background()
	app := testApp()
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	if _, err := r.Apply(ctx, app); err != nil {
		t.Fatalf("initial Apply: %v", err)
	}

	reach := newFakeReach()
	reach.Set("local", "local", false)
	r.SetReachability(reach)
	f.HealthState = "exited" // dead, but node unreachable must short-circuit before heal

	preRun := countPrefix(f.Calls, "RunContainer:")

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	postRun := countPrefix(f.Calls, "RunContainer:")
	if postRun != preRun {
		t.Errorf("want no RunContainer call when node unreachable, pre=%d post=%d calls=%v", preRun, postRun, f.Calls)
	}

	snap := r.HealthSnapshot()
	var found bool
	for _, ah := range snap.Apps {
		if ah.App == app.Name {
			found = true
			if ah.State != SurfaceDegraded {
				t.Errorf("app health state = %q, want degraded", ah.State)
			}
		}
	}
	if !found {
		t.Fatalf("want %q present in health snapshot Apps", app.Name)
	}
}

// TestReconcileBackoffAndGiveUp covers the heal backoff/give-up state
// machine: a surface that keeps failing to heal accumulates attempts gated by
// a backoff window, and once LWD_HEAL_MAX_ATTEMPTS is reached the app is
// reported SurfaceFailed ("gave up") without further attempts. A subsequent
// successful manual Apply clears the heal bookkeeping.
func TestReconcileBackoffAndGiveUp(t *testing.T) {
	t.Setenv("LWD_HEAL_MAX_ATTEMPTS", "2")

	r, f, fr, _ := newTestReconciler(t)
	ctx := context.Background()
	app := testApp()
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("initial Apply: %v", err)
	}

	reach := newFakeReach()
	r.SetReachability(reach)
	// Simulate the current container having died (see
	// TestReconcileHealsDeadSurface for why RemoveContainer, not
	// HealthState, is used).
	if err := f.RemoveContainer(ctx, dep.ContainerID); err != nil {
		t.Fatalf("RemoveContainer (simulate death): %v", err)
	}
	f.RunErr = errBoom // every heal attempt fails to run a new container

	// Attempt 1: fails, records nextEligible backoff.
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}
	snap := r.HealthSnapshot()
	ah := findAppHealth(t, snap, app.Name)
	if ah.State != SurfaceFailed {
		t.Errorf("after attempt 1: state = %q, want failed", ah.State)
	}
	if ah.HealAttempts != 1 {
		t.Errorf("after attempt 1: heal attempts = %d, want 1", ah.HealAttempts)
	}

	// Immediate second call: gated by backoff, must not increment attempts.
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile #2 (backoff): %v", err)
	}
	snap = r.HealthSnapshot()
	ah = findAppHealth(t, snap, app.Name)
	if ah.State != SurfaceDegraded {
		t.Errorf("during backoff: state = %q, want degraded", ah.State)
	}
	if ah.HealAttempts != 1 {
		t.Errorf("during backoff: heal attempts = %d, want unchanged 1", ah.HealAttempts)
	}

	// Force the backoff window open (white-box: same package) so the next
	// call attempts a heal immediately instead of waiting out real time.
	r.mu.Lock()
	if hs := r.heal[app.Name]; hs != nil {
		hs.nextEligible = time.Time{}
	}
	r.mu.Unlock()

	// Attempt 2: fails, reaches max attempts.
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile #3 (attempt 2): %v", err)
	}
	snap = r.HealthSnapshot()
	ah = findAppHealth(t, snap, app.Name)
	if ah.State != SurfaceFailed {
		t.Errorf("after attempt 2: state = %q, want failed", ah.State)
	}
	if ah.HealAttempts != 2 {
		t.Errorf("after attempt 2: heal attempts = %d, want 2", ah.HealAttempts)
	}

	// Force the backoff window open again, then reconcile once more: this
	// time attempts (2) >= max (2), so it must give up WITHOUT another
	// RunContainer call.
	r.mu.Lock()
	if hs := r.heal[app.Name]; hs != nil {
		hs.nextEligible = time.Time{}
	}
	r.mu.Unlock()
	preRun := countPrefix(f.Calls, "RunContainer:")
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile #4 (give up): %v", err)
	}
	postRun := countPrefix(f.Calls, "RunContainer:")
	if postRun != preRun {
		t.Errorf("want no further RunContainer call after giving up, pre=%d post=%d", preRun, postRun)
	}
	snap = r.HealthSnapshot()
	ah = findAppHealth(t, snap, app.Name)
	if ah.State != SurfaceFailed {
		t.Errorf("after giving up: state = %q, want failed", ah.State)
	}
	if ah.LastError == "" {
		t.Errorf("after giving up: want a non-empty LastError")
	}

	// A successful manual Apply clears the heal bookkeeping.
	f.RunErr = nil
	if _, err := r.Apply(ctx, app); err != nil {
		t.Fatalf("recovering Apply: %v", err)
	}
	r.mu.Lock()
	_, exists := r.heal[app.Name]
	r.mu.Unlock()
	if exists {
		t.Errorf("want heal map cleared for %q after a successful Apply", app.Name)
	}
}

// TestReconcileSnapshotNodesAndEdge covers probeNodes/probeEdge: the health
// snapshot includes "local" plus every registered node with its reported
// reachability, and Edge.Reachable reflects the router's Healthy() result.
func TestReconcileSnapshotNodesAndEdge(t *testing.T) {
	r, _, fr, s := newTestReconciler(t)
	ctx := context.Background()

	if err := s.AddNode(store.Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2"}); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	reach := newFakeReach()
	reach.Set("local", "local", true)
	reach.Set("web1", "ssh", true)
	r.SetReachability(reach)
	fr.HealthyResult = false

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	snap := r.HealthSnapshot()
	if snap.Edge.Reachable {
		t.Errorf("Edge.Reachable = true, want false (router.Healthy returned false)")
	}

	byName := map[string]NodeHealth{}
	for _, n := range snap.Nodes {
		byName[n.Name] = n
	}
	local, ok := byName["local"]
	if !ok {
		t.Fatalf("want 'local' present in Nodes, got %+v", snap.Nodes)
	}
	if !local.Reachable || local.Transport != "local" {
		t.Errorf("local node = %+v, want Reachable=true Transport=local", local)
	}
	web1, ok := byName["web1"]
	if !ok {
		t.Fatalf("want 'web1' present in Nodes, got %+v", snap.Nodes)
	}
	if !web1.Reachable || web1.Transport != "ssh" {
		t.Errorf("web1 node = %+v, want Reachable=true Transport=ssh", web1)
	}
}

// TestReconcileConcurrentWithApply runs Reconcile and Apply from separate
// goroutines under -race to prove the reconciler's lock discipline: Apply
// (which takes r.mu for its whole duration) and Reconcile (whose reconcileApp
// probes are lock-free, only tryHeal briefly taking r.mu) never race on
// shared state and never deadlock.
func TestReconcileConcurrentWithApply(t *testing.T) {
	r, f, fr, _ := newTestReconciler(t)
	ctx := context.Background()
	app := testApp()
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	if _, err := r.Apply(ctx, app); err != nil {
		t.Fatalf("initial Apply: %v", err)
	}

	reach := newFakeReach()
	r.SetReachability(reach)
	_ = f

	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = r.Reconcile(ctx)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			_, _ = r.Apply(ctx, app)
		}
	}()
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out — possible deadlock between Reconcile and Apply")
	}
}

// TestTryHealSkipsSupersededDeploy covers the FIX 1 TOCTOU guard: tryHeal is
// handed a *store.Deployment snapshot (cur) that reconcileApp read WITHOUT
// r.mu held. If a manual Apply lands in the window between that read and
// tryHeal's lock — landing a v2 deployment while tryHeal is still holding a
// stale v1 snapshot — tryHeal must re-check under the lock and skip rather
// than healing (redeploying) the superseded v1, which would silently undo
// the v2 the operator just shipped.
func TestTryHealSkipsSupersededDeploy(t *testing.T) {
	r, f, _, s := newTestReconciler(t)
	ctx := context.Background()
	app := testApp()
	app.Health.Timeout = shortTimeout

	if _, err := r.Apply(ctx, app); err != nil {
		t.Fatalf("initial Apply (v1): %v", err)
	}
	v1cur, err := s.CurrentDeployment(app.Name)
	if err != nil || v1cur == nil {
		t.Fatalf("CurrentDeployment after v1 Apply: %v, %+v", err, v1cur)
	}

	// A manual Apply(v2) supersedes v1 — simulating it landing in the window
	// between reconcileApp's lock-free read of v1cur and tryHeal's r.mu.Lock.
	app2 := testApp()
	app2.Image = "img:2"
	if _, err := r.Apply(ctx, app2); err != nil {
		t.Fatalf("second Apply (v2): %v", err)
	}
	v2cur, err := s.CurrentDeployment(app.Name)
	if err != nil || v2cur == nil {
		t.Fatalf("CurrentDeployment after v2 Apply: %v, %+v", err, v2cur)
	}
	if v2cur.ID == v1cur.ID {
		t.Fatalf("want v2 to be a new deployment row, got same ID %d", v2cur.ID)
	}

	preRun := countPrefix(f.Calls, "RunContainer:")

	// Call tryHeal directly with the stale v1cur snapshot, exactly as
	// reconcileApp would if it had observed v1's container dead before the
	// v2 Apply landed. tryHeal takes r.mu itself (for its whole body), so it
	// must NOT already be held here — that would deadlock.
	ah := r.tryHeal(ctx, app.Name, v1cur)

	if ah != nil {
		t.Errorf("tryHeal(stale v1cur) = %+v, want nil (superseded heal skipped)", ah)
	}

	postRun := countPrefix(f.Calls, "RunContainer:")
	if postRun != preRun {
		t.Errorf("want no RunContainer call from a superseded heal, pre=%d post=%d calls=%v", preRun, postRun, f.Calls)
	}

	after, err := s.CurrentDeployment(app.Name)
	if err != nil {
		t.Fatalf("CurrentDeployment after tryHeal: %v", err)
	}
	if after == nil || after.ID != v2cur.ID || after.Image != "img:2" {
		t.Errorf("want current deployment unchanged from v2 %+v, got %+v", v2cur, after)
	}
}

// TestReconcileDoesNotResurrectRemovedApp covers the FIX 1 TOCTOU guard from
// the Remove side: if an app's surface dies and, before the reconciler gets
// to it, the app is removed (`lwd rm`) — retiring its current deployment row
// entirely — a subsequent Reconcile pass must not resurrect it by running a
// brand-new container for a deployment that no longer exists.
func TestReconcileDoesNotResurrectRemovedApp(t *testing.T) {
	r, f, fr, s := newTestReconciler(t)
	ctx := context.Background()
	app := testApp()
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("initial Apply: %v", err)
	}

	reach := newFakeReach()
	r.SetReachability(reach)

	// Simulate the current container having died (see
	// TestReconcileHealsDeadSurface for why RemoveContainer, not
	// HealthState, is used).
	if err := f.RemoveContainer(ctx, dep.ContainerID); err != nil {
		t.Fatalf("RemoveContainer (simulate death): %v", err)
	}

	// The app is removed before the reconciler observes/heals the dead
	// container — retiring its current deployment row.
	if err := r.Remove(ctx, app.Name); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	preRun := countPrefix(f.Calls, "RunContainer:")

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	postRun := countPrefix(f.Calls, "RunContainer:")
	if postRun != preRun {
		t.Errorf("want no RunContainer call resurrecting a removed app, pre=%d post=%d calls=%v", preRun, postRun, f.Calls)
	}

	cur, err := s.CurrentDeployment(app.Name)
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur != nil {
		t.Errorf("want no current deployment after Remove+Reconcile (not resurrected), got %+v", cur)
	}

	if _, ok := fr.Routes[app.Domain]; ok {
		t.Errorf("want no live route for removed app %q after Reconcile, got %+v", app.Domain, fr.Routes[app.Domain])
	}
}

func findAppHealth(t *testing.T, h Health, app string) AppHealth {
	t.Helper()
	for _, ah := range h.Apps {
		if ah.App == app {
			return ah
		}
	}
	t.Fatalf("no AppHealth found for %q in %+v", app, h.Apps)
	return AppHealth{}
}
