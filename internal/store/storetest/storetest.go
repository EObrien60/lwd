// Package storetest gives tests a migrated store on a throwaway Postgres
// database. It uses LWD_TEST_DATABASE_URL when set, otherwise the local
// podman container documented in the README of the tests
// (postgres://postgres:test@127.0.0.1:55432/postgres). When no server answers,
// the calling test is skipped rather than failed, so `go test ./...` works on
// machines without Postgres.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"lwd/internal/store"
)

// DefaultURL is the podman test container:
//
//	podman run -d --rm --name lwd-test-pg -e POSTGRES_PASSWORD=test \
//	  -p 127.0.0.1:55432:5432 docker.io/library/postgres:17-alpine
const DefaultURL = "postgres://postgres:test@127.0.0.1:55432/postgres?sslmode=disable"

// URL creates a fresh database for t, dropped at cleanup, and returns its URL.
func URL(t testing.TB) string {
	t.Helper()
	admin := os.Getenv("LWD_TEST_DATABASE_URL")
	if admin == "" {
		admin = DefaultURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Skipf("no test Postgres at %s: %v", redact(admin), err)
	}
	defer conn.Close(context.Background())

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	db := "lwd_test_" + hex.EncodeToString(b)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Logf("drop %s: %v", db, err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)"); err != nil {
			t.Logf("drop %s: %v", db, err)
		}
	})

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parse %s: %v", redact(admin), err)
	}
	u.Path = "/" + db
	return u.String()
}

// New returns an open, migrated store on a fresh database.
func New(t testing.TB) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, URL(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable url)"
	}
	return u.Redacted()
}
