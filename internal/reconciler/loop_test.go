package reconciler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// countingReconcilable is a fake Reconcilable that counts how many times
// Reconcile is invoked and lets a test-controlled error be returned.
type countingReconcilable struct {
	calls atomic.Int64
}

func (c *countingReconcilable) Reconcile(ctx context.Context) error {
	c.calls.Add(1)
	return nil
}

// TestRunLoopInitialAndTick verifies RunLoop runs an initial pass immediately
// on start, then continues to fire on each ticker interval until ctx is
// canceled, at which point it returns.
func TestRunLoopInitialAndTick(t *testing.T) {
	rec := &countingReconcilable{}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	nudge := make(chan struct{})
	done := make(chan struct{})
	go func() {
		RunLoop(ctx, rec, 20*time.Millisecond, nudge)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not return after ctx cancellation")
	}

	if got := rec.calls.Load(); got < 2 {
		t.Errorf("Reconcile called %d times, want >= 2 (initial + at least one tick)", got)
	}
}

// TestRunLoopNudgeTriggersPass verifies that sending on the nudge channel
// triggers an extra Reconcile pass even when the ticker interval is too long
// to have fired on its own.
func TestRunLoopNudgeTriggersPass(t *testing.T) {
	rec := &countingReconcilable{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	nudge := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		RunLoop(ctx, rec, time.Hour, nudge)
		close(done)
	}()

	// Wait for the initial pass to complete.
	deadline := time.After(time.Second)
	for rec.calls.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("initial pass never ran")
		case <-time.After(time.Millisecond):
		}
	}

	nudge <- struct{}{}

	deadline = time.After(time.Second)
	for rec.calls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("nudge did not trigger a second pass")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not return after ctx cancellation")
	}
}

// panicThenSucceed panics on the first call and succeeds on every subsequent
// call, to verify RunLoop recovers a panicking pass and keeps running.
type panicThenSucceed struct {
	calls atomic.Int64
}

func (p *panicThenSucceed) Reconcile(ctx context.Context) error {
	n := p.calls.Add(1)
	if n == 1 {
		panic("boom")
	}
	return nil
}

// TestRunLoopRecoversPanic verifies a panicking Reconcile pass is recovered
// (logged, not propagated) and the loop keeps ticking afterward.
func TestRunLoopRecoversPanic(t *testing.T) {
	rec := &panicThenSucceed{}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	nudge := make(chan struct{})
	done := make(chan struct{})
	go func() {
		RunLoop(ctx, rec, 20*time.Millisecond, nudge)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not return after ctx cancellation")
	}

	if got := rec.calls.Load(); got < 2 {
		t.Errorf("Reconcile called %d times, want >= 2 (panic recovered, loop continued)", got)
	}
}
