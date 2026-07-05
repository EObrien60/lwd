package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lwd/internal/node"
	"lwd/internal/reconciler"
)

// TestApiHealth covers GET /api/health: it proxies the daemon's reconciler
// health snapshot as-is (nodes, edge, apps), matching what `lwd health` and
// client.Health render.
func TestApiHealth(t *testing.T) {
	fd := newFakeDaemon()
	now := time.Now().UTC().Truncate(time.Second)
	fd.health = reconciler.Health{
		Nodes: []reconciler.NodeHealth{
			{Name: "local", Transport: "local", Reachable: true, UpdatedAt: now, Capacity: node.Capacity{
				CPUCores: 8, CPUUsed: 1.5, MemTotal: 16 << 30, MemAvailable: 10 << 30,
				DiskTotal: 200 << 30, DiskFree: 120 << 30, Known: true,
			}},
			{Name: "web1", Transport: "agent", Reachable: false, UpdatedAt: now},
		},
		Edge: reconciler.EdgeHealth{Reachable: true, UpdatedAt: now},
		Apps: []reconciler.AppHealth{
			{App: "blog", State: reconciler.SurfaceHealthy, HealAttempts: 0, UpdatedAt: now},
			{App: "shop", State: reconciler.SurfaceFailed, LastError: "gave up after max heal attempts", HealAttempts: 5, UpdatedAt: now},
		},
	}
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodGet, "/api/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got reconciler.Health
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body)
	}
	if len(got.Nodes) != 2 || got.Nodes[1].Name != "web1" || got.Nodes[1].Reachable {
		t.Errorf("unexpected Nodes: %+v", got.Nodes)
	}
	// Capacity (Phase 11a Task 8: surfaced in the web Health panel) must
	// round-trip through the /api/health proxy exactly as the daemon reported
	// it — this is the field the frontend renders as CPU/MEM/DISK bars.
	gotCap := got.Nodes[0].Capacity
	if !gotCap.Known || gotCap.CPUCores != 8 || gotCap.CPUUsed != 1.5 || gotCap.MemTotal != 16<<30 || gotCap.MemAvailable != 10<<30 || gotCap.DiskTotal != 200<<30 || gotCap.DiskFree != 120<<30 {
		t.Errorf("unexpected Capacity for local: %+v", gotCap)
	}
	if got.Nodes[1].Capacity.Known {
		t.Errorf("unexpected Capacity for web1 (no probe succeeded): %+v", got.Nodes[1].Capacity)
	}
	if !got.Edge.Reachable {
		t.Errorf("unexpected Edge: %+v", got.Edge)
	}
	if len(got.Apps) != 2 || got.Apps[1].App != "shop" || got.Apps[1].State != reconciler.SurfaceFailed || got.Apps[1].HealAttempts != 5 {
		t.Errorf("unexpected Apps: %+v", got.Apps)
	}
}

// TestApiHealthError covers a daemon-side error (e.g. an internal failure
// building the snapshot) surfacing as a 500, mirroring the other /api proxy
// handlers' error handling.
func TestApiHealthError(t *testing.T) {
	fd := newFakeDaemon()
	fd.healthErr = errString("boom")
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodGet, "/api/health", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

// TestApiHealthNilSlicesNormalized covers that a zero-value Health (Nodes and
// Apps both nil, as returned before the reconciler's first pass) is
// normalized to empty arrays in the JSON response, not `null` — matching
// handleApps/handleNodes, and letting the frontend call .length/.filter on
// them unconditionally.
func TestApiHealthNilSlicesNormalized(t *testing.T) {
	fd := newFakeDaemon() // fd.health is the zero value: Nodes == nil, Apps == nil
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodGet, "/api/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if want := `"nodes":[]`; !strings.Contains(body, want) {
		t.Errorf("body = %s, want it to contain %s", body, want)
	}
	if want := `"apps":[]`; !strings.Contains(body, want) {
		t.Errorf("body = %s, want it to contain %s", body, want)
	}
}

// TestApiHealthRequiresAuth covers that GET /api/health, like every other
// /api route, 401s without a valid session cookie.
func TestApiHealthRequiresAuth(t *testing.T) {
	fd := newFakeDaemon()
	srv, _ := testServer(fd)

	rec := do(srv, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}
