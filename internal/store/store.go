// Package store is the controller's Postgres state: hosts, apps and their
// environments, immutable releases, deployment history, versioned encrypted
// secrets and the append-only event log. Plain SQL over pgxpool; the schema
// lives in embedded migrations applied at controller start.
//
// The store never encrypts or decrypts: callers pass ciphertext for node
// tokens and secret values, so plaintext never reaches this layer.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotFound means the addressed row does not exist.
	ErrNotFound = errors.New("not found")
	// ErrBusy means another operation holds the app-env lock.
	ErrBusy = errors.New("busy")
	// ErrInvalid means a write violated a constraint (e.g. unknown host).
	ErrInvalid = errors.New("invalid")
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is a handle on the controller database.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects and pings the database at url.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases all connections.
func (s *Store) Close() { s.pool.Close() }

// migrationLockKey serialises concurrent Migrate calls (e.g. two controllers
// starting at once). Arbitrary constant: "lwd" in ASCII.
const migrationLockKey = 0x6c7764

// Migrate applies pending embedded migrations in filename order. Each runs in
// its own transaction together with its schema_migrations row, so a failed
// migration leaves no partial schema behind.
func (s *Store) Migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	type mig struct {
		version int
		file    string
	}
	var ms []mig
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil || !strings.HasSuffix(e.Name(), ".sql") {
			return fmt.Errorf("migration %s: name must be NNNN_name.sql", e.Name())
		}
		ms = append(ms, mig{v, e.Name()})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })

	for _, m := range ms {
		sql, err := migrations.ReadFile("migrations/" + m.file)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockKey); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
				version int PRIMARY KEY,
				applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
				return err
			}
			var done bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", m.version).Scan(&done); err != nil {
				return err
			}
			if done {
				return nil
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", m.version)
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", m.file, err)
		}
	}
	return nil
}

// SchemaVersion returns the highest applied migration.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.pool.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&v)
	return v, err
}

// WithLock runs fn while holding the per app-env deploy lock, or returns
// ErrBusy immediately if someone else holds it. It is a session-level
// advisory lock on a dedicated connection rather than a transaction lock,
// because fn spans a node call of up to minutes and must not hold a
// transaction open; if the controller dies, the connection and lock go too.
func (s *Store) WithLock(ctx context.Context, app, env string, fn func(context.Context) error) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	key := "lwd/deploy/" + app + "/" + env
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", key).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrBusy
	}
	defer func() {
		uctx := context.WithoutCancel(ctx)
		if _, err := conn.Exec(uctx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", key); err != nil {
			// Could not unlock: kill the session so the lock cannot leak
			// into the pool with a reused connection.
			_ = conn.Conn().Close(uctx)
		}
	}()
	return fn(ctx)
}

// mapErr turns pgx/Postgres errors into the package's sentinel errors.
func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23503", "23514", "23505": // foreign key, check, unique
			return fmt.Errorf("%w: %s", ErrInvalid, pe.Detail+" "+pe.Message)
		}
	}
	return err
}
