package reconciler

import (
	"context"
	"fmt"
	"log"
	"time"

	"lwd/internal/config"
	"lwd/internal/node"
	"lwd/internal/store"
)

// healBackoffBase is the initial (attempt 1) backoff delay a failed heal
// attempt imposes before the next attempt is eligible; healBackoffMax caps
// the exponential growth of subsequent attempts.
const (
	healBackoffBase = 15 * time.Second
	healBackoffMax  = 5 * time.Minute
)

// Reconcile runs one pass of the continuous reconciler loop: for every app
// the store knows about, it observes (and, if dead, attempts to self-heal)
// that app's current surface via reconcileApp, then stores a fresh Health
// snapshot combining the collected per-app health with a probe of every
// registered node's reachability and the shared edge (router)'s health.
// Finally (Phase 11b Task 5), it runs failoverLostNodes to automatically
// evacuate any registered node that has been unreachable past
// config.FailoverGrace().
//
// A per-app error (an unresolvable node, a failed heal attempt, ...) is
// isolated into that app's AppHealth entry and never aborts the pass — only
// a failure to even list the apps to reconcile (a store error) fails the
// whole call. failoverLostNodes similarly isolates its own errors/panics
// (see its doc comment) so a failover hiccup never aborts a pass either.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	apps, err := r.store.ListApps()
	if err != nil {
		return fmt.Errorf("reconcile: list apps: %w", err)
	}

	appHealth := make([]AppHealth, 0, len(apps))
	for _, app := range apps {
		if ah := r.reconcileApp(ctx, app); ah != nil {
			appHealth = append(appHealth, *ah)
		}
	}

	r.setHealth(Health{
		Nodes: r.probeNodes(ctx),
		Edge:  r.probeEdge(ctx),
		Apps:  appHealth,
	})

	r.failoverLostNodes(ctx)

	return nil
}

// failoverLostNodes implements Phase 11b Task 5's automatic node-loss
// failover: for every REGISTERED node (r.store.ListNodes() — this never
// includes the implicit "local" node, which is also defensively skipped
// below even if somehow seeded into unreachableSince), it tracks how long
// that node has been continuously unreachable and, once that streak exceeds
// config.FailoverGrace(), evacuates its scheduler-placed surfaces via
// EvacuateNode exactly as a manual `lwd node drain` would.
//
// If no Reachability has been wired up (r.reach == nil, e.g. a test that
// never calls SetReachability), there is nothing to probe, so this is a
// no-op — a node's reachability can never be determined, and treating
// "unknown" as "unreachable" would evacuate healthy fleets on every pass.
//
// Lock discipline (critical — mirrors reconcileApp/tryHeal's split in
// Reconcile's per-app loop): unreachableSince is read/mutated only under
// short r.mu critical sections; EvacuateNode is ALWAYS called with r.mu NOT
// held, since it takes the lock itself — holding it across that call would
// deadlock the whole reconcile loop.
//
// The entire pass is wrapped in a deferred recover so a panic here (e.g. a
// misbehaving Reachability/store implementation) can never crash the
// reconcile loop or leave r.mu held; a plain error from EvacuateNode or
// store.ListNodes is logged and otherwise swallowed for the same reason —
// this is best-effort self-healing, not a critical path whose failure
// should abort the pass.
func (r *Reconciler) failoverLostNodes(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("failover: recovered from panic: %v", rec)
		}
	}()

	if r.reach == nil {
		return
	}

	nodes, err := r.store.ListNodes()
	if err != nil {
		log.Printf("failover: list nodes: %v", err)
		return
	}

	for _, n := range nodes {
		if n.Name == "local" {
			// Defensive: store.ListNodes() never actually returns "local",
			// but the implicit local node must never be a failover target
			// regardless — it's the controller itself, not something that
			// can be "lost".
			continue
		}

		_, ok := r.reach.Reachable(ctx, n.Name)
		if ok {
			r.mu.Lock()
			delete(r.unreachableSince, n.Name)
			r.mu.Unlock()
			continue
		}

		r.mu.Lock()
		since, seen := r.unreachableSince[n.Name]
		if !seen {
			// First pass to observe this node unreachable: start the grace
			// timer, but don't fail over yet.
			r.unreachableSince[n.Name] = time.Now()
			r.mu.Unlock()
			continue
		}
		r.mu.Unlock()

		if time.Since(since) < config.FailoverGrace() {
			continue
		}

		// Past grace: evacuate WITHOUT r.mu held (EvacuateNode takes it
		// itself).
		log.Printf("failover: node %q unreachable for over %s, evacuating scheduled surfaces", n.Name, config.FailoverGrace())
		result, everr := r.EvacuateNode(ctx, n.Name)
		if everr != nil {
			log.Printf("failover: evacuate %q: %v", n.Name, everr)
		} else {
			log.Printf("failover: evacuate %q done: moved=%v skipped=%v failed=%v", n.Name, result.Moved, result.Skipped, result.Failed)
		}

		// Clear the timer regardless of outcome: a fully successful
		// evacuation has nothing left to move, and anything EvacuateNode
		// couldn't move (result.Failed — e.g. no capacity elsewhere) is
		// picked up by the app's own normal degraded/heal reporting rather
		// than re-attempting a fresh failover on every subsequent pass.
		r.mu.Lock()
		delete(r.unreachableSince, n.Name)
		r.mu.Unlock()
	}
}

// reconcileApp observes the current health of one app's surface and, if it's
// found dead, attempts to self-heal it. It deliberately does NOT hold r.mu
// while reading the current deployment or probing the node/router — those
// are all network calls (or, for the fake node in tests, at least
// logically so) that must not block a concurrent Apply. Only tryHeal, which
// mutates the shared heal-backoff bookkeeping and actually redeploys, takes
// r.mu.
//
// It returns nil for anything outside its scope (no current deployment, or a
// compose app — its container lifecycle is owned by `docker compose`, not
// lwd's blue-green surface healer) so Reconcile leaves it out of the
// snapshot's Apps entirely, rather than reporting a misleading health state
// for a surface it never manages.
func (r *Reconciler) reconcileApp(ctx context.Context, app string) *AppHealth {
	now := time.Now()

	cur, err := r.store.CurrentDeployment(app)
	if err != nil {
		return &AppHealth{App: app, State: SurfaceDegraded, LastError: err.Error(), HealAttempts: r.healAttempts(app), UpdatedAt: now}
	}
	if cur == nil {
		return nil
	}

	if cur.ContainerID == "" || isComposeApp(cur.Spec) {
		return nil
	}

	nodeName := nodeFromSpec(cur.Spec)
	if nodeName == "" {
		nodeName = "local"
	}

	if r.reach != nil {
		if _, ok := r.reach.Reachable(ctx, nodeName); !ok {
			return &AppHealth{App: app, State: SurfaceDegraded, LastError: "node " + nodeName + " unreachable", HealAttempts: r.healAttempts(app), UpdatedAt: now}
		}
	}

	n, _, _, _, rerr := r.resolver.ResolveMeta(nodeName)
	if rerr != nil {
		return &AppHealth{App: app, State: SurfaceDegraded, LastError: rerr.Error(), HealAttempts: r.healAttempts(app), UpdatedAt: now}
	}

	state, _, herr := n.ContainerHealth(ctx, cur.ContainerID)
	if !surfaceIsDead(state, herr) {
		r.clearHealAttempts(app)
		return &AppHealth{App: app, State: SurfaceHealthy, HealAttempts: 0, UpdatedAt: now}
	}

	return r.tryHeal(ctx, app, cur)
}

// tryHeal attempts to self-heal app's dead surface (cur, its current
// deployment row as last observed by reconcileApp, WITHOUT r.mu held), gated
// by a per-app attempt count and exponential backoff: once
// config.HealMaxAttempts consecutive attempts have failed, it gives up
// (SurfaceFailed) without trying again; between attempts, a caller that comes
// back before the backoff window elapses is told to wait (SurfaceDegraded)
// rather than retrying immediately. It takes r.mu for its whole body since a
// heal actually redeploys (via healSurfaceLocked, which itself assumes r.mu
// is already held) and must not interleave with a concurrent Apply.
//
// Because cur was read lock-free, the first thing it does after acquiring
// r.mu is re-fetch the current deployment and bail (nil, no AppHealth entry)
// if it no longer matches cur — see the comment inline below for why.
func (r *Reconciler) tryHeal(ctx context.Context, app string, cur *store.Deployment) *AppHealth {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Re-validate under the lock: reconcileApp observed `cur` dead without
	// holding r.mu, and a manual Apply/Rollback/Remove (all serialize on
	// r.mu) may have superseded it in the meantime. Healing from a stale
	// snapshot would resurrect a just-removed app or revert a newer deploy.
	// If the current deployment is gone (removed/retired) or is a different
	// row, a manual action won — skip; the next pass re-observes reality.
	fresh, err := r.store.CurrentDeployment(app)
	if err != nil {
		return &AppHealth{App: app, State: SurfaceDegraded, LastError: "recheck current deployment: " + err.Error(), UpdatedAt: time.Now()}
	}
	if fresh == nil || fresh.ID != cur.ID {
		return nil
	}
	cur = fresh

	hs := r.heal[app]
	if hs == nil {
		hs = &healState{}
		r.heal[app] = hs
	}

	now := time.Now()
	if hs.attempts >= config.HealMaxAttempts() {
		return &AppHealth{App: app, State: SurfaceFailed, LastError: "gave up after max heal attempts", HealAttempts: hs.attempts, UpdatedAt: now}
	}
	if now.Before(hs.nextEligible) {
		return &AppHealth{App: app, State: SurfaceDegraded, LastError: "waiting for heal backoff", HealAttempts: hs.attempts, UpdatedAt: now}
	}

	hs.attempts++
	if _, err := r.healSurfaceLocked(ctx, cur); err != nil {
		hs.nextEligible = now.Add(healBackoff(hs.attempts))
		return &AppHealth{App: app, State: SurfaceFailed, LastError: err.Error(), HealAttempts: hs.attempts, UpdatedAt: now}
	}

	// Success: deployBlueGreenSurface already called resetHeal(app), so hs is
	// gone from r.heal now; report healthy with a reset attempt count.
	return &AppHealth{App: app, State: SurfaceHealthy, HealAttempts: 0, UpdatedAt: now}
}

// healBackoff returns the backoff delay before heal attempt number attempts+1
// is eligible: exponential from healBackoffBase, capped at healBackoffMax.
func healBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := healBackoffBase << (attempts - 1)
	if d > healBackoffMax || d <= 0 {
		return healBackoffMax
	}
	return d
}

// clearHealAttempts drops app's in-progress self-heal bookkeeping, if any. It
// is called from reconcileApp — which does not already hold r.mu — after
// observing a surface as healthy, so it locks internally; this is distinct
// from resetHeal, which assumes the lock is already held (called from within
// deployBlueGreenSurface/tryHeal).
func (r *Reconciler) clearHealAttempts(app string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.heal, app)
}

// healAttempts returns the number of consecutive heal attempts currently
// recorded for app (0 if none).
func (r *Reconciler) healAttempts(app string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if hs := r.heal[app]; hs != nil {
		return hs.attempts
	}
	return 0
}

// probeNodes returns a reachability snapshot of every node the reconciler
// knows about: the implicit "local" node first, then every node registered
// in the store, each probed via r.reach. Returns nil (rather than an empty
// snapshot) if no Reachability has been configured via SetReachability, since
// there's nothing meaningful to report.
func (r *Reconciler) probeNodes(ctx context.Context) []NodeHealth {
	if r.reach == nil {
		return nil
	}

	now := time.Now()
	out := make([]NodeHealth, 0, 1)

	transport, ok := r.reach.Reachable(ctx, "local")
	out = append(out, NodeHealth{Name: "local", Transport: transport, Reachable: ok, UpdatedAt: now, Capacity: r.probeCapacity(ctx, "local")})

	nodes, err := r.store.ListNodes()
	if err != nil {
		return out
	}
	for _, n := range nodes {
		transport, ok := r.reach.Reachable(ctx, n.Name)
		out = append(out, NodeHealth{Name: n.Name, Transport: transport, Reachable: ok, UpdatedAt: now, Capacity: r.probeCapacity(ctx, n.Name), Cordoned: !n.Schedulable})
	}
	return out
}

// probeCapacity fetches nodeName's Capacity for a health snapshot,
// best-effort and bounded (capacityBounded): a failed/timed-out fetch (e.g.
// the node is actually unreachable) reports a zero Capacity rather than
// failing the whole probe pass.
func (r *Reconciler) probeCapacity(ctx context.Context, nodeName string) node.Capacity {
	n, err := r.resolver.Resolve(nodeName)
	if err != nil {
		return node.Capacity{}
	}
	c, err := capacityBounded(ctx, n)
	if err != nil {
		return node.Capacity{}
	}
	return c
}

// probeEdge returns the current reachability of the shared edge (router).
func (r *Reconciler) probeEdge(ctx context.Context) EdgeHealth {
	return EdgeHealth{Reachable: r.router.Healthy(ctx), UpdatedAt: time.Now()}
}
