package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "lwd.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRecordAndCurrent(t *testing.T) {
	s := openTemp(t)
	_, err := s.RecordDeployment(Deployment{
		App: "blog", Image: "img:1", ContainerID: "c1",
		Status: StatusRunning, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("RecordDeployment: %v", err)
	}
	cur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil || cur.ContainerID != "c1" {
		t.Fatalf("CurrentDeployment = %+v, want c1", cur)
	}
}

func TestCurrentReturnsNilWhenNone(t *testing.T) {
	s := openTemp(t)
	cur, err := s.CurrentDeployment("nope")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur != nil {
		t.Fatalf("want nil, got %+v", cur)
	}
}

func TestSetStatusRetiresOld(t *testing.T) {
	s := openTemp(t)
	id, _ := s.RecordDeployment(Deployment{App: "blog", Image: "img:1", ContainerID: "c1", Status: StatusRunning, CreatedAt: time.Now()})
	if err := s.SetStatus(id, StatusRetired); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	cur, _ := s.CurrentDeployment("blog")
	if cur != nil {
		t.Fatalf("want no running deployment, got %+v", cur)
	}
}

func TestListApps(t *testing.T) {
	s := openTemp(t)
	s.RecordDeployment(Deployment{App: "blog", Image: "i", ContainerID: "c1", Status: StatusRunning, CreatedAt: time.Now()})
	s.RecordDeployment(Deployment{App: "api", Image: "i", ContainerID: "c2", Status: StatusRunning, CreatedAt: time.Now()})
	apps, err := s.ListApps()
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 2 || apps[0] != "api" || apps[1] != "blog" {
		t.Fatalf("ListApps = %v, want [api blog]", apps)
	}
}

func TestSpecRoundTripsThroughRecordAndCurrent(t *testing.T) {
	s := openTemp(t)
	wantSpec := `{"image":"img:1","env":{"FOO":"bar"}}`
	_, err := s.RecordDeployment(Deployment{
		App: "blog", Image: "img:1", ContainerID: "c1",
		Status: StatusRunning, CreatedAt: time.Now(), Spec: wantSpec,
	})
	if err != nil {
		t.Fatalf("RecordDeployment: %v", err)
	}
	cur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil || cur.Spec != wantSpec {
		t.Fatalf("CurrentDeployment.Spec = %+v, want %q", cur, wantSpec)
	}
}

func TestPreviousDeploymentReturnsLastRetired(t *testing.T) {
	s := openTemp(t)
	firstID, err := s.RecordDeployment(Deployment{
		App: "blog", Image: "img:1", ContainerID: "c1",
		Status: StatusRunning, CreatedAt: time.Now(), Spec: `{"image":"img:1"}`,
	})
	if err != nil {
		t.Fatalf("RecordDeployment (first): %v", err)
	}
	if err := s.SetStatus(firstID, StatusRetired); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if _, err := s.RecordDeployment(Deployment{
		App: "blog", Image: "img:2", ContainerID: "c2",
		Status: StatusRunning, CreatedAt: time.Now(), Spec: `{"image":"img:2"}`,
	}); err != nil {
		t.Fatalf("RecordDeployment (second): %v", err)
	}

	prev, err := s.PreviousDeployment("blog")
	if err != nil {
		t.Fatalf("PreviousDeployment: %v", err)
	}
	if prev == nil || prev.ID != firstID || prev.Spec != `{"image":"img:1"}` {
		t.Fatalf("PreviousDeployment = %+v, want id=%d spec=img:1", prev, firstID)
	}
}

func TestPreviousDeploymentNilWhenNoneRetired(t *testing.T) {
	s := openTemp(t)
	if _, err := s.RecordDeployment(Deployment{
		App: "blog", Image: "img:1", ContainerID: "c1",
		Status: StatusRunning, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("RecordDeployment: %v", err)
	}

	prev, err := s.PreviousDeployment("blog")
	if err != nil {
		t.Fatalf("PreviousDeployment: %v", err)
	}
	if prev != nil {
		t.Fatalf("PreviousDeployment = %+v, want nil", prev)
	}
}

func TestDeploymentsForAppNewestFirst(t *testing.T) {
	s := openTemp(t)
	id1, _ := s.RecordDeployment(Deployment{App: "blog", Image: "img:1", ContainerID: "c1", Status: StatusRetired, CreatedAt: time.Now()})
	id2, _ := s.RecordDeployment(Deployment{App: "blog", Image: "img:2", ContainerID: "c2", Status: StatusRetired, CreatedAt: time.Now()})
	id3, _ := s.RecordDeployment(Deployment{App: "blog", Image: "img:3", ContainerID: "c3", Status: StatusRunning, CreatedAt: time.Now()})
	// Unrelated app should not appear.
	s.RecordDeployment(Deployment{App: "other", Image: "img:9", ContainerID: "c9", Status: StatusRunning, CreatedAt: time.Now()})

	deps, err := s.DeploymentsForApp("blog")
	if err != nil {
		t.Fatalf("DeploymentsForApp: %v", err)
	}
	if len(deps) != 3 {
		t.Fatalf("DeploymentsForApp len = %d, want 3: %+v", len(deps), deps)
	}
	if deps[0].ID != id3 || deps[1].ID != id2 || deps[2].ID != id1 {
		t.Fatalf("DeploymentsForApp order = %+v, want newest-first [%d %d %d]", deps, id3, id2, id1)
	}
}

func TestMigrationIdempotentAcrossReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lwd.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	if _, err := s1.RecordDeployment(Deployment{
		App: "blog", Image: "img:1", ContainerID: "c1",
		Status: StatusRunning, CreatedAt: time.Now(), Spec: `{"image":"img:1"}`,
	}); err != nil {
		t.Fatalf("RecordDeployment: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close (first): %v", err)
	}

	// Reopen the same DB file multiple times; the migration must be safe to
	// re-run and must not fail with "duplicate column name".
	for i := 0; i < 3; i++ {
		s2, err := Open(path)
		if err != nil {
			t.Fatalf("Open (reopen %d): %v", i, err)
		}
		cur, err := s2.CurrentDeployment("blog")
		if err != nil {
			t.Fatalf("CurrentDeployment (reopen %d): %v", i, err)
		}
		if cur == nil || cur.Spec != `{"image":"img:1"}` {
			t.Fatalf("CurrentDeployment (reopen %d) = %+v, want spec to persist", i, cur)
		}
		if err := s2.Close(); err != nil {
			t.Fatalf("Close (reopen %d): %v", i, err)
		}
	}
}
