package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
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

func TestNextDeployIDStartsAtOneAndIncrements(t *testing.T) {
	s := openTemp(t)
	id, err := s.NextDeployID()
	if err != nil {
		t.Fatalf("NextDeployID: %v", err)
	}
	if id != 1 {
		t.Fatalf("NextDeployID (empty store) = %d, want 1", id)
	}

	if _, err := s.RecordDeployment(Deployment{App: "blog", Image: "img:1", ContainerID: "c1", Status: StatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("RecordDeployment: %v", err)
	}
	id2, err := s.NextDeployID()
	if err != nil {
		t.Fatalf("NextDeployID (after one row): %v", err)
	}
	if id2 != 2 {
		t.Fatalf("NextDeployID (after one row) = %d, want 2", id2)
	}

	// Adding rows for a different app still advances the single shared
	// counter: deploy ids are unique across the whole store, not per-app.
	if _, err := s.RecordDeployment(Deployment{App: "other", Image: "img:2", ContainerID: "c2", Status: StatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("RecordDeployment (other app): %v", err)
	}
	id3, err := s.NextDeployID()
	if err != nil {
		t.Fatalf("NextDeployID (after two rows): %v", err)
	}
	if id3 != 3 {
		t.Fatalf("NextDeployID (after two rows) = %d, want 3", id3)
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

func TestMigrationFromPreSpecSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lwd.db")

	// Step 1: Create a pre-Phase-2 DB with the old schema (no spec column).
	// Use raw sql.Open to bypass Open() which would create the new schema.
	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}

	// Create the old Phase-1 deployments table without the spec column.
	oldSchema := `
	CREATE TABLE deployments (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		app          TEXT    NOT NULL,
		image        TEXT    NOT NULL,
		container_id TEXT    NOT NULL,
		status       TEXT    NOT NULL,
		created_at   INTEGER NOT NULL
	);
	CREATE INDEX idx_deployments_app ON deployments(app);
	`
	if _, err := rawDB.Exec(oldSchema); err != nil {
		rawDB.Close()
		t.Fatalf("create old schema: %v", err)
	}

	// Insert one legacy row.
	if _, err := rawDB.Exec(
		`INSERT INTO deployments (app, image, container_id, status, created_at) VALUES (?, ?, ?, ?, ?)`,
		"legacy", "img:0", "c0", StatusRunning, time.Now().Unix(),
	); err != nil {
		rawDB.Close()
		t.Fatalf("insert legacy row: %v", err)
	}

	if err := rawDB.Close(); err != nil {
		t.Fatalf("close raw DB: %v", err)
	}

	// Step 2: Open via the package's Open(), which runs migrateAddSpecColumn.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrated): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Step 3: Verify the legacy row was preserved and Spec defaulted to "".
	cur, err := s.CurrentDeployment("legacy")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil {
		t.Fatalf("CurrentDeployment returned nil, want legacy row")
	}
	if cur.App != "legacy" || cur.Image != "img:0" || cur.ContainerID != "c0" {
		t.Fatalf("CurrentDeployment data mismatch: got %+v", cur)
	}
	if cur.Spec != "" {
		t.Fatalf("CurrentDeployment.Spec = %q, want empty string for migrated row", cur.Spec)
	}

	// Step 4: Record a new deployment with a non-empty Spec and verify it works.
	newSpec := `{"image":"img:new","env":{"FOO":"bar"}}`
	if _, err := s.RecordDeployment(Deployment{
		App: "legacy", Image: "img:new", ContainerID: "c1",
		Status: StatusRunning, CreatedAt: time.Now(), Spec: newSpec,
	}); err != nil {
		t.Fatalf("RecordDeployment (new spec): %v", err)
	}

	// The new deployment should be current and have the correct Spec.
	cur2, err := s.CurrentDeployment("legacy")
	if err != nil {
		t.Fatalf("CurrentDeployment (after new): %v", err)
	}
	if cur2 == nil || cur2.Spec != newSpec {
		t.Fatalf("CurrentDeployment (after new) Spec = %q, want %q", cur2.Spec, newSpec)
	}
}

func TestSecretSetGetDelete(t *testing.T) {
	s := openTemp(t)
	if err := s.SetSecret("blog", "DB", []byte{1, 2, 3}); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	got, err := s.GetSecret("blog", "DB")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if string(got) != string([]byte{1, 2, 3}) {
		t.Errorf("got %v", got)
	}
	// upsert
	if err := s.SetSecret("blog", "DB", []byte{9}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ = s.GetSecret("blog", "DB")
	if len(got) != 1 || got[0] != 9 {
		t.Errorf("upsert failed: %v", got)
	}
	// delete
	if err := s.DeleteSecret("blog", "DB"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	got, _ = s.GetSecret("blog", "DB")
	if got != nil {
		t.Errorf("want nil after delete, got %v", got)
	}
}

func TestGetSecretAbsentReturnsNil(t *testing.T) {
	s := openTemp(t)
	got, err := s.GetSecret("nope", "X")
	if err != nil || got != nil {
		t.Fatalf("want (nil,nil), got (%v,%v)", got, err)
	}
}

func TestListSecretKeysSortedAndScoped(t *testing.T) {
	s := openTemp(t)
	s.SetSecret("blog", "B", []byte{1})
	s.SetSecret("blog", "A", []byte{1})
	s.SetSecret("api", "Z", []byte{1})
	keys, err := s.ListSecretKeys("blog")
	if err != nil {
		t.Fatalf("ListSecretKeys: %v", err)
	}
	if len(keys) != 2 || keys[0] != "A" || keys[1] != "B" {
		t.Fatalf("keys = %v, want [A B]", keys)
	}
}
