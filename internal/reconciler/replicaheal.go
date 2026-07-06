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

// healDeadReplicasLocked recreates ONLY the replicas of cur named by deadIdx
// (indexes into cur.Replicas, as observed by reconcileReplicaSet), each IN
// PLACE on its OWN node — every other replica (healthy, or on a node this
// pass couldn't even assess) is left completely untouched: its container
// keeps running and its entry in cur.Replicas is copied through unchanged.
// This is deliberately NOT a call into deployReplicaSet: that redeploys and
// health-gates the WHOLE set as one blue-green generation, which would churn
// (stop and replace) every already-healthy replica just to fix one dead one.
//
// It reconstructs the spec.App that produced cur from cur.Spec (same
// approach as healSurfaceLocked), pins Image back to cur.Image (a git app's
// image is already built and local — no re-clone/rebuild here, matching
// healSurfaceLocked's git branch), resolves secrets and the app's backing
// network (if any — though Phase 12 Task 5's spec.Validate guard means a
// multi-replica app can never actually declare backing services; this is
// belt-and-suspenders for an N==1 "replica set" that does), and then for each
// dead index: best-effort removes the old (already-dead) container, runs a
// fresh one on the SAME node, reconnects it to the backing network if any,
// stages it behind a single shared throwaway host alongside any other
// replica being recreated in this same call, and health-gates it — exactly
// the health policy deployReplicaSet uses, just scoped to the new
// containers only. Only once every recreated replica passes its health check
// does the live route flip to the merged (healthy-old + fresh-new) upstream
// set; a failure tears down only the just-created container(s) via cleanup,
// leaving the previous (dead) entries and the live route completely
// untouched — the same isolation guarantee blue-green gives the whole-set
// path, just applied to a subset.
//
// On success, the deployment row is updated IN PLACE (store.UpdateReplicas) —
// same row id, same StatusRunning — rather than recording a new generation
// and retiring the old one, since only a subset of one generation's
// containers actually changed. Callers must hold r.mu.
func (r *Reconciler) healDeadReplicasLocked(ctx context.Context, cur *store.Deployment, deadIdx []int) (*store.Deployment, error) {
	if cur.Spec == "" {
		return nil, fmt.Errorf("no spec snapshot to heal %q", cur.App)
	}
	if len(deadIdx) == 0 {
		return cur, nil
	}

	var app spec.App
	if err := json.Unmarshal([]byte(cur.Spec), &app); err != nil {
		return nil, fmt.Errorf("unmarshal spec snapshot for %q: %w", cur.App, err)
	}
	app.Image = cur.Image

	if app.Git != nil {
		// Validate rejects a git app that also declares Image (meaningless
		// for user-authored lwd.toml — Image is only ever populated by a
		// completed build) — same rationale as rollbackGitLocked's `unpinned`
		// copy.
		unpinned := app
		unpinned.Image = ""
		if err := unpinned.Validate(); err != nil {
			return nil, fmt.Errorf("invalid spec snapshot for %q: %w", cur.App, err)
		}
	} else if err := app.Validate(); err != nil {
		return nil, fmt.Errorf("invalid spec snapshot for %q: %w", cur.App, err)
	}
	if app.Image == "" {
		return nil, fmt.Errorf("no image recorded to heal %q", cur.App)
	}

	secretVals, err := r.secrets.Resolve(app.Name, app.Secrets)
	if err != nil {
		return nil, fmt.Errorf("resolve secrets: %w", err)
	}
	env := mergeEnv(app.Env, secretVals)

	var backingNetwork string
	if len(app.Services) > 0 {
		_, backingNetwork = RenderBackingCompose(app.Name, app.Services)
	}

	// Work on a copy: cur.Replicas/upstreams are only mutated at the indexes
	// actually healed; every other entry is copied through byte-for-byte.
	replicas := append([]store.Replica(nil), cur.Replicas...)
	upstreams := make([]router.Upstream, len(replicas))
	for i, rep := range replicas {
		upstreams[i] = router.Upstream{Host: rep.Upstream, Port: rep.Port}
	}

	deployID, err := r.store.NextDeployID()
	if err != nil {
		return nil, fmt.Errorf("next deploy id: %w", err)
	}
	stageHost := stagingHost(deployID)

	type recreated struct {
		idx int
		n   node.Node
		id  string
	}
	var created []recreated
	var stagingUpstreams []router.Upstream

	cleanup := func() {
		_ = r.router.RemoveStaging(ctx, stageHost)
		for _, rc := range created {
			_ = rc.n.RemoveContainer(ctx, rc.id)
		}
	}

	for _, i := range deadIdx {
		if i < 0 || i >= len(replicas) {
			cleanup()
			return nil, fmt.Errorf("replica index %d out of range for %q (%d replicas)", i, cur.App, len(replicas))
		}
		old := replicas[i]
		nodeName := old.Node
		if nodeName == "" {
			nodeName = "local"
		}

		n, meshAddr, _, isLocal, rerr := r.resolver.ResolveMeta(nodeName)
		if rerr != nil {
			cleanup()
			return nil, fmt.Errorf("resolve node %q for replica %d: %w", nodeName, i, rerr)
		}
		if err := n.EnsureNetwork(ctx, lwdNetwork); err != nil {
			cleanup()
			return nil, fmt.Errorf("ensure network on %q: %w", nodeName, err)
		}
		if isLocal {
			if err := n.EnsureImage(ctx, app.Image); err != nil {
				cleanup()
				return nil, fmt.Errorf("ensure image for replica %d: %w", i, err)
			}
		} else {
			if err := r.ensureImageOnNode(ctx, n, app.Image); err != nil {
				cleanup()
				return nil, fmt.Errorf("ensure image for replica %d: %w", i, err)
			}
		}

		// Best-effort: the dead container is very likely already gone (that's
		// usually WHY it's dead); an error here is not fatal, and a real
		// Docker refuses to reuse a still-occupied name anyway, so we always
		// mint a fresh name below rather than relying on this succeeding.
		if old.ContainerID != "" {
			_ = n.RemoveContainer(ctx, old.ContainerID)
		}

		name := containerName(&app, deployID)
		if len(replicas) > 1 {
			name = fmt.Sprintf("%s-%d", name, i)
		}

		runSpec := node.RunSpec{
			Name:  name,
			Image: app.Image,
			Env:   env,
			Labels: map[string]string{
				"lwd.app":     app.Name,
				"lwd.role":    "surface",
				"lwd.deploy":  strconv.FormatInt(deployID, 10),
				"lwd.replica": strconv.Itoa(i),
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
			cleanup()
			return nil, fmt.Errorf("run container for replica %d: %w", i, err)
		}
		created = append(created, recreated{idx: i, n: n, id: c.ID})

		upstream := name
		upstreamPort := app.Port
		if !isLocal {
			upstream = meshAddr
			upstreamPort = c.HostPort
		}

		if backingNetwork != "" {
			if err := n.ConnectContainerToNetwork(ctx, c.ID, backingNetwork); err != nil {
				cleanup()
				return nil, fmt.Errorf("connect replica %d to backing network: %w", i, err)
			}
		}

		stagingUpstreams = append(stagingUpstreams, router.Upstream{Host: upstream, Port: upstreamPort})
		replicas[i] = store.Replica{ContainerID: c.ID, Node: nodeName, Upstream: upstream, Port: upstreamPort}
		upstreams[i] = router.Upstream{Host: upstream, Port: upstreamPort}
	}

	if err := r.router.SetStaging(ctx, stageHost, stagingUpstreams); err != nil {
		cleanup()
		return nil, fmt.Errorf("set staging route: %w", err)
	}

	for _, rc := range created {
		if healthErr := r.checkHealth(ctx, rc.n, &app, stageHost, rc.id); healthErr != nil {
			cleanup()
			return nil, fmt.Errorf("health check failed for replica %d: %w", rc.idx, healthErr)
		}
	}

	if err := r.router.SetRoute(ctx, router.Route{
		Domain:      app.Domain,
		Upstreams:   upstreams,
		TLSInternal: router.UseInternalTLS(app.Domain),
	}); err != nil {
		cleanup()
		return nil, fmt.Errorf("set route: %w", err)
	}
	_ = r.router.RemoveStaging(ctx, stageHost)

	if err := r.store.UpdateReplicas(cur.ID, replicas, replicas[0].ContainerID); err != nil {
		return nil, fmt.Errorf("persist healed replicas: %w", err)
	}

	healed := *cur
	healed.Replicas = replicas
	healed.ContainerID = replicas[0].ContainerID
	return &healed, nil
}
