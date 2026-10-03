package node

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"lwd/internal/bundle"
	"lwd/internal/router"
)

// Platform services (DESIGN.md "M2"): the node-owned compose project
// lwd-platform runs one Postgres and one Garage S3 server on the docker
// network lwd-platform. Neither publishes a host port. The project is started
// the first time a resource is provisioned and re-upped on every node start
// once it exists. All provisioning is idempotent and goes through
// `docker exec` into the platform containers; SQL is passed on stdin, never
// on a command line, and every name is validated before use.

const (
	platformProject   = "lwd-platform"
	postgresImage     = "postgres:17-alpine"
	postgresContainer = "lwd-postgres"
	s3Container       = "lwd-s3"
	// garageImage is Garage v2.4.1 (multi-arch index digest), pinned so a
	// re-up never silently changes the S3 server's on-disk format.
	garageImage = "dxflrs/garage:v2.4.1@sha256:9c96caa2612d3411acc5b0e6701fb238dbfba33e533a6d7d3d811a4b12d0d020"
	// garageCapacity is the single node's layout capacity. With one node it
	// only weights partitions; it is not a quota.
	garageCapacity = "100G"
	keepBackups    = 14
	// platformWait bounds `compose up --wait` (first start pulls images and
	// runs initdb).
	platformWait = 180
)

func (n *Node) platformDir() string { return filepath.Join(n.cfg.Dir, "platform") }
func (n *Node) backupDir(db string) string {
	return filepath.Join(n.cfg.Dir, "backups", "postgres", db)
}

func (n *Node) platformProject() project {
	dir := n.platformDir()
	return project{name: platformProject, dir: dir, files: []string{filepath.Join(dir, composeFile)}}
}

// platformExists reports whether this node has ever started platform services.
func (n *Node) platformExists() bool {
	_, err := os.Stat(filepath.Join(n.platformDir(), composeFile))
	return err == nil
}

// renderPlatformCompose renders platform/compose.yaml. It holds the Postgres
// superuser password, so it is written 0600 in a 0700 directory.
func renderPlatformCompose(root, pgPassword string) []byte {
	health := func(test ...string) *composeHealthcheck {
		return &composeHealthcheck{Test: append([]string{"CMD"}, test...), Interval: "2s", Timeout: "5s", Retries: 30, StartPeriod: "10s"}
	}
	doc := composeDoc{
		Name: platformProject,
		Services: map[string]*composeService{
			"postgres": {
				Image:         postgresImage,
				ContainerName: postgresContainer,
				Restart:       "unless-stopped",
				Networks:      []string{bundle.PlatformNetwork},
				Volumes:       []string{filepath.Join(root, "postgres") + ":/var/lib/postgresql/data"},
				Environment:   map[string]string{"POSTGRES_PASSWORD": esc(pgPassword)},
				// TCP, not the socket: initdb's temporary server listens on the
				// socket only, so this turns healthy when the real one is up.
				Healthcheck: health("pg_isready", "-U", "postgres", "-h", "127.0.0.1"),
				Logging:     defaultLogging(),
			},
			"s3": {
				Image:         garageImage,
				ContainerName: s3Container,
				Restart:       "unless-stopped",
				Networks:      []string{bundle.PlatformNetwork},
				Volumes: []string{
					filepath.Join(root, "platform", "garage.toml") + ":/etc/garage.toml:ro",
					filepath.Join(root, "garage", "meta") + ":/var/lib/garage/meta",
					filepath.Join(root, "garage", "data") + ":/var/lib/garage/data",
				},
				Healthcheck: health("/garage", "status"),
				Logging:     defaultLogging(),
			},
		},
		Networks: platformNetworks(),
	}
	out, _ := json.MarshalIndent(doc, "", "  ")
	return out
}

// renderGarageConfig renders garage.toml for a single-node cluster. The admin
// API binds to the container's loopback only; the node uses the CLI.
func renderGarageConfig(rpcSecret, adminToken string) []byte {
	return []byte(fmt.Sprintf(`metadata_dir = "/var/lib/garage/meta"
data_dir = "/var/lib/garage/data"
db_engine = "sqlite"
replication_factor = 1

rpc_bind_addr = "[::]:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = %q

[s3_api]
s3_region = %q
api_bind_addr = "[::]:3900"

[admin]
api_bind_addr = "127.0.0.1:3903"
admin_token = %q
`, rpcSecret, bundle.S3Region, adminToken))
}

func randomHex(nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on Linux
	}
	return hex.EncodeToString(b)
}

// secretFile returns the content of path, creating it with a fresh random
// value first if it does not exist. Generated once: these never rotate.
func secretFile(path string, nbytes int) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	v := randomHex(nbytes)
	if err := router.WriteFileAtomic(path, []byte(v+"\n"), 0o600); err != nil {
		return "", err
	}
	return v, nil
}

// ensurePlatform brings the platform project up and configured. Idempotent;
// callers hold platformMu.
func (n *Node) ensurePlatform(ctx context.Context) error {
	for _, d := range []struct {
		path string
		perm os.FileMode
	}{
		{n.platformDir(), 0o700},
		{filepath.Join(n.cfg.Dir, "postgres"), 0o700},
		{filepath.Join(n.cfg.Dir, "garage", "meta"), 0o700},
		{filepath.Join(n.cfg.Dir, "garage", "data"), 0o700},
		{filepath.Join(n.cfg.Dir, "backups", "postgres"), 0o700},
	} {
		if err := os.MkdirAll(d.path, d.perm); err != nil {
			return err
		}
	}
	pw, err := secretFile(filepath.Join(n.platformDir(), "postgres.password"), 32)
	if err != nil {
		return err
	}
	garageToml := filepath.Join(n.platformDir(), "garage.toml")
	if _, err := os.Stat(garageToml); errors.Is(err, os.ErrNotExist) {
		// The rpc secret must stay stable for the life of the data dir.
		if err := router.WriteFileAtomic(garageToml, renderGarageConfig(randomHex(32), randomHex(32)), 0o600); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	p := n.platformProject()
	if err := router.WriteFileAtomic(p.files[0], renderPlatformCompose(n.cfg.Dir, pw), 0o600); err != nil {
		return err
	}
	if err := n.ensureNetwork(ctx); err != nil {
		return err
	}
	if _, err := n.docker.Run(ctx, p.args("up", "-d", "--wait", "--wait-timeout", fmt.Sprint(platformWait))...); err != nil {
		return fmt.Errorf("start platform services: %w", err)
	}
	if err := n.ensureGarageLayout(ctx); err != nil {
		return fmt.Errorf("garage layout: %w", err)
	}
	// App roles may connect only to their own database; the maintenance
	// databases are closed to PUBLIC too.
	if err := n.psql(ctx, "postgres", "REVOKE CONNECT, TEMPORARY ON DATABASE postgres, template1 FROM PUBLIC;\n"); err != nil {
		return fmt.Errorf("postgres hardening: %w", err)
	}
	return nil
}

// ensureNetwork creates the platform network if it does not exist. It is
// created by the node, not compose, so every project can treat it as external.
func (n *Node) ensureNetwork(ctx context.Context) error {
	if _, err := n.docker.Run(ctx, "network", "inspect", "--format", "{{.Name}}", bundle.PlatformNetwork); err == nil {
		return nil
	}
	if _, err := n.docker.Run(ctx, "network", "create", bundle.PlatformNetwork); err != nil {
		// Lost a race with another creator: fine if it exists now.
		if _, ierr := n.docker.Run(ctx, "network", "inspect", "--format", "{{.Name}}", bundle.PlatformNetwork); ierr == nil {
			return nil
		}
		return fmt.Errorf("create network %s: %w", bundle.PlatformNetwork, err)
	}
	return nil
}

// startPlatform re-ups an existing platform project at node start. Failure
// is logged, not fatal: Caddy and apps without resources must keep serving,
// and the next provisioning request retries.
func (n *Node) startPlatform(ctx context.Context) {
	if !n.platformExists() {
		return
	}
	n.platformMu.Lock()
	defer n.platformMu.Unlock()
	if err := n.ensurePlatform(ctx); err != nil {
		log.Printf("platform services: %v", err)
	}
}

func (n *Node) garage(ctx context.Context, args ...string) ([]byte, error) {
	return n.docker.Run(ctx, append([]string{"exec", s3Container, "/garage"}, args...)...)
}

var (
	garageApplyRE  = regexp.MustCompile(`garage layout apply --version ([0-9]+)`)
	garageKeyIDRE  = regexp.MustCompile(`(?m)^Key ID:\s+(GK[0-9a-f]+)\s*$`)
	garageSecretRE = regexp.MustCompile(`(?m)^Secret key:\s+([0-9a-f]+)\s*$`)
)

// ensureGarageLayout gives the single node a role once. It also completes a
// layout change that was staged but not applied (a crash in between).
func (n *Node) ensureGarageLayout(ctx context.Context) error {
	out, err := n.garage(ctx, "layout", "show")
	if err != nil {
		return err
	}
	if strings.Contains(string(out), "No nodes currently have a role") {
		idOut, err := n.garage(ctx, "node", "id", "-q")
		if err != nil {
			return err
		}
		id, _, _ := strings.Cut(strings.TrimSpace(string(idOut)), "@")
		if !regexp.MustCompile(`^[0-9a-f]{16,64}$`).MatchString(id) {
			return fmt.Errorf("unexpected node id %q", id)
		}
		if _, err := n.garage(ctx, "layout", "assign", "-z", "dc1", "-c", garageCapacity, id); err != nil {
			return err
		}
		if out, err = n.garage(ctx, "layout", "show"); err != nil {
			return err
		}
	}
	if m := garageApplyRE.FindStringSubmatch(string(out)); m != nil {
		if _, err := n.garage(ctx, "layout", "apply", "--version", m[1]); err != nil {
			return err
		}
	}
	return nil
}

// psql runs sql (stdin) as the superuser against db, stopping at the first error.
func (n *Node) psql(ctx context.Context, db, sql string) error {
	return n.docker.Stream(ctx, strings.NewReader(sql), io.Discard,
		"exec", "-i", postgresContainer, "psql", "-U", "postgres", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-d", db)
}

// databaseSQL creates or updates role and database name with the M2 posture:
// role LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE, database and public schema
// owned by it, CONNECT revoked from PUBLIC. name and password are validated
// by the caller against bundle.DatabaseNameRE / bundle.PasswordRE, which
// admit no quote characters.
func databaseSQL(name, password string) string {
	return fmt.Sprintf(`SELECT 'CREATE ROLE "%[1]s" LOGIN' WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%[1]s')\gexec
ALTER ROLE "%[1]s" WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '%[2]s';
SELECT 'CREATE DATABASE "%[1]s" OWNER "%[1]s"' WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = '%[1]s')\gexec
ALTER DATABASE "%[1]s" OWNER TO "%[1]s";
REVOKE CONNECT, TEMPORARY ON DATABASE "%[1]s" FROM PUBLIC;
\connect "%[1]s"
ALTER SCHEMA public OWNER TO "%[1]s";
`, name, password)
}

// recreateSQL drops database name (disconnecting everyone) and creates it
// empty with the same owner posture, ready for pg_restore.
func recreateSQL(name string) string {
	return fmt.Sprintf(`DROP DATABASE IF EXISTS "%[1]s" WITH (FORCE);
CREATE DATABASE "%[1]s" OWNER "%[1]s";
REVOKE CONNECT, TEMPORARY ON DATABASE "%[1]s" FROM PUBLIC;
\connect "%[1]s"
ALTER SCHEMA public OWNER TO "%[1]s";
`, name)
}

// ProvisionDatabase ensures the platform is up and role/database name exist
// with the given password.
func (n *Node) ProvisionDatabase(ctx context.Context, name string, req bundle.DatabaseRequest) error {
	if !bundle.DatabaseNameRE.MatchString(name) {
		return invalidf("invalid database name %q (must match %s)", name, bundle.DatabaseNameRE)
	}
	if !bundle.PasswordRE.MatchString(req.Password) {
		return invalidf("invalid password: must match %s", bundle.PasswordRE)
	}
	n.platformMu.Lock()
	defer n.platformMu.Unlock()
	if err := n.ensurePlatform(ctx); err != nil {
		return err
	}
	if err := n.psql(ctx, "postgres", databaseSQL(name, req.Password)); err != nil {
		return fmt.Errorf("provision database %s: %w", name, err)
	}
	return nil
}

// ProvisionBucket ensures the platform is up, bucket name exists and an
// access key owning it exists. The key named by req is kept if it still
// exists; otherwise the key named lwd-<bucket> is reused, or a new one made.
func (n *Node) ProvisionBucket(ctx context.Context, name string, req bundle.BucketRequest) (bundle.BucketCredentials, error) {
	var creds bundle.BucketCredentials
	if !bundle.BucketNameRE.MatchString(name) {
		return creds, invalidf("invalid bucket name %q (must match %s)", name, bundle.BucketNameRE)
	}
	if req.AccessKeyID != "" && !bundle.AccessKeyIDRE.MatchString(req.AccessKeyID) {
		return creds, invalidf("invalid access key id %q", req.AccessKeyID)
	}
	n.platformMu.Lock()
	defer n.platformMu.Unlock()
	if err := n.ensurePlatform(ctx); err != nil {
		return creds, err
	}
	if _, err := n.garage(ctx, "bucket", "info", name); err != nil {
		if !garageNotFound(err) {
			return creds, err
		}
		if _, err := n.garage(ctx, "bucket", "create", name); err != nil {
			return creds, err
		}
	}
	keyName := "lwd-" + name
	id := req.AccessKeyID
	if id != "" {
		if _, err := n.garage(ctx, "key", "info", id); err != nil {
			if !garageNotFound(err) {
				return creds, err
			}
			id = ""
		}
	}
	if id == "" {
		out, err := n.garage(ctx, "key", "list")
		if err != nil {
			return creds, err
		}
		id = findGarageKey(string(out), keyName)
	}
	var out []byte
	var err error
	if id == "" {
		out, err = n.garage(ctx, "key", "create", keyName)
	} else {
		out, err = n.garage(ctx, "key", "info", id, "--show-secret")
	}
	if err != nil {
		return creds, err
	}
	kid, secret := garageKeyIDRE.FindSubmatch(out), garageSecretRE.FindSubmatch(out)
	if kid == nil || secret == nil || !bundle.AccessKeyIDRE.Match(kid[1]) {
		return creds, errors.New("could not parse garage key output")
	}
	creds = bundle.BucketCredentials{AccessKeyID: string(kid[1]), SecretAccessKey: string(secret[1])}
	if _, err := n.garage(ctx, "bucket", "allow", "--read", "--write", "--owner", name, "--key", creds.AccessKeyID); err != nil {
		return bundle.BucketCredentials{}, err
	}
	return creds, nil
}

func garageNotFound(err error) bool {
	s := err.Error()
	return strings.Contains(s, "NoSuchBucket") || strings.Contains(s, "NoSuchAccessKey") || strings.Contains(s, "not found")
}

// findGarageKey returns the id of the key called name in `garage key list`
// output, or "" if there is not exactly one.
func findGarageKey(list, name string) string {
	var found []string
	for _, line := range strings.Split(list, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && bundle.AccessKeyIDRE.MatchString(f[0]) && f[2] == name {
			found = append(found, f[0])
		}
	}
	if len(found) != 1 {
		return ""
	}
	return found[0]
}

const backupTimeFormat = "20060102T150405.000Z"

// BackupDatabase writes `pg_dump -Fc` of name to
// backups/postgres/<name>/<UTC timestamp>.dump and keeps the newest 14.
func (n *Node) BackupDatabase(ctx context.Context, name string) (bundle.BackupFile, error) {
	var bf bundle.BackupFile
	if !bundle.DatabaseNameRE.MatchString(name) {
		return bf, invalidf("invalid database name %q", name)
	}
	lock := "db:" + name
	if !n.tryLock(lock) {
		return bf, errBusy
	}
	defer n.unlock(lock)
	dir := n.backupDir(name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return bf, err
	}
	at := time.Now().UTC()
	file := at.Format(backupTimeFormat) + ".dump"
	final := filepath.Join(dir, file)
	tmp, err := os.CreateTemp(dir, ".partial-*")
	if err != nil {
		return bf, err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(tmp, h)}
	err = n.docker.Stream(ctx, nil, cw, "exec", postgresContainer, "pg_dump", "-U", "postgres", "-Fc", "-d", name)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return bf, fmt.Errorf("pg_dump %s: %w", name, err)
	}
	if cw.n == 0 {
		return bf, fmt.Errorf("pg_dump %s produced no output", name)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return bf, err
	}
	n.pruneBackups(name)
	return bundle.BackupFile{File: file, Bytes: cw.n, SHA256: hex.EncodeToString(h.Sum(nil)), CreatedAt: at.Format(time.RFC3339)}, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// ListBackups returns name's backups, newest first.
func (n *Node) ListBackups(name string) ([]bundle.BackupFile, error) {
	if !bundle.DatabaseNameRE.MatchString(name) {
		return nil, invalidf("invalid database name %q", name)
	}
	entries, err := os.ReadDir(n.backupDir(name))
	if errors.Is(err, fs.ErrNotExist) {
		return []bundle.BackupFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []bundle.BackupFile{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !bundle.BackupFileRE.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		bf := bundle.BackupFile{File: e.Name(), Bytes: info.Size()}
		if t, err := time.Parse(backupTimeFormat, strings.TrimSuffix(e.Name(), ".dump")); err == nil {
			bf.CreatedAt = t.Format(time.RFC3339)
		}
		out = append(out, bf)
	}
	// Timestamped names sort chronologically.
	sort.Slice(out, func(i, j int) bool { return out[i].File > out[j].File })
	return out, nil
}

func (n *Node) pruneBackups(name string) {
	bs, err := n.ListBackups(name)
	if err != nil {
		return
	}
	for i, b := range bs {
		if i >= keepBackups {
			os.Remove(filepath.Join(n.backupDir(name), b.File))
		}
	}
}

// RestoreDatabase replaces database name with the content of a backup: the
// live project of req.App/req.Env is stopped, the database dropped and
// recreated with the same owner posture, the dump restored as the owning
// role, and the project started again (also when the restore failed, so the
// app is never left down by it).
func (n *Node) RestoreDatabase(ctx context.Context, name string, req bundle.RestoreRequest) (bundle.RestoreResult, error) {
	res := bundle.RestoreResult{File: req.File}
	switch {
	case !bundle.DatabaseNameRE.MatchString(name):
		return res, invalidf("invalid database name %q", name)
	case !bundle.BackupFileRE.MatchString(req.File):
		return res, invalidf("invalid backup file %q", req.File)
	case !nameRE.MatchString(req.App) || !nameRE.MatchString(req.Env):
		return res, invalidf("invalid app/env %q/%q", req.App, req.Env)
	}
	f, err := os.Open(filepath.Join(n.backupDir(name), req.File))
	if errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("%w: backup %s of %s", errNoBackup, req.File, name)
	}
	if err != nil {
		return res, err
	}
	defer f.Close()

	key := req.App + "-" + req.Env
	if !n.tryLock(key) {
		return res, errBusy
	}
	defer n.unlock(key)
	dbLock := "db:" + name
	if !n.tryLock(dbLock) {
		return res, errBusy
	}
	defer n.unlock(dbLock)

	start := time.Now()
	st, err := loadState(n.statePath(key))
	if err != nil {
		return res, err
	}
	var proj *project
	if st.Live != nil {
		p := n.liveProject(key, st.Live)
		proj = &p
		t := time.Now()
		if _, err := n.docker.Run(ctx, p.args("stop")...); err != nil {
			// Whatever stopped is started again.
			n.docker.Run(ctx, p.args("start")...)
			return res, fmt.Errorf("stop %s: %w", p.name, err)
		}
		res.Stopped, res.StopMS = true, time.Since(t).Milliseconds()
	}

	t := time.Now()
	rerr := n.psql(ctx, "postgres", recreateSQL(name))
	if rerr != nil {
		rerr = fmt.Errorf("recreate database %s: %w", name, rerr)
	} else if err := n.docker.Stream(ctx, f, io.Discard, "exec", "-i", postgresContainer,
		"pg_restore", "-U", "postgres", "--no-owner", "--role="+name, "--exit-on-error", "-d", name); err != nil {
		rerr = fmt.Errorf("pg_restore %s: %w", req.File, err)
	}
	res.RestoreMS = time.Since(t).Milliseconds()

	if proj != nil {
		t := time.Now()
		if _, err := n.docker.Run(ctx, proj.args("start")...); err != nil {
			rerr = errors.Join(rerr, fmt.Errorf("start %s: %w", proj.name, err))
		}
		res.StartMS = time.Since(t).Milliseconds()
	}
	res.TotalMS = time.Since(start).Milliseconds()
	return res, rerr
}

var errNoBackup = errors.New("no such backup")
