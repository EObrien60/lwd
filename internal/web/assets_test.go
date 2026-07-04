package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStaticAssetsWiring exercises the embedded-asset routes end to end
// through Server.Handler (auth middleware included): the app shell and login
// page are public and serve their placeholder markers, /static/ files are
// public, and the JSON API remains gated by the session middleware.
func TestStaticAssetsWiring(t *testing.T) {
	auth := NewAuthenticator([]byte("test-signing-key"), "hunter2")
	srv := NewServer(newFakeDaemon(), auth)
	handler := srv.Handler()

	// GET / -> 200, app-shell marker, no session required.
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "lwd-web app shell") {
		t.Fatalf("GET / body missing app-shell marker: %s", rec.Body.String())
	}

	// GET /login -> 200, login marker, no session required.
	req = httptest.NewRequest("GET", "/login", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /login = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "lwd-web login") {
		t.Fatalf("GET /login body missing login marker: %s", rec.Body.String())
	}

	// GET /static/app.css -> 200, public.
	req = httptest.NewRequest("GET", "/static/app.css", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /static/app.css = %d, want 200", rec.Code)
	}

	// GET /api/apps with no cookie -> still 401; auth is intact.
	req = httptest.NewRequest("GET", "/api/apps", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("GET /api/apps (no cookie) = %d, want 401", rec.Code)
	}
}
