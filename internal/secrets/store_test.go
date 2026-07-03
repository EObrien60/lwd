package secrets

import (
	"path/filepath"
	"strings"
	"testing"

	"lwd/internal/store"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	c, err := NewCipher(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	db, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(c, db)
}

func TestSetGetList(t *testing.T) {
	s := newTestStore(t)
	if err := s.Set("blog", "DB", "postgres://x"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, ok, err := s.Get("blog", "DB")
	if err != nil || !ok || v != "postgres://x" {
		t.Fatalf("Get = %q,%v,%v", v, ok, err)
	}
	names, _ := s.List("blog")
	if len(names) != 1 || names[0] != "DB" {
		t.Fatalf("List = %v", names)
	}
}

func TestResolveAllPresent(t *testing.T) {
	s := newTestStore(t)
	s.Set("blog", "A", "1")
	s.Set("blog", "B", "2")
	got, err := s.Resolve("blog", []string{"A", "B"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["A"] != "1" || got["B"] != "2" {
		t.Fatalf("Resolve = %v", got)
	}
}

func TestResolveFailsClosedOnMissing(t *testing.T) {
	s := newTestStore(t)
	s.Set("blog", "A", "1")
	_, err := s.Resolve("blog", []string{"A", "MISSING"})
	if err == nil {
		t.Fatal("want error for missing secret")
	}
	if !strings.Contains(err.Error(), "MISSING") {
		t.Errorf("error should name the missing secret, got %v", err)
	}
}

func TestResolveEmptyNames(t *testing.T) {
	s := newTestStore(t)
	got, err := s.Resolve("blog", nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("Resolve(nil) = %v, %v", got, err)
	}
}
