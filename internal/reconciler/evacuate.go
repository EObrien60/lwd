package reconciler

import (
	"context"
	"encoding/json"
	"fmt"

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
