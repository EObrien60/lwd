package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Resource kinds.
const (
	ResourceDatabase = "database"
	ResourceBucket   = "bucket"
)

// Resource is a platform resource provisioned for an app environment. The
// JSON form omits credentials.
type Resource struct {
	App         string    `json:"app"`
	Env         string    `json:"env"`
	Kind        string    `json:"kind"`
	Host        string    `json:"host"`
	Name        string    `json:"name"`
	Credentials []byte    `json:"-"` // encrypted JSON
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PutResource creates the resource or replaces its host and credentials.
func (s *Store) PutResource(ctx context.Context, r Resource) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO resources (app, env, kind, host, name, credentials) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (app, env, kind) DO UPDATE SET host = excluded.host, name = excluded.name,
			credentials = excluded.credentials, updated_at = now()`,
		r.App, r.Env, r.Kind, r.Host, r.Name, r.Credentials)
	return mapErr(err)
}

const resourceCols = `app, env, kind, host, name, credentials, created_at, updated_at`

func scanResource(r pgx.Row) (Resource, error) {
	var res Resource
	err := r.Scan(&res.App, &res.Env, &res.Kind, &res.Host, &res.Name, &res.Credentials, &res.CreatedAt, &res.UpdatedAt)
	return res, mapErr(err)
}

// GetResource returns one resource of app/env.
func (s *Store) GetResource(ctx context.Context, app, env, kind string) (Resource, error) {
	return scanResource(s.pool.QueryRow(ctx, `SELECT `+resourceCols+` FROM resources WHERE app = $1 AND env = $2 AND kind = $3`, app, env, kind))
}

// ListResources returns resources ordered by app, env, kind; empty app/env
// match any.
func (s *Store) ListResources(ctx context.Context, app, env string) ([]Resource, error) {
	return list(ctx, s, scanResource, `SELECT `+resourceCols+` FROM resources
		WHERE ($1 = '' OR app = $1) AND ($2 = '' OR env = $2) ORDER BY app, env, kind`, app, env)
}

// Backup kinds and statuses.
const (
	BackupManual    = "manual"
	BackupScheduled = "scheduled"

	BackupSucceeded = "succeeded"
	BackupFailed    = "failed"
)

// Backup is one database backup attempt.
type Backup struct {
	ID         int64      `json:"id"`
	App        string     `json:"app"`
	Env        string     `json:"env"`
	Host       string     `json:"host"`
	Database   string     `json:"database"`
	File       string     `json:"file,omitempty"`
	Bytes      int64      `json:"bytes"`
	SHA256     string     `json:"sha256,omitempty"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// AddBackup records a finished backup attempt and fills in its ID.
func (s *Store) AddBackup(ctx context.Context, b *Backup) error {
	err := s.pool.QueryRow(ctx, `INSERT INTO backups (app, env, host, database, file, bytes, sha256, kind, status, error, started_at, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING id`,
		b.App, b.Env, b.Host, b.Database, b.File, b.Bytes, b.SHA256, b.Kind, b.Status, b.Error, b.StartedAt, b.FinishedAt).Scan(&b.ID)
	return mapErr(err)
}

const backupCols = `id, app, env, host, database, file, bytes, sha256, kind, status, error, started_at, finished_at`

func scanBackup(r pgx.Row) (Backup, error) {
	var b Backup
	err := r.Scan(&b.ID, &b.App, &b.Env, &b.Host, &b.Database, &b.File, &b.Bytes, &b.SHA256, &b.Kind, &b.Status, &b.Error, &b.StartedAt, &b.FinishedAt)
	return b, mapErr(err)
}

// GetBackup returns one backup.
func (s *Store) GetBackup(ctx context.Context, id int64) (Backup, error) {
	return scanBackup(s.pool.QueryRow(ctx, `SELECT `+backupCols+` FROM backups WHERE id = $1`, id))
}

// ListBackups returns up to limit backups of app/env, newest first.
func (s *Store) ListBackups(ctx context.Context, app, env string, limit int) ([]Backup, error) {
	return list(ctx, s, scanBackup, `SELECT `+backupCols+` FROM backups WHERE app = $1 AND env = $2 ORDER BY id DESC LIMIT $3`, app, env, limit)
}

// LatestBackup returns the newest backup of app/env with status.
func (s *Store) LatestBackup(ctx context.Context, app, env, status string) (Backup, error) {
	return scanBackup(s.pool.QueryRow(ctx, `SELECT `+backupCols+` FROM backups
		WHERE app = $1 AND env = $2 AND status = $3 ORDER BY id DESC LIMIT 1`, app, env, status))
}

// ScheduledBackupSince reports whether a scheduled backup of app/env, in any
// status, started at or after since.
func (s *Store) ScheduledBackupSince(ctx context.Context, app, env string, since time.Time) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM backups
		WHERE app = $1 AND env = $2 AND kind = 'scheduled' AND started_at >= $3)`, app, env, since).Scan(&ok)
	return ok, mapErr(err)
}

// FailedBackupsSince returns failed backups started at or after since, newest first.
func (s *Store) FailedBackupsSince(ctx context.Context, since time.Time) ([]Backup, error) {
	return list(ctx, s, scanBackup, `SELECT `+backupCols+` FROM backups
		WHERE status = 'failed' AND started_at >= $1 ORDER BY id DESC LIMIT 100`, since)
}
