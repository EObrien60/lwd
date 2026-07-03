// Package store persists lwd's runtime state (deployment history) in SQLite.
// App definitions live in lwd.toml files, not here; this DB is derived state.
package store

import (
	"database/sql"
	"fmt"
	"strings"
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
	// Spec is a JSON snapshot of the resolved spec.App used for this
	// deployment, captured at record time. It lets rollback restore the
	// exact prior image + config without re-resolving lwd.toml.
	Spec string
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
	created_at   INTEGER NOT NULL,
	spec         TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_deployments_app ON deployments(app);
`

// Open opens (creating if needed) the SQLite database at path and migrates it.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := migrateAddSpecColumn(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrateAddSpecColumn adds the "spec" column to a pre-Phase-2 deployments
// table that predates it. Safe to call on every Open: it first checks
// PRAGMA table_info for the column and only issues ALTER TABLE if missing,
// and additionally tolerates a concurrent/duplicate "add column" error
// (e.g. "duplicate column name: spec") so it never fails on a DB that
// already has the column, including one created by the base schema above.
func migrateAddSpecColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(deployments)`)
	if err != nil {
		return fmt.Errorf("table_info: %w", err)
	}
	hasSpec := false
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notnull   int
			dfltValue any
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("scan table_info: %w", err)
		}
		if name == "spec" {
			hasSpec = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("table_info rows: %w", err)
	}
	rows.Close()
	if hasSpec {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE deployments ADD COLUMN spec TEXT NOT NULL DEFAULT ''`); err != nil {
		// Tolerate a race/duplicate add: some other process (or a prior
		// partial run) already added it between our check and this call.
		if strings.Contains(err.Error(), "duplicate column name") {
			return nil
		}
		return fmt.Errorf("add spec column: %w", err)
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// RecordDeployment inserts a deployment row and returns its id.
func (s *Store) RecordDeployment(d Deployment) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO deployments (app, image, container_id, status, created_at, spec) VALUES (?, ?, ?, ?, ?, ?)`,
		d.App, d.Image, d.ContainerID, d.Status, d.CreatedAt.Unix(), d.Spec,
	)
	if err != nil {
		return 0, fmt.Errorf("insert deployment: %w", err)
	}
	return res.LastInsertId()
}

// CurrentDeployment returns the most recent running deployment for app, or nil.
func (s *Store) CurrentDeployment(app string) (*Deployment, error) {
	row := s.db.QueryRow(
		`SELECT id, app, image, container_id, status, created_at, spec
		 FROM deployments WHERE app = ? AND status = ?
		 ORDER BY id DESC LIMIT 1`,
		app, StatusRunning,
	)
	var d Deployment
	var ts int64
	switch err := row.Scan(&d.ID, &d.App, &d.Image, &d.ContainerID, &d.Status, &ts, &d.Spec); err {
	case nil:
		d.CreatedAt = time.Unix(ts, 0)
		return &d, nil
	case sql.ErrNoRows:
		return nil, nil
	default:
		return nil, fmt.Errorf("query current: %w", err)
	}
}

// PreviousDeployment returns the most recent retired deployment for app —
// the last version that was running before being superseded — or nil if
// there is none. This is what rollback targets.
func (s *Store) PreviousDeployment(app string) (*Deployment, error) {
	row := s.db.QueryRow(
		`SELECT id, app, image, container_id, status, created_at, spec
		 FROM deployments WHERE app = ? AND status = ?
		 ORDER BY id DESC LIMIT 1`,
		app, StatusRetired,
	)
	var d Deployment
	var ts int64
	switch err := row.Scan(&d.ID, &d.App, &d.Image, &d.ContainerID, &d.Status, &ts, &d.Spec); err {
	case nil:
		d.CreatedAt = time.Unix(ts, 0)
		return &d, nil
	case sql.ErrNoRows:
		return nil, nil
	default:
		return nil, fmt.Errorf("query previous: %w", err)
	}
}

// DeploymentsForApp returns all deployments for app, newest first.
func (s *Store) DeploymentsForApp(app string) ([]Deployment, error) {
	rows, err := s.db.Query(
		`SELECT id, app, image, container_id, status, created_at, spec
		 FROM deployments WHERE app = ?
		 ORDER BY id DESC`,
		app,
	)
	if err != nil {
		return nil, fmt.Errorf("query deployments: %w", err)
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		var d Deployment
		var ts int64
		if err := rows.Scan(&d.ID, &d.App, &d.Image, &d.ContainerID, &d.Status, &ts, &d.Spec); err != nil {
			return nil, fmt.Errorf("scan deployment: %w", err)
		}
		d.CreatedAt = time.Unix(ts, 0)
		out = append(out, d)
	}
	return out, rows.Err()
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
