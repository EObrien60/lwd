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

func TestComposeRoundTrip(t *testing.T) {
	s := openTemp(t)
	wantCompose := `services:
  web:
    image: myapp:latest
    ports:
      - "8080:8080"`
	_, err := s.RecordDeployment(Deployment{
		App: "blog", Image: "img:1", ContainerID: "c1",
		Status: StatusRunning, CreatedAt: time.Now(), Compose: wantCompose,
	})
	if err != nil {
		t.Fatalf("RecordDeployment: %v", err)
	}
	cur, err := s.CurrentDeployment("blog")
	if err != nil {
		t.Fatalf("CurrentDeployment: %v", err)
	}
	if cur == nil || cur.Compose != wantCompose {
		t.Fatalf("CurrentDeployment.Compose = %q, want %q", cur.Compose, wantCompose)
	}
}

func TestMigrationFromPreComposeSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lwd.db")

	// Step 1: Create a pre-Phase-4 DB with spec but no compose column.
	// Use raw sql.Open to bypass Open() which would create the new schema.
	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}

	// Create the Phase-2/Phase-3 deployments table with spec but without compose.
	oldSchema := `
	CREATE TABLE deployments (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		app          TEXT    NOT NULL,
		image        TEXT    NOT NULL,
		container_id TEXT    NOT NULL,
		status       TEXT    NOT NULL,
		created_at   INTEGER NOT NULL,
		spec         TEXT    NOT NULL DEFAULT ''
	);
	CREATE INDEX idx_deployments_app ON deployments(app);
	`
	if _, err := rawDB.Exec(oldSchema); err != nil {
		rawDB.Close()
		t.Fatalf("create old schema: %v", err)
	}

	// Insert one legacy row with spec but no compose.
	if _, err := rawDB.Exec(
		`INSERT INTO deployments (app, image, container_id, status, created_at, spec) VALUES (?, ?, ?, ?, ?, ?)`,
		"legacy", "img:0", "c0", StatusRunning, time.Now().Unix(), `{"image":"img:0"}`,
	); err != nil {
		rawDB.Close()
		t.Fatalf("insert legacy row: %v", err)
	}

	if err := rawDB.Close(); err != nil {
		t.Fatalf("close raw DB: %v", err)
	}

	// Step 2: Open via the package's Open(), which runs migrateAddSpecColumn and migrateAddComposeColumn.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrated): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Step 3: Verify the legacy row was preserved, Spec persisted, and Compose defaulted to "".
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
	if cur.Spec != `{"image":"img:0"}` {
		t.Fatalf("CurrentDeployment.Spec = %q, want {\"image\":\"img:0\"}", cur.Spec)
	}
	if cur.Compose != "" {
		t.Fatalf("CurrentDeployment.Compose = %q, want empty string for migrated row", cur.Compose)
	}

	// Step 4: Record a new deployment with non-empty Compose and verify it works.
	newCompose := `services:
  web:
    image: myapp:v1`
	if _, err := s.RecordDeployment(Deployment{
		App: "legacy", Image: "img:new", ContainerID: "c1",
		Status: StatusRunning, CreatedAt: time.Now(), Spec: `{"image":"img:new"}`, Compose: newCompose,
	}); err != nil {
		t.Fatalf("RecordDeployment (new compose): %v", err)
	}

	// The new deployment should be current and have the correct Compose.
	cur2, err := s.CurrentDeployment("legacy")
	if err != nil {
		t.Fatalf("CurrentDeployment (after new): %v", err)
	}
	if cur2 == nil || cur2.Compose != newCompose {
		t.Fatalf("CurrentDeployment (after new) Compose = %q, want %q", cur2.Compose, newCompose)
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

func TestAddGetNode(t *testing.T) {
	s := openTemp(t)
	n := Node{
		Name:      "web1",
		SSHHost:   "deploy@web1",
		MeshAddr:  "100.64.0.2",
		CreatedAt: time.Now(),
	}
	if err := s.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	got, err := s.GetNode("web1")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got == nil {
		t.Fatalf("GetNode returned nil, want node")
	}
	if got.Name != "web1" || got.SSHHost != "deploy@web1" || got.MeshAddr != "100.64.0.2" {
		t.Fatalf("GetNode mismatch: got %+v", got)
	}
}

func TestAddNodeUpsert(t *testing.T) {
	s := openTemp(t)
	firstCreatedAt := time.Now()
	n1 := Node{
		Name:      "web1",
		SSHHost:   "deploy@web1",
		MeshAddr:  "100.64.0.1",
		CreatedAt: firstCreatedAt,
	}
	if err := s.AddNode(n1); err != nil {
		t.Fatalf("AddNode (first): %v", err)
	}

	n2 := Node{
		Name:      "web1",
		SSHHost:   "deploy@web1-new",
		MeshAddr:  "100.64.0.2",
		CreatedAt: time.Now().Add(10 * time.Second),
	}
	if err := s.AddNode(n2); err != nil {
		t.Fatalf("AddNode (second): %v", err)
	}

	got, err := s.GetNode("web1")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got == nil {
		t.Fatalf("GetNode returned nil")
	}
	if got.SSHHost != "deploy@web1-new" || got.MeshAddr != "100.64.0.2" {
		t.Fatalf("GetNode upsert failed: got %+v, want new values", got)
	}
	// created_at is NOT part of the upsert's SET clause (only ssh_host and
	// mesh_addr are), so it must be preserved from the original AddNode, not
	// overwritten by n2's CreatedAt.
	if !got.CreatedAt.Equal(firstCreatedAt.Truncate(time.Second)) {
		t.Fatalf("GetNode.CreatedAt = %v, want preserved original %v", got.CreatedAt, firstCreatedAt.Truncate(time.Second))
	}
}

func TestGetNodeAbsent(t *testing.T) {
	s := openTemp(t)
	got, err := s.GetNode("nope")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got != nil {
		t.Fatalf("GetNode want nil, got %+v", got)
	}
}

func TestListNodesSorted(t *testing.T) {
	s := openTemp(t)
	n2 := Node{Name: "web2", SSHHost: "deploy@web2", MeshAddr: "100.64.0.3", CreatedAt: time.Now()}
	n1 := Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", CreatedAt: time.Now()}

	if err := s.AddNode(n2); err != nil {
		t.Fatalf("AddNode web2: %v", err)
	}
	if err := s.AddNode(n1); err != nil {
		t.Fatalf("AddNode web1: %v", err)
	}

	nodes, err := s.ListNodes()
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("ListNodes len = %d, want 2", len(nodes))
	}
	if nodes[0].Name != "web1" || nodes[1].Name != "web2" {
		t.Fatalf("ListNodes order = [%s %s], want [web1 web2]", nodes[0].Name, nodes[1].Name)
	}
}

func TestAddNodeAgentURLRoundTrip(t *testing.T) {
	s := openTemp(t)
	n := Node{
		Name:      "web1",
		SSHHost:   "deploy@web1",
		MeshAddr:  "100.64.0.2",
		AgentURL:  "http://100.64.0.2:8078",
		CreatedAt: time.Now(),
	}
	if err := s.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	got, err := s.GetNode("web1")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got == nil {
		t.Fatalf("GetNode returned nil, want node")
	}
	if got.AgentURL != "http://100.64.0.2:8078" {
		t.Fatalf("GetNode.AgentURL = %q, want http://100.64.0.2:8078", got.AgentURL)
	}
}

func TestAddNodeUpsertUpdatesAgentURL(t *testing.T) {
	s := openTemp(t)
	n1 := Node{
		Name:      "web1",
		SSHHost:   "deploy@web1",
		MeshAddr:  "100.64.0.1",
		AgentURL:  "http://100.64.0.1:8078",
		CreatedAt: time.Now(),
	}
	if err := s.AddNode(n1); err != nil {
		t.Fatalf("AddNode (first): %v", err)
	}

	n2 := Node{
		Name:      "web1",
		SSHHost:   "deploy@web1",
		MeshAddr:  "100.64.0.1",
		AgentURL:  "http://100.64.0.1:9999",
		CreatedAt: time.Now(),
	}
	if err := s.AddNode(n2); err != nil {
		t.Fatalf("AddNode (second): %v", err)
	}

	got, err := s.GetNode("web1")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got == nil {
		t.Fatalf("GetNode returned nil")
	}
	if got.AgentURL != "http://100.64.0.1:9999" {
		t.Fatalf("GetNode.AgentURL upsert failed: got %q, want http://100.64.0.1:9999", got.AgentURL)
	}
}

func TestMigrationFromPreAgentURLSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lwd.db")

	// Step 1: Create a pre-Phase-9b nodes table (no agent_url column).
	// Use raw sql.Open to bypass Open() which would create the new schema.
	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}

	oldSchema := `
	CREATE TABLE nodes (
		name      TEXT    PRIMARY KEY,
		ssh_host  TEXT    NOT NULL,
		mesh_addr TEXT    NOT NULL,
		created_at INTEGER NOT NULL
	);
	`
	if _, err := rawDB.Exec(oldSchema); err != nil {
		rawDB.Close()
		t.Fatalf("create old schema: %v", err)
	}

	if _, err := rawDB.Exec(
		`INSERT INTO nodes (name, ssh_host, mesh_addr, created_at) VALUES (?, ?, ?, ?)`,
		"legacy", "deploy@legacy", "100.64.0.9", time.Now().Unix(),
	); err != nil {
		rawDB.Close()
		t.Fatalf("insert legacy row: %v", err)
	}

	if err := rawDB.Close(); err != nil {
		t.Fatalf("close raw DB: %v", err)
	}

	// Step 2: Open via the package's Open(), which runs migrateAddAgentURLColumn.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrated): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Step 3: Verify the legacy row was preserved and AgentURL defaulted to "".
	got, err := s.GetNode("legacy")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got == nil {
		t.Fatalf("GetNode returned nil, want legacy row")
	}
	if got.SSHHost != "deploy@legacy" || got.MeshAddr != "100.64.0.9" {
		t.Fatalf("GetNode data mismatch: got %+v", got)
	}
	if got.AgentURL != "" {
		t.Fatalf("GetNode.AgentURL = %q, want empty string for migrated row", got.AgentURL)
	}
}

// TestAddNodePoolRoundTrip covers Phase 11a Task 4: a node's pool persists
// through AddNode/GetNode/ListNodes, and an empty pool normalizes to
// "default".
func TestAddNodePoolRoundTrip(t *testing.T) {
	s := openTemp(t)

	n := Node{
		Name:      "web1",
		SSHHost:   "deploy@web1",
		MeshAddr:  "100.64.0.2",
		Pool:      "web",
		CreatedAt: time.Now(),
	}
	if err := s.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	got, err := s.GetNode("web1")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got == nil {
		t.Fatalf("GetNode returned nil, want node")
	}
	if got.Pool != "web" {
		t.Fatalf("GetNode.Pool = %q, want %q", got.Pool, "web")
	}

	// Empty pool normalizes to "default".
	n2 := Node{
		Name:      "web2",
		SSHHost:   "deploy@web2",
		MeshAddr:  "100.64.0.3",
		CreatedAt: time.Now(),
	}
	if err := s.AddNode(n2); err != nil {
		t.Fatalf("AddNode (empty pool): %v", err)
	}
	got2, err := s.GetNode("web2")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got2 == nil {
		t.Fatalf("GetNode returned nil, want node")
	}
	if got2.Pool != "default" {
		t.Fatalf("GetNode.Pool = %q, want %q (default normalization)", got2.Pool, "default")
	}

	nodes, err := s.ListNodes()
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	byName := map[string]Node{}
	for _, n := range nodes {
		byName[n.Name] = n
	}
	if byName["web1"].Pool != "web" {
		t.Fatalf("ListNodes web1.Pool = %q, want %q", byName["web1"].Pool, "web")
	}
	if byName["web2"].Pool != "default" {
		t.Fatalf("ListNodes web2.Pool = %q, want %q", byName["web2"].Pool, "default")
	}
}

// TestMigrationFromPrePoolSchema covers Phase 11a Task 4: a pre-11a nodes
// table (no pool column) migrates cleanly on Open, and existing rows default
// to pool "default".
func TestMigrationFromPrePoolSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lwd.db")

	// Step 1: Create a pre-Phase-11a nodes table (no pool column). Use raw
	// sql.Open to bypass Open() which would create the new schema.
	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}

	oldSchema := `
	CREATE TABLE nodes (
		name      TEXT    PRIMARY KEY,
		ssh_host  TEXT    NOT NULL,
		mesh_addr TEXT    NOT NULL,
		created_at INTEGER NOT NULL,
		agent_url TEXT    NOT NULL DEFAULT ''
	);
	`
	if _, err := rawDB.Exec(oldSchema); err != nil {
		rawDB.Close()
		t.Fatalf("create old schema: %v", err)
	}

	if _, err := rawDB.Exec(
		`INSERT INTO nodes (name, ssh_host, mesh_addr, created_at, agent_url) VALUES (?, ?, ?, ?, ?)`,
		"legacy", "deploy@legacy", "100.64.0.9", time.Now().Unix(), "",
	); err != nil {
		rawDB.Close()
		t.Fatalf("insert legacy row: %v", err)
	}

	if err := rawDB.Close(); err != nil {
		t.Fatalf("close raw DB: %v", err)
	}

	// Step 2: Open via the package's Open(), which runs migrateAddPoolColumn.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrated): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Step 3: Verify the legacy row was preserved and Pool defaulted to "default".
	got, err := s.GetNode("legacy")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got == nil {
		t.Fatalf("GetNode returned nil, want legacy row")
	}
	if got.SSHHost != "deploy@legacy" || got.MeshAddr != "100.64.0.9" {
		t.Fatalf("GetNode data mismatch: got %+v", got)
	}
	if got.Pool != "default" {
		t.Fatalf("GetNode.Pool = %q, want %q for migrated row", got.Pool, "default")
	}
}

func TestDeleteNode(t *testing.T) {
	s := openTemp(t)
	n := Node{Name: "web1", SSHHost: "deploy@web1", MeshAddr: "100.64.0.2", CreatedAt: time.Now()}
	if err := s.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	if err := s.DeleteNode("web1"); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}

	got, err := s.GetNode("web1")
	if err != nil {
		t.Fatalf("GetNode after delete: %v", err)
	}
	if got != nil {
		t.Fatalf("GetNode after delete want nil, got %+v", got)
	}

	nodes, err := s.ListNodes()
	if err != nil {
		t.Fatalf("ListNodes after delete: %v", err)
	}
	if len(nodes) != 0 {
		t.Fatalf("ListNodes after delete len = %d, want 0", len(nodes))
	}
}
