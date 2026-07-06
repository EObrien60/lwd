package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"lwd/internal/node"
	"lwd/internal/router"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// rescheduleSurfaceLocked moves a scheduler-placed ("movable") surface off
// excludeNode onto a different fitting node: it reconstructs the spec.App
// that produced cur (the app's current recorded deployment row) from
// cur.Spec exactly like healSurfaceLocked, pins Image back to cur.Image,
// asks the scheduler for a fitting node other than excludeNode via
// placeExcluding, and redeploys through the same blue-green path
// healSurfaceLocked uses (rollbackGitLocked for a git app, reusing the
// recorded tag with no re-clone/rebuild; applyImageProvenance otherwise) —
// producing a brand-new StatusRunning surface on the new node with
// Scheduled forced true (this is only ever called for a surface the
// scheduler already placed; see EvacuateNode, which enforces that via
// cur.Scheduled before calling this).
//
// If placeExcluding finds no other fitting node, that error is returned
// unchanged and cur is left completely untouched — no capacity elsewhere
// means nothing to do.
//
// Old-set retirement is NOT done here: since Phase 12 Task 4,
// deployReplicaSet is the single owner of retiring the prior current
// deployment, and it does so correctly per-replica. When the new surface
// goes live inside deployReplicaSet (reached here via applyImageProvenance
// / rollbackGitLocked), store.CurrentDeployment(app.Name) still resolves to
// cur (the new row isn't recorded yet), so deployReplicaSet's retire loop
// removes each of cur's replica containers on ITS OWN node — which is
// excludeNode for this reschedule — and marks cur.ID StatusRetired. It also
// honors the same reachability guard this function used to: a
// known-unreachable excludeNode (a node-loss eviction) has its container
// removal skipped (the container dies with the node) while the row is still
// retired. So rescheduleSurfaceLocked must NOT also remove/retire cur — that
// would RemoveContainer + SetStatus the same container/row twice (masked by
// node.Fake's tolerant map delete, but a wasteful already-removed error on a
// real Local/AgentNode backend).
//
// Callers MUST hold r.mu. cur must be the app's current, Scheduled surface
// deployment, currently running on excludeNode.
func (r *Reconciler) rescheduleSurfaceLocked(ctx context.Context, cur *store.Deployment, excludeNode string) (*store.Deployment, error) {
	var restored spec.App
	if cur.Spec == "" {
		return nil, fmt.Errorf("no spec snapshot to reschedule %q", cur.App)
	}
	if err := json.Unmarshal([]byte(cur.Spec), &restored); err != nil {
		return nil, fmt.Errorf("unmarshal spec snapshot for %q: %w", cur.App, err)
	}
	restored.Image = cur.Image

	newNode, err := r.placeExcluding(ctx, &restored, excludeNode)
	if err != nil {
		// No capacity elsewhere: leave cur untouched, caller decides.
		return nil, fmt.Errorf("place %q off %q: %w", cur.App, excludeNode, err)
	}
	restored.Node = newNode

	scheduled := true
	var moved *store.Deployment
	if restored.Git != nil {
		// Reuses the already-built tag, no clone/build — same shape as
		// healSurfaceLocked's git branch.
		moved, err = r.rollbackGitLocked(ctx, &restored, scheduled)
	} else {
		if verr := restored.Validate(); verr != nil {
			return nil, fmt.Errorf("invalid spec snapshot for %q: %w", cur.App, verr)
		}
		// applyImageProvenance assumes r.mu held; image already present on
		// (or reachable from) the new node. The override forces Scheduled
		// true regardless of what resolvePlacement's now-moot pinned/
		// unpinned test would otherwise compute for the already-concrete
		// restored.Node.
		moved, err = r.applyImageProvenance(ctx, &restored, &scheduled)
	}
	if err != nil {
		// The new deploy failed (health check, route flip, etc): blue-green
		// isolation means the failed attempt never touched cur or its live
		// route. Nothing to retire; return the error as-is.
		return nil, fmt.Errorf("redeploy %q on %q: %w", cur.App, newNode, err)
	}

	// Old-set retirement was already handled by deployReplicaSet (see this
	// function's doc comment) — removing cur's container on excludeNode and
	// marking cur.ID retired. Doing it again here would double-remove/
	// double-retire, so we return the new deployment directly.
	return moved, nil
}

// EvacuateResult reports the outcome of an EvacuateNode call: which apps'
// surfaces were moved off the node, which were left alone because they're
// pinned (not the scheduler's to move), and which failed to move along with
// why.
type EvacuateResult struct {
	Moved   []string      `json:"moved"`
	Skipped []string      `json:"skipped"`
	Failed  []EvacFailure `json:"failed"`
}

// EvacFailure names an app whose surface EvacuateNode attempted to move off
// the node but couldn't (e.g. no other node currently has room for it), and
// why.
type EvacFailure struct {
	App string `json:"app"`
	Err string `json:"err"`
}

// normalizeNode returns n, or "local" if n is empty — the same "" ==
// "local" convention used throughout the replica-aware heal/deploy paths
// (see healDeadReplicasLocked, reconcileReplicaSet) for a store.Replica.Node
// or spec Node value that predates that field always being populated.
func normalizeNode(n string) string {
	if n == "" {
		return "local"
	}
	return n
}

// affectedByNode reports whether cur (an app's current deployment) is
// affected by node name being drained/lost, and — for a replica-aware
// deployment — exactly which of its Replicas sit on it.
//
// A legacy pre-Phase-12 row (Replicas empty) falls back to today's P11b
// single-surface test (nodeFromSpec(cur.Spec) == name): there is no replica
// set to inspect, only the one spec-recorded node. A Phase-12
// deployReplicaSet row (Replicas always populated, including N==1) is
// tested per-replica: affected iff ANY entry's Node matches, and movingIdx
// names exactly those entries (in Replicas order) — an app with 2 of 3
// replicas on name reports both indexes, not just one.
func affectedByNode(cur *store.Deployment, name string) (affected bool, movingIdx []int) {
	if len(cur.Replicas) == 0 {
		return nodeFromSpec(cur.Spec) == name, nil
	}
	for i, rep := range cur.Replicas {
		if normalizeNode(rep.Node) == name {
			movingIdx = append(movingIdx, i)
		}
	}
	return len(movingIdx) > 0, movingIdx
}

// EvacuateNode moves every scheduler-placed surface currently running on the
// named node onto some other fitting node, leaving explicitly pinned
// surfaces and every compose/backing stack completely untouched — compose
// apps are never even considered (they're never Scheduled, but are also
// filtered out explicitly below since this is a data-loss-sensitive
// operation and the intent should never depend on that alone).
//
// For each app (store.ListApps): if it has no current (StatusRunning)
// deployment, isn't a plain surface (a Phase-4 compose app, or has no
// container at all), or has no replica at all currently on name
// (affectedByNode), it's skipped entirely — not reported anywhere in the
// result. Otherwise, a pinned (non-Scheduled) surface is left running and
// reported Skipped.
//
// A Scheduled surface is moved one of two ways (Phase 12 Task 6):
//
//   - A legacy row or a single-replica (len(cur.Replicas) <= 1) deployment
//     gets P11b's whole-surface move via rescheduleSurfaceLocked — a brand
//     new deploy generation on a new node, exactly today's behavior
//     (non-regression: this is the only shape those rows can have, and it's
//     also the correct shape for an N==1 replica set — nothing else to
//     preserve the spread of).
//   - A genuine replica set (len(cur.Replicas) > 1) gets evacuateReplicasLocked:
//     ONLY the affected indexes are rescheduled, in place on the SAME
//     deployment row, each excluding name and every other (untouched)
//     replica's node to preserve spread. A replica that couldn't be placed
//     is reported as a Failed entry for the app WITHOUT aborting the
//     others — so an app can appear in both Moved (>=1 replica actually
//     moved) and Failed (>=1 replica couldn't be) for the same call.
func (r *Reconciler) EvacuateNode(ctx context.Context, name string) (EvacuateResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var result EvacuateResult

	apps, err := r.store.ListApps()
	if err != nil {
		return result, fmt.Errorf("list apps: %w", err)
	}

	for _, app := range apps {
		cur, cerr := r.store.CurrentDeployment(app)
		if cerr != nil || cur == nil {
			continue
		}
		if cur.ContainerID == "" || isComposeApp(cur.Spec) {
			continue
		}
		affected, movingIdx := affectedByNode(cur, name)
		if !affected {
			continue
		}

		if !cur.Scheduled {
			result.Skipped = append(result.Skipped, app)
			continue
		}

		if len(cur.Replicas) <= 1 {
			if _, rerr := r.rescheduleSurfaceLocked(ctx, cur, name); rerr != nil {
				result.Failed = append(result.Failed, EvacFailure{App: app, Err: rerr.Error()})
				continue
			}
			result.Moved = append(result.Moved, app)
			continue
		}

		moved, failed := r.evacuateReplicasLocked(ctx, cur, name, movingIdx)
		if len(moved) > 0 {
			result.Moved = append(result.Moved, app)
		}
		for _, idx := range movingIdx {
			if ferr, ok := failed[idx]; ok {
				result.Failed = append(result.Failed, EvacFailure{App: app, Err: fmt.Sprintf("replica %d: %v", idx, ferr)})
			}
		}
	}

	return result, nil
}

// evacuateReplicasLocked moves ONLY the replicas of cur named by movingIdx
// (indexes into cur.Replicas — every entry currently on excludeNode, per
// affectedByNode) to newly-scheduled nodes, one replica at a time; every
// other replica (on a different, untouched node) is left completely
// alone — its container keeps running and its entry in cur.Replicas is
// copied through unchanged. This mirrors healDeadReplicasLocked's
// "recreate ONLY the named subset in place" shape (Phase 12 Task 5), not
// deployReplicaSet's whole-generation swap: only a subset of replicas
// actually needs to move, so redeploying the whole set would needlessly
// churn every already-healthy replica.
//
// For each moving index: a new node is chosen via placeExcludingSet,
// excluding excludeNode AND every OTHER (untouched) replica's current
// node — preserving spread by never stacking the moved replica onto a node
// already running this app, unless no other node fits (placeExcludingSet's
// normal "fewer schedulable nodes than replicas" outcome, same fallback
// placeReplicas/deployReplicaSet already accept elsewhere). A fresh
// container is run on that node, staged behind its own throwaway host, and
// health-gated exactly like a single-replica blue-green deploy; only once
// it's healthy does the live route flip to the merged (untouched-replicas +
// this new one) upstream set. The OLD replica's container on excludeNode is
// then best-effort removed — skipped if excludeNode is already known
// unreachable (a node-loss eviction: the container dies with the node,
// same reach guard deployReplicaSet's old-set retirement uses) — and the
// deployment row is persisted IN PLACE (store.UpdateReplicas: same id, same
// StatusRunning generation), mutating cur itself so a later index in this
// same call sees the just-moved replica's new node/container.
//
// A moving index that fails to place, run, connect, or health-check is left
// completely as it was: its old entry in cur.Replicas is kept, its old
// container is NOT removed, and its error is recorded in the returned
// failed map — every other moving index is still attempted regardless.
// Callers must hold r.mu; cur must be Scheduled, StatusRunning, and carry a
// populated (len > 1) Replicas.
func (r *Reconciler) evacuateReplicasLocked(ctx context.Context, cur *store.Deployment, excludeNode string, movingIdx []int) (moved []int, failed map[int]error) {
	failed = make(map[int]error)

	failAll := func(err error) ([]int, map[int]error) {
		for _, i := range movingIdx {
			failed[i] = err
		}
		return nil, failed
	}

	if cur.Spec == "" {
		return failAll(fmt.Errorf("no spec snapshot to evacuate %q", cur.App))
	}
	var app spec.App
	if err := json.Unmarshal([]byte(cur.Spec), &app); err != nil {
		return failAll(fmt.Errorf("unmarshal spec snapshot for %q: %w", cur.App, err))
	}
	app.Image = cur.Image

	if app.Git != nil {
		unpinned := app
		unpinned.Image = ""
		if err := unpinned.Validate(); err != nil {
			return failAll(fmt.Errorf("invalid spec snapshot for %q: %w", cur.App, err))
		}
	} else if err := app.Validate(); err != nil {
		return failAll(fmt.Errorf("invalid spec snapshot for %q: %w", cur.App, err))
	}
	if app.Image == "" {
		return failAll(fmt.Errorf("no image recorded to evacuate %q", cur.App))
	}

	secretVals, err := r.secrets.Resolve(app.Name, app.Secrets)
	if err != nil {
		return failAll(fmt.Errorf("resolve secrets: %w", err))
	}
	env := mergeEnv(app.Env, secretVals)

	var backingNetwork string
	if len(app.Services) > 0 {
		_, backingNetwork = RenderBackingCompose(app.Name, app.Services)
	}

	for _, idx := range movingIdx {
		if idx < 0 || idx >= len(cur.Replicas) {
			failed[idx] = fmt.Errorf("replica index %d out of range for %q (%d replicas)", idx, cur.App, len(cur.Replicas))
			continue
		}

		// Preserve spread: exclude excludeNode plus every OTHER replica's
		// current node (survivors, and any sibling still-unmoved/failed
		// mover) — never this call's own already-successfully-moved
		// replicas' OLD node, since cur.Replicas[idx] is updated in place
		// below as each index succeeds, so a later iteration naturally sees
		// its new node instead.
		exclude := []string{excludeNode}
		for j, rep := range cur.Replicas {
			if j == idx {
				continue
			}
			exclude = append(exclude, normalizeNode(rep.Node))
		}

		newNodeName, perr := r.placeExcludingSet(ctx, &app, exclude)
		if perr != nil {
			failed[idx] = fmt.Errorf("place replica %d off %q: %w", idx, excludeNode, perr)
			continue
		}

		n, meshAddr, _, isLocal, rerr := r.resolver.ResolveMeta(newNodeName)
		if rerr != nil {
			failed[idx] = fmt.Errorf("resolve node %q for replica %d: %w", newNodeName, idx, rerr)
			continue
		}
		if err := n.EnsureNetwork(ctx, lwdNetwork); err != nil {
			failed[idx] = fmt.Errorf("ensure network on %q: %w", newNodeName, err)
			continue
		}
		if isLocal {
			if err := n.EnsureImage(ctx, app.Image); err != nil {
				failed[idx] = fmt.Errorf("ensure image for replica %d: %w", idx, err)
				continue
			}
		} else if err := r.ensureImageOnNode(ctx, n, app.Image); err != nil {
			failed[idx] = fmt.Errorf("ensure image for replica %d: %w", idx, err)
			continue
		}

		deployID, err := r.store.NextDeployID()
		if err != nil {
			failed[idx] = fmt.Errorf("next deploy id: %w", err)
			continue
		}
		stageHost := stagingHost(deployID)

		name := containerName(&app, deployID)
		if len(cur.Replicas) > 1 {
			name = fmt.Sprintf("%s-%d", name, idx)
		}

		runSpec := node.RunSpec{
			Name:  name,
			Image: app.Image,
			Env:   env,
			Labels: map[string]string{
				"lwd.app":     app.Name,
				"lwd.role":    "surface",
				"lwd.deploy":  strconv.FormatInt(deployID, 10),
				"lwd.replica": strconv.Itoa(idx),
			},
			Port:        app.Port,
			Network:     lwdNetwork,
			CPUs:        reqCPU(&app),
			MemoryBytes: reqMem(&app),
		}
		if !isLocal {
			runSpec.Publish = []node.PortMapping{{HostIP: meshAddr, HostPort: 0, ContainerPort: app.Port}}
		}

		c, err := n.RunContainer(ctx, runSpec)
		if err != nil {
			failed[idx] = fmt.Errorf("run container for replica %d: %w", idx, err)
			continue
		}
		cleanup := func() {
			_ = r.router.RemoveStaging(ctx, stageHost)
			_ = n.RemoveContainer(ctx, c.ID)
		}

		upstream := name
		upstreamPort := app.Port
		if !isLocal {
			upstream = meshAddr
			upstreamPort = c.HostPort
		}

		if backingNetwork != "" {
			if err := n.ConnectContainerToNetwork(ctx, c.ID, backingNetwork); err != nil {
				cleanup()
				failed[idx] = fmt.Errorf("connect replica %d to backing network: %w", idx, err)
				continue
			}
		}

		if err := r.router.SetStaging(ctx, stageHost, []router.Upstream{{Host: upstream, Port: upstreamPort}}); err != nil {
			cleanup()
			failed[idx] = fmt.Errorf("set staging route: %w", err)
			continue
		}

		if healthErr := r.checkHealth(ctx, n, &app, stageHost, c.ID); healthErr != nil {
			cleanup()
			failed[idx] = fmt.Errorf("health check failed for replica %d: %w", idx, healthErr)
			continue
		}
		_ = r.router.RemoveStaging(ctx, stageHost)

		// Healthy: replace this index and flip the live route to the merged
		// (untouched replicas + this new one) upstream set.
		oldReplica := cur.Replicas[idx]
		newReplicas := append([]store.Replica(nil), cur.Replicas...)
		newReplicas[idx] = store.Replica{ContainerID: c.ID, Node: newNodeName, Upstream: upstream, Port: upstreamPort}

		upstreams := make([]router.Upstream, len(newReplicas))
		for i, rep := range newReplicas {
			upstreams[i] = router.Upstream{Host: rep.Upstream, Port: rep.Port}
		}

		if err := r.router.SetRoute(ctx, router.Route{
			Domain:      app.Domain,
			Upstreams:   upstreams,
			TLSInternal: router.UseInternalTLS(app.Domain),
		}); err != nil {
			_ = n.RemoveContainer(ctx, c.ID)
			failed[idx] = fmt.Errorf("set route for replica %d: %w", idx, err)
			continue
		}

		// Old container: best-effort removal on ITS OWN (excludeNode) node —
		// skipped if excludeNode is already known unreachable (node-loss:
		// the container dies with the node). The replica entry is replaced
		// in cur.Replicas either way.
		oldNodeName := normalizeNode(oldReplica.Node)
		removeOld := true
		if r.reach != nil {
			if _, ok := r.reach.Reachable(ctx, oldNodeName); !ok {
				removeOld = false
			}
		}
		if removeOld {
			if oldNode, _, _, _, rerr := r.resolver.ResolveMeta(oldNodeName); rerr == nil {
				_ = oldNode.RemoveContainer(ctx, oldReplica.ContainerID)
			}
		}

		if err := r.store.UpdateReplicas(cur.ID, newReplicas, newReplicas[0].ContainerID); err != nil {
			failed[idx] = fmt.Errorf("persist evacuated replica %d: %w", idx, err)
			continue
		}

		cur.Replicas = newReplicas
		cur.ContainerID = newReplicas[0].ContainerID
		moved = append(moved, idx)
	}

	return moved, failed
}
