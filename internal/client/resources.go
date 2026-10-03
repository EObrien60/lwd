package client

import (
	"context"
	"net/http"

	"lwd/internal/bundle"
	"lwd/internal/store"
)

// RestoreRequest restores a recorded backup into its app environment's
// database. Destructive: the database is replaced.
type RestoreRequest struct {
	BackupID int64 `json:"backup_id"`
}

// RestoreResult is the backup restored plus the node's step timings.
type RestoreResult struct {
	Backup store.Backup `json:"backup"`
	bundle.RestoreResult
}

// DatabaseBackups is one database resource with its newest successful and
// newest failed backup.
type DatabaseBackups struct {
	App         string        `json:"app"`
	Env         string        `json:"env"`
	Host        string        `json:"host"`
	Database    string        `json:"database"`
	Latest      *store.Backup `json:"latest,omitempty"`
	LastFailure *store.Backup `json:"last_failure,omitempty"`
}

// BackupStatus is the answer of GET /v1/backups.
type BackupStatus struct {
	Databases []DatabaseBackups `json:"databases"`
	Failures  []store.Backup    `json:"failures"` // failed backups in the last 7 days
}

// Resources lists the platform resources of an app environment (no credentials).
func (c *Client) Resources(ctx context.Context, app, env string) ([]store.Resource, error) {
	var rs []store.Resource
	err := c.call(ctx, http.MethodGet, p("apps", app, "envs", env, "resources"), nil, &rs)
	return rs, err
}

// BackupDB takes a manual backup of the app environment's database.
func (c *Client) BackupDB(ctx context.Context, app, env string) (store.Backup, error) {
	var b store.Backup
	err := c.call(ctx, http.MethodPost, p("apps", app, "envs", env, "db", "backup"), nil, &b)
	return b, err
}

// DBBackups lists recorded backups of the app environment's database, newest first.
func (c *Client) DBBackups(ctx context.Context, app, env string) ([]store.Backup, error) {
	var bs []store.Backup
	err := c.call(ctx, http.MethodGet, p("apps", app, "envs", env, "db", "backups"), nil, &bs)
	return bs, err
}

// RestoreDB replaces the app environment's database with backup id.
func (c *Client) RestoreDB(ctx context.Context, app, env string, id int64) (RestoreResult, error) {
	var r RestoreResult
	err := c.call(ctx, http.MethodPost, p("apps", app, "envs", env, "db", "restore"), RestoreRequest{BackupID: id}, &r)
	return r, err
}

// BackupStatus summarises backups of every database.
func (c *Client) BackupStatus(ctx context.Context) (BackupStatus, error) {
	var s BackupStatus
	err := c.call(ctx, http.MethodGet, "/v1/backups", nil, &s)
	return s, err
}
