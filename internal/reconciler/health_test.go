package reconciler

import (
	"sync"
	"testing"
	"time"
)

// TestHealthSnapshotReturnsCopy proves HealthSnapshot returns a deep copy: a
// caller mutating the returned slices/elements must not affect the
// Reconciler's internal state observed by a later snapshot.
func TestHealthSnapshotReturnsCopy(t *testing.T) {
	r, _, _, _ := newTestReconciler(t)

	r.setHealth(Health{
		Nodes: []NodeHealth{{Name: "a"}},
		Apps:  []AppHealth{{App: "x", State: SurfaceHealthy}},
	})

	got := r.HealthSnapshot()
	if len(got.Nodes) != 1 || got.Nodes[0].Name != "a" {
		t.Fatalf("HealthSnapshot() Nodes = %+v, want [{Name: a}]", got.Nodes)
	}
	if len(got.Apps) != 1 || got.Apps[0].App != "x" {
		t.Fatalf("HealthSnapshot() Apps = %+v, want [{App: x}]", got.Apps)
	}

	// Mutate the returned copy.
	got.Nodes[0].Name = "mutated"
	got.Apps[0].App = "mutated"

	again := r.HealthSnapshot()
	if again.Nodes[0].Name != "a" {
		t.Errorf("internal state mutated via returned Nodes slice: got %q, want %q", again.Nodes[0].Name, "a")
	}
	if again.Apps[0].App != "x" {
		t.Errorf("internal state mutated via returned Apps slice: got %q, want %q", again.Apps[0].App, "x")
	}
}

// TestHealthSnapshotConcurrent exercises setHealth/HealthSnapshot from many
// goroutines concurrently; run with -race to confirm the RWMutex actually
// guards the shared state.
func TestHealthSnapshotConcurrent(t *testing.T) {
	r, _, _, _ := newTestReconciler(t)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r.setHealth(Health{
				Nodes: []NodeHealth{{Name: "node", UpdatedAt: time.Now()}},
				Apps:  []AppHealth{{App: "app", State: SurfaceHealthy}},
			})
		}(i)
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.HealthSnapshot()
		}()
	}
	wg.Wait()
}

// TestSignalNudgeNonBlocking exercises signalNudge's contract: a no-op when
// nudge is unset, a delivered send on a buffered channel, and a non-blocking
// no-op once that buffer is full.
func TestSignalNudgeNonBlocking(t *testing.T) {
	r, _, _, _ := newTestReconciler(t)

	// No nudge channel set: must not panic or block.
	r.signalNudge()

	ch := make(chan struct{}, 1)
	r.SetNudge(ch)

	r.signalNudge()
	select {
	case <-ch:
	default:
		t.Fatal("signalNudge() did not deliver on empty buffered channel")
	}

	// Fill the buffer, then confirm a second signalNudge does not block.
	ch <- struct{}{}
	done := make(chan struct{})
	go func() {
		r.signalNudge()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("signalNudge() blocked on a full channel")
	}
}
