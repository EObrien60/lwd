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
	"os"
	"strconv"
	"sync"
	"time"

	"lwd/internal/compose"
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

// SecretResolver resolves an app's declared secret names to their plaintext
// values at deploy time. It is expected to fail closed: Resolve must return
// an error (naming the offending secret) rather than silently omitting or
// substituting a value for any name it cannot resolve. secrets.Store
// satisfies this interface.
type SecretResolver interface {
	Resolve(app string, names []string) (map[string]string, error)
}

// Reconciler applies desired app specs against a node and a router, recording
// history in the store. A single mutex serializes all Apply calls: blue-green
// swaps involve multiple external side effects (container, staging route,
// live route) that must not interleave across concurrent deploys.
type Reconciler struct {
	node    node.Node
	router  router.Router
	store   *store.Store
	secrets SecretResolver
	compose compose.Composer
	mu      sync.Mutex
}

// New returns a Reconciler bound to a node, a router, a store, a secret
// resolver used to inject an app's declared secrets into its container env,
// and a Composer used to delegate compose-app deploys to `docker compose`.
func New(n node.Node, r router.Router, s *store.Store, sec SecretResolver, comp compose.Composer) *Reconciler {
	return &Reconciler{node: n, router: r, store: s, secrets: sec, compose: comp}
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

	if app.Compose != "" {
		return r.applyCompose(ctx, app)
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

	// Resolve declared secrets BEFORE starting anything. This must fail
	// closed: if any named secret can't be resolved, we record the failed
	// attempt and return without touching the node, the router, or the
	// currently running (old) deployment — there is no new container yet to
	// remove, and the live route/domain is left exactly as it was.
	secretVals, err := r.secrets.Resolve(app.Name, app.Secrets)
	if err != nil {
		_, _ = r.store.RecordDeployment(store.Deployment{
			App:       app.Name,
			Image:     app.Image,
			Status:    store.StatusFailed,
			CreatedAt: time.Now(),
			Spec:      string(specJSON),
		})
		return nil, fmt.Errorf("resolve secrets: %w", err)
	}
	env := make(map[string]string, len(app.Env)+len(secretVals))
	for k, v := range app.Env {
		env[k] = v
	}
	for k, v := range secretVals {
		env[k] = v // secrets win on key collision with plain env
	}

	c, err := r.node.RunContainer(ctx, node.RunSpec{
		Name:  name,
		Image: app.Image,
		Env:   env,
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
		// The prior running deployment (if any) is left completely untouched:
		// its container keeps running and the live domain/route still points
		// at it. Blue-green means a failed candidate never affects what's
		// currently serving traffic, so we must not retire or otherwise mutate
		// that row here.
		r.recordFailedCandidate(ctx, app, stageHost, c.ID, specJSON)
		return nil, fmt.Errorf("health check failed: %w", healthErr)
	}

	if err := r.router.SetRoute(ctx, router.Route{
		Domain:      app.Domain,
		Upstream:    name,
		Port:        app.Port,
		TLSInternal: router.UseInternalTLS(app.Domain),
	}); err != nil {
		// The flip itself failed: undo the staging state and the new
		// container, and record the attempt as failed (same isolation
		// guarantee as the health-failure branch above — the live domain,
		// still pointing at the old container or unset if this is the first
		// deploy, is unaffected, and the prior running deployment row is left
		// untouched).
		r.recordFailedCandidate(ctx, app, stageHost, c.ID, specJSON)
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

// applyCompose deploys a compose app by delegating orchestration to `docker
// compose` and layering lwd's Caddy routing + health-gating on top. Unlike
// the single-service blue-green path, this is an in-place recreate: compose
// only recreates services whose image/config changed (so an unchanged
// database stays up), the declared web service is routed live immediately,
// and only then is it health-checked. A failed health check therefore leaves
// the (possibly broken) new stack live and flagged — see the design doc's
// documented tradeoff. Callers must hold r.mu; Apply does so before branching
// here.
func (r *Reconciler) applyCompose(ctx context.Context, app *spec.App) (*store.Deployment, error) {
	if err := r.router.EnsureUp(ctx); err != nil {
		return nil, fmt.Errorf("ensure router: %w", err)
	}
	if err := r.node.EnsureNetwork(ctx, lwdNetwork); err != nil {
		return nil, fmt.Errorf("ensure network: %w", err)
	}

	specJSON, err := json.Marshal(app)
	if err != nil {
		return nil, fmt.Errorf("marshal spec snapshot: %w", err)
	}

	// Read the compose file content up front so failure snapshots (including
	// a fail-closed secret resolve, below) can carry it whenever it happens
	// to be readable, per the design's "Compose content if readable" note.
	content, readErr := os.ReadFile(app.Compose)

	// Resolve declared secrets BEFORE running compose. Fail closed: on error,
	// record the failed attempt and return without ever shelling out to
	// `docker compose` — the (possibly still running, if this is a redeploy)
	// existing stack and live route are left completely untouched.
	secretVals, err := r.secrets.Resolve(app.Name, app.Secrets)
	if err != nil {
		r.recordComposeFailed(app, "", specJSON, string(content))
		return nil, fmt.Errorf("resolve secrets: %w", err)
	}
	if readErr != nil {
		r.recordComposeFailed(app, "", specJSON, "")
		return nil, fmt.Errorf("read compose file %s: %w", app.Compose, readErr)
	}

	env := make(map[string]string, len(app.Env)+len(secretVals))
	for k, v := range app.Env {
		env[k] = v
	}
	for k, v := range secretVals {
		env[k] = v // secrets win on key collision with plain env
	}

	project := "lwd-" + app.Name
	if err := r.compose.Up(ctx, compose.UpSpec{Project: project, File: app.Compose, Env: env}); err != nil {
		r.recordComposeFailed(app, "", specJSON, string(content))
		return nil, fmt.Errorf("compose up: %w", err)
	}

	id, name, err := r.compose.ServiceContainer(ctx, project, app.Service)
	if err != nil {
		r.recordComposeFailed(app, "", specJSON, string(content))
		return nil, fmt.Errorf("resolve service container: %w", err)
	}

	if err := r.node.ConnectContainerToNetwork(ctx, id, lwdNetwork); err != nil {
		r.recordComposeFailed(app, id, specJSON, string(content))
		return nil, fmt.Errorf("connect container to network: %w", err)
	}

	// The new stack is live immediately: unlike blue-green there is no old
	// container left to keep serving during health-gating, so the route
	// flips to the freshly (re)created service before it's been proven ready.
	if err := r.router.SetRoute(ctx, router.Route{
		Domain:      app.Domain,
		Upstream:    name,
		Port:        app.Port,
		TLSInternal: router.UseInternalTLS(app.Domain),
	}); err != nil {
		r.recordComposeFailed(app, id, specJSON, string(content))
		return nil, fmt.Errorf("set route: %w", err)
	}

	if healthErr := r.checkHealthCompose(ctx, app); healthErr != nil {
		// Deliberately do NOT compose down or otherwise tear anything down:
		// compose owns this stack's lifecycle, and the honest tradeoff of the
		// in-place delegate model is that a failed health check leaves the
		// new (possibly broken) stack live, flagged by this StatusFailed
		// record, until fixed or rolled back.
		r.recordComposeFailed(app, id, specJSON, string(content))
		return nil, fmt.Errorf("health check failed: %w", healthErr)
	}

	// Retire the prior running deployment ROW only — the compose stack itself
	// is not torn down; compose already recreated only what changed.
	if prev, perr := r.store.CurrentDeployment(app.Name); perr == nil && prev != nil {
		_ = r.store.SetStatus(prev.ID, store.StatusRetired)
	}

	dep := store.Deployment{
		App:         app.Name,
		Image:       app.Image,
		ContainerID: id,
		Status:      store.StatusRunning,
		CreatedAt:   time.Now(),
		Spec:        string(specJSON),
		Compose:     string(content),
	}
	depID, err := r.store.RecordDeployment(dep)
	if err != nil {
		return nil, fmt.Errorf("record deployment: %w", err)
	}
	dep.ID = depID
	return &dep, nil
}

// recordComposeFailed records a StatusFailed deployment for a compose apply
// attempt, carrying whatever Spec/Compose snapshot is available. Errors from
// recording are intentionally swallowed — the caller always returns its own
// wrapped error for the actual failure cause.
func (r *Reconciler) recordComposeFailed(app *spec.App, containerID string, specJSON []byte, content string) {
	_, _ = r.store.RecordDeployment(store.Deployment{
		App:         app.Name,
		Image:       app.Image,
		ContainerID: containerID,
		Status:      store.StatusFailed,
		CreatedAt:   time.Now(),
		Spec:        string(specJSON),
		Compose:     content,
	})
}

// checkHealthCompose gates a compose deploy's success using the same layered
// policy shape as checkHealth, but probed against the app's real domain
// (already live on the router by this point) rather than a staging host:
// there is no separate not-yet-live candidate to probe out of band, since the
// compose flow routes the new stack live before health-gating it. It does not
// consult node.ContainerHealth: compose (not lwd) owns the container
// lifecycle, so lwd only has an opinion about reachability through Caddy.
func (r *Reconciler) checkHealthCompose(ctx context.Context, app *spec.App) error {
	timeout := app.Health.Timeout
	if timeout <= 0 {
		timeout = defaultHealthTimeout
	}
	deadline := time.Now().Add(timeout)

	if app.Health.Path != "" {
		return r.checkHealthPath(ctx, deadline, app.Domain, app.Health.Path)
	}
	return r.checkLivenessThroughRoute(ctx, deadline, app.Domain)
}

// checkLivenessThroughRoute is checkLiveness's compose counterpart: after a
// short settle window, it requires a GET through Caddy at domain to return
// anything other than Caddy's own 502/503 (which mean it couldn't reach the
// upstream at all). It has no container-state check to layer in, since
// compose containers are not tracked as node.Node surfaces.
func (r *Reconciler) checkLivenessThroughRoute(ctx context.Context, deadline time.Time, domain string) error {
	select {
	case <-time.After(livenessSettle):
	case <-ctx.Done():
		return ctx.Err()
	}

	var lastStatus int
	var lastErr error
	for {
		status, err := r.router.ProbeThroughCaddy(ctx, domain, "/")
		lastStatus, lastErr = status, err
		if err == nil && status != 502 && status != 503 {
			return nil
		}

		if done, werr := waitOrDeadline(ctx, deadline); done {
			if werr != nil {
				return werr
			}
			if lastErr != nil {
				return fmt.Errorf("liveness probe %s did not become ready: %w (last status %d)", domain, lastErr, lastStatus)
			}
			return fmt.Errorf("liveness check timed out: last probe status=%d", lastStatus)
		}
	}
}

// Remove tears down an app entirely: for a compose app (its current
// deployment carries non-empty Compose content) it runs `docker compose
// down` against the stored compose content, removes the Caddy route, and
// retires the deployment row. For a single-service app it removes every
// container labeled lwd.app=<app>, removes the Caddy route, and retires the
// deployment row. It holds r.mu for the same reason Apply does: a concurrent
// Apply must not interleave with a Remove.
func (r *Reconciler) Remove(ctx context.Context, appName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	cur, err := r.store.CurrentDeployment(appName)
	if err != nil {
		return fmt.Errorf("current deployment: %w", err)
	}

	if cur != nil && cur.Compose != "" {
		return r.removeCompose(ctx, appName, cur)
	}
	return r.removeSingleService(ctx, appName, cur)
}

// removeCompose tears down a compose app's stack via `docker compose down`
// (writing its stored compose content to a temp file, since Down takes a
// file path), removes its Caddy route, and retires the deployment row.
func (r *Reconciler) removeCompose(ctx context.Context, appName string, cur *store.Deployment) error {
	tmp, err := os.CreateTemp("", "lwd-compose-rm-*.yml")
	if err != nil {
		return fmt.Errorf("create temp compose file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(cur.Compose); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp compose file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp compose file: %w", err)
	}

	project := "lwd-" + appName
	if err := r.compose.Down(ctx, project, tmp.Name()); err != nil {
		return fmt.Errorf("compose down: %w", err)
	}

	if domain := domainFromSpec(cur.Spec); domain != "" {
		// Best-effort: a failure here shouldn't stop the app from being
		// retired (its stack is already gone), but it does mean the domain
		// may keep 502ing until a later reload/rm fixes it up.
		_ = r.router.RemoveRoute(ctx, domain)
	}

	return r.store.SetStatus(cur.ID, store.StatusRetired)
}

// removeSingleService removes every container labeled lwd.app=appName,
// removes the app's Caddy route (resolved from cur's Spec snapshot, if any),
// and retires cur, if present. cur may be nil (nothing recorded for appName
// yet) — removal still runs as a defensive cleanup of any stray containers.
func (r *Reconciler) removeSingleService(ctx context.Context, appName string, cur *store.Deployment) error {
	var domain string
	if cur != nil {
		domain = domainFromSpec(cur.Spec)
	}

	containers, err := r.node.ListContainers(ctx, map[string]string{"lwd.app": appName})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	for _, c := range containers {
		if err := r.node.RemoveContainer(ctx, c.ID); err != nil {
			return fmt.Errorf("remove container %s: %w", c.ID, err)
		}
	}

	if domain != "" {
		// Best-effort, same rationale as removeCompose above.
		_ = r.router.RemoveRoute(ctx, domain)
	}

	if cur != nil {
		return r.store.SetStatus(cur.ID, store.StatusRetired)
	}
	return nil
}

// domainFromSpec extracts the Domain field from a deployment's JSON Spec
// snapshot, returning "" if specJSON is empty or fails to unmarshal.
func domainFromSpec(specJSON string) string {
	if specJSON == "" {
		return ""
	}
	var a spec.App
	if err := json.Unmarshal([]byte(specJSON), &a); err != nil {
		return ""
	}
	return a.Domain
}

// Rollback redeploys the most recent retired ("previous") deployment for app,
// restoring its exact image via a fresh blue-green Apply — so a rollback is
// itself zero-downtime and health-gated like any other deploy. It reads the
// previous deployment's stored Spec snapshot (captured at the time that
// deployment was originally applied) rather than re-resolving lwd.toml, so it
// restores precisely what was running before, even if the local spec file has
// since changed.
//
// For a compose app (prev.Compose non-empty), the previous deployment's
// stored compose content — not whatever currently lives at the original
// file path, which may have changed or been deleted since — is written to a
// fresh temp file and restored.Compose is repointed at it before delegating
// to Apply, so the rollback re-applies the exact prior stack. The temp file
// is removed once Apply returns (applyCompose reads it synchronously before
// then, so this is safe).
//
// Rollback does not hold r.mu itself: it only reads from the store and
// unmarshals JSON before delegating to Apply, which takes the lock. Locking
// here too would deadlock against Apply's own Lock/Unlock.
func (r *Reconciler) Rollback(ctx context.Context, app string) (*store.Deployment, error) {
	prev, err := r.store.PreviousDeployment(app)
	if err != nil {
		return nil, fmt.Errorf("load previous deployment: %w", err)
	}
	if prev == nil {
		return nil, fmt.Errorf("no previous deployment for %q", app)
	}

	var restored spec.App
	if prev.Spec == "" {
		return nil, fmt.Errorf("no spec snapshot recorded for previous deployment of %q", app)
	}
	if err := json.Unmarshal([]byte(prev.Spec), &restored); err != nil {
		return nil, fmt.Errorf("unmarshal spec snapshot for %q: %w", app, err)
	}
	// Pin the image to exactly what that deployment recorded, guaranteeing
	// the same digest/ref is restored even if the snapshot's Image field
	// somehow diverged from it. Harmless for a compose app, whose Image is
	// always empty.
	restored.Image = prev.Image

	if prev.Compose != "" {
		tmp, err := os.CreateTemp("", "lwd-compose-rollback-*.yml")
		if err != nil {
			return nil, fmt.Errorf("create temp compose file: %w", err)
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.WriteString(prev.Compose); err != nil {
			tmp.Close()
			return nil, fmt.Errorf("write temp compose file: %w", err)
		}
		if err := tmp.Close(); err != nil {
			return nil, fmt.Errorf("close temp compose file: %w", err)
		}
		restored.Compose = tmp.Name()
	}

	return r.Apply(ctx, &restored)
}

// recordFailedCandidate tears down a failed candidate deployment: it removes
// the staging route (a no-op if already removed) and the new container, then
// records a StatusFailed deployment carrying the attempt's Spec snapshot.
// Errors from cleanup and from recording are intentionally swallowed here —
// the caller always returns its own wrapped error for the actual failure
// cause. The prior running deployment/route is never touched by this helper,
// preserving blue-green's isolation guarantee: a failed candidate, however it
// failed, never affects what's currently serving traffic.
func (r *Reconciler) recordFailedCandidate(ctx context.Context, app *spec.App, stageHost, containerID string, specJSON []byte) {
	_ = r.router.RemoveStaging(ctx, stageHost)
	_ = r.node.RemoveContainer(ctx, containerID)

	_, _ = r.store.RecordDeployment(store.Deployment{
		App:         app.Name,
		Image:       app.Image,
		ContainerID: containerID,
		Status:      store.StatusFailed,
		CreatedAt:   time.Now(),
		Spec:        string(specJSON),
	})
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
