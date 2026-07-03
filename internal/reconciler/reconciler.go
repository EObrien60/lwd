// Package reconciler makes the running state match a desired app spec.
// It is written entirely against node.Node, router.Router, and store.Store so
// it can be tested with no Docker daemon and no real Caddy.
//
// Phase 2 reworks the deploy path from Phase 1's recreate (stop old, start
// new, hope nothing looked at it in between) to zero-downtime blue-green: a
// new "surface" container is started alongside the old one, staged behind a
// throwaway host on the shared Caddy router, health-checked THROUGH Caddy
// (never directly, since surfaces publish no host ports), and only then does
// the real domain flip to it. The old container keeps serving until the new
// one has proven itself; a failed health check never touches the live route.
package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"lwd/internal/node"
	"lwd/internal/router"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// lwdNetwork is the private Docker network all surface containers and the
// Caddy router join. Kept local to this package: router.Router.EnsureUp is
// responsible for the router's own idea of the network name, but the
// reconciler also attaches surface containers to it directly via node.Node.
const lwdNetwork = "lwd"

// healthPollInterval is the delay between health-check polls (HTTP probes or
// Docker health inspections) while waiting for a new deployment to become
// ready.
const healthPollInterval = 50 * time.Millisecond

// livenessSettle is a short grace period given to a container with neither a
// declared health path nor a Docker HEALTHCHECK before checking whether it is
// still running and reachable through Caddy. It exists to avoid flagging a
// container as dead on the very first instant after start.
const livenessSettle = 50 * time.Millisecond

// defaultHealthTimeout is used when an app declares no health timeout at all
// (spec.Parse normally defaults this to 30s, but callers constructing
// spec.App directly, e.g. tests or the API, may leave it zero).
const defaultHealthTimeout = 30 * time.Second

// Reconciler applies desired app specs against a node and a router, recording
// history in the store. A single mutex serializes all Apply calls: blue-green
// swaps involve multiple external side effects (container, staging route,
// live route) that must not interleave across concurrent deploys.
type Reconciler struct {
	node   node.Node
	router router.Router
	store  *store.Store
	mu     sync.Mutex
}

// New returns a Reconciler bound to a node, a router, and a store.
func New(n node.Node, r router.Router, s *store.Store) *Reconciler {
	return &Reconciler{node: n, router: r, store: s}
}

// containerName returns the name of the surface container for one blue-green
// attempt. Deploy ids are unique and increasing (store.NextDeployID), so
// successive deploys of the same app never collide even while the old
// container is still running alongside the new one.
func containerName(app *spec.App, deployID int64) string {
	return fmt.Sprintf("lwd-%s-%d", app.Name, deployID)
}

// stagingHost returns the throwaway internal hostname used to probe a
// not-yet-live deployment through Caddy ahead of cutover.
func stagingHost(deployID int64) string {
	return fmt.Sprintf("stage-%d.lwd.internal", deployID)
}

// Apply reconciles one app using zero-downtime blue-green:
//  1. Validate the spec.
//  2. Ensure the router and the shared network are up, and the image present.
//  3. Start a new surface container under a fresh, uniquely numbered name.
//  4. Stage it behind a throwaway internal host on the router and health-check
//     it THROUGH Caddy (or via Docker health / liveness, layered — see
//     checkHealth) rather than talking to it directly.
//  5. On success: flip the real domain to the new container, drop the staging
//     route, retire and remove the old surface (if any), and record a
//     StatusRunning deployment.
//  6. On failure: drop the staging route, remove the new container, and
//     record a StatusFailed deployment. The live domain is never touched.
func (r *Reconciler) Apply(ctx context.Context, app *spec.App) (*store.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := app.Validate(); err != nil {
		return nil, fmt.Errorf("invalid spec: %w", err)
	}

	if err := r.router.EnsureUp(ctx); err != nil {
		return nil, fmt.Errorf("ensure router: %w", err)
	}
	if err := r.node.EnsureNetwork(ctx, lwdNetwork); err != nil {
		return nil, fmt.Errorf("ensure network: %w", err)
	}
	if err := r.node.EnsureImage(ctx, app.Image); err != nil {
		return nil, fmt.Errorf("ensure image: %w", err)
	}

	deployID, err := r.store.NextDeployID()
	if err != nil {
		return nil, fmt.Errorf("next deploy id: %w", err)
	}
	name := containerName(app, deployID)
	stageHost := stagingHost(deployID)

	specJSON, err := json.Marshal(app)
	if err != nil {
		return nil, fmt.Errorf("marshal spec snapshot: %w", err)
	}

	c, err := r.node.RunContainer(ctx, node.RunSpec{
		Name:  name,
		Image: app.Image,
		Env:   app.Env,
		Labels: map[string]string{
			"lwd.app":    app.Name,
			"lwd.role":   "surface",
			"lwd.deploy": strconv.FormatInt(deployID, 10),
		},
		Port:    app.Port,
		Network: lwdNetwork,
	})
	if err != nil {
		return nil, fmt.Errorf("run container: %w", err)
	}

	if err := r.router.SetStaging(ctx, stageHost, name, app.Port); err != nil {
		_ = r.node.RemoveContainer(ctx, c.ID)
		return nil, fmt.Errorf("set staging route: %w", err)
	}

	if healthErr := r.checkHealth(ctx, app, stageHost, c.ID); healthErr != nil {
		_ = r.router.RemoveStaging(ctx, stageHost)
		_ = r.node.RemoveContainer(ctx, c.ID)

		// Retire any prior running deployment row so the store never claims a
		// version is "running" once a newer attempt has been recorded against
		// this app, even though (per the design) the live domain/route itself
		// is left completely untouched and the old container keeps serving.
		if prev, perr := r.store.CurrentDeployment(app.Name); perr == nil && prev != nil {
			_ = r.store.SetStatus(prev.ID, store.StatusRetired)
		}

		_, _ = r.store.RecordDeployment(store.Deployment{
			App:         app.Name,
			Image:       app.Image,
			ContainerID: c.ID,
			Status:      store.StatusFailed,
			CreatedAt:   time.Now(),
			Spec:        string(specJSON),
		})
		return nil, fmt.Errorf("health check failed: %w", healthErr)
	}

	if err := r.router.SetRoute(ctx, router.Route{
		Domain:      app.Domain,
		Upstream:    name,
		Port:        app.Port,
		TLSInternal: router.UseInternalTLS(app.Domain),
	}); err != nil {
		// The flip itself failed: undo the staging state and the new
		// container. The live domain (still pointing at the old container,
		// or unset if this is the first deploy) is unaffected.
		_ = r.router.RemoveStaging(ctx, stageHost)
		_ = r.node.RemoveContainer(ctx, c.ID)
		return nil, fmt.Errorf("set route: %w", err)
	}
	_ = r.router.RemoveStaging(ctx, stageHost)

	// Retire and remove the old surface, if any, now that the new one is live.
	if prev, perr := r.store.CurrentDeployment(app.Name); perr == nil && prev != nil {
		_ = r.node.RemoveContainer(ctx, prev.ContainerID)
		_ = r.store.SetStatus(prev.ID, store.StatusRetired)
	}

	dep := store.Deployment{
		App:         app.Name,
		Image:       app.Image,
		ContainerID: c.ID,
		Status:      store.StatusRunning,
		CreatedAt:   time.Now(),
		Spec:        string(specJSON),
	}
	id, err := r.store.RecordDeployment(dep)
	if err != nil {
		return nil, fmt.Errorf("record deployment: %w", err)
	}
	dep.ID = id
	return &dep, nil
}

// checkHealth gates the blue-green cutover using lwd's layered health policy,
// evaluated against the staged (not-yet-live) container:
//
//  1. app.Health.Path set -> strict readiness: poll GET stagingHost+path
//     through Caddy until it answers 2xx, or the timeout elapses.
//  2. No path, but the container's image declares a Docker HEALTHCHECK ->
//     honor it: poll node.ContainerHealth until "healthy" (success),
//     "unhealthy" (immediate failure), or the timeout elapses.
//  3. Neither -> liveness fallback: after a short settle window, require the
//     container to still be "running" AND a GET "/" through Caddy to return
//     anything other than Caddy's own 502/503 (which mean it couldn't reach
//     the upstream at all) — any other status, including 404, proves the app
//     is listening and accepting connections.
func (r *Reconciler) checkHealth(ctx context.Context, app *spec.App, stagingHost, containerID string) error {
	timeout := app.Health.Timeout
	if timeout <= 0 {
		timeout = defaultHealthTimeout
	}
	deadline := time.Now().Add(timeout)

	if app.Health.Path != "" {
		return r.checkHealthPath(ctx, deadline, stagingHost, app.Health.Path)
	}

	_, dockerHealth, err := r.node.ContainerHealth(ctx, containerID)
	if err != nil {
		return fmt.Errorf("container health: %w", err)
	}
	if dockerHealth != "" {
		return r.checkDockerHealth(ctx, deadline, containerID)
	}

	return r.checkLiveness(ctx, deadline, stagingHost, containerID)
}

// checkHealthPath polls stagingHost+path through Caddy until it returns a 2xx
// status or deadline passes. Transport errors and non-2xx statuses are both
// treated as "not ready yet" and retried; only the deadline turns them into a
// hard failure.
func (r *Reconciler) checkHealthPath(ctx context.Context, deadline time.Time, stagingHost, path string) error {
	var lastStatus int
	var lastErr error
	for {
		status, err := r.router.ProbeThroughCaddy(ctx, stagingHost, path)
		lastStatus, lastErr = status, err
		if err == nil && status >= 200 && status < 300 {
			return nil
		}
		if done, werr := waitOrDeadline(ctx, deadline); done {
			if werr != nil {
				return werr
			}
			if lastErr != nil {
				return fmt.Errorf("health probe %s%s did not become ready: %w (last status %d)", stagingHost, path, lastErr, lastStatus)
			}
			return fmt.Errorf("health probe %s%s did not become ready: timed out (last status %d)", stagingHost, path, lastStatus)
		}
	}
}

// checkDockerHealth polls the container's Docker health status until it
// reports "healthy" (success), "unhealthy" (immediate failure — no point
// waiting out the clock once the image itself says it's broken), or the
// deadline passes.
func (r *Reconciler) checkDockerHealth(ctx context.Context, deadline time.Time, containerID string) error {
	var lastHealth string
	for {
		_, h, err := r.node.ContainerHealth(ctx, containerID)
		if err != nil {
			return fmt.Errorf("container health: %w", err)
		}
		lastHealth = h
		if h == "healthy" {
			return nil
		}
		if h == "unhealthy" {
			return fmt.Errorf("container reported unhealthy")
		}
		if done, werr := waitOrDeadline(ctx, deadline); done {
			if werr != nil {
				return werr
			}
			return fmt.Errorf("docker healthcheck did not become healthy: timed out (last=%q)", lastHealth)
		}
	}
}

// checkLiveness is the fallback used when an app declares neither a health
// path nor a Docker HEALTHCHECK: after a short settle window, require the
// container to stay "running" (i.e. not crash-loop) and a request through
// Caddy to reach it (any status other than Caddy's own 502/503 proves the
// app, not just the container, is up).
func (r *Reconciler) checkLiveness(ctx context.Context, deadline time.Time, stagingHost, containerID string) error {
	select {
	case <-time.After(livenessSettle):
	case <-ctx.Done():
		return ctx.Err()
	}

	var lastState string
	var lastStatus int
	for {
		state, _, err := r.node.ContainerHealth(ctx, containerID)
		if err != nil {
			return fmt.Errorf("container health: %w", err)
		}
		lastState = state
		if state != "running" {
			return fmt.Errorf("container is not running (state=%q)", state)
		}

		status, perr := r.router.ProbeThroughCaddy(ctx, stagingHost, "/")
		lastStatus = status
		if perr == nil && status != 502 && status != 503 {
			return nil
		}

		if done, werr := waitOrDeadline(ctx, deadline); done {
			if werr != nil {
				return werr
			}
			return fmt.Errorf("liveness check timed out: state=%q last probe status=%d", lastState, lastStatus)
		}
	}
}

// waitOrDeadline reports whether polling should stop: either the deadline has
// already passed (done=true, err=nil — caller should report a timeout) or the
// context was canceled (done=true, err=ctx.Err()). Otherwise it sleeps one
// healthPollInterval and returns done=false so the caller retries.
func waitOrDeadline(ctx context.Context, deadline time.Time) (done bool, err error) {
	if time.Now().After(deadline) {
		return true, nil
	}
	select {
	case <-ctx.Done():
		return true, ctx.Err()
	case <-time.After(healthPollInterval):
		return false, nil
	}
}
