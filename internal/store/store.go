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
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure sqlite: %w", err)
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
