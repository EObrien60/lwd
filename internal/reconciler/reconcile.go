package reconciler

import (
	"context"
	"fmt"
	"time"

	"lwd/internal/config"
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
//
// A per-app error (an unresolvable node, a failed heal attempt, ...) is
// isolated into that app's AppHealth entry and never aborts the pass — only
// a failure to even list the apps to reconcile (a store error) fails the
// whole call.
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
	return nil
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
// deployment row), gated by a per-app attempt count and exponential backoff:
// once config.HealMaxAttempts consecutive attempts have failed, it gives up
// (SurfaceFailed) without trying again; between attempts, a caller that comes
// back before the backoff window elapses is told to wait (SurfaceDegraded)
// rather than retrying immediately. It takes r.mu for its whole body since a
// heal actually redeploys (via healSurfaceLocked, which itself assumes r.mu
// is already held) and must not interleave with a concurrent Apply.
func (r *Reconciler) tryHeal(ctx context.Context, app string, cur *store.Deployment) *AppHealth {
	r.mu.Lock()
	defer r.mu.Unlock()

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
	out = append(out, NodeHealth{Name: "local", Transport: transport, Reachable: ok, UpdatedAt: now})

	nodes, err := r.store.ListNodes()
	if err != nil {
		return out
	}
	for _, n := range nodes {
		transport, ok := r.reach.Reachable(ctx, n.Name)
		out = append(out, NodeHealth{Name: n.Name, Transport: transport, Reachable: ok, UpdatedAt: now})
	}
	return out
}

// probeEdge returns the current reachability of the shared edge (router).
func (r *Reconciler) probeEdge(ctx context.Context) EdgeHealth {
	return EdgeHealth{Reachable: r.router.Healthy(ctx), UpdatedAt: time.Now()}
}
