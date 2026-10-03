package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// Host is a workload host running `lwd node`. The JSON form omits the token.
type Host struct {
	Name      string    `json:"name"`
	Addr      string    `json:"addr"`
	TokenEnc  []byte    `json:"-"` // encrypted node bearer token
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// App is an application and its current manifest text.
type App struct {
	Name      string    `json:"name"`
	Manifest  string    `json:"manifest,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Environment is the current placement of one app environment.
type Environment struct {
	App    string `json:"app"`
	Name   string `json:"name"`
	Host   string `json:"host"`
	Domain string `json:"domain"`
	TLS    string `json:"tls"`
}

// Release is an immutable, digest-pinned build of an app.
type Release struct {
	ID        int64             `json:"id"`
	App       string            `json:"app"`
	Commit    string            `json:"commit"`
	Tag       string            `json:"tag"`
	Manifest  string            `json:"manifest,omitempty"`
	Images    map[string]string `json:"images"` // service -> repo@sha256:...
	Actor     string            `json:"actor"`
	CreatedAt time.Time         `json:"created_at"`
}

// Deployment statuses and kinds.
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusReverted  = "reverted"

	KindDeploy   = "deploy"
	KindRollback = "rollback"
)

// Deployment is one attempt to make a release live in an app environment.
type Deployment struct {
	ID         int64           `json:"id"`
	App        string          `json:"app"`
	Env        string          `json:"env"`
	Release    int64           `json:"release"`
	Host       string          `json:"host"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	Phase      string          `json:"phase"`
	Message    string          `json:"message,omitempty"`
	Actor      string          `json:"actor"`
	Reason     string          `json:"reason,omitempty"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
}

// SecretMeta describes the current version of a secret. Values are never
// part of it.
type SecretMeta struct {
	Key       string    `json:"key"`
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Event is one entry of the append-only audit log.
type Event struct {
	ID         int64           `json:"id"`
	At         time.Time       `json:"at"`
	App        string          `json:"app,omitempty"`
	Env        string          `json:"env,omitempty"`
	Deployment *int64          `json:"deployment,omitempty"`
	Kind       string          `json:"kind"`
	Message    string          `json:"message"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// --- hosts ---

// PutHost creates a host or replaces its address and token (re-adding a host
// is how its token is rotated).
func (s *Store) PutHost(ctx context.Context, h Host) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO hosts (name, addr, token_enc) VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE SET addr = excluded.addr, token_enc = excluded.token_enc, updated_at = now()`,
		h.Name, h.Addr, h.TokenEnc)
	return mapErr(err)
}

const hostCols = `name, addr, token_enc, created_at, updated_at`

func scanHost(r pgx.Row) (Host, error) {
	var h Host
	err := r.Scan(&h.Name, &h.Addr, &h.TokenEnc, &h.CreatedAt, &h.UpdatedAt)
	return h, mapErr(err)
}

// GetHost returns one host.
func (s *Store) GetHost(ctx context.Context, name string) (Host, error) {
	return scanHost(s.pool.QueryRow(ctx, `SELECT `+hostCols+` FROM hosts WHERE name = $1`, name))
}

// ListHosts returns all hosts by name.
func (s *Store) ListHosts(ctx context.Context) ([]Host, error) {
	return list(ctx, s, scanHost, `SELECT `+hostCols+` FROM hosts ORDER BY name`)
}

// --- apps and environments ---

// PutApp stores the manifest text and syncs environments to envs. An
// environment dropped from the manifest is deleted unless deployments
// reference it; the names of those kept are returned so callers can say so.
func (s *Store) PutApp(ctx context.Context, a App, envs []Environment) (kept []string, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO apps (name, manifest) VALUES ($1, $2)
			ON CONFLICT (name) DO UPDATE SET manifest = excluded.manifest, updated_at = now()`, a.Name, a.Manifest); err != nil {
			return err
		}
		names := make([]string, 0, len(envs))
		for _, e := range envs {
			names = append(names, e.Name)
			if _, err := tx.Exec(ctx, `INSERT INTO environments (app, name, host, domain, tls) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (app, name) DO UPDATE SET host = excluded.host, domain = excluded.domain, tls = excluded.tls`,
				a.Name, e.Name, e.Host, e.Domain, e.TLS); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM environments e WHERE e.app = $1 AND NOT (e.name = ANY($2))
			AND NOT EXISTS (SELECT 1 FROM deployments d WHERE d.app = e.app AND d.env = e.name)`, a.Name, names); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT name FROM environments WHERE app = $1 AND NOT (name = ANY($2)) ORDER BY name`, a.Name, names)
		if err != nil {
			return err
		}
		kept, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	return kept, mapErr(err)
}

const appCols = `name, manifest, created_at, updated_at`

func scanApp(r pgx.Row) (App, error) {
	var a App
	err := r.Scan(&a.Name, &a.Manifest, &a.CreatedAt, &a.UpdatedAt)
	return a, mapErr(err)
}

// GetApp returns one app including its manifest.
func (s *Store) GetApp(ctx context.Context, name string) (App, error) {
	return scanApp(s.pool.QueryRow(ctx, `SELECT `+appCols+` FROM apps WHERE name = $1`, name))
}

// ListApps returns all apps by name, without manifests.
func (s *Store) ListApps(ctx context.Context) ([]App, error) {
	return list(ctx, s, scanApp, `SELECT name, '', created_at, updated_at FROM apps ORDER BY name`)
}

func scanEnv(r pgx.Row) (Environment, error) {
	var e Environment
	err := r.Scan(&e.App, &e.Name, &e.Host, &e.Domain, &e.TLS)
	return e, mapErr(err)
}

// ListEnvironments returns an app's environments by name.
func (s *Store) ListEnvironments(ctx context.Context, app string) ([]Environment, error) {
	return list(ctx, s, scanEnv, `SELECT app, name, host, domain, tls FROM environments WHERE app = $1 ORDER BY name`, app)
}

// GetEnvironment returns one environment.
func (s *Store) GetEnvironment(ctx context.Context, app, env string) (Environment, error) {
	return scanEnv(s.pool.QueryRow(ctx, `SELECT app, name, host, domain, tls FROM environments WHERE app = $1 AND name = $2`, app, env))
}

// --- releases ---

// CreateRelease inserts r and fills in its ID and CreatedAt.
func (s *Store) CreateRelease(ctx context.Context, r *Release) error {
	images, err := json.Marshal(r.Images)
	if err != nil {
		return err
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO releases (app, commit, tag, manifest, images, actor)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at`,
		r.App, r.Commit, r.Tag, r.Manifest, images, r.Actor).Scan(&r.ID, &r.CreatedAt)
	return mapErr(err)
}

const releaseCols = `id, app, commit, tag, manifest, images, actor, created_at`

func scanRelease(r pgx.Row) (Release, error) {
	var rel Release
	err := r.Scan(&rel.ID, &rel.App, &rel.Commit, &rel.Tag, &rel.Manifest, &rel.Images, &rel.Actor, &rel.CreatedAt)
	return rel, mapErr(err)
}

// GetRelease returns release id of app (ErrNotFound if it belongs elsewhere).
func (s *Store) GetRelease(ctx context.Context, app string, id int64) (Release, error) {
	return scanRelease(s.pool.QueryRow(ctx, `SELECT `+releaseCols+` FROM releases WHERE app = $1 AND id = $2`, app, id))
}

// LatestRelease returns the newest release of app.
func (s *Store) LatestRelease(ctx context.Context, app string) (Release, error) {
	return scanRelease(s.pool.QueryRow(ctx, `SELECT `+releaseCols+` FROM releases WHERE app = $1 ORDER BY id DESC LIMIT 1`, app))
}

// ListReleases returns up to limit releases of app, newest first, without
// manifest snapshots.
func (s *Store) ListReleases(ctx context.Context, app string, limit int) ([]Release, error) {
	return list(ctx, s, scanRelease, `SELECT id, app, commit, tag, '', images, actor, created_at
		FROM releases WHERE app = $1 ORDER BY id DESC LIMIT $2`, app, limit)
}

// --- deployments ---

// CreateDeployment inserts d as running and fills in ID, Status, StartedAt.
func (s *Store) CreateDeployment(ctx context.Context, d *Deployment) error {
	d.Status = StatusRunning
	err := s.pool.QueryRow(ctx, `INSERT INTO deployments (app, env, release_id, host, kind, status, phase, actor, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id, started_at`,
		d.App, d.Env, d.Release, d.Host, d.Kind, d.Status, d.Phase, d.Actor, d.Reason).Scan(&d.ID, &d.StartedAt)
	return mapErr(err)
}

// FinishDeployment records the outcome of a running deployment.
func (s *Store) FinishDeployment(ctx context.Context, id int64, status, phase, message string, result json.RawMessage) error {
	var res any
	if len(result) > 0 {
		res = result
	}
	tag, err := s.pool.Exec(ctx, `UPDATE deployments SET status = $2, phase = $3, message = $4, result = $5, finished_at = now()
		WHERE id = $1`, id, status, phase, message, res)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FailInterrupted marks deployments left running by a previous controller
// process as failed. Their lock died with that process, so nothing else will
// ever finish them; the node may still have completed the work, which the
// message points out.
func (s *Store) FailInterrupted(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE deployments SET status = 'failed', finished_at = now(),
		message = 'interrupted: controller stopped during deployment; check node status'
		WHERE status = 'running'`)
	return tag.RowsAffected(), mapErr(err)
}

const deploymentCols = `id, app, env, release_id, host, kind, status, phase, message, actor, reason, started_at, finished_at, result`

func scanDeployment(r pgx.Row) (Deployment, error) {
	var d Deployment
	var result []byte
	err := r.Scan(&d.ID, &d.App, &d.Env, &d.Release, &d.Host, &d.Kind, &d.Status, &d.Phase, &d.Message,
		&d.Actor, &d.Reason, &d.StartedAt, &d.FinishedAt, &result)
	if len(result) > 0 {
		d.Result = result
	}
	return d, mapErr(err)
}

// GetDeployment returns one deployment.
func (s *Store) GetDeployment(ctx context.Context, id int64) (Deployment, error) {
	return scanDeployment(s.pool.QueryRow(ctx, `SELECT `+deploymentCols+` FROM deployments WHERE id = $1`, id))
}

// ListDeployments returns up to limit deployments of app/env, newest first.
func (s *Store) ListDeployments(ctx context.Context, app, env string, limit int) ([]Deployment, error) {
	return list(ctx, s, scanDeployment, `SELECT `+deploymentCols+` FROM deployments
		WHERE app = $1 AND env = $2 ORDER BY id DESC LIMIT $3`, app, env, limit)
}

// LastDeployment returns the newest deployment of app/env in any status.
func (s *Store) LastDeployment(ctx context.Context, app, env string) (Deployment, error) {
	return scanDeployment(s.pool.QueryRow(ctx, `SELECT `+deploymentCols+` FROM deployments
		WHERE app = $1 AND env = $2 ORDER BY id DESC LIMIT 1`, app, env))
}

// LiveDeployment returns the deployment currently serving app/env: the newest
// succeeded one (failed and reverted attempts leave the previous one live).
func (s *Store) LiveDeployment(ctx context.Context, app, env string) (Deployment, error) {
	return scanDeployment(s.pool.QueryRow(ctx, `SELECT `+deploymentCols+` FROM deployments
		WHERE app = $1 AND env = $2 AND status = 'succeeded' ORDER BY id DESC LIMIT 1`, app, env))
}

// RollbackTarget returns the newest succeeded deployment of app/env whose
// release differs from liveRelease.
func (s *Store) RollbackTarget(ctx context.Context, app, env string, liveRelease int64) (Deployment, error) {
	return scanDeployment(s.pool.QueryRow(ctx, `SELECT `+deploymentCols+` FROM deployments
		WHERE app = $1 AND env = $2 AND status = 'succeeded' AND release_id <> $3 ORDER BY id DESC LIMIT 1`, app, env, liveRelease))
}

// --- secrets ---

// SetSecret appends a new version of key holding the encrypted value.
func (s *Store) SetSecret(ctx context.Context, app, env, key string, valueEnc []byte) (int, error) {
	var v int
	err := s.pool.QueryRow(ctx, `INSERT INTO secrets (app, env, key, version, value)
		SELECT $1, $2, $3, coalesce(max(version), 0) + 1, $4 FROM secrets WHERE app = $1 AND env = $2 AND key = $3
		RETURNING version`, app, env, key, valueEnc).Scan(&v)
	return v, mapErr(err)
}

// DeleteSecret appends a tombstone; ErrNotFound if key is not currently set.
func (s *Store) DeleteSecret(ctx context.Context, app, env, key string) error {
	tag, err := s.pool.Exec(ctx, `INSERT INTO secrets (app, env, key, version, deleted)
		SELECT app, env, key, version + 1, true FROM secrets
		WHERE app = $1 AND env = $2 AND key = $3 AND NOT deleted
		  AND version = (SELECT max(version) FROM secrets WHERE app = $1 AND env = $2 AND key = $3)`, app, env, key)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// latestSecrets selects the newest version of each key that is not a tombstone.
const latestSecrets = `SELECT key, version, value, created_at FROM (
		SELECT DISTINCT ON (key) key, version, value, deleted, created_at FROM secrets
		WHERE app = $1 AND env = $2 ORDER BY key, version DESC) s
	WHERE NOT deleted ORDER BY key`

// ListSecrets returns names and versions of the currently set secrets.
func (s *Store) ListSecrets(ctx context.Context, app, env string) ([]SecretMeta, error) {
	return list(ctx, s, func(r pgx.Row) (SecretMeta, error) {
		var m SecretMeta
		var value []byte
		err := r.Scan(&m.Key, &m.Version, &value, &m.UpdatedAt)
		return m, err
	}, latestSecrets, app, env)
}

// SecretValues returns the encrypted current value of every set secret.
func (s *Store) SecretValues(ctx context.Context, app, env string) (map[string][]byte, error) {
	rows, err := s.pool.Query(ctx, latestSecrets, app, env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var key string
		var version int
		var value []byte
		var at time.Time
		if err := rows.Scan(&key, &version, &value, &at); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, rows.Err()
}

// --- events ---

// AddEvent appends e to the event log.
func (s *Store) AddEvent(ctx context.Context, e Event) error {
	var data any
	if len(e.Data) > 0 {
		data = e.Data
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO events (app, env, deployment_id, kind, message, data) VALUES ($1, $2, $3, $4, $5, $6)`,
		e.App, e.Env, e.Deployment, e.Kind, e.Message, data)
	return mapErr(err)
}

// ListEvents returns up to limit events, newest first, optionally filtered by
// app and env (empty = any).
func (s *Store) ListEvents(ctx context.Context, app, env string, limit int) ([]Event, error) {
	return list(ctx, s, func(r pgx.Row) (Event, error) {
		var e Event
		var data []byte
		err := r.Scan(&e.ID, &e.At, &e.App, &e.Env, &e.Deployment, &e.Kind, &e.Message, &data)
		if len(data) > 0 {
			e.Data = data
		}
		return e, err
	}, `SELECT id, at, app, env, deployment_id, kind, message, data FROM events
		WHERE ($1 = '' OR app = $1) AND ($2 = '' OR env = $2) ORDER BY id DESC LIMIT $3`, app, env, limit)
}

// list runs a query and scans every row with scan.
func list[T any](ctx context.Context, s *Store, scan func(pgx.Row) (T, error), sql string, args ...any) ([]T, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
