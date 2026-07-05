package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"lwd/internal/client"
)

// TestApiPoolsList covers GET /api/pools: it proxies the daemon's pool list
// (name + node count) as-is, matching what `lwd pool ls` renders.
func TestApiPoolsList(t *testing.T) {
	fd := newFakeDaemon()
	fd.pools = []client.Pool{
		{Name: "default", Nodes: 1},
		{Name: "web", Nodes: 2},
	}
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodGet, "/api/pools", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got []client.Pool
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body)
	}
	if len(got) != 2 || got[0].Name != "default" || got[0].Nodes != 1 || got[1].Name != "web" || got[1].Nodes != 2 {
		t.Fatalf("got %+v", got)
	}
}

// TestApiPoolsNilNormalized covers that a nil pool list (e.g. a daemon-side
// error the fake doesn't otherwise simulate) is normalized to an empty array,
// not `null`, matching handleApps/handleNodes/handleHealth.
func TestApiPoolsNilNormalized(t *testing.T) {
	fd := newFakeDaemon() // fd.pools is nil
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodGet, "/api/pools", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if rec.Body.String() != "[]\n" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "[]\n")
	}
}

// TestApiPoolsError covers a daemon-side error surfacing as a 500, mirroring
// the other /api proxy handlers' error handling.
func TestApiPoolsError(t *testing.T) {
	fd := newFakeDaemon()
	fd.poolsErr = errString("boom")
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodGet, "/api/pools", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

// TestApiPoolsRequireAuth covers that GET /api/pools, like every other /api
// route, 401s without a valid session cookie.
func TestApiPoolsRequireAuth(t *testing.T) {
	fd := newFakeDaemon()
	srv, _ := testServer(fd)

	rec := do(srv, httptest.NewRequest(http.MethodGet, "/api/pools", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}
