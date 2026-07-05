package reconciler

import (
	"context"
	"errors"
	"testing"

	"lwd/internal/store"
)

// TestHealSurfaceImageApp covers the image-app heal path: given a current
// StatusRunning deployment whose Spec snapshot describes a plain image app
// (Git/Compose nil), healSurfaceLocked (called with r.mu already held, as the
// Phase 10 control loop will do) must redeploy a brand-new surface through
// the same blue-green path Apply uses — a new container run on the node, a
// live route set on the router, and a fresh StatusRunning row recorded.
func TestHealSurfaceImageApp(t *testing.T) {
	r, f, fr, s := newTestReconciler(t)
	ctx := context.Background()
	app := testApp()
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("initial Apply: %v", err)
	}

	cur, err := s.CurrentDeployment(app.Name)
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil {
		t.Fatalf("want a current deployment before healing")
	}

	preRunCalls := 0
	for _, c := range f.Calls {
		if hasPrefix(c, "RunContainer:") {
			preRunCalls++
		}
	}

	r.mu.Lock()
	healed, err := r.healSurfaceLocked(ctx, cur)
	r.mu.Unlock()
	if err != nil {
		t.Fatalf("healSurfaceLocked: %v", err)
	}

	if healed.Status != store.StatusRunning {
		t.Errorf("healed status = %q, want running", healed.Status)
	}
	if healed.ContainerID == dep.ContainerID {
		t.Errorf("want a brand-new surface container, got same container id %q", healed.ContainerID)
	}

	postRunCalls := 0
	for _, c := range f.Calls {
		if hasPrefix(c, "RunContainer:") {
			postRunCalls++
		}
	}
	if postRunCalls <= preRunCalls {
		t.Errorf("want a new RunContainer call during heal, calls: %v", f.Calls)
	}

	route, ok := fr.Routes[app.Domain]
	if !ok {
		t.Fatalf("want a live route for %q after heal", app.Domain)
	}
	if route.Upstream != healed.ContainerID && route.Upstream == "" {
		t.Errorf("want live route upstream set, got %+v", route)
	}

	newCur, err := s.CurrentDeployment(app.Name)
	if err != nil {
		t.Fatalf("CurrentDeployment after heal: %v", err)
	}
	if newCur == nil || newCur.ID != healed.ID {
		t.Fatalf("want current deployment to be the healed row, got %+v", newCur)
	}
}

// TestHealSurfaceGitAppReusesTagNoBuild covers the git-app heal path: the
// current deployment's Spec snapshot has Git != nil and Image already pinned
// to a built tag (lwd-build/<app>:<sha>). healSurfaceLocked must redeploy
// that exact tag directly through rollbackGitLocked — no re-clone, no
// rebuild — since the image is already local from the original deploy.
func TestHealSurfaceGitAppReusesTagNoBuild(t *testing.T) {
	r, _, fr, s, sf, bf := newTestReconcilerWithGit(t)
	ctx := context.Background()
	app := testGitApp()
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	sf.SHA = "1111111111111111111111111111111111111a"
	dep, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("initial Apply: %v", err)
	}

	cur, err := s.CurrentDeployment(app.Name)
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil || cur.Image != dep.Image {
		t.Fatalf("want current deployment recorded with built tag, got %+v", cur)
	}

	preCloneCalls := len(sf.Calls)
	preBuildCalls := len(bf.Calls)

	r.mu.Lock()
	healed, err := r.healSurfaceLocked(ctx, cur)
	r.mu.Unlock()
	if err != nil {
		t.Fatalf("healSurfaceLocked: %v", err)
	}

	if healed.Status != store.StatusRunning {
		t.Errorf("healed status = %q, want running", healed.Status)
	}
	if healed.Image != dep.Image {
		t.Errorf("healed image = %q, want reused tag %q", healed.Image, dep.Image)
	}
	if len(sf.Calls) != preCloneCalls {
		t.Errorf("want no additional Clone call during heal, calls: %v", sf.Calls)
	}
	if len(bf.Calls) != preBuildCalls {
		t.Errorf("want no additional Build/ImageExists call during heal, calls: %v", bf.Calls)
	}
}

// TestRollbackStillWorks confirms Rollback's behavior is unchanged after
// splitting rollbackGit into a locking wrapper plus rollbackGitLocked: a
// public API caller (which does not hold r.mu) must still be able to roll
// back a git app without deadlocking, and get the same redeploy-without-
// rebuild semantics as before the refactor.
func TestRollbackStillWorks(t *testing.T) {
	r, _, fr, s, sf, bf := newTestReconcilerWithGit(t)
	ctx := context.Background()
	app := testGitApp()
	app.Health.Timeout = shortTimeout
	fr.ProbeStatus = 200

	sf.SHA = "1111111111111111111111111111111111111a"
	v1, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("v1 Apply: %v", err)
	}
	sf.SHA = "2222222222222222222222222222222222222b"
	v2, err := r.Apply(ctx, app)
	if err != nil {
		t.Fatalf("v2 Apply: %v", err)
	}
	if v1.Image == v2.Image {
		t.Fatalf("sanity: v1/v2 should have built different tags, both = %q", v1.Image)
	}

	preCloneCalls := len(sf.Calls)
	preBuildCalls := len(bf.Calls)

	back, err := r.Rollback(ctx, "gitapp")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if back.Image != v1.Image {
		t.Errorf("Rollback image = %q, want %q", back.Image, v1.Image)
	}
	if len(sf.Calls) != preCloneCalls {
		t.Errorf("want no additional Clone call on rollback, calls: %v", sf.Calls)
	}
	if len(bf.Calls) != preBuildCalls {
		t.Errorf("want no additional Build call on rollback, calls: %v", bf.Calls)
	}
	if back.Status != store.StatusRunning {
		t.Errorf("status = %q, want running", back.Status)
	}

	cur, err := s.CurrentDeployment("gitapp")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil || cur.Image != v1.Image {
		t.Fatalf("want current deployment restored to %q, got %+v", v1.Image, cur)
	}
}

// TestSurfaceIsDead covers the classification helper surfaceIsDead uses to
// decide whether a container health observation means the surface needs
// healing: any error observing health, or any non-"running" state, is dead.
func TestSurfaceIsDead(t *testing.T) {
	cases := []struct {
		name  string
		state string
		err   error
		want  bool
	}{
		{"running, no error -> alive", "running", nil, false},
		{"exited, no error -> dead", "exited", nil, true},
		{"empty state with error -> dead", "", errors.New("not found"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := surfaceIsDead(tc.state, tc.err); got != tc.want {
				t.Errorf("surfaceIsDead(%q, %v) = %v, want %v", tc.state, tc.err, got, tc.want)
			}
		})
	}
}

// TestHealSurfaceNoSpecSnapshotErrors covers the guard in healSurfaceLocked:
// a deployment row with no Spec snapshot (cur.Spec == "") cannot be healed,
// since there is nothing to reconstruct a spec.App from.
func TestHealSurfaceNoSpecSnapshotErrors(t *testing.T) {
	r, _, _, _ := newTestReconciler(t)
	ctx := context.Background()

	cur := &store.Deployment{App: "blog", Image: "img:1", Status: store.StatusRunning}

	r.mu.Lock()
	_, err := r.healSurfaceLocked(ctx, cur)
	r.mu.Unlock()
	if err == nil {
		t.Fatal("want error healing a deployment with no spec snapshot")
	}
}

// hasPrefix avoids importing strings twice across test files just for a
// one-line check; kept local to this file.
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
