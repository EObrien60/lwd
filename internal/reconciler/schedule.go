package reconciler

import (
	"context"
	"time"

	"lwd/internal/node"
	"lwd/internal/scheduler"
	"lwd/internal/spec"
)

// capacityFetchTimeout bounds a single node's Capacity call while gathering
// scheduling candidates (or probing node health): one hung/unreachable node
// must never stall placement (or a whole probeNodes pass) waiting on it.
const capacityFetchTimeout = 5 * time.Second

// reqCPU returns app's declared CPU requirement in cores, or 0 if the app
// declares no [requirements] at all.
func reqCPU(app *spec.App) float64 {
	if app.Requirements == nil {
		return 0
	}
	return app.Requirements.CPU
}

// reqMem returns app's declared memory requirement in bytes, or 0 if the app
// declares no [requirements] at all. A parse error is ignored (treated as 0)
// since app.Validate() already gated Requirements.Memory as parseable before
// any deploy reaches this point.
func reqMem(app *spec.App) int64 {
	if app.Requirements == nil {
		return 0
	}
	b, _ := spec.ParseSize(app.Requirements.Memory)
	return b
}

// poolOf returns app's declared pool, defaulting to "default" when unset.
func poolOf(app *spec.App) string {
	if app.Pool == "" {
		return "default"
	}
	return app.Pool
}

// capacityBounded fetches n's Capacity under a bounded timeout, so a single
// hung/unreachable node can't stall the caller indefinitely. An error or
// timeout is reported back as (zero Capacity, err) — callers treat that as
// "this node's capacity is unknown" rather than failing outright.
func capacityBounded(ctx context.Context, n node.Node) (node.Capacity, error) {
	ctxB, cancel := context.WithTimeout(ctx, capacityFetchTimeout)
	defer cancel()
	return n.Capacity(ctxB)
}

// resolvePlacement decides the concrete node a surface deploy should run on,
// and reports whether that decision came from the scheduler. A pinned app
// (Node set to anything other than "" or "local") bypasses the scheduler
// entirely and is returned as-is. An app with Node == "local" is also left
// alone here (ResolveMeta already treats "local" as the local node); only a
// fully unset Node ("") is actually scheduled: candidates are gathered (the
// local node, always, plus every registered node in the app's pool) and
// handed to scheduler.Place.
//
// The returned scheduled bool is placement provenance (Phase 11b): true iff
// this call actually invoked the scheduler (original app.Node == ""), false
// for a pinned node (including "local"). Callers record it on the resulting
// deployment so later phases can tell which surfaces the scheduler placed
// (and may therefore move) from ones an operator explicitly pinned (which
// must never be moved).
func (r *Reconciler) resolvePlacement(ctx context.Context, app *spec.App) (string, bool, error) {
	// Any explicitly set Node ("local" or a named remote node) is pinned and
	// bypasses the scheduler entirely; only a fully unset Node ("") is
	// actually scheduled.
	if app.Node != "" {
		return app.Node, false, nil
	}

	pool := poolOf(app)
	candidates := r.schedulableCandidates(ctx, app, nil)

	req := scheduler.Requirements{CPUCores: reqCPU(app), MemBytes: reqMem(app)}
	chosen, err := scheduler.Place(candidates, pool, req)
	if err != nil {
		return "", false, err
	}
	return chosen, true, nil
}

// placeExcluding picks a node for app exactly like resolvePlacement's
// scheduling branch, except exclude (a node name, or "local") is dropped
// from the candidate set before scheduler.Place runs. It always invokes the
// scheduler — unlike resolvePlacement there is no pinned short-circuit,
// since this is only ever called to reschedule a surface the scheduler
// already placed (and may therefore move) off a specific node, e.g. because
// that node was just cordoned or went unreachable.
//
// It is a thin wrapper over placeExcludingSet (Phase 12 Task 3), which
// generalizes this to an exclude SET for replica spread.
func (r *Reconciler) placeExcluding(ctx context.Context, app *spec.App, exclude string) (string, error) {
	return r.placeExcludingSet(ctx, app, []string{exclude})
}

// placeExcludingSet picks a node for app exactly like resolvePlacement's
// scheduling branch, except every name in exclude (node names, or "local")
// is dropped from the candidate set before scheduler.Place runs. It always
// invokes the scheduler — unlike resolvePlacement there is no pinned
// short-circuit. placeExcluding(x) is the exclude-one-node special case
// (used to reschedule a surface off a single cordoned/unreachable node);
// placeReplicas (Phase 12 Task 3) uses the general set form to spread a
// surface's replicas across distinct nodes, excluding every node already
// chosen for an earlier replica.
func (r *Reconciler) placeExcludingSet(ctx context.Context, app *spec.App, exclude []string) (string, error) {
	pool := poolOf(app)
	candidates := r.schedulableCandidates(ctx, app, exclude)

	req := scheduler.Requirements{CPUCores: reqCPU(app), MemBytes: reqMem(app)}
	return scheduler.Place(candidates, pool, req)
}

// excludes reports whether name appears in the exclude set.
func excludes(exclude []string, name string) bool {
	for _, x := range exclude {
		if x == name {
			return true
		}
	}
	return false
}

// schedulableCandidates gathers every scheduler.NodeInfo candidate for app's
// pool: the local node (always Schedulable: true — you cannot cordon the
// controller) plus every registered store node in the pool, each carrying
// its store Schedulable flag through so scheduler.Place can exclude cordoned
// nodes. Every candidate whose Name appears in exclude (the local node if
// exclude contains "local", or a named store node) is dropped entirely —
// used by placeExcludingSet to reschedule a surface off a specific node, or
// spread replicas across the nodes not already chosen. resolvePlacement
// calls this with exclude==nil.
func (r *Reconciler) schedulableCandidates(ctx context.Context, app *spec.App, exclude []string) []scheduler.NodeInfo {
	pool := poolOf(app)
	var candidates []scheduler.NodeInfo

	// The local node is ALWAYS offered as a candidate — and always marked
	// Reachable and Schedulable — when the app's pool is "default" (local is
	// implicitly in "default"): it's the controller's own Docker, so unlike
	// a remote node it's never genuinely "unreachable" the way SSH/agent
	// connectivity to a registered node can be, and it cannot be cordoned (a
	// cordon flag applies to registered store nodes only). A failed/timed-out
	// Capacity probe only zeroes out its Cap (Known: false, optimistically
	// treated as fitting any requirement by the scheduler) rather than
	// excluding it — this preserves the single-node non-regression
	// guarantee: an unpinned app with no registered nodes must always still
	// be placeable on local, even if its capacity happens to be momentarily
	// unreadable. The only case local is left out of candidates entirely is
	// if it somehow fails to resolve at all (which never happens in
	// practice), or if the caller explicitly excluded it.
	if pool == "default" && !excludes(exclude, "local") {
		if local, err := r.localNode(); err == nil {
			localCap, _ := capacityBounded(ctx, local)
			candidates = append(candidates, scheduler.NodeInfo{
				Name:        "local",
				Pool:        "default",
				Reachable:   true,
				Schedulable: true,
				Cap:         localCap,
			})
		}
	}

	nodes, _ := r.store.ListNodes()
	for _, n := range nodes {
		if n.Pool != pool || excludes(exclude, n.Name) {
			continue
		}
		reachable := true
		if r.reach != nil {
			_, ok := r.reach.Reachable(ctx, n.Name)
			if !ok {
				// Already known unreachable — skip the Resolve+capacity
				// fetch entirely (a 3s ping + a 5s capacity timeout for a
				// node that scheduler.Place will exclude anyway via its
				// Reachable filter). Placement outcome is unchanged; this
				// only saves latency.
				candidates = append(candidates, scheduler.NodeInfo{
					Name:        n.Name,
					Pool:        n.Pool,
					Reachable:   false,
					Schedulable: n.Schedulable,
				})
				continue
			}
		}

		var nodeCap node.Capacity
		target, rerr := r.resolver.Resolve(n.Name)
		if rerr != nil {
			reachable = false
		} else {
			c, cerr := capacityBounded(ctx, target)
			if cerr != nil {
				reachable = false
			} else {
				nodeCap = c
			}
		}

		candidates = append(candidates, scheduler.NodeInfo{
			Name:        n.Name,
			Pool:        n.Pool,
			Reachable:   reachable,
			Schedulable: n.Schedulable,
			Cap:         nodeCap,
		})
	}

	return candidates
}

// placeReplicas decides the node for each of n replicas of app, spreading
// them across distinct nodes for failure isolation (Phase 12): if one node
// goes down, only the replicas on that node are lost. n<=0 is treated as 1
// (belt-and-suspenders — an old snapshot could carry a zero Replicas).
//
// NOTE (P12 final-review FIX 4): deployReplicaSet does NOT call this
// function — it has its own inline copy of the same spread-vs-stack loop for
// replicas beyond the anchor (reconciler.go, in deployReplicaSet, guarded by
// `if scheduled`). That duplication is intentional, not an oversight: by the
// time deployReplicaSet runs, the caller (applyImageProvenance/applyGit) has
// ALREADY resolved the anchor's node via resolvePlacement and used it to
// ensure the image/network/backing services — so deployReplicaSet seeds
// index 0 from that already-resolved target instead of asking the scheduler
// again. Calling placeReplicas here would re-run scheduler.Place for index 0
// a second time, against candidates gathered at a slightly later instant
// (capacity can shift between calls), risking a DIFFERENT node than the one
// already ensured/recorded as app.Node — a correctness regression, not a
// refactor. Only the "replicas 1..n-1" half of the algorithm is shared
// (verbatim) between the two; keep both in sync by hand if that spread rule
// ever changes. This means placeReplicas itself has no production caller
// today — it exists as a directly-testable specification of the spread rule
// (see schedule_test.go) that deployReplicaSet's inline loop must match.
//
// A pinned app (Node set to anything other than "" or "local") places every
// replica on that same node: a pin is an explicit operator choice with no
// spread, and scheduled is reported false (Phase 11b placement provenance —
// this call never touched the scheduler).
//
// An unpinned app schedules every replica: replica 0 is placed via
// placeExcludingSet with an empty exclude set (the plain most-free-node
// pick); each subsequent replica calls placeExcludingSet with every node
// chosen so far excluded, preferring a fresh distinct node. If that errors —
// fewer schedulable nodes than replicas, so every remaining candidate is
// either already used or unschedulable — placement falls back to
// placeExcludingSet with an empty exclude set again, allowing that replica
// to stack onto the most-free node overall rather than failing the whole
// call. Only replica 0 failing to place (no schedulable node at all) fails
// placeReplicas itself. scheduled is reported true.
func (r *Reconciler) placeReplicas(ctx context.Context, app *spec.App, n int) ([]string, bool, error) {
	if n <= 0 {
		n = 1
	}

	if app.Node != "" && app.Node != "local" {
		nodes := make([]string, n)
		for i := range nodes {
			nodes[i] = app.Node
		}
		return nodes, false, nil
	}

	nodes := make([]string, 0, n)
	first, err := r.placeExcludingSet(ctx, app, nil)
	if err != nil {
		return nil, false, err
	}
	nodes = append(nodes, first)

	for i := 1; i < n; i++ {
		next, err := r.placeExcludingSet(ctx, app, nodes)
		if err != nil {
			// Fewer schedulable nodes than replicas: stack this replica onto
			// the most-free node overall rather than failing the whole
			// placement just because we ran out of distinct nodes.
			next, err = r.placeExcludingSet(ctx, app, nil)
			if err != nil {
				return nil, false, err
			}
		}
		nodes = append(nodes, next)
	}

	return nodes, true, nil
}
