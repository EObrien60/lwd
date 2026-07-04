package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApiLogsSSE(t *testing.T) {
	fd := newFakeDaemon()
	fd.logsData = "line1\nline2\n"
	srv, auth := testServer(fd)

	req := authedRequest(t, auth, http.MethodGet, "/api/apps/blog/logs", nil)
	rec := do(srv, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: line1\n\n") {
		t.Fatalf("body missing data: line1 frame: %q", body)
	}
	if !strings.Contains(body, "data: line2\n\n") {
		t.Fatalf("body missing data: line2 frame: %q", body)
	}
}

func TestApiLogsRequiresAuth(t *testing.T) {
	fd := newFakeDaemon()
	srv, _ := testServer(fd)

	req := httptest.NewRequest(http.MethodGet, "/api/apps/blog/logs", nil)
	rec := do(srv, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}
