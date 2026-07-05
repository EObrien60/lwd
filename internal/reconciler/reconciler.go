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
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"lwd/internal/build"
	"lwd/internal/compose"
	"lwd/internal/node"
	"lwd/internal/router"
	"lwd/internal/source"
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

// Reconciler applies desired app specs against a node — resolved per-deploy
// from each app's declared `node = "..."` (spec.App.Node) via a
// node.Resolver — and a router, recording history in the store. A single
// mutex serializes all Apply calls: blue-green swaps involve multiple
// external side effects (container, staging route, live route) that must not
// interleave across concurrent deploys.
type Reconciler struct {
	resolver node.Resolver
	router   router.Router
	store    *store.Store
	secrets  SecretResolver
	compose  compose.Composer
	src      source.Git
	bld      build.Builder
	mu       sync.Mutex

	// healthMu guards only health, populated by the Phase 10 continuous
	// reconciler loop and read via HealthSnapshot. heal and unreachableSince
	// are guarded by r.mu instead (they're only ever touched by code paths
	// that already hold it). reach and nudge are set once at startup via
	// their setters below.
	healthMu sync.RWMutex
	health   Health
	reach    Reachability
	nudge    chan<- struct{}
	heal     map[string]*healState

	// unreachableSince tracks, per registered node name (never "local"), the
	// time it was FIRST observed unreachable by the current unbroken streak
	// of failed probes — Phase 11b Task 5's automatic node-loss failover
	// gate. failoverLostNodes deletes a node's entry the moment it's
	// observed reachable again (so a later blip starts a fresh grace
	// window) or once it's been evacuated (so a successful failover doesn't
	// re-fire on every subsequent pass). Guarded by r.mu.
	unreachableSince map[string]time.Time
}

// healState tracks one app's in-progress self-heal attempts: how many
// consecutive attempts have been made, and the earliest time the next
// attempt is allowed to run (a backoff gate). Unexported: internal
// bookkeeping for the loop added in a later Phase 10 task.
type healState struct {
	attempts     int
	nextEligible time.Time
}

// New returns a Reconciler bound to a node.Resolver (used to place each
// app's containers on the node its spec declares — "" or "local" is always
// the local Docker daemon), a router, a store, a secret resolver used to
// inject an app's declared secrets into its container env, a Composer used
// to delegate compose-app deploys (and rendered backing-service stacks) to
// `docker compose`, a Git source used to clone git-built apps, and a Builder
// used to `docker build` them.
func New(resolver node.Resolver, r router.Router, s *store.Store, sec SecretResolver, comp compose.Composer, src source.Git, bld build.Builder) *Reconciler {
	return &Reconciler{resolver: resolver, router: r, store: s, secrets: sec, compose: comp, src: src, bld: bld, heal: map[string]*healState{}, unreachableSince: map[string]time.Time{}}
}

// SetReachability supplies the Reachability implementation (typically a
// *node.RegistryResolver) the continuous reconciler loop uses to observe
// node health. Exists as a setter, rather than a New parameter, so New's
// many existing call sites don't need to change.
func (r *Reconciler) SetReachability(rr Reachability) {
	r.reach = rr
}

// SetNudge supplies a channel the reconciler sends on (non-blocking) to wake
// a waiting continuous-reconciler loop early — e.g. right after an Apply —
// rather than it sitting idle until the next timer tick. Exists as a setter
// for the same reason as SetReachability.
func (r *Reconciler) SetNudge(ch chan<- struct{}) {
	r.nudge = ch
}

// HealthSnapshot returns a deep copy of the reconciler's current health
// view: a caller mutating the returned Health (or its Nodes/Apps elements)
// cannot affect the reconciler's internal state.
func (r *Reconciler) HealthSnapshot() Health {
	r.healthMu.RLock()
	defer r.healthMu.RUnlock()
	return copyHealth(r.health)
}

// setHealth stores a deep copy of h as the reconciler's current health view.
func (r *Reconciler) setHealth(h Health) {
	r.healthMu.Lock()
	defer r.healthMu.Unlock()
	r.health = copyHealth(h)
}

// copyHealth returns a deep copy of h: fresh Nodes/Apps slices (with their
// elements copied by value — none of them contain reference types), so
// neither the source nor the returned copy alias the other's backing array.
func copyHealth(h Health) Health {
	out := Health{Edge: h.Edge}
	if h.Nodes != nil {
		out.Nodes = make([]NodeHealth, len(h.Nodes))
		copy(out.Nodes, h.Nodes)
	}
	if h.Apps != nil {
		out.Apps = make([]AppHealth, len(h.Apps))
		copy(out.Apps, h.Apps)
	}
	return out
}

// signalNudge sends a non-blocking wake-up on r.nudge, if one has been set
// via SetNudge. It never blocks: if the channel is unbuffered/full (a wake-up
// is already pending), the send is silently skipped.
func (r *Reconciler) signalNudge() {
	if r.nudge == nil {
		return
	}
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

// localNode resolves the local node through the same resolver used for
// per-app placement. Used by callers (e.g. the Phase 9a-Task 4 image
// transfer path) that need the controller's own Docker regardless of which
// node an app is placed on.
func (r *Reconciler) localNode() (node.Node, error) {
	return r.resolver.Resolve("local")
}

// ensureImageOnNode makes ref available on target, which may be a remote
// (docker-over-ssh) node that does not share the controller's local image
// store. It is used in place of a plain target.EnsureImage for any node that
// ResolveMeta reports as non-local, and mirrors Local.EnsureImage's
// pinned-vs-mutable distinction so a remote app re-pulls a moved tag the
// same way a local one does:
//
//  1. If ref is a pinned digest (contains "@sha256:") and target already
//     reports it present, nothing to do — pinned digests are immutable, so a
//     present copy is used as-is with no pull attempted.
//  2. Otherwise (a mutable tag, or a pinned digest not yet present), try
//     target.EnsureImage (a registry pull run ON the target node itself) —
//     this covers the common case of a public/private image ref the target
//     can fetch on its own, with no data ever flowing through the
//     controller, AND picks up a tag that has moved since it was last
//     pulled. Its error, if any, is deliberately ignored here: a
//     locally-built or otherwise unregistered ref is expected to fail this
//     step, and the only thing that matters is whether the image ended up
//     present.
//  3. If ref is still absent, it isn't pullable by the target directly (e.g.
//     a controller-local `lwd-build/...` tag): move it via `docker save` on
//     the controller's own local node piped into `docker load` on target.
//
// An ImagePresent failure (as opposed to a clean "not present" answer) is a
// hard failure at any step — it means the target's Docker is unreachable or
// misbehaving, not that the image needs fetching, so no pull or transfer is
// attempted.
func (r *Reconciler) ensureImageOnNode(ctx context.Context, target node.Node, ref string) error {
	if strings.Contains(ref, "@sha256:") {
		present, err := target.ImagePresent(ctx, ref)
		if err != nil {
			return fmt.Errorf("check image present: %w", err)
		}
		if present {
			return nil
		}
	}

	// Mutable tag (always re-pulled regardless of presence, so a moved tag
	// is picked up), or a pinned digest confirmed absent above: attempt a
	// pull on the target itself before falling back to a save|load transfer.
	_ = target.EnsureImage(ctx, ref)

	present, err := target.ImagePresent(ctx, ref)
	if err != nil {
		return fmt.Errorf("check image present after pull: %w", err)
	}
	if present {
		return nil
	}

	local, err := r.localNode()
	if err != nil {
		return fmt.Errorf("resolve local node for image transfer: %w", err)
	}
	rc, err := local.SaveImage(ctx, ref)
	if err != nil {
		return fmt.Errorf("save image %s: %w", ref, err)
	}
	defer rc.Close()
	if err := target.LoadImage(ctx, rc); err != nil {
		return fmt.Errorf("load image %s on target node: %w", ref, err)
	}

	present, err = target.ImagePresent(ctx, ref)
	if err != nil {
		return fmt.Errorf("check image present after transfer: %w", err)
	}
	if !present {
		return fmt.Errorf("image %s still absent on target node after save/load transfer", ref)
	}
	return nil
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

// Apply reconciles one app. After validating the spec, it dispatches on the
// app's shape: a git-built app ([git]+[build]) goes through applyGit (clone,
// build, blue-green surface); a Phase-4 compose app goes through
// applyCompose (delegated to `docker compose`, in place); anything else is a
// plain image app, deployed via zero-downtime blue-green:
//  1. Ensure the router and the shared network are up, the image present, and
//     any declared backing services running.
//  2. Start a new surface container under a fresh, uniquely numbered name,
//     connected to the backing network too if the app declares services.
//  3. Stage it behind a throwaway internal host on the router and health-check
//     it THROUGH Caddy (or via Docker health / liveness, layered — see
//     checkHealth) rather than talking to it directly.
//  4. On success: flip the real domain to the new container, drop the staging
//     route, retire and remove the old surface (if any), and record a
//     StatusRunning deployment.
//  5. On failure: drop the staging route, remove the new container, and
//     record a StatusFailed deployment. The live domain is never touched.
func (r *Reconciler) Apply(ctx context.Context, app *spec.App) (*store.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := app.Validate(); err != nil {
		return nil, fmt.Errorf("invalid spec: %w", err)
	}

	var dep *store.Deployment
	var err error
	switch {
	case app.Git != nil:
		dep, err = r.applyGit(ctx, app)
	case app.Compose != "":
		dep, err = r.applyCompose(ctx, app)
	default:
		dep, err = r.applyImage(ctx, app)
	}
	// Wake a waiting continuous-reconciler loop (Phase 10) right after a
	// successful manual deploy, rather than leaving it idle until the next
	// ticker tick. Non-blocking: signalNudge is a no-op if no loop has been
	// wired up via SetNudge (e.g. in tests) or a wake-up is already pending.
	if err == nil {
		r.signalNudge()
	}
	return dep, err
}

// applyImage deploys a plain single-service image app via zero-downtime
// blue-green, ensuring any declared backing services first. Callers must
// hold r.mu; Apply does so before branching here.
func (r *Reconciler) applyImage(ctx context.Context, app *spec.App) (*store.Deployment, error) {
	return r.applyImageProvenance(ctx, app, nil)
}

// applyImageProvenance is applyImage's body, parameterized by an optional
// scheduledOverride: nil means "use whatever resolvePlacement determines"
// (the normal Apply path), while a non-nil value forces the Scheduled
// provenance recorded on the resulting deployment regardless of what
// resolvePlacement computes for app.Node. This exists for
// healSurfaceLocked: the spec.App it reconstructs from a prior deployment's
// snapshot already carries a concrete Node (the scheduler's original
// choice, or an explicit pin), so resolvePlacement's own pinned/unpinned
// test can no longer tell the two apart — the healed surface must instead
// keep the ORIGINAL deployment's provenance verbatim.
func (r *Reconciler) applyImageProvenance(ctx context.Context, app *spec.App, scheduledOverride *bool) (*store.Deployment, error) {
	// Resolve placement (scheduling an unpinned app, or passing a pinned
	// Node through unchanged) BEFORE the spec snapshot is marshaled, so the
	// recorded deployment's Spec captures the concrete node an unpinned app
	// actually landed on, not the "" it declared.
	chosen, scheduled, err := r.resolvePlacement(ctx, app)
	if scheduledOverride != nil {
		scheduled = *scheduledOverride
	}
	if err != nil {
		_, _ = r.store.RecordDeployment(store.Deployment{
			App:       app.Name,
			Image:     app.Image,
			Status:    store.StatusFailed,
			CreatedAt: time.Now(),
		})
		return nil, fmt.Errorf("schedule: %w", err)
	}
	app.Node = chosen

	specJSON, err := json.Marshal(app)
	if err != nil {
		return nil, fmt.Errorf("marshal spec snapshot: %w", err)
	}

	// Resolve the target node BEFORE touching anything (router, network,
	// image, the currently-running deployment): an unknown/unreachable node
	// fails the deploy closed, same as a secret resolve failure below.
	n, meshAddr, dockerHost, isLocal, err := r.resolver.ResolveMeta(app.Node)
	if err != nil {
		_, _ = r.store.RecordDeployment(store.Deployment{
			App:       app.Name,
			Image:     app.Image,
			Status:    store.StatusFailed,
			CreatedAt: time.Now(),
			Spec:      string(specJSON),
		})
		return nil, fmt.Errorf("resolve node %q: %w", app.Node, err)
	}

	if err := r.router.EnsureUp(ctx); err != nil {
		return nil, fmt.Errorf("ensure router: %w", err)
	}
	if err := n.EnsureNetwork(ctx, lwdNetwork); err != nil {
		return nil, fmt.Errorf("ensure network: %w", err)
	}
	if isLocal {
		if err := n.EnsureImage(ctx, app.Image); err != nil {
			return nil, fmt.Errorf("ensure image: %w", err)
		}
	} else {
		if err := r.ensureImageOnNode(ctx, n, app.Image); err != nil {
			return nil, fmt.Errorf("ensure image: %w", err)
		}
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
	env := mergeEnv(app.Env, secretVals)

	// Backing services (if declared) are ensured before the surface starts,
	// same as the git path: they're pinned infrastructure the surface may
	// depend on at startup (e.g. a database URL pointing at it by name).
	network, composeContent, err := r.ensureBacking(ctx, n, app, env, dockerHost)
	if err != nil {
		_, _ = r.store.RecordDeployment(store.Deployment{
			App:       app.Name,
			Image:     app.Image,
			Status:    store.StatusFailed,
			CreatedAt: time.Now(),
			Spec:      string(specJSON),
		})
		return nil, fmt.Errorf("ensure backing: %w", err)
	}

	return r.deployBlueGreenSurface(ctx, n, app, app.Image, env, network, composeContent, specJSON, isLocal, meshAddr, scheduled)
}

// applyGit deploys a git-built app: clone the declared ref with the box's
// git, build its Dockerfile into a locally-tagged image (skipping the build
// if that exact sha's tag already exists — idempotent redeploys), and run it
// via the same zero-downtime blue-green surface path as an image app. Backing
// services, if declared, are ensured first (same as applyImage). Callers must
// hold r.mu; Apply does so before branching here.
func (r *Reconciler) applyGit(ctx context.Context, app *spec.App) (*store.Deployment, error) {
	// Resolve placement BEFORE the spec snapshot is marshaled, same rationale
	// as applyImage.
	chosen, scheduled, err := r.resolvePlacement(ctx, app)
	if err != nil {
		_, _ = r.store.RecordDeployment(store.Deployment{
			App:       app.Name,
			Status:    store.StatusFailed,
			CreatedAt: time.Now(),
		})
		return nil, fmt.Errorf("schedule: %w", err)
	}
	app.Node = chosen

	specJSON, err := json.Marshal(app)
	if err != nil {
		return nil, fmt.Errorf("marshal spec snapshot: %w", err)
	}

	// Resolve the target node BEFORE touching anything, same rationale as
	// applyImage.
	n, meshAddr, dockerHost, isLocal, err := r.resolver.ResolveMeta(app.Node)
	if err != nil {
		_, _ = r.store.RecordDeployment(store.Deployment{
			App:       app.Name,
			Status:    store.StatusFailed,
			CreatedAt: time.Now(),
			Spec:      string(specJSON),
		})
		return nil, fmt.Errorf("resolve node %q: %w", app.Node, err)
	}

	if err := r.router.EnsureUp(ctx); err != nil {
		return nil, fmt.Errorf("ensure router: %w", err)
	}
	if err := n.EnsureNetwork(ctx, lwdNetwork); err != nil {
		return nil, fmt.Errorf("ensure network: %w", err)
	}

	// Resolve declared secrets BEFORE cloning/building/ensuring backing.
	// Fail closed: on error, record the failed attempt and return without
	// ever shelling out to git, docker build, or docker compose.
	secretVals, err := r.secrets.Resolve(app.Name, app.Secrets)
	if err != nil {
		_, _ = r.store.RecordDeployment(store.Deployment{
			App:       app.Name,
			Status:    store.StatusFailed,
			CreatedAt: time.Now(),
			Spec:      string(specJSON),
		})
		return nil, fmt.Errorf("resolve secrets: %w", err)
	}
	env := mergeEnv(app.Env, secretVals)

	// Backing first: the source clone and build are the slowest and most
	// failure-prone steps, so bring up any declared pinned services (and fail
	// fast if that doesn't work) before spending time on them.
	network, composeContent, err := r.ensureBacking(ctx, n, app, env, dockerHost)
	if err != nil {
		_, _ = r.store.RecordDeployment(store.Deployment{
			App:       app.Name,
			Status:    store.StatusFailed,
			CreatedAt: time.Now(),
			Spec:      string(specJSON),
		})
		return nil, fmt.Errorf("ensure backing: %w", err)
	}

	dir, err := os.MkdirTemp("", "lwd-git-*")
	if err != nil {
		return nil, fmt.Errorf("create temp clone dir: %w", err)
	}
	defer os.RemoveAll(dir)

	sha, err := r.src.Clone(ctx, app.Git.URL, app.Git.Ref, dir)
	if err != nil {
		_, _ = r.store.RecordDeployment(store.Deployment{
			App:       app.Name,
			Status:    store.StatusFailed,
			CreatedAt: time.Now(),
			Spec:      string(specJSON),
			Compose:   composeContent,
		})
		return nil, fmt.Errorf("clone: %w", err)
	}

	tag := fmt.Sprintf("lwd-build/%s:%s", app.Name, shortSHA(sha))
	contextDir := filepath.Join(dir, app.Git.Path)
	dockerfile := "Dockerfile"
	if app.Build != nil && app.Build.Dockerfile != "" {
		dockerfile = app.Build.Dockerfile
	}

	exists, err := r.bld.ImageExists(ctx, tag)
	if err != nil {
		_, _ = r.store.RecordDeployment(store.Deployment{
			App:       app.Name,
			Image:     tag,
			Status:    store.StatusFailed,
			CreatedAt: time.Now(),
			Spec:      string(specJSON),
			Compose:   composeContent,
		})
		return nil, fmt.Errorf("check image exists: %w", err)
	}
	if !exists {
		if err := r.bld.Build(ctx, contextDir, dockerfile, tag); err != nil {
			_, _ = r.store.RecordDeployment(store.Deployment{
				App:       app.Name,
				Image:     tag,
				Status:    store.StatusFailed,
				CreatedAt: time.Now(),
				Spec:      string(specJSON),
				Compose:   composeContent,
			})
			return nil, fmt.Errorf("build: %w", err)
		}
	}

	// The build above always runs on the controller (this method's node.Node,
	// bld, is always local); a build stays local even when the app is placed
	// on a remote node. Move the freshly built tag to the target node before
	// running it there.
	if !isLocal {
		if err := r.ensureImageOnNode(ctx, n, tag); err != nil {
			_, _ = r.store.RecordDeployment(store.Deployment{
				App:       app.Name,
				Image:     tag,
				Status:    store.StatusFailed,
				CreatedAt: time.Now(),
				Spec:      string(specJSON),
				Compose:   composeContent,
			})
			return nil, fmt.Errorf("ensure image on node %q: %w", app.Node, err)
		}
	}

	return r.deployBlueGreenSurface(ctx, n, app, tag, env, network, composeContent, specJSON, isLocal, meshAddr, scheduled)
}

// rollbackGit redeploys app (a git-built app's spec snapshot, with Image
// already pinned to the prior deployment's built tag) directly through the
// blue-green surface path, WITHOUT re-cloning or rebuilding — the tag is
// already local from the original deploy. Backing services are re-ensured
// (idempotent; pinned services already running are left alone by compose
// semantics). Unlike applyGit/applyImage, this takes r.mu itself: Rollback
// (its only caller) does not hold the lock, to avoid deadlocking against
// Apply.
//
// scheduled carries the placement provenance of the snapshot being restored
// (see rollbackGitLocked): it is NOT recomputed here, since app.Node is
// already the concrete node from that snapshot and resolvePlacement isn't
// even consulted on this path.
func (r *Reconciler) rollbackGit(ctx context.Context, app *spec.App, scheduled bool) (*store.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rollbackGitLocked(ctx, app, scheduled)
}

// rollbackGitLocked is rollbackGit's body, extracted so the Phase 10 heal
// path (healSurfaceLocked) can redeploy a git app's recorded spec+built tag
// while ALREADY holding r.mu — calling rollbackGit itself would deadlock.
// Callers must hold r.mu.
//
// scheduled is the Scheduled provenance of the deployment being restored —
// Rollback passes prev.Scheduled (the previous deployment's provenance),
// healSurfaceLocked passes cur.Scheduled (the dead deployment's provenance)
// — so a rolled-back/healed git surface keeps its original movability
// rather than being (re-)classified from app.Node, which is always a
// concrete node by the time it reaches here.
func (r *Reconciler) rollbackGitLocked(ctx context.Context, app *spec.App, scheduled bool) (*store.Deployment, error) {
	// Validate the declared shape (git url, build, domain, port, services...)
	// against a copy with Image cleared: spec.Validate rejects a git app that
	// declares Image (that combination is meaningless for user-authored
	// lwd.toml, since Image is only ever populated by a completed build), but
	// here Image is deliberately the previous deployment's built tag, pinned
	// by Rollback so this redeploy skips cloning/building entirely.
	unpinned := *app
	unpinned.Image = ""
	if err := unpinned.Validate(); err != nil {
		return nil, fmt.Errorf("invalid spec: %w", err)
	}
	if app.Image == "" {
		return nil, fmt.Errorf("no built image tag recorded to roll back to for %q", app.Name)
	}

	specJSON, err := json.Marshal(app)
	if err != nil {
		return nil, fmt.Errorf("marshal spec snapshot: %w", err)
	}

	n, meshAddr, dockerHost, isLocal, err := r.resolver.ResolveMeta(app.Node)
	if err != nil {
		_, _ = r.store.RecordDeployment(store.Deployment{
			App:       app.Name,
			Image:     app.Image,
			Status:    store.StatusFailed,
			CreatedAt: time.Now(),
			Spec:      string(specJSON),
		})
		return nil, fmt.Errorf("resolve node %q: %w", app.Node, err)
	}

	if err := r.router.EnsureUp(ctx); err != nil {
		return nil, fmt.Errorf("ensure router: %w", err)
	}
	if err := n.EnsureNetwork(ctx, lwdNetwork); err != nil {
		return nil, fmt.Errorf("ensure network: %w", err)
	}

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
	env := mergeEnv(app.Env, secretVals)

	network, composeContent, err := r.ensureBacking(ctx, n, app, env, dockerHost)
	if err != nil {
		_, _ = r.store.RecordDeployment(store.Deployment{
			App:       app.Name,
			Image:     app.Image,
			Status:    store.StatusFailed,
			CreatedAt: time.Now(),
			Spec:      string(specJSON),
		})
		return nil, fmt.Errorf("ensure backing: %w", err)
	}

	// The restored image tag was built (or transferred) on the controller by
	// the original deploy; if this rollback targets a non-local node, make
	// sure that tag is present there too before running it.
	if !isLocal {
		if err := r.ensureImageOnNode(ctx, n, app.Image); err != nil {
			_, _ = r.store.RecordDeployment(store.Deployment{
				App:       app.Name,
				Image:     app.Image,
				Status:    store.StatusFailed,
				CreatedAt: time.Now(),
				Spec:      string(specJSON),
				Compose:   composeContent,
			})
			return nil, fmt.Errorf("ensure image on node %q: %w", app.Node, err)
		}
	}

	return r.deployBlueGreenSurface(ctx, n, app, app.Image, env, network, composeContent, specJSON, isLocal, meshAddr, scheduled)
}

// rollbackImage redeploys app (a restored plain image app's spec snapshot —
// neither git-built nor compose) directly through the blue-green surface
// path, preserving scheduled (the placement provenance of the snapshot being
// restored) exactly the way rollbackGit does for a git app: Rollback's
// caller passes prev.Scheduled, since app.Node is already the concrete node
// that snapshot ran on, and Apply's own resolvePlacement can no longer tell
// a scheduler-placed surface apart from an explicitly pinned one at that
// point — see applyImageProvenance's doc comment for the same reasoning
// healSurfaceLocked/rescheduleSurfaceLocked rely on. Unlike applyImage, this
// takes r.mu itself: Rollback (its only caller for the image branch) does
// not hold the lock, to avoid deadlocking against Apply.
func (r *Reconciler) rollbackImage(ctx context.Context, app *spec.App, scheduled bool) (*store.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := app.Validate(); err != nil {
		return nil, fmt.Errorf("invalid spec: %w", err)
	}
	return r.applyImageProvenance(ctx, app, &scheduled)
}

// healSurfaceLocked recreates a dead surface from cur, the app's current
// recorded deployment row: it reconstructs the spec.App that produced it from
// cur.Spec (the JSON snapshot captured at deploy time), pins Image back to
// cur.Image, and redeploys through the existing blue-green path — reusing the
// already-built/already-present image, never re-cloning or rebuilding a git
// app. This is the Phase 10 control loop's self-heal primitive: called while
// the loop ALREADY holds r.mu (it observed cur as dead via surfaceIsDead
// under the same lock), so — unlike Rollback and Apply — it must NOT lock
// r.mu itself; doing so would deadlock.
func (r *Reconciler) healSurfaceLocked(ctx context.Context, cur *store.Deployment) (*store.Deployment, error) {
	var restored spec.App
	if cur.Spec == "" {
		return nil, fmt.Errorf("no spec snapshot to heal %q", cur.App)
	}
	if err := json.Unmarshal([]byte(cur.Spec), &restored); err != nil {
		return nil, fmt.Errorf("unmarshal spec snapshot for %q: %w", cur.App, err)
	}
	restored.Image = cur.Image
	if restored.Git != nil {
		// reuses built tag, no clone/build; cur.Scheduled carries the dead
		// deployment's placement provenance forward, since restored.Node is
		// already the concrete node it was running on, not "".
		return r.rollbackGitLocked(ctx, &restored, cur.Scheduled)
	}
	if err := restored.Validate(); err != nil {
		return nil, fmt.Errorf("invalid spec snapshot for %q: %w", cur.App, err)
	}
	// applyImageProvenance assumes r.mu held; image already present. The
	// override forces Scheduled to cur.Scheduled: resolvePlacement would
	// otherwise see restored.Node already concrete (the scheduler's earlier
	// choice) and misclassify it as pinned.
	return r.applyImageProvenance(ctx, &restored, &cur.Scheduled)
}

// surfaceIsDead classifies a node.ContainerHealth observation (state, err) as
// needing a heal: any error observing health, or any non-"running" state,
// counts as dead. A container that has disappeared entirely may surface as
// either an error or an empty/non-running state depending on the node.Node
// implementation; both are treated the same way here.
func surfaceIsDead(state string, err error) bool {
	return err != nil || state != "running"
}

// mergeEnv merges an app's plain declared env with its resolved secret
// values, with secrets winning on key collision. Used by both the image and
// git deploy paths.
func mergeEnv(base, secretVals map[string]string) map[string]string {
	env := make(map[string]string, len(base)+len(secretVals))
	for k, v := range base {
		env[k] = v
	}
	for k, v := range secretVals {
		env[k] = v // secrets win on key collision with plain env
	}
	return env
}

// shortSHA returns the first 12 characters of a git commit sha (the
// convention used for lwd-build/<app>:<shortsha> image tags), or the whole
// string if it's shorter than that.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// ensureBacking renders app's declared [[service]] backing services (if any)
// into a compose project and brings it up, PINNED, on a dedicated per-app
// network — used by both the image and git deploy paths, and idempotently
// re-run on every redeploy/rollback (compose only recreates what changed, so
// already-running backing services and their data are left alone). Returns
// ("", "", nil) if the app declares no services.
//
// The rendered YAML (composeContent) contains only ${NAME} references for a
// service's declared secrets — never a resolved value — because it is
// persisted verbatim to store.Deployment.Compose (plaintext) and served back
// over the API. env is the app's merged env+resolved-secrets map; it is used
// ONLY as the transient process env passed to `docker compose up` (UpSpec.Env
// below), which is how compose actually resolves each ${NAME} reference at
// up-time. env is never written to disk or to the store.
// dockerHost is the target node's DOCKER_HOST value from the same
// ResolveMeta call that produced n ("" for the local node). When non-empty,
// it is added to the process env `docker compose up` runs with, so a backing
// project for an app placed on a remote node is created on that node's own
// Docker daemon instead of the controller's local one — n's EnsureNetwork
// call above already targets the remote node directly, but the Composer
// shells out to the `docker compose` CLI plugin, which only respects
// DOCKER_HOST from its own process environment, not from n.
func (r *Reconciler) ensureBacking(ctx context.Context, n node.Node, app *spec.App, env map[string]string, dockerHost string) (network string, composeContent string, err error) {
	if len(app.Services) == 0 {
		return "", "", nil
	}

	yamlContent, net := RenderBackingCompose(app.Name, app.Services)

	if err := n.EnsureNetwork(ctx, net); err != nil {
		return "", "", fmt.Errorf("ensure backing network: %w", err)
	}

	tmp, err := os.CreateTemp("", "lwd-backing-*.yml")
	if err != nil {
		return "", "", fmt.Errorf("create temp backing compose file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(yamlContent); err != nil {
		tmp.Close()
		return "", "", fmt.Errorf("write temp backing compose file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", "", fmt.Errorf("close temp backing compose file: %w", err)
	}

	composeEnv := env
	if dockerHost != "" {
		composeEnv = make(map[string]string, len(env)+1)
		for k, v := range env {
			composeEnv[k] = v
		}
		composeEnv["DOCKER_HOST"] = dockerHost
	}

	if err := r.compose.Up(ctx, compose.UpSpec{Project: "lwd-" + app.Name, File: tmp.Name(), Env: composeEnv}); err != nil {
		return "", "", fmt.Errorf("backing compose up: %w", err)
	}

	return net, yamlContent, nil
}

// deployBlueGreenSurface runs the shared back half of an image or git deploy:
// start a new surface container under image, connect it to backingNetwork
// (if non-empty), stage it behind a throwaway host, health-check it, flip
// the live route, retire the old surface, and record a StatusRunning
// deployment (with composeContent, if any, so Remove/Rollback know a backing
// project exists). On any failure, the attempt is torn down and recorded
// StatusFailed; the live route and any previously-running deployment are
// left completely untouched (blue-green's isolation guarantee). Callers must
// hold r.mu, have already validated the spec, ensured the router/lwd network,
// resolved secrets into env, and ensured backing services.
//
// isLocal and meshAddr (from the same ResolveMeta call that produced n)
// decide how the surface is exposed to the central Caddy: a local surface
// publishes no host port at all — Caddy reaches it by container name on the
// shared lwd network, exactly as before Phase 9a. A remote surface instead
// publishes an ephemeral host port bound to the node's mesh address (the
// only address Caddy, running on the controller, can reach across nodes),
// and Caddy's upstream becomes that meshAddr:port instead of the container
// name.
//
// scheduled is placement provenance (Phase 11b): recorded verbatim as
// Scheduled on the StatusRunning deployment this call produces. Callers pass
// whatever resolvePlacement determined (applyImage/applyGit), or the
// provenance carried forward from a restored snapshot (rollbackGitLocked).
func (r *Reconciler) deployBlueGreenSurface(ctx context.Context, n node.Node, app *spec.App, image string, env map[string]string, backingNetwork, composeContent string, specJSON []byte, isLocal bool, meshAddr string, scheduled bool) (*store.Deployment, error) {
	deployID, err := r.store.NextDeployID()
	if err != nil {
		return nil, fmt.Errorf("next deploy id: %w", err)
	}
	name := containerName(app, deployID)
	stageHost := stagingHost(deployID)

	runSpec := node.RunSpec{
		Name:  name,
		Image: image,
		Env:   env,
		Labels: map[string]string{
			"lwd.app":    app.Name,
			"lwd.role":   "surface",
			"lwd.deploy": strconv.FormatInt(deployID, 10),
		},
		Port:        app.Port,
		Network:     lwdNetwork,
		CPUs:        reqCPU(app),
		MemoryBytes: reqMem(app),
	}
	if !isLocal {
		// HostPort 0 asks the node to assign an ephemeral port; the actual
		// bound port comes back on Container.HostPort below.
		runSpec.Publish = []node.PortMapping{{HostIP: meshAddr, HostPort: 0, ContainerPort: app.Port}}
	}

	c, err := n.RunContainer(ctx, runSpec)
	if err != nil {
		return nil, fmt.Errorf("run container: %w", err)
	}

	// upstream/upstreamPort are what Caddy is told to reverse_proxy to, for
	// both the staging probe below and the live route on success: the
	// container's name+declared port for a local surface (Caddy resolves the
	// name on the shared network), or the node's mesh address + the
	// container's freshly published host port for a remote one.
	upstream := name
	upstreamPort := app.Port
	if !isLocal {
		upstream = meshAddr
		upstreamPort = c.HostPort
	}

	if backingNetwork != "" {
		if err := n.ConnectContainerToNetwork(ctx, c.ID, backingNetwork); err != nil {
			_ = n.RemoveContainer(ctx, c.ID)
			return nil, fmt.Errorf("connect container to backing network: %w", err)
		}
	}

	if err := r.router.SetStaging(ctx, stageHost, upstream, upstreamPort); err != nil {
		_ = n.RemoveContainer(ctx, c.ID)
		return nil, fmt.Errorf("set staging route: %w", err)
	}

	if healthErr := r.checkHealth(ctx, n, app, stageHost, c.ID); healthErr != nil {
		// The prior running deployment (if any) is left completely untouched:
		// its container keeps running and the live domain/route still points
		// at it. Blue-green means a failed candidate never affects what's
		// currently serving traffic, so we must not retire or otherwise mutate
		// that row here.
		r.recordFailedSurface(ctx, n, app, stageHost, c.ID, image, specJSON, composeContent)
		return nil, fmt.Errorf("health check failed: %w", healthErr)
	}

	if err := r.router.SetRoute(ctx, router.Route{
		Domain:      app.Domain,
		Upstream:    upstream,
		Port:        upstreamPort,
		TLSInternal: router.UseInternalTLS(app.Domain),
	}); err != nil {
		// The flip itself failed: undo the staging state and the new
		// container, and record the attempt as failed (same isolation
		// guarantee as the health-failure branch above — the live domain,
		// still pointing at the old container or unset if this is the first
		// deploy, is unaffected, and the prior running deployment row is left
		// untouched).
		r.recordFailedSurface(ctx, n, app, stageHost, c.ID, image, specJSON, composeContent)
		return nil, fmt.Errorf("set route: %w", err)
	}
	_ = r.router.RemoveStaging(ctx, stageHost)

	// Retire and remove the old surface, if any, now that the new one is live.
	if prev, perr := r.store.CurrentDeployment(app.Name); perr == nil && prev != nil {
		_ = n.RemoveContainer(ctx, prev.ContainerID)
		_ = r.store.SetStatus(prev.ID, store.StatusRetired)
	}

	dep := store.Deployment{
		App:         app.Name,
		Image:       image,
		ContainerID: c.ID,
		Status:      store.StatusRunning,
		CreatedAt:   time.Now(),
		Spec:        string(specJSON),
		Compose:     composeContent,
		Scheduled:   scheduled,
	}
	id, err := r.store.RecordDeployment(dep)
	if err != nil {
		return nil, fmt.Errorf("record deployment: %w", err)
	}
	dep.ID = id

	// Every successful surface deploy — manual Apply, Rollback, or a Phase 10
	// self-heal — clears the app's heal backoff: whatever was making it
	// unhealthy (if anything) has now demonstrably been resolved by a fresh,
	// health-gated container.
	r.resetHeal(app.Name)

	return &dep, nil
}

// resetHeal clears app's in-progress self-heal bookkeeping (attempt count and
// backoff gate), if any. Called on every successful surface deploy so a
// healthy redeploy doesn't carry forward a stale backoff from before it was
// fixed. Callers must hold r.mu.
func (r *Reconciler) resetHeal(app string) {
	delete(r.heal, app)
}

// recordFailedSurface tears down a failed blue-green candidate: it removes
// the staging route (a no-op if already removed) and the new container, then
// records a StatusFailed deployment carrying the attempt's Spec (and, if the
// app declares backing services, its rendered Compose) snapshot. Errors from
// cleanup and from recording are intentionally swallowed here — the caller
// always returns its own wrapped error for the actual failure cause. The
// prior running deployment/route is never touched by this helper, preserving
// blue-green's isolation guarantee: a failed candidate, however it failed,
// never affects what's currently serving traffic.
func (r *Reconciler) recordFailedSurface(ctx context.Context, n node.Node, app *spec.App, stageHost, containerID, image string, specJSON []byte, composeContent string) {
	_ = r.router.RemoveStaging(ctx, stageHost)
	_ = n.RemoveContainer(ctx, containerID)

	_, _ = r.store.RecordDeployment(store.Deployment{
		App:         app.Name,
		Image:       image,
		ContainerID: containerID,
		Status:      store.StatusFailed,
		CreatedAt:   time.Now(),
		Spec:        string(specJSON),
		Compose:     composeContent,
	})
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
	specJSON, err := json.Marshal(app)
	if err != nil {
		return nil, fmt.Errorf("marshal spec snapshot: %w", err)
	}

	n, err := r.resolver.Resolve(app.Node)
	if err != nil {
		r.recordComposeFailed(app, "", specJSON, "")
		return nil, fmt.Errorf("resolve node %q: %w", app.Node, err)
	}

	if err := r.router.EnsureUp(ctx); err != nil {
		return nil, fmt.Errorf("ensure router: %w", err)
	}
	if err := n.EnsureNetwork(ctx, lwdNetwork); err != nil {
		return nil, fmt.Errorf("ensure network: %w", err)
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

	if err := n.ConnectContainerToNetwork(ctx, id, lwdNetwork); err != nil {
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

	// Both a Phase-4 compose app and an image/git app with backing services
	// leave cur.Compose non-empty, but they're removed differently: a
	// compose app's Compose IS the app (docker compose down tears down
	// everything, no separate surface container); a git/image app's Compose
	// is only its PINNED backing project alongside a surface container that
	// still needs removing by label. isComposeApp distinguishes the two by
	// checking whether the app's own spec snapshot declared a Compose file.
	if cur != nil && cur.Compose != "" && isComposeApp(cur.Spec) {
		return r.removeCompose(ctx, appName, cur)
	}
	return r.removeSingleService(ctx, appName, cur)
}

// isComposeApp reports whether specJSON's snapshot is a Phase-4
// user-provided compose app (its App.Compose field names the original
// compose file), as opposed to a git/image app that merely has backing
// services rendered into deployment.Compose.
func isComposeApp(specJSON string) bool {
	if specJSON == "" {
		return false
	}
	var a spec.App
	if err := json.Unmarshal([]byte(specJSON), &a); err != nil {
		return false
	}
	return a.Compose != ""
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

	// A Phase-4 compose app's own remote-node DOCKER_HOST targeting is a
	// separate, pre-existing gap (its Up call in applyCompose has the same
	// limitation) — out of scope for this fix, so no env is passed here.
	project := "lwd-" + appName
	if err := r.compose.Down(ctx, project, tmp.Name(), nil); err != nil {
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

// removeSingleService removes every container labeled lwd.app=appName, downs
// the app's PINNED backing project if it had one (cur.Compose non-empty —
// named volumes are intentionally left in place, data is not auto-destroyed),
// removes the app's Caddy route (resolved from cur's Spec snapshot, if any),
// and retires cur, if present. cur may be nil (nothing recorded for appName
// yet) — removal still runs as a defensive cleanup of any stray containers.
// The node it operates against is resolved from cur's Spec snapshot's Node
// field (the node the app was actually placed on), defaulting to "local"
// when cur is nil or carries no snapshot.
//
// If that node no longer resolves (e.g. it was deregistered via `lwd node
// rm` after the app was deployed to it), remote container/backing teardown
// is impossible — there is nothing left to talk to — but the app must still
// be removable: the controller-side Caddy route is removed and the
// deployment row is retired regardless, with only the node-side cleanup
// skipped (logged, not failed). This mirrors removeCompose's and
// RemoveRoute's own best-effort rationale below: an app should never be
// stuck un-removable because of state that outlived its backing node.
func (r *Reconciler) removeSingleService(ctx context.Context, appName string, cur *store.Deployment) error {
	var domain string
	nodeName := "local"
	if cur != nil {
		domain = domainFromSpec(cur.Spec)
		if nn := nodeFromSpec(cur.Spec); nn != "" {
			nodeName = nn
		}
	}

	// ResolveMeta (not plain Resolve): a PINNED backing project's teardown
	// below needs the node's DOCKER_HOST too, to target the same remote
	// daemon its Up originally ran against.
	n, _, dockerHost, _, err := r.resolver.ResolveMeta(nodeName)
	if err != nil {
		log.Printf("remove %s: node %q no longer resolves (%v); skipping remote container/backing teardown, still removing route and retiring", appName, nodeName, err)
	} else {
		containers, err := n.ListContainers(ctx, map[string]string{"lwd.app": appName})
		if err != nil {
			return fmt.Errorf("list containers: %w", err)
		}
		for _, c := range containers {
			if err := n.RemoveContainer(ctx, c.ID); err != nil {
				return fmt.Errorf("remove container %s: %w", c.ID, err)
			}
		}

		if cur != nil && cur.Compose != "" {
			if err := r.downBacking(ctx, appName, cur.Compose, dockerHost); err != nil {
				return err
			}
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

// downBacking tears down a git/image app's PINNED backing project via
// `docker compose down`, writing its stored rendered compose content
// (composeContent) to a temp file since Down takes a file path. Named
// volumes are not pruned by `down` without `-v`, so data persists.
//
// dockerHost is the app's node's DOCKER_HOST value ("" for the local node),
// passed through so a remote node's backing project is torn down against its
// own Docker daemon — the same one ensureBacking originally brought it up
// on — rather than the controller's.
func (r *Reconciler) downBacking(ctx context.Context, appName, composeContent, dockerHost string) error {
	tmp, err := os.CreateTemp("", "lwd-backing-rm-*.yml")
	if err != nil {
		return fmt.Errorf("create temp backing compose file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(composeContent); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp backing compose file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp backing compose file: %w", err)
	}

	var env map[string]string
	if dockerHost != "" {
		env = map[string]string{"DOCKER_HOST": dockerHost}
	}

	project := "lwd-" + appName
	if err := r.compose.Down(ctx, project, tmp.Name(), env); err != nil {
		return fmt.Errorf("backing compose down: %w", err)
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

// nodeFromSpec extracts the Node field from a deployment's JSON Spec
// snapshot, returning "" if specJSON is empty, fails to unmarshal, or the
// snapshot predates the Node field (all of which the caller treats as "use
// the local node").
func nodeFromSpec(specJSON string) string {
	if specJSON == "" {
		return ""
	}
	var a spec.App
	if err := json.Unmarshal([]byte(specJSON), &a); err != nil {
		return ""
	}
	return a.Node
}

// Rollback redeploys the most recent retired ("previous") deployment for app,
// restoring its exact image via a fresh blue-green deploy — so a rollback is
// itself zero-downtime and health-gated like any other deploy. It reads the
// previous deployment's stored Spec snapshot (captured at the time that
// deployment was originally applied) rather than re-resolving lwd.toml, so it
// restores precisely what was running before, even if the local spec file has
// since changed.
//
// It dispatches on the restored spec's shape:
//
//   - A git-built app (restored.Git != nil) goes through rollbackGit, which
//     redeploys the previous deployment's built image tag
//     (lwd-build/<app>:<oldsha>, still local) directly — no re-clone, no
//     rebuild — re-ensuring backing services from the restored spec.
//   - A Phase-4 compose app (restored.Compose non-empty — i.e. the app's OWN
//     spec named a compose file, not merely a git/image app with backing
//     services rendered into the deployment row's Compose) has the previous
//     deployment's stored compose content — not whatever currently lives at
//     the original file path, which may have changed or been deleted since —
//     written to a fresh temp file, and restored.Compose repointed at it
//     before delegating to Apply, so the rollback re-applies the exact prior
//     stack. The temp file is removed once Apply returns (applyCompose reads
//     it synchronously before then, so this is safe).
//   - Anything else (a plain image app, with or without backing services)
//     delegates to Apply directly; applyImage re-ensures backing itself.
//
// Rollback does not hold r.mu itself: it only reads from the store and
// unmarshals JSON before delegating to Apply/rollbackGit, which take the
// lock themselves. Locking here too would deadlock against their own
// Lock/Unlock.
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
	// the same digest/ref/built-tag is restored even if the snapshot's Image
	// field somehow diverged from it. Harmless for a compose app, whose
	// Image is always empty.
	restored.Image = prev.Image

	if restored.Git != nil {
		// rollbackGit bypasses Apply entirely (it redeploys the previous
		// built tag directly), so it needs its own nudge on success; the
		// Compose/image path below falls through to Apply, which already
		// nudges on its own success return. prev.Scheduled carries the
		// restored deployment's placement provenance forward, since
		// restored.Node is already the concrete node it ran on, not "".
		dep, err := r.rollbackGit(ctx, &restored, prev.Scheduled)
		if err == nil {
			r.signalNudge()
		}
		return dep, err
	}

	if restored.Compose != "" {
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
		// A compose app is never Scheduled (compose apps aren't
		// scheduler-placed — resolvePlacement/applyCompose never consult the
		// scheduler for one), so Apply's own provenance here is already
		// correct; no override needed, unlike the plain-image branch below.
		return r.Apply(ctx, &restored)
	}

	// A plain image app (neither git-built nor compose): restored.Node is
	// already the concrete node the snapshot being restored ran on, so
	// delegating to Apply would let its resolvePlacement misclassify a
	// scheduler-placed surface as an operator pin (Scheduled=false),
	// silently losing placement provenance — exactly the bug the git branch
	// above avoids via rollbackGit(..., prev.Scheduled). rollbackImage closes
	// that gap for the image path.
	dep, err := r.rollbackImage(ctx, &restored, prev.Scheduled)
	if err == nil {
		r.signalNudge()
	}
	return dep, err
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
func (r *Reconciler) checkHealth(ctx context.Context, n node.Node, app *spec.App, stagingHost, containerID string) error {
	timeout := app.Health.Timeout
	if timeout <= 0 {
		timeout = defaultHealthTimeout
	}
	deadline := time.Now().Add(timeout)

	if app.Health.Path != "" {
		return r.checkHealthPath(ctx, deadline, stagingHost, app.Health.Path)
	}

	_, dockerHealth, err := n.ContainerHealth(ctx, containerID)
	if err != nil {
		return fmt.Errorf("container health: %w", err)
	}
	if dockerHealth != "" {
		return r.checkDockerHealth(ctx, n, deadline, containerID)
	}

	return r.checkLiveness(ctx, n, deadline, stagingHost, containerID)
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
func (r *Reconciler) checkDockerHealth(ctx context.Context, n node.Node, deadline time.Time, containerID string) error {
	var lastHealth string
	for {
		_, h, err := n.ContainerHealth(ctx, containerID)
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
func (r *Reconciler) checkLiveness(ctx context.Context, n node.Node, deadline time.Time, stagingHost, containerID string) error {
	select {
	case <-time.After(livenessSettle):
	case <-ctx.Done():
		return ctx.Err()
	}

	var lastState string
	var lastStatus int
	for {
		state, _, err := n.ContainerHealth(ctx, containerID)
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
