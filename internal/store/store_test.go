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
