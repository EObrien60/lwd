package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"lwd/internal/bundle"
	"lwd/internal/client"
	"lwd/internal/manifest"
	"lwd/internal/nodeclient"
	"lwd/internal/store"
)

// Platform resources (DESIGN.md "M2"). The controller owns names and
// credentials: it generates a database password once, keeps every
// credential encrypted in `resources`, and asks the env's node on every
// deploy to make the resource exist (idempotent, so a rebuilt host heals on
// its next deploy). Credentials reach only the bundle's vars, which are
// never persisted or returned by the controller.

// databaseName is the role and database of app/env.
func databaseName(app, env string) string { return strings.ReplaceAll(app+"_"+env, "-", "_") }

// bucketName is the S3 bucket of app/env.
func bucketName(app, env string) string { return app + "-" + env }

// newPassword returns 32 random bytes, base64url: SQL- and URL-safe.
func newPassword() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

type databaseCreds struct {
	Password string `json:"password"`
}

func (c *Controller) seal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return c.cipher.Encrypt(b)
}

func (c *Controller) unseal(enc []byte, v any) error {
	b, err := c.cipher.Decrypt(enc)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// platformVarOwners maps each variable injected for m's resources to the
// manifest flag that injects it.
func platformVarOwners(m *manifest.Manifest) map[string]string {
	out := map[string]string{}
	if m.Database {
		for _, v := range bundle.DatabaseVars {
			out[v] = "database = true"
		}
	}
	if m.Storage {
		for _, v := range bundle.StorageVars {
			out[v] = "storage = true"
		}
	}
	return out
}

// checkNameFree refuses a resource name already held by another app-env
// (app "a-b" env "c" and app "a" env "b-c" map to the same names).
func (c *Controller) checkNameFree(ctx context.Context, app, env, kind, name string) error {
	all, err := c.store.ListResources(ctx, "", "")
	if err != nil {
		return err
	}
	for _, r := range all {
		if r.Kind == kind && r.Name == name && (r.App != app || r.Env != env) {
			return errf(client.CodeInvalid, "%s name %s is already used by %s/%s", kind, name, r.App, r.Env)
		}
	}
	return nil
}

// provision makes the resources declared by m exist for e on its host and
// returns the variables to inject.
func (c *Controller) provision(ctx context.Context, e store.Environment, m *manifest.Manifest) (map[string]string, error) {
	vars := map[string]string{}
	if !m.Database && !m.Storage {
		return vars, nil
	}
	nc, _, err := c.node(ctx, e.Host)
	if err != nil {
		return nil, err
	}
	if m.Database {
		u, err := c.provisionDatabase(ctx, nc, e)
		if err != nil {
			return nil, err
		}
		vars["DATABASE_URL"] = u
	}
	if m.Storage {
		creds, name, err := c.provisionBucket(ctx, nc, e)
		if err != nil {
			return nil, err
		}
		vars["S3_ENDPOINT"] = bundle.S3Endpoint
		vars["S3_REGION"] = bundle.S3Region
		vars["S3_BUCKET"] = name
		vars["S3_ACCESS_KEY_ID"] = creds.AccessKeyID
		vars["S3_SECRET_ACCESS_KEY"] = creds.SecretAccessKey
	}
	return vars, nil
}

func (c *Controller) provisionDatabase(ctx context.Context, nc *nodeclient.Client, e store.Environment) (string, error) {
	name := databaseName(e.App, e.Name)
	row, err := c.store.GetResource(ctx, e.App, e.Name, store.ResourceDatabase)
	var creds databaseCreds
	switch {
	case err == nil:
		if err := c.unseal(row.Credentials, &creds); err != nil {
			return "", fmt.Errorf("database credentials of %s/%s: %w", e.App, e.Name, err)
		}
		name = row.Name
	case errors.Is(err, store.ErrNotFound):
		if err := c.checkNameFree(ctx, e.App, e.Name, store.ResourceDatabase, name); err != nil {
			return "", err
		}
		creds.Password = newPassword()
	default:
		return "", err
	}
	isNew := err != nil
	// Stored before the node call: the password is generated exactly once,
	// and a failed or repeated call reuses it.
	if isNew || row.Host != e.Host {
		enc, err := c.seal(creds)
		if err != nil {
			return "", err
		}
		if err := c.store.PutResource(ctx, store.Resource{App: e.App, Env: e.Name, Kind: store.ResourceDatabase, Host: e.Host, Name: name, Credentials: enc}); err != nil {
			if errors.Is(err, store.ErrInvalid) {
				return "", errf(client.CodeInvalid, "database name %s is already used by another app environment", name)
			}
			return "", err
		}
	}
	if err := nc.ProvisionDatabase(ctx, name, bundle.DatabaseRequest{Password: creds.Password}); err != nil {
		return "", provisionErr(e.Host, "database "+name, err)
	}
	if isNew {
		c.event(ctx, store.Event{App: e.App, Env: e.Name, Kind: "resource.provisioned",
			Message: fmt.Sprintf("database %s provisioned on %s", name, e.Host),
			Data:    jsonData(map[string]string{"kind": store.ResourceDatabase, "name": name, "host": e.Host})})
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(name, creds.Password),
		Host:     bundle.PostgresHost + ":" + strconv.Itoa(bundle.PostgresPort),
		Path:     "/" + name,
		RawQuery: "sslmode=disable",
	}
	return u.String(), nil
}

func (c *Controller) provisionBucket(ctx context.Context, nc *nodeclient.Client, e store.Environment) (bundle.BucketCredentials, string, error) {
	var creds bundle.BucketCredentials
	name := bucketName(e.App, e.Name)
	row, err := c.store.GetResource(ctx, e.App, e.Name, store.ResourceBucket)
	switch {
	case err == nil:
		if err := c.unseal(row.Credentials, &creds); err != nil {
			return creds, "", fmt.Errorf("bucket credentials of %s/%s: %w", e.App, e.Name, err)
		}
		name = row.Name
	case errors.Is(err, store.ErrNotFound):
		// Checked before the node call: the node would hand an existing
		// bucket's key to whoever asks for its name.
		if err := c.checkNameFree(ctx, e.App, e.Name, store.ResourceBucket, name); err != nil {
			return creds, "", err
		}
	default:
		return creds, "", err
	}
	isNew := err != nil
	got, err := nc.ProvisionBucket(ctx, name, bundle.BucketRequest{AccessKeyID: creds.AccessKeyID})
	if err != nil {
		return creds, "", provisionErr(e.Host, "bucket "+name, err)
	}
	if got.AccessKeyID == "" || got.SecretAccessKey == "" {
		return creds, "", fmt.Errorf("host %s returned no credentials for bucket %s", e.Host, name)
	}
	if isNew || got != creds || row.Host != e.Host {
		enc, err := c.seal(got)
		if err != nil {
			return creds, "", err
		}
		if err := c.store.PutResource(ctx, store.Resource{App: e.App, Env: e.Name, Kind: store.ResourceBucket, Host: e.Host, Name: name, Credentials: enc}); err != nil {
			if errors.Is(err, store.ErrInvalid) {
				return creds, "", errf(client.CodeInvalid, "bucket name %s is already used by another app environment", name)
			}
			return creds, "", err
		}
	}
	if isNew {
		c.event(ctx, store.Event{App: e.App, Env: e.Name, Kind: "resource.provisioned",
			Message: fmt.Sprintf("bucket %s provisioned on %s", name, e.Host),
			Data:    jsonData(map[string]string{"kind": store.ResourceBucket, "name": name, "host": e.Host})})
	} else if got.AccessKeyID != creds.AccessKeyID {
		c.event(ctx, store.Event{App: e.App, Env: e.Name, Kind: "resource.rekeyed",
			Message: fmt.Sprintf("bucket %s on %s has a new access key (previous key no longer existed)", name, e.Host),
			Data:    jsonData(map[string]string{"kind": store.ResourceBucket, "name": name, "host": e.Host})})
	}
	return got, name, nil
}

// provisionErr names the resource in a node failure.
func provisionErr(host, what string, err error) error {
	apiErr := nodeErr(host, err)
	var ae *Error
	if errors.As(apiErr, &ae) {
		return errf(ae.Code, "provision %s: %s", what, ae.Message)
	}
	return fmt.Errorf("provision %s on %s: %w", what, host, apiErr)
}

// --- API ---

// Resources lists app/env's resources without credentials.
func (c *Controller) Resources(ctx context.Context, app, env string) ([]store.Resource, error) {
	if _, err := c.store.GetEnvironment(ctx, app, env); err != nil {
		return nil, notFound(err, "environment %s/%s", app, env)
	}
	return c.store.ListResources(ctx, app, env)
}

func (c *Controller) database(ctx context.Context, app, env string) (store.Resource, error) {
	if _, err := c.store.GetEnvironment(ctx, app, env); err != nil {
		return store.Resource{}, notFound(err, "environment %s/%s", app, env)
	}
	r, err := c.store.GetResource(ctx, app, env, store.ResourceDatabase)
	if errors.Is(err, store.ErrNotFound) {
		return r, errf(client.CodeNotFound, "%s/%s has no database (declare database = true and deploy)", app, env)
	}
	return r, err
}

// BackupDB backs up app/env's database and records the attempt, failed or
// not. kind is store.BackupManual or store.BackupScheduled.
func (c *Controller) BackupDB(ctx context.Context, app, env, kind, actor string) (store.Backup, error) {
	r, err := c.database(ctx, app, env)
	if err != nil {
		return store.Backup{}, err
	}
	b := store.Backup{App: app, Env: env, Host: r.Host, Database: r.Name, Kind: kind, StartedAt: time.Now()}
	var bf bundle.BackupFile
	nc, _, err := c.node(ctx, r.Host)
	if err == nil {
		bf, err = nc.BackupDatabase(ctx, r.Name)
		if err != nil {
			err = nodeErr(r.Host, err)
		}
	}
	fin := time.Now()
	b.FinishedAt = &fin
	if err != nil {
		b.Status = store.BackupFailed
		b.Error = err.Error()
		var ae *Error
		if errors.As(err, &ae) {
			b.Error = ae.Message
		}
	} else {
		b.Status, b.File, b.Bytes, b.SHA256 = store.BackupSucceeded, bf.File, bf.Bytes, bf.SHA256
	}
	if aerr := c.store.AddBackup(ctx, &b); aerr != nil {
		return b, errors.Join(err, aerr)
	}
	msg := fmt.Sprintf("%s backup %d of %s on %s %s", kind, b.ID, r.Name, r.Host, b.Status)
	if b.Error != "" {
		msg += ": " + b.Error
	} else {
		msg += fmt.Sprintf(" (%s, %d bytes)", b.File, b.Bytes)
	}
	c.event(ctx, store.Event{App: app, Env: env, Kind: "backup." + b.Status, Message: msg + " by " + actor,
		Data: jsonData(map[string]any{"backup": b.ID, "database": r.Name, "host": r.Host, "kind": kind, "file": b.File, "actor": actor})})
	if err != nil {
		return b, err
	}
	return b, nil
}

// RestoreDB restores a recorded backup into app/env's database under the
// app-env deploy lock.
func (c *Controller) RestoreDB(ctx context.Context, app, env string, id int64, actor string) (client.RestoreResult, error) {
	var out client.RestoreResult
	r, err := c.database(ctx, app, env)
	if err != nil {
		return out, err
	}
	b, err := c.store.GetBackup(ctx, id)
	if err != nil {
		return out, notFound(err, "backup %d", id)
	}
	switch {
	case b.App != app || b.Env != env:
		return out, errf(client.CodeInvalid, "backup %d belongs to %s/%s, not %s/%s", id, b.App, b.Env, app, env)
	case b.Status != store.BackupSucceeded:
		return out, errf(client.CodeInvalid, "backup %d %s; only successful backups can be restored", id, b.Status)
	case b.Host != r.Host || b.Database != r.Name:
		return out, errf(client.CodeInvalid, "backup %d is of %s on %s; the database is now %s on %s", id, b.Database, b.Host, r.Name, r.Host)
	}
	out.Backup = b
	err = c.store.WithLock(ctx, app, env, func(ctx context.Context) error {
		nc, _, err := c.node(ctx, r.Host)
		if err != nil {
			return err
		}
		res, err := nc.RestoreDatabase(ctx, r.Name, bundle.RestoreRequest{File: b.File, App: app, Env: env})
		if err != nil {
			err = nodeErr(r.Host, err)
			c.event(ctx, store.Event{App: app, Env: env, Kind: "db.restore_failed",
				Message: fmt.Sprintf("restore of backup %d into %s failed: %v (by %s)", id, r.Name, err, actor),
				Data:    jsonData(map[string]any{"backup": id, "database": r.Name, "actor": actor})})
			return err
		}
		out.RestoreResult = res
		c.event(ctx, store.Event{App: app, Env: env, Kind: "db.restored",
			Message: fmt.Sprintf("backup %d (%s) restored into %s on %s in %dms by %s", id, b.File, r.Name, r.Host, res.TotalMS, actor),
			Data:    jsonData(map[string]any{"backup": id, "database": r.Name, "file": b.File, "total_ms": res.TotalMS, "actor": actor})})
		return nil
	})
	if errors.Is(err, store.ErrBusy) {
		return out, errf(client.CodeConflict, "a deployment or restore of %s/%s is in progress", app, env)
	}
	return out, err
}

// BackupStatus summarises every database's newest backup and recent failures.
func (c *Controller) BackupStatus(ctx context.Context) (client.BackupStatus, error) {
	out := client.BackupStatus{Databases: []client.DatabaseBackups{}}
	rs, err := c.store.ListResources(ctx, "", "")
	if err != nil {
		return out, err
	}
	for _, r := range rs {
		if r.Kind != store.ResourceDatabase {
			continue
		}
		d := client.DatabaseBackups{App: r.App, Env: r.Env, Host: r.Host, Database: r.Name}
		if b, err := c.store.LatestBackup(ctx, r.App, r.Env, store.BackupSucceeded); err == nil {
			d.Latest = &b
		} else if !errors.Is(err, store.ErrNotFound) {
			return out, err
		}
		if b, err := c.store.LatestBackup(ctx, r.App, r.Env, store.BackupFailed); err == nil && (d.Latest == nil || b.ID > d.Latest.ID) {
			d.LastFailure = &b
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return out, err
		}
		out.Databases = append(out.Databases, d)
	}
	if out.Failures, err = c.store.FailedBackupsSince(ctx, time.Now().Add(-7*24*time.Hour)); err != nil {
		return out, err
	}
	return out, nil
}

// RunScheduledBackups backs up every database that has no scheduled backup
// since today's backup hour, if that hour has passed. Running it again the
// same day, or after a controller restart, does not repeat work; a
// controller that was down at the hour catches up when it starts.
func (c *Controller) RunScheduledBackups(ctx context.Context, now time.Time, hour int) {
	since := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if now.Before(since) {
		return
	}
	rs, err := c.store.ListResources(ctx, "", "")
	if err != nil {
		c.log.Error("scheduled backups: list resources", "err", err)
		return
	}
	rs = slices.DeleteFunc(rs, func(r store.Resource) bool { return r.Kind != store.ResourceDatabase })
	for _, r := range rs {
		done, err := c.store.ScheduledBackupSince(ctx, r.App, r.Env, since)
		if err != nil {
			c.log.Error("scheduled backups", "app", r.App, "env", r.Env, "err", err)
			continue
		}
		if done {
			continue
		}
		b, err := c.BackupDB(ctx, r.App, r.Env, store.BackupScheduled, "scheduler")
		if err != nil {
			c.log.Error("scheduled backup failed", "app", r.App, "env", r.Env, "backup", b.ID, "err", err)
		} else {
			c.log.Info("scheduled backup", "app", r.App, "env", r.Env, "backup", b.ID, "file", b.File, "bytes", b.Bytes)
		}
	}
}

// backupLoop runs RunScheduledBackups every interval until ctx ends.
func (c *Controller) backupLoop(ctx context.Context, hour int, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		c.RunScheduledBackups(ctx, time.Now(), hour)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
