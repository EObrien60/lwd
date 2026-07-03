# lwd Core Deploy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deploy a pre-built Docker image to the local host with one command (`lwd apply`), list/log/remove apps, backed by a daemon exposing an HTTP API over a unix socket.

**Architecture:** One Go binary is both daemon and CLI. The daemon holds a `Reconciler` that makes the local Docker daemon match a parsed `lwd.toml` app spec. All Docker work goes through a `Node` interface (federation seam) with one implementation (`localDocker`) and a `fakeNode` for tests. Runtime state (deployment history) lives in SQLite. The CLI is a thin HTTP client of the daemon's unix-socket API.

**Tech Stack:** Go 1.22+, `github.com/docker/docker/client` (Docker SDK), `github.com/BurntSushi/toml`, `modernc.org/sqlite` (pure-Go, no cgo).

## Global Constraints

- Go version floor: **1.22**.
- **No cgo.** All dependencies must build with `CGO_ENABLED=0` so the result is a single static binary. This is why SQLite is `modernc.org/sqlite` (pure Go), not `mattn/go-sqlite3`.
- Module path: **`lwd`**.
- Import paths are `lwd/internal/<pkg>`.
- This plan covers **single-service, pre-built images only.** Compose, build-from-source, secrets, routing/TLS, and blue-green are later plans. A spec using `[build]`, `compose`, or `surfaces` must be rejected by validation with a clear "not supported yet" error, not silently ignored.
- MVP deploy is **recreate** (stop old, start new), not blue-green. Zero-downtime is Plan 2 and requires the router. Document this; do not fake it.
- Default data dir: **`/var/lib/lwd`**; default socket: **`/var/lib/lwd/lwd.sock`**. Both overridable by env `LWD_DATA_DIR`. Tests must use a temp dir, never the default.

---

### Task 1: Project scaffold

**Files:**
- Create: `go.mod`
- Create: `cmd/lwd/main.go`
- Create: `internal/version/version.go`

**Interfaces:**
- Consumes: nothing.
- Produces: a buildable `lwd` binary whose `main` prints usage; `version.String` constant.

- [ ] **Step 1: Initialize the module**

Run:
```bash
cd /Users/ethanobrien/dev/misc/lwd
go mod init lwd
go mod edit -go=1.22
```

- [ ] **Step 2: Write the version package**

Create `internal/version/version.go`:
```go
// Package version holds build identity for lwd.
package version

// String is the human-readable version of lwd.
const String = "0.1.0-dev"
```

- [ ] **Step 3: Write the entrypoint skeleton**

Create `cmd/lwd/main.go`:
```go
// Command lwd is the lightweight deploy engine: daemon + CLI in one binary.
package main

import (
	"fmt"
	"os"

	"lwd/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println("lwd", version.String)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: lwd <command> [args]

commands:
  daemon            run the lwd daemon
  apply <dir>       deploy the app defined in <dir>/lwd.toml
  ls                list apps and status
  logs <app> [-f]   stream an app's logs
  rm <app>          stop and deregister an app
  version           print version`)
}
```

- [ ] **Step 4: Verify it builds and runs**

Run:
```bash
CGO_ENABLED=0 go build -o /tmp/lwd ./cmd/lwd && /tmp/lwd version
```
Expected: prints `lwd 0.1.0-dev`.

- [ ] **Step 5: Commit**

```bash
git add go.mod cmd/lwd/main.go internal/version/version.go
git commit -m "feat: scaffold lwd binary with version command"
```

---

### Task 2: App spec parsing and validation

**Files:**
- Create: `internal/spec/spec.go`
- Test: `internal/spec/spec_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type App struct { Name, Image, Domain string; Port int; Node string; Env map[string]string; Secrets []string; Health Health; Build *Build; Compose string; Surfaces []string }`
  - `type Health struct { Path string; Timeout time.Duration }`
  - `type Build struct { Context, Dockerfile string }`
  - `func Parse(data []byte) (*App, error)`
  - `func Load(dir string) (*App, error)` — reads `<dir>/lwd.toml`
  - `func (a *App) Validate() error`

- [ ] **Step 1: Add the TOML dependency**

Run:
```bash
go get github.com/BurntSushi/toml@v1.4.0
```

- [ ] **Step 2: Write the failing tests**

Create `internal/spec/spec_test.go`:
```go
package spec

import (
	"testing"
	"time"
)

const singleService = `
name = "blog"
image = "ghcr.io/me/blog:latest"
domain = "blog.example.com"
port = 8080
env = { LOG_LEVEL = "info" }

[health]
path = "/healthz"
timeout = "30s"
`

func TestParseSingleService(t *testing.T) {
	a, err := Parse([]byte(singleService))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Name != "blog" {
		t.Errorf("Name = %q, want blog", a.Name)
	}
	if a.Image != "ghcr.io/me/blog:latest" {
		t.Errorf("Image = %q", a.Image)
	}
	if a.Port != 8080 {
		t.Errorf("Port = %d, want 8080", a.Port)
	}
	if a.Node != "local" {
		t.Errorf("Node = %q, want local (default)", a.Node)
	}
	if a.Env["LOG_LEVEL"] != "info" {
		t.Errorf("Env[LOG_LEVEL] = %q", a.Env["LOG_LEVEL"])
	}
	if a.Health.Path != "/healthz" {
		t.Errorf("Health.Path = %q", a.Health.Path)
	}
	if a.Health.Timeout != 30*time.Second {
		t.Errorf("Health.Timeout = %v, want 30s", a.Health.Timeout)
	}
}

func TestValidateRejectsMissingName(t *testing.T) {
	a := &App{Image: "x", Port: 80}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for missing name")
	}
}

func TestValidateRejectsMissingImage(t *testing.T) {
	a := &App{Name: "x", Port: 80}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for missing image")
	}
}

func TestValidateRejectsMissingPort(t *testing.T) {
	a := &App{Name: "x", Image: "y"}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for missing port")
	}
}

func TestValidateRejectsUnsupportedFeatures(t *testing.T) {
	cases := map[string]*App{
		"compose":  {Name: "x", Image: "y", Port: 80, Compose: "docker-compose.yml"},
		"build":    {Name: "x", Port: 80, Build: &Build{Context: "."}},
		"surfaces": {Name: "x", Image: "y", Port: 80, Surfaces: []string{"web"}},
	}
	for label, a := range cases {
		if err := a.Validate(); err == nil {
			t.Errorf("%s: want 'not supported yet' error", label)
		}
	}
}

func TestValidateAcceptsGoodSpec(t *testing.T) {
	a := &App{Name: "x", Image: "y", Port: 80, Node: "local"}
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/spec/ -v`
Expected: FAIL — `undefined: Parse`, `undefined: App`.

- [ ] **Step 4: Write the implementation**

Create `internal/spec/spec.go`:
```go
// Package spec parses and validates lwd.toml app definitions.
// The parsed App is the source of truth the reconciler acts on.
package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// App is a single deployable application as declared in lwd.toml.
type App struct {
	Name    string            `toml:"name"`
	Image   string            `toml:"image"`
	Domain  string            `toml:"domain"`
	Port    int               `toml:"port"`
	Node    string            `toml:"node"`
	Env     map[string]string `toml:"env"`
	Secrets []string          `toml:"secrets"`
	Health  Health            `toml:"health"`

	// Not yet supported — parsed so we can reject them explicitly.
	Build    *Build   `toml:"build"`
	Compose  string   `toml:"compose"`
	Surfaces []string `toml:"surfaces"`
}

// Health describes how the reconciler decides a container is up.
type Health struct {
	Path    string        `toml:"path"`
	Timeout time.Duration `toml:"-"`
	RawTimeout string     `toml:"timeout"`
}

// Build describes build-from-source (not yet supported in this plan).
type Build struct {
	Context    string `toml:"context"`
	Dockerfile string `toml:"dockerfile"`
}

// Parse decodes an lwd.toml document, applies defaults, and returns the App.
func Parse(data []byte) (*App, error) {
	var a App
	if err := toml.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("parse lwd.toml: %w", err)
	}
	if a.Node == "" {
		a.Node = "local"
	}
	if a.Health.RawTimeout != "" {
		d, err := time.ParseDuration(a.Health.RawTimeout)
		if err != nil {
			return nil, fmt.Errorf("health.timeout %q: %w", a.Health.RawTimeout, err)
		}
		a.Health.Timeout = d
	}
	if a.Health.Timeout == 0 {
		a.Health.Timeout = 30 * time.Second
	}
	return &a, nil
}

// Load reads and parses <dir>/lwd.toml.
func Load(dir string) (*App, error) {
	path := filepath.Join(dir, "lwd.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(data)
}

// Validate returns an error if the App cannot be deployed by this version.
func (a *App) Validate() error {
	if a.Name == "" {
		return fmt.Errorf("name is required")
	}
	if a.Compose != "" {
		return fmt.Errorf("compose apps are not supported yet")
	}
	if a.Build != nil {
		return fmt.Errorf("build-from-source is not supported yet")
	}
	if len(a.Surfaces) > 0 {
		return fmt.Errorf("surfaces are not supported yet")
	}
	if a.Image == "" {
		return fmt.Errorf("image is required")
	}
	if a.Port == 0 {
		return fmt.Errorf("port is required")
	}
	return nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/spec/ -v`
Expected: PASS (all tests).

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/spec/
git commit -m "feat: parse and validate lwd.toml app specs"
```

---

### Task 3: Node interface and fake node

**Files:**
- Create: `internal/node/node.go`
- Create: `internal/node/fake.go`
- Test: `internal/node/fake_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type RunSpec struct { Name, Image string; Env map[string]string; Labels map[string]string; Port int }`
  - `type Container struct { ID, Name, Image, State string; Labels map[string]string; HostPort int }`
  - `type HealthSpec struct { Path string; Timeout time.Duration }`
  - `type Node interface { EnsureImage(ctx, imageRef string) error; RunContainer(ctx, RunSpec) (Container, error); RemoveContainer(ctx, id string) error; ListContainers(ctx, labels map[string]string) ([]Container, error); ContainerLogs(ctx, id string, follow bool) (io.ReadCloser, error); Health(ctx, c Container, h HealthSpec) error }`
  - `type Fake struct { ... }` with `NewFake() *Fake` implementing `Node`, plus knobs `HealthErr error`, `EnsureErr error`, and inspectable `Calls []string`.

- [ ] **Step 1: Write the failing test**

Create `internal/node/fake_test.go`:
```go
package node

import (
	"context"
	"io"
	"testing"
)

func TestFakeRunAndList(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	c, err := f.RunContainer(ctx, RunSpec{
		Name:   "lwd-blog",
		Image:  "img:1",
		Labels: map[string]string{"lwd.app": "blog"},
		Port:   8080,
	})
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}
	if c.ID == "" {
		t.Fatal("expected non-empty container ID")
	}

	got, err := f.ListContainers(ctx, map[string]string{"lwd.app": "blog"})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(got) != 1 || got[0].Name != "lwd-blog" {
		t.Fatalf("ListContainers = %+v, want one lwd-blog", got)
	}
}

func TestFakeRemove(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	c, _ := f.RunContainer(ctx, RunSpec{Name: "x", Image: "i", Labels: map[string]string{"lwd.app": "x"}})
	if err := f.RemoveContainer(ctx, c.ID); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	got, _ := f.ListContainers(ctx, map[string]string{"lwd.app": "x"})
	if len(got) != 0 {
		t.Fatalf("after remove, ListContainers = %+v, want empty", got)
	}
}

func TestFakeLogs(t *testing.T) {
	f := NewFake()
	rc, err := f.ContainerLogs(context.Background(), "any", false)
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if len(b) == 0 {
		t.Fatal("expected some fake log output")
	}
}

// Compile-time assertion that Fake implements Node.
var _ Node = (*Fake)(nil)
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/ -v`
Expected: FAIL — `undefined: NewFake`, `undefined: Node`.

- [ ] **Step 3: Write the interface**

Create `internal/node/node.go`:
```go
// Package node abstracts container operations behind the Node interface.
// This is lwd's federation seam: today the only implementation is the local
// Docker daemon, but the reconciler is written against Node so a remote agent
// can be dropped in later without changing the core.
package node

import (
	"context"
	"io"
	"time"
)

// RunSpec is the request to create and start one container.
type RunSpec struct {
	Name   string
	Image  string
	Env    map[string]string
	Labels map[string]string
	Port   int // container port to publish to the host (MVP: same host port)
}

// Container describes a container known to a node.
type Container struct {
	ID       string
	Name     string
	Image    string
	State    string // "running", "exited", etc.
	Labels   map[string]string
	HostPort int // host port the container's Port is published on
}

// HealthSpec describes how to decide a container is healthy.
type HealthSpec struct {
	Path    string // HTTP path; empty means TCP-connect check only
	Timeout time.Duration
}

// Node is the set of operations lwd performs on a deployment target.
// image refs are the only cross-node currency; a Node never assumes locality.
type Node interface {
	EnsureImage(ctx context.Context, imageRef string) error
	RunContainer(ctx context.Context, spec RunSpec) (Container, error)
	RemoveContainer(ctx context.Context, id string) error
	ListContainers(ctx context.Context, labels map[string]string) ([]Container, error)
	ContainerLogs(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
	Health(ctx context.Context, c Container, h HealthSpec) error
}
```

- [ ] **Step 4: Write the fake implementation**

Create `internal/node/fake.go`:
```go
package node

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Fake is an in-memory Node for tests. It records call names in Calls and lets
// tests force failures via the *Err fields.
type Fake struct {
	mu    sync.Mutex
	seq   int
	items map[string]Container // keyed by container ID
	Calls []string

	EnsureErr error
	HealthErr error
	RunErr    error
}

// NewFake returns a ready-to-use Fake node.
func NewFake() *Fake {
	return &Fake{items: map[string]Container{}}
}

func (f *Fake) record(name string) {
	f.Calls = append(f.Calls, name)
}

func (f *Fake) EnsureImage(ctx context.Context, imageRef string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("EnsureImage:" + imageRef)
	return f.EnsureErr
}

func (f *Fake) RunContainer(ctx context.Context, spec RunSpec) (Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RunContainer:" + spec.Name)
	if f.RunErr != nil {
		return Container{}, f.RunErr
	}
	f.seq++
	c := Container{
		ID:       fmt.Sprintf("fake-%d", f.seq),
		Name:     spec.Name,
		Image:    spec.Image,
		State:    "running",
		Labels:   spec.Labels,
		HostPort: spec.Port,
	}
	f.items[c.ID] = c
	return c, nil
}

func (f *Fake) RemoveContainer(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RemoveContainer:" + id)
	delete(f.items, id)
	return nil
}

func (f *Fake) ListContainers(ctx context.Context, labels map[string]string) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListContainers")
	var out []Container
	for _, c := range f.items {
		if matches(c.Labels, labels) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *Fake) ContainerLogs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ContainerLogs:" + id)
	return io.NopCloser(strings.NewReader("fake log line\n")), nil
}

func (f *Fake) Health(ctx context.Context, c Container, h HealthSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Health:" + c.ID)
	return f.HealthErr
}

func matches(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/node/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/node/node.go internal/node/fake.go internal/node/fake_test.go
git commit -m "feat: add Node interface and in-memory fake"
```

---

### Task 4: SQLite runtime store

**Files:**
- Create: `internal/store/store.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Deployment struct { ID int64; App, Image, ContainerID, Status string; CreatedAt time.Time }`
  - Status constants: `StatusRunning = "running"`, `StatusFailed = "failed"`, `StatusRetired = "retired"`
  - `func Open(path string) (*Store, error)` — opens/creates the DB and runs migrations
  - `func (s *Store) Close() error`
  - `func (s *Store) RecordDeployment(d Deployment) (int64, error)`
  - `func (s *Store) CurrentDeployment(app string) (*Deployment, error)` — latest with StatusRunning, or `(nil, nil)` if none
  - `func (s *Store) SetStatus(id int64, status string) error`
  - `func (s *Store) ListApps() ([]string, error)` — distinct app names, sorted

- [ ] **Step 1: Add the SQLite dependency**

Run:
```bash
go get modernc.org/sqlite@v1.34.1
```

- [ ] **Step 2: Write the failing tests**

Create `internal/store/store_test.go`:
```go
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
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/store/ -v`
Expected: FAIL — `undefined: Open`.

- [ ] **Step 4: Write the implementation**

Create `internal/store/store.go`:
```go
// Package store persists lwd's runtime state (deployment history) in SQLite.
// App definitions live in lwd.toml files, not here; this DB is derived state.
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Deployment status values.
const (
	StatusRunning = "running"
	StatusFailed  = "failed"
	StatusRetired = "retired"
)

// Deployment is one recorded attempt to run an app at a given image.
type Deployment struct {
	ID          int64
	App         string
	Image       string
	ContainerID string
	Status      string
	CreatedAt   time.Time
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS deployments (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	app          TEXT    NOT NULL,
	image        TEXT    NOT NULL,
	container_id TEXT    NOT NULL,
	status       TEXT    NOT NULL,
	created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_deployments_app ON deployments(app);
`

// Open opens (creating if needed) the SQLite database at path and migrates it.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// RecordDeployment inserts a deployment row and returns its id.
func (s *Store) RecordDeployment(d Deployment) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO deployments (app, image, container_id, status, created_at) VALUES (?, ?, ?, ?, ?)`,
		d.App, d.Image, d.ContainerID, d.Status, d.CreatedAt.Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("insert deployment: %w", err)
	}
	return res.LastInsertId()
}

// CurrentDeployment returns the most recent running deployment for app, or nil.
func (s *Store) CurrentDeployment(app string) (*Deployment, error) {
	row := s.db.QueryRow(
		`SELECT id, app, image, container_id, status, created_at
		 FROM deployments WHERE app = ? AND status = ?
		 ORDER BY id DESC LIMIT 1`,
		app, StatusRunning,
	)
	var d Deployment
	var ts int64
	switch err := row.Scan(&d.ID, &d.App, &d.Image, &d.ContainerID, &d.Status, &ts); err {
	case nil:
		d.CreatedAt = time.Unix(ts, 0)
		return &d, nil
	case sql.ErrNoRows:
		return nil, nil
	default:
		return nil, fmt.Errorf("query current: %w", err)
	}
}

// SetStatus updates a deployment's status.
func (s *Store) SetStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE deployments SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("set status: %w", err)
	}
	return nil
}

// ListApps returns the distinct app names, sorted ascending.
func (s *Store) ListApps() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT app FROM deployments ORDER BY app ASC`)
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/store/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/store/
git commit -m "feat: add SQLite runtime store for deployment history"
```

---

### Task 5: Reconciler

**Files:**
- Create: `internal/reconciler/reconciler.go`
- Test: `internal/reconciler/reconciler_test.go`

**Interfaces:**
- Consumes: `spec.App`, `node.Node`, `node.RunSpec`, `node.HealthSpec`, `store.Store`, `store.Deployment`, `store.StatusRunning/Failed/Retired`.
- Produces:
  - `type Reconciler struct { ... }`
  - `func New(n node.Node, s *store.Store) *Reconciler`
  - `func (r *Reconciler) Apply(ctx context.Context, app *spec.App) (*store.Deployment, error)`
  - Container naming: `containerName(app) = "lwd-" + app.Name`; label key `"lwd.app"` carries `app.Name`.

Reconcile logic (MVP recreate): validate → EnsureImage → remove any existing containers labeled for this app → RunContainer → Health (on failure: RemoveContainer the new one, record StatusFailed, return error, leaving app removed) → retire prior running deployment rows → record StatusRunning.

- [ ] **Step 1: Write the failing tests**

Create `internal/reconciler/reconciler_test.go`:
```go
package reconciler

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"lwd/internal/node"
	"lwd/internal/spec"
	"lwd/internal/store"
)

func newTestReconciler(t *testing.T) (*Reconciler, *node.Fake, *store.Store) {
	t.Helper()
	f := node.NewFake()
	s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return New(f, s), f, s
}

func testApp() *spec.App {
	return &spec.App{Name: "blog", Image: "img:1", Port: 8080, Node: "local"}
}

func TestApplyStartsContainerAndRecords(t *testing.T) {
	r, f, s := newTestReconciler(t)
	dep, err := r.Apply(context.Background(), testApp())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if dep.Status != store.StatusRunning {
		t.Errorf("status = %q, want running", dep.Status)
	}
	cur, _ := s.CurrentDeployment("blog")
	if cur == nil || cur.ContainerID != dep.ContainerID {
		t.Errorf("CurrentDeployment mismatch: %+v", cur)
	}
	// Image must be ensured before the container runs.
	if !containsInOrder(f.Calls, "EnsureImage:img:1", "RunContainer:lwd-blog") {
		t.Errorf("call order wrong: %v", f.Calls)
	}
}

func TestApplyFailsWhenUnhealthy(t *testing.T) {
	r, f, s := newTestReconciler(t)
	f.HealthErr = errors.New("unhealthy")
	_, err := r.Apply(context.Background(), testApp())
	if err == nil {
		t.Fatal("want error when health fails")
	}
	// New container must be removed on health failure.
	if !contains(f.Calls, "RemoveContainer:fake-1") {
		t.Errorf("expected new container removed, calls: %v", f.Calls)
	}
	// No running deployment should remain.
	if cur, _ := s.CurrentDeployment("blog"); cur != nil {
		t.Errorf("want no running deployment, got %+v", cur)
	}
}

func TestApplyRecreatesRetiringOld(t *testing.T) {
	r, f, _ := newTestReconciler(t)
	ctx := context.Background()
	first, _ := r.Apply(ctx, testApp())
	second, err := r.Apply(ctx, testApp())
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if first.ContainerID == second.ContainerID {
		t.Fatal("expected a new container on redeploy")
	}
	// The first container should have been removed during the second apply.
	if !contains(f.Calls, "RemoveContainer:"+first.ContainerID) {
		t.Errorf("expected old container removed, calls: %v", f.Calls)
	}
}

func TestApplyRejectsInvalidSpec(t *testing.T) {
	r, _, _ := newTestReconciler(t)
	_, err := r.Apply(context.Background(), &spec.App{Name: "x"}) // missing image/port
	if err == nil {
		t.Fatal("want validation error")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func containsInOrder(xs []string, a, b string) bool {
	ai, bi := -1, -1
	for i, x := range xs {
		if x == a && ai == -1 {
			ai = i
		}
		if x == b {
			bi = i
		}
	}
	return ai != -1 && bi != -1 && ai < bi
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/reconciler/ -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Write the implementation**

Create `internal/reconciler/reconciler.go`:
```go
// Package reconciler makes the running state match a desired app spec.
// It is written entirely against node.Node and store.Store so it can be tested
// with no Docker daemon.
package reconciler

import (
	"context"
	"fmt"
	"time"

	"lwd/internal/node"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// Reconciler applies desired app specs against a node, recording history.
type Reconciler struct {
	node  node.Node
	store *store.Store
}

// New returns a Reconciler bound to a node and store.
func New(n node.Node, s *store.Store) *Reconciler {
	return &Reconciler{node: n, store: s}
}

func containerName(app *spec.App) string { return "lwd-" + app.Name }

// Apply reconciles one app: ensure image, recreate the container, health-check,
// and record the deployment. On health failure the new container is removed and
// the app is left with no running deployment (MVP recreate semantics; blue-green
// arrives with the router in a later plan).
func (r *Reconciler) Apply(ctx context.Context, app *spec.App) (*store.Deployment, error) {
	if err := app.Validate(); err != nil {
		return nil, fmt.Errorf("invalid spec: %w", err)
	}

	if err := r.node.EnsureImage(ctx, app.Image); err != nil {
		return nil, fmt.Errorf("ensure image: %w", err)
	}

	label := map[string]string{"lwd.app": app.Name}

	// Remove any existing containers for this app (recreate).
	existing, err := r.node.ListContainers(ctx, label)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	for _, c := range existing {
		if err := r.node.RemoveContainer(ctx, c.ID); err != nil {
			return nil, fmt.Errorf("remove old container %s: %w", c.ID, err)
		}
	}

	// Start the new container.
	c, err := r.node.RunContainer(ctx, node.RunSpec{
		Name:   containerName(app),
		Image:  app.Image,
		Env:    app.Env,
		Labels: label,
		Port:   app.Port,
	})
	if err != nil {
		return nil, fmt.Errorf("run container: %w", err)
	}

	// Health check the new container.
	hErr := r.node.Health(ctx, c, node.HealthSpec{Path: app.Health.Path, Timeout: app.Health.Timeout})
	if hErr != nil {
		_ = r.node.RemoveContainer(ctx, c.ID)
		_, _ = r.store.RecordDeployment(store.Deployment{
			App: app.Name, Image: app.Image, ContainerID: c.ID,
			Status: store.StatusFailed, CreatedAt: time.Now(),
		})
		return nil, fmt.Errorf("health check failed: %w", hErr)
	}

	// Retire the previous running deployment, if any.
	if prev, err := r.store.CurrentDeployment(app.Name); err == nil && prev != nil {
		_ = r.store.SetStatus(prev.ID, store.StatusRetired)
	}

	// Record the new running deployment.
	dep := store.Deployment{
		App: app.Name, Image: app.Image, ContainerID: c.ID,
		Status: store.StatusRunning, CreatedAt: time.Now(),
	}
	id, err := r.store.RecordDeployment(dep)
	if err != nil {
		return nil, fmt.Errorf("record deployment: %w", err)
	}
	dep.ID = id
	return &dep, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/reconciler/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/reconciler/
git commit -m "feat: add reconciler with recreate-and-health-check apply"
```

---

### Task 6: Local Docker node implementation

**Files:**
- Create: `internal/node/local.go`
- Test: `internal/node/local_test.go` (integration, skipped without Docker)

**Interfaces:**
- Consumes: `Node`, `RunSpec`, `Container`, `HealthSpec` from Task 3.
- Produces: `func NewLocal() (*Local, error)` returning a `*Local` that implements `Node` using the Docker SDK.

**Note:** `Local` talks to a real Docker daemon, so its test is an integration test guarded by `LWD_DOCKER_TEST`. It is not part of the normal unit run. Correctness of the reconciler is covered by the fake in Task 5.

- [ ] **Step 1: Add the Docker SDK dependency**

Run:
```bash
go get github.com/docker/docker@v27.3.1+incompatible
go get github.com/docker/go-connections@v0.5.0
```

- [ ] **Step 2: Write the implementation**

Create `internal/node/local.go`:
```go
package node

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	dtypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

// Local implements Node against the host's Docker daemon.
type Local struct {
	cli *client.Client
}

// NewLocal connects to the local Docker daemon using the environment.
func NewLocal() (*Local, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Local{cli: cli}, nil
}

// EnsureImage pulls the image if it is not already present locally.
func (l *Local) EnsureImage(ctx context.Context, imageRef string) error {
	_, _, err := l.cli.ImageInspectWithRaw(ctx, imageRef)
	if err == nil {
		return nil // already present
	}
	rc, err := l.cli.ImagePull(ctx, imageRef, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", imageRef, err)
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, rc) // drain to completion
	return nil
}

// RunContainer creates and starts a container, publishing Port to the same host port.
func (l *Local) RunContainer(ctx context.Context, spec RunSpec) (Container, error) {
	var env []string
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}

	cfg := &container.Config{
		Image:  spec.Image,
		Env:    env,
		Labels: spec.Labels,
	}
	hostCfg := &container.HostConfig{}
	if spec.Port != 0 {
		p := nat.Port(strconv.Itoa(spec.Port) + "/tcp")
		cfg.ExposedPorts = nat.PortSet{p: struct{}{}}
		hostCfg.PortBindings = nat.PortMap{
			p: []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(spec.Port)}},
		}
	}

	created, err := l.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, spec.Name)
	if err != nil {
		return Container{}, fmt.Errorf("create container: %w", err)
	}
	if err := l.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return Container{}, fmt.Errorf("start container: %w", err)
	}
	return Container{
		ID:       created.ID,
		Name:     spec.Name,
		Image:    spec.Image,
		State:    "running",
		Labels:   spec.Labels,
		HostPort: spec.Port,
	}, nil
}

// RemoveContainer stops (with a short timeout) and force-removes a container.
func (l *Local) RemoveContainer(ctx context.Context, id string) error {
	timeout := 10
	_ = l.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})
	if err := l.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("remove container %s: %w", id, err)
	}
	return nil
}

// ListContainers returns containers matching all given labels (running or not).
func (l *Local) ListContainers(ctx context.Context, labels map[string]string) ([]Container, error) {
	args := filters.NewArgs()
	for k, v := range labels {
		args.Add("label", k+"="+v)
	}
	list, err := l.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	out := make([]Container, 0, len(list))
	for _, c := range list {
		name := ""
		if len(c.Names) > 0 {
			name = c.Names[0]
			if len(name) > 0 && name[0] == '/' {
				name = name[1:]
			}
		}
		var hostPort int
		for _, p := range c.Ports {
			if p.PublicPort != 0 {
				hostPort = int(p.PublicPort)
				break
			}
		}
		out = append(out, Container{
			ID: c.ID, Name: name, Image: c.Image, State: c.State,
			Labels: c.Labels, HostPort: hostPort,
		})
	}
	return out, nil
}

// ContainerLogs streams a container's combined stdout/stderr.
func (l *Local) ContainerLogs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	return l.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Follow: follow, Tail: "200",
	})
}

// Health polls the container until healthy or the timeout elapses. With a Path
// it expects an HTTP 2xx on 127.0.0.1:HostPort; otherwise it does a TCP connect.
func (l *Local) Health(ctx context.Context, c Container, h HealthSpec) error {
	if c.HostPort == 0 {
		return nil // nothing to probe
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(c.HostPort))
	deadline := time.Now().Add(h.Timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if h.Path != "" {
			lastErr = probeHTTP("http://" + addr + h.Path)
		} else {
			lastErr = probeTCP(addr)
		}
		if lastErr == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("health check timed out: %w", lastErr)
}

func probeHTTP(url string) error {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func probeTCP(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// Compile-time assertion that Local implements Node.
var _ Node = (*Local)(nil)

// silence unused import if dtypes ends up unreferenced across SDK versions
var _ = dtypes.Version
```

- [ ] **Step 3: Write the integration test**

Create `internal/node/local_test.go`:
```go
package node

import (
	"context"
	"os"
	"testing"
	"time"
)

// This test requires a real Docker daemon and network access to pull an image.
// Run with: LWD_DOCKER_TEST=1 go test ./internal/node/ -run TestLocal -v
func TestLocalRunRemove(t *testing.T) {
	if os.Getenv("LWD_DOCKER_TEST") == "" {
		t.Skip("set LWD_DOCKER_TEST=1 to run Docker integration tests")
	}
	l, err := NewLocal()
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := l.EnsureImage(ctx, "traefik/whoami:latest"); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	c, err := l.RunContainer(ctx, RunSpec{
		Name:   "lwd-itest-whoami",
		Image:  "traefik/whoami:latest",
		Labels: map[string]string{"lwd.app": "itest"},
		Port:   80,
	})
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}
	defer l.RemoveContainer(ctx, c.ID)

	if err := l.Health(ctx, c, HealthSpec{Path: "/", Timeout: 20 * time.Second}); err != nil {
		t.Fatalf("Health: %v", err)
	}
	got, err := l.ListContainers(ctx, map[string]string{"lwd.app": "itest"})
	if err != nil || len(got) == 0 {
		t.Fatalf("ListContainers = %+v, err %v", got, err)
	}
}
```

- [ ] **Step 4: Verify build and unit tests still pass**

Run:
```bash
CGO_ENABLED=0 go build ./... && go test ./internal/node/ -v
```
Expected: build succeeds; unit tests pass; `TestLocalRunRemove` reports SKIP.

- [ ] **Step 5: Optionally run the integration test (if Docker is available)**

Run: `LWD_DOCKER_TEST=1 go test ./internal/node/ -run TestLocal -v`
Expected: PASS if a Docker daemon is reachable. If unavailable, this step may be skipped.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/node/local.go internal/node/local_test.go
git commit -m "feat: add local Docker node implementation"
```

---

### Task 7: HTTP API over unix socket

**Files:**
- Create: `internal/api/api.go`
- Test: `internal/api/api_test.go`

**Interfaces:**
- Consumes: `reconciler.Reconciler`, `spec.App`, `store.Store`, `node.Node`.
- Produces:
  - `type Server struct { ... }`
  - `func New(r *reconciler.Reconciler, s *store.Store, n node.Node) *Server`
  - `func (srv *Server) Handler() http.Handler`
  - JSON wire types: `type AppStatus struct { Name, Image, ContainerID, Status string }`
  - Routes:
    - `POST /apply` — body is a JSON `spec.App`; responds `200` with JSON `store.Deployment` or `400`/`500` with `{"error": "..."}`.
    - `GET /apps` — responds JSON `[]AppStatus`.
    - `GET /apps/{name}/logs?follow=true|false` — streams `text/plain` logs.
    - `DELETE /apps/{name}` — removes containers + records; responds `204`.

- [ ] **Step 1: Write the failing tests**

Create `internal/api/api_test.go`:
```go
package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/spec"
	"lwd/internal/store"
)

func newTestServer(t *testing.T) (*httptest.Server, *node.Fake) {
	t.Helper()
	f := node.NewFake()
	s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	srv := New(reconciler.New(f, s), s, f)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, f
}

func TestApplyEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	body, _ := json.Marshal(spec.App{Name: "blog", Image: "img:1", Port: 8080, Node: "local"})
	resp, err := http.Post(ts.URL+"/apply", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /apply: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
	var dep store.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&dep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dep.Status != store.StatusRunning {
		t.Errorf("status = %q, want running", dep.Status)
	}
}

func TestApplyEndpointRejectsBadSpec(t *testing.T) {
	ts, _ := newTestServer(t)
	body, _ := json.Marshal(spec.App{Name: "blog"}) // no image/port
	resp, _ := http.Post(ts.URL+"/apply", "application/json", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAppsEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	body, _ := json.Marshal(spec.App{Name: "blog", Image: "img:1", Port: 8080, Node: "local"})
	http.Post(ts.URL+"/apply", "application/json", bytes.NewReader(body))

	resp, err := http.Get(ts.URL + "/apps")
	if err != nil {
		t.Fatalf("GET /apps: %v", err)
	}
	defer resp.Body.Close()
	var apps []AppStatus
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "blog" || apps[0].Status != store.StatusRunning {
		t.Fatalf("apps = %+v", apps)
	}
}

func TestDeleteEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	body, _ := json.Marshal(spec.App{Name: "blog", Image: "img:1", Port: 8080, Node: "local"})
	http.Post(ts.URL+"/apply", "application/json", bytes.NewReader(body))

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/apps/blog", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -v`
Expected: FAIL — `undefined: New`, `undefined: AppStatus`.

- [ ] **Step 3: Write the implementation**

Create `internal/api/api.go`:
```go
// Package api exposes the daemon's HTTP API. The CLI and (later) the web UI are
// its only clients. It holds no business logic beyond request/response mapping.
package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// Server wires HTTP routes to the reconciler, store, and node.
type Server struct {
	rec   *reconciler.Reconciler
	store *store.Store
	node  node.Node
}

// AppStatus is the wire representation of an app's current state.
type AppStatus struct {
	Name        string `json:"name"`
	Image       string `json:"image"`
	ContainerID string `json:"container_id"`
	Status      string `json:"status"`
}

// New returns a Server.
func New(r *reconciler.Reconciler, s *store.Store, n node.Node) *Server {
	return &Server{rec: r, store: s, node: n}
}

// Handler returns the HTTP handler for all routes.
func (srv *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /apply", srv.handleApply)
	mux.HandleFunc("GET /apps", srv.handleApps)
	mux.HandleFunc("GET /apps/{name}/logs", srv.handleLogs)
	mux.HandleFunc("DELETE /apps/{name}", srv.handleDelete)
	return mux
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (srv *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var app spec.App
	if err := json.NewDecoder(r.Body).Decode(&app); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := app.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	dep, err := srv.rec.Apply(r.Context(), &app)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, dep)
}

func (srv *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	names, err := srv.store.ListApps()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]AppStatus, 0, len(names))
	for _, name := range names {
		cur, err := srv.store.CurrentDeployment(name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		st := AppStatus{Name: name, Status: store.StatusRetired}
		if cur != nil {
			st.Image = cur.Image
			st.ContainerID = cur.ContainerID
			st.Status = cur.Status
		}
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, out)
}

func (srv *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cur, err := srv.store.CurrentDeployment(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if cur == nil {
		writeErr(w, http.StatusNotFound, io.EOF)
		return
	}
	follow := r.URL.Query().Get("follow") == "true"
	rc, err := srv.node.ContainerLogs(r.Context(), cur.ContainerID, follow)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func (srv *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := srv.removeApp(r.Context(), name); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) removeApp(ctx context.Context, name string) error {
	containers, err := srv.node.ListContainers(ctx, map[string]string{"lwd.app": name})
	if err != nil {
		return err
	}
	for _, c := range containers {
		if err := srv.node.RemoveContainer(ctx, c.ID); err != nil {
			return err
		}
	}
	cur, err := srv.store.CurrentDeployment(name)
	if err != nil {
		return err
	}
	if cur != nil {
		return srv.store.SetStatus(cur.ID, store.StatusRetired)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/
git commit -m "feat: add HTTP API for apply, list, logs, delete"
```

---

### Task 8: Unix-socket client and CLI wiring

**Files:**
- Create: `internal/client/client.go`
- Create: `internal/cli/cli.go`
- Create: `internal/config/config.go`
- Modify: `cmd/lwd/main.go`
- Test: `internal/client/client_test.go`

**Interfaces:**
- Consumes: `spec` (Load/App), `store.Deployment`, `api.AppStatus`, `api.Server`, `reconciler`, `node.NewLocal`, `store.Open`.
- Produces:
  - `config.DataDir() string` (honors `LWD_DATA_DIR`, default `/var/lib/lwd`), `config.SocketPath() string`, `config.DBPath() string`.
  - `client.Client` with `func New(socketPath string) *Client`, methods `Apply(ctx, *spec.App) (*store.Deployment, error)`, `Apps(ctx) ([]api.AppStatus, error)`, `Logs(ctx, name string, follow bool, w io.Writer) error`, `Remove(ctx, name string) error`.
  - `cli.Run(args []string) int` dispatching `daemon|apply|ls|logs|rm`.

- [ ] **Step 1: Write the failing client test**

Create `internal/client/client_test.go`:
```go
package client

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"lwd/internal/api"
	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// startUnixServer runs the real api.Server on a unix socket for the test.
func startUnixServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "lwd.sock")
	f := node.NewFake()
	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	srv := api.New(reconciler.New(f, s), s, f)

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	httpSrv := &http.Server{Handler: srv.Handler()}
	go httpSrv.Serve(ln)
	t.Cleanup(func() {
		httpSrv.Close()
		s.Close()
		os.Remove(sock)
	})
	return sock
}

func TestClientApplyAndApps(t *testing.T) {
	sock := startUnixServer(t)
	c := New(sock)
	ctx := context.Background()

	dep, err := c.Apply(ctx, &spec.App{Name: "blog", Image: "img:1", Port: 8080, Node: "local"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if dep.Status != store.StatusRunning {
		t.Errorf("status = %q, want running", dep.Status)
	}

	apps, err := c.Apps(ctx)
	if err != nil {
		t.Fatalf("Apps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "blog" {
		t.Fatalf("apps = %+v", apps)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/client/ -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Write the config package**

Create `internal/config/config.go`:
```go
// Package config resolves lwd's filesystem locations.
package config

import (
	"os"
	"path/filepath"
)

// DataDir returns the lwd data directory (LWD_DATA_DIR or /var/lib/lwd).
func DataDir() string {
	if d := os.Getenv("LWD_DATA_DIR"); d != "" {
		return d
	}
	return "/var/lib/lwd"
}

// SocketPath returns the daemon's unix socket path.
func SocketPath() string { return filepath.Join(DataDir(), "lwd.sock") }

// DBPath returns the SQLite database path.
func DBPath() string { return filepath.Join(DataDir(), "lwd.db") }
```

- [ ] **Step 4: Write the client**

Create `internal/client/client.go`:
```go
// Package client is an HTTP client for the daemon's unix-socket API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"lwd/internal/api"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// Client talks to the lwd daemon over a unix socket.
type Client struct {
	http *http.Client
}

// New returns a Client that dials the given unix socket path. The host in URLs
// is a dummy; the dialer always connects to the socket.
func New(socketPath string) *Client {
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

func (c *Client) url(path string) string { return "http://lwd" + path }

func decodeErr(resp *http.Response) error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Error == "" {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return fmt.Errorf("%s", e.Error)
}

// Apply deploys an app and returns the resulting deployment.
func (c *Client) Apply(ctx context.Context, app *spec.App) (*store.Deployment, error) {
	body, err := json.Marshal(app)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/apply"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp)
	}
	var dep store.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&dep); err != nil {
		return nil, err
	}
	return &dep, nil
}

// Apps lists apps and their statuses.
func (c *Client) Apps(ctx context.Context) ([]api.AppStatus, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/apps"), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp)
	}
	var apps []api.AppStatus
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return nil, err
	}
	return apps, nil
}

// Logs streams an app's logs to w.
func (c *Client) Logs(ctx context.Context, name string, follow bool, w io.Writer) error {
	u := c.url("/apps/" + name + "/logs")
	if follow {
		u += "?follow=true"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeErr(resp)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// Remove stops and deregisters an app.
func (c *Client) Remove(ctx context.Context, name string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, c.url("/apps/"+name), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return decodeErr(resp)
	}
	return nil
}
```

- [ ] **Step 5: Run the client test to verify it passes**

Run: `go test ./internal/client/ -v`
Expected: PASS.

- [ ] **Step 6: Write the CLI**

Create `internal/cli/cli.go`:
```go
// Package cli implements lwd's command dispatch for both the daemon and the
// client subcommands.
package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"

	"lwd/internal/api"
	"lwd/internal/client"
	"lwd/internal/config"
	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// Run dispatches a subcommand and returns a process exit code.
func Run(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "missing command")
		return 2
	}
	switch args[0] {
	case "daemon":
		return runDaemon()
	case "apply":
		return runApply(args[1:])
	case "ls":
		return runLs()
	case "logs":
		return runLogs(args[1:])
	case "rm":
		return runRm(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		return 2
	}
}

func runDaemon() int {
	if err := os.MkdirAll(config.DataDir(), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir data dir:", err)
		return 1
	}
	n, err := node.NewLocal()
	if err != nil {
		fmt.Fprintln(os.Stderr, "docker:", err)
		return 1
	}
	s, err := store.Open(config.DBPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "store:", err)
		return 1
	}
	defer s.Close()

	srv := api.New(reconciler.New(n, s), s, n)

	sock := config.SocketPath()
	_ = os.Remove(sock) // clean stale socket
	ln, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		return 1
	}
	fmt.Println("lwd daemon listening on", sock)
	httpSrv := &http.Server{Handler: srv.Handler()}
	if err := httpSrv.Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		return 1
	}
	return 0
}

func newClient() *client.Client { return client.New(config.SocketPath()) }

func runApply(args []string) int {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}
	app, err := spec.Load(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	dep, err := newClient().Apply(context.Background(), app)
	if err != nil {
		fmt.Fprintln(os.Stderr, "apply:", err)
		return 1
	}
	fmt.Printf("deployed %s (%s) container %s\n", dep.App, dep.Image, dep.ContainerID)
	return 0
}

func runLs() int {
	apps, err := newClient().Apps(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "ls:", err)
		return 1
	}
	fmt.Printf("%-20s %-10s %s\n", "APP", "STATUS", "IMAGE")
	for _, a := range apps {
		fmt.Printf("%-20s %-10s %s\n", a.Name, a.Status, a.Image)
	}
	return 0
}

func runLogs(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: lwd logs <app> [-f]")
		return 2
	}
	name := args[0]
	follow := false
	for _, a := range args[1:] {
		if a == "-f" || a == "--follow" {
			follow = true
		}
	}
	if err := newClient().Logs(context.Background(), name, follow, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "logs:", err)
		return 1
	}
	return 0
}

func runRm(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: lwd rm <app>")
		return 2
	}
	if err := newClient().Remove(context.Background(), args[0]); err != nil {
		fmt.Fprintln(os.Stderr, "rm:", err)
		return 1
	}
	fmt.Println("removed", args[0])
	return 0
}
```

- [ ] **Step 7: Rewrite main.go to dispatch through cli.Run**

Replace `cmd/lwd/main.go` entirely with:
```go
// Command lwd is the lightweight deploy engine: daemon + CLI in one binary.
package main

import (
	"fmt"
	"os"

	"lwd/internal/cli"
	"lwd/internal/version"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "version" {
		fmt.Println("lwd", version.String)
		return
	}
	os.Exit(cli.Run(os.Args[1:]))
}
```

- [ ] **Step 8: Verify the whole thing builds and all unit tests pass**

Run:
```bash
CGO_ENABLED=0 go build -o /tmp/lwd ./cmd/lwd && go test ./... && /tmp/lwd version
```
Expected: build succeeds; all tests pass (Docker integration test SKIPs); prints `lwd 0.1.0-dev`.

- [ ] **Step 9: Commit**

```bash
git add internal/client/ internal/cli/ internal/config/ cmd/lwd/main.go
git commit -m "feat: add unix-socket client and CLI (daemon, apply, ls, logs, rm)"
```

---

### Task 9: End-to-end smoke test and README

**Files:**
- Create: `README.md`
- Create: `test/e2e_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces: a documented, integration-tested MVP.

**Note:** The e2e test drives the real binary + real Docker and is guarded by `LWD_DOCKER_TEST`, matching Task 6.

- [ ] **Step 1: Write the e2e test**

Create `test/e2e_test.go`:
```go
package e2e

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lwd/internal/api"
	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// Full stack against real Docker over a unix socket.
// Run with: LWD_DOCKER_TEST=1 go test ./test/ -v
func TestEndToEndDeploy(t *testing.T) {
	if os.Getenv("LWD_DOCKER_TEST") == "" {
		t.Skip("set LWD_DOCKER_TEST=1 to run the end-to-end test")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "lwd.sock")

	n, err := node.NewLocal()
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	srv := api.New(reconciler.New(n, s), s, n)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: srv.Handler()}
	go httpSrv.Serve(ln)
	defer httpSrv.Close()

	app := &spec.App{Name: "e2e-whoami", Image: "traefik/whoami:latest", Port: 9280, Node: "local"}
	app.Health.Path = "/"
	app.Health.Timeout = 30 * time.Second

	rec := reconciler.New(n, s)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dep, err := rec.Apply(ctx, app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	defer n.RemoveContainer(context.Background(), dep.ContainerID)

	resp, err := http.Get("http://127.0.0.1:9280/")
	if err != nil {
		t.Fatalf("GET app: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("app status = %d", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run unit tests (e2e skips without Docker)**

Run: `go test ./...`
Expected: PASS with `TestEndToEndDeploy` SKIP.

- [ ] **Step 3: Optionally run e2e with Docker**

Run: `LWD_DOCKER_TEST=1 go test ./test/ -v`
Expected: PASS if Docker is available.

- [ ] **Step 4: Write the README**

Create `README.md`:
```markdown
# lwd — lightweight deploy

A suckless, self-hosted deployment engine for Docker apps. Point it at an app,
deploy with one command, list and inspect running apps. Single static Go binary
that is both the daemon and the CLI.

> This is the **core deploy** milestone. Routing/TLS, blue-green, rollback,
> secrets, compose apps, and the web UI arrive in later milestones.

## Build

```bash
CGO_ENABLED=0 go build -o lwd ./cmd/lwd
```

## Run the daemon

```bash
sudo LWD_DATA_DIR=/var/lib/lwd ./lwd daemon
```

The daemon listens on a unix socket at `$LWD_DATA_DIR/lwd.sock` (default
`/var/lib/lwd/lwd.sock`) and talks to the local Docker daemon.

## Define an app

Create `lwd.toml` in a directory:

```toml
name = "blog"
image = "ghcr.io/me/blog:latest"
port = 8080

[health]
path = "/healthz"
timeout = "30s"
```

## Deploy and inspect

```bash
lwd apply ./myapp     # deploy the app in ./myapp/lwd.toml
lwd ls                # list apps and status
lwd logs blog -f      # stream logs
lwd rm blog           # stop and deregister
```

## Scope of this milestone

- Single host, pre-built images only.
- Deploys are recreate (brief downtime); zero-downtime blue-green comes with the
  router milestone.
- `compose`, `[build]`, and `surfaces` in `lwd.toml` are parsed but rejected with
  a clear error until their milestones land.

## Testing

```bash
go test ./...                              # unit tests
LWD_DOCKER_TEST=1 go test ./... -v         # + Docker integration/e2e tests
```
```

- [ ] **Step 5: Commit**

```bash
git add README.md test/e2e_test.go
git commit -m "docs: add README and end-to-end smoke test"
```

---

## Self-Review

**Spec coverage (this milestone's slice):**
- Declarative `lwd.toml` source of truth → Task 2. ✓
- Node interface / federation seam (`node` in schema, image-as-precondition via `EnsureImage`) → Tasks 3, 6; `Node` field parsed in Task 2. ✓
- Reconciler makes reality match spec, idempotent-ish recreate, health-gated → Task 5. ✓
- SQLite runtime state / deployment history → Task 4. ✓
- HTTP API over unix socket; CLI as a client → Tasks 7, 8. ✓
- Single-service pre-built image deploy end to end → Task 9. ✓
- Out-of-milestone features (compose, build, surfaces, routing/TLS, blue-green, secrets, web UI) explicitly rejected or deferred → Task 2 validation + README. ✓

**Deferred to later plans (by design, not gaps):** Caddy routing + TLS, blue-green + rollback (Plan 2); secrets store (Plan 3); compose + surfaces/pinned (Plan 4); web UI (Plan 5).

**Placeholder scan:** No TBD/TODO/"handle edge cases" — every code step has complete code. ✓

**Type consistency:** `Node` method set identical across `node.go`, `fake.go`, `local.go`, and consumers (reconciler/api). `store.Deployment` fields and status constants consistent across store/reconciler/api/client. `api.AppStatus` used identically in api and client. `spec.App` field set (incl. `Health.RawTimeout`) consistent between parse and consumers. ✓
