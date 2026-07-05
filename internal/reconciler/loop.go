package reconciler

import (
	"context"
	"log"
	"time"
)

// Reconcilable is the minimal surface RunLoop needs to drive a continuous
// reconciliation loop. *Reconciler satisfies it via its Reconcile method;
// the interface exists so the loop can be tested with a fake, no Docker or
// store required.
type Reconcilable interface {
	Reconcile(ctx context.Context) error
}

// RunLoop drives rec.Reconcile on a ticker and on nudge until ctx is done. An
// initial pass runs immediately on start (so a freshly started daemon doesn't
// wait a full interval before its first reconciliation), then a pass runs on
// every ticker tick and every nudge until ctx.Done() fires, at which point
// RunLoop returns.
//
// Each pass recovers its own panics: a single bad Reconcile call (a nil deref
// deep in a driver, say) is logged and the loop keeps running rather than
// taking down the whole daemon. Reconcile errors are likewise logged, not
// propagated — RunLoop has no caller to report them to.
func RunLoop(ctx context.Context, rec Reconcilable, interval time.Duration, nudge <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()

	pass := func() {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("reconcile loop: recovered panic: %v", p)
			}
		}()
		if err := rec.Reconcile(ctx); err != nil {
			log.Printf("reconcile: %v", err)
		}
	}

	pass() // initial

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		case <-nudge:
			pass()
		}
	}
}
