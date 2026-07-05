package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

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
// Once the new surface is live, deployBlueGreenSurface's OWN "retire the
// prior CurrentDeployment" logic has already run — but against the NEW
// node's client n, since deployBlueGreenSurface always operates against
// whichever node the deploy targets. At the point that logic runs,
// store.CurrentDeployment(app.Name) still resolves to cur (the new row
// hasn't been recorded yet), so it calls n.RemoveContainer(cur.ContainerID)
// against the NEW node — which never had that container — a harmless no-op —
// and marks cur.ID StatusRetired. The OLD container, still actually running
// on excludeNode, is therefore never removed by that path. This function
// closes that gap explicitly: it removes cur's container from excludeNode
// itself (best-effort — skipped, with a log line, if excludeNode is
// unreachable, e.g. a node-loss eviction, in which case the container dies
// along with its node) and retires cur (idempotent: a no-op if
// deployBlueGreenSurface's own logic already did it, which it will have in
// the common case).
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

	r.retireOldSurfaceLocked(ctx, cur, excludeNode)

	return moved, nil
}

// retireOldSurfaceLocked removes cur's container from excludeNode
// (best-effort) and marks cur retired. Removal is skipped — with a log line,
// not an error — if excludeNode is known-unreachable via r.reach (r.reach may
// be nil, e.g. in tests that don't wire one up, in which case removal is
// always attempted) or if excludeNode fails to resolve at all: in the
// node-loss case the surface being moved off it is exactly BECAUSE the node
// is gone, so its container dies along with it rather than lingering
// unremoved. cur is always marked retired regardless — the new surface
// (already live by the time this is called) is what serves traffic now.
//
// Callers must hold r.mu.
func (r *Reconciler) retireOldSurfaceLocked(ctx context.Context, cur *store.Deployment, excludeNode string) {
	reachable := true
	if r.reach != nil {
		if _, ok := r.reach.Reachable(ctx, excludeNode); !ok {
			reachable = false
		}
	}

	if reachable {
		oldNode, rerr := r.resolver.Resolve(excludeNode)
		if rerr != nil {
			log.Printf("reschedule %s: resolve old node %q for cleanup: %v", cur.App, excludeNode, rerr)
		} else if rmErr := oldNode.RemoveContainer(ctx, cur.ContainerID); rmErr != nil {
			log.Printf("reschedule %s: remove old container %s on %q: %v", cur.App, cur.ContainerID, excludeNode, rmErr)
		}
	} else {
		log.Printf("reschedule %s: node %q unreachable, skipping old container removal (dies with node)", cur.App, excludeNode)
	}

	if err := r.store.SetStatus(cur.ID, store.StatusRetired); err != nil {
		log.Printf("reschedule %s: mark deployment %d retired: %v", cur.App, cur.ID, err)
	}
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

// EvacuateNode moves every scheduler-placed surface currently running on the
// named node onto some other fitting node, leaving explicitly pinned
// surfaces and every compose/backing stack completely untouched — compose
// apps are never even considered (they're never Scheduled, but are also
// filtered out explicitly below since this is a data-loss-sensitive
// operation and the intent should never depend on that alone).
//
// For each app (store.ListApps): if it has no current (StatusRunning)
// deployment, isn't a plain surface (a Phase-4 compose app, or has no
// container at all), or isn't currently placed on name, it's skipped
// entirely — not reported anywhere in the result. Otherwise: a Scheduled
// surface is moved via rescheduleSurfaceLocked (appended to Moved on
// success, Failed on error); a pinned (non-Scheduled) surface is left
// running and reported Skipped.
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
		if cur.ContainerID == "" || isComposeApp(cur.Spec) || nodeFromSpec(cur.Spec) != name {
			continue
		}

		if !cur.Scheduled {
			result.Skipped = append(result.Skipped, app)
			continue
		}

		if _, rerr := r.rescheduleSurfaceLocked(ctx, cur, name); rerr != nil {
			result.Failed = append(result.Failed, EvacFailure{App: app, Err: rerr.Error()})
			continue
		}
		result.Moved = append(result.Moved, app)
	}

	return result, nil
}
