package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lwd/internal/client"
	"lwd/internal/reconciler"
	"lwd/internal/store"
)

func TestApiNodesList(t *testing.T) {
	fd := newFakeDaemon()
	fd.nodes = []client.NodeStatus{
		{Node: store.Node{Name: "web1", SSHHost: "deploy@web1.example.com", MeshAddr: "100.64.0.2", AgentURL: "http://100.64.0.2:8078", Pool: "web"}, Transport: "agent", Reachable: true},
		{Node: store.Node{Name: "web2", SSHHost: "deploy@web2.example.com", MeshAddr: "100.64.0.3", Pool: "default"}, Transport: "ssh", Reachable: false},
	}
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodGet, "/api/nodes", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got []client.NodeStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Name != "web1" || got[0].Transport != "agent" || !got[0].Reachable || got[0].Pool != "web" {
		t.Errorf("unexpected node[0]: %+v", got[0])
	}
	if got[1].Name != "web2" || got[1].Transport != "ssh" || got[1].Reachable || got[1].Pool != "default" {
		t.Errorf("unexpected node[1]: %+v", got[1])
	}
}

// TestApiNodesListIncludesSchedulable covers that GET /api/nodes round-trips
// each node's cordon state (store.Node.Schedulable, embedded in
// client.NodeStatus) — the field the frontend renders as a
// schedulable/cordoned badge and gates the Drain/Evacuate/Uncordon buttons
// on.
func TestApiNodesListIncludesSchedulable(t *testing.T) {
	fd := newFakeDaemon()
	fd.nodes = []client.NodeStatus{
		{Node: store.Node{Name: "web1", Schedulable: true}, Transport: "agent", Reachable: true},
		{Node: store.Node{Name: "web2", Schedulable: false}, Transport: "ssh", Reachable: true},
	}
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodGet, "/api/nodes", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got []client.NodeStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body)
	}
	if len(got) != 2 || !got[0].Schedulable || got[1].Schedulable {
		t.Fatalf("got %+v", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"schedulable":true`) || !strings.Contains(body, `"schedulable":false`) {
		t.Errorf("body = %s, want it to contain both schedulable states", body)
	}
}

func TestApiNodeAdd(t *testing.T) {
	fd := newFakeDaemon()
	srv, auth := testServer(fd)

	body := `{"name":"web1","ssh_host":"deploy@web1.example.com","mesh_addr":"100.64.0.2","agent_url":"http://100.64.0.2:8078","pool":"web"}`
	req := authedRequest(t, auth, http.MethodPost, "/api/nodes", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := do(srv, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(fd.addedNodes) != 1 {
		t.Fatalf("expected AddNode called once, got %d", len(fd.addedNodes))
	}
	got := fd.addedNodes[0]
	if got.Name != "web1" || got.SSHHost != "deploy@web1.example.com" || got.MeshAddr != "100.64.0.2" || got.AgentURL != "http://100.64.0.2:8078" || got.Pool != "web" {
		t.Errorf("unexpected AddNode call: %+v", got)
	}
}

func TestApiNodeAddError(t *testing.T) {
	fd := newFakeDaemon()
	fd.addNodeErr = errString("boom")
	srv, auth := testServer(fd)

	body := `{"name":"web1","ssh_host":"h","mesh_addr":"m"}`
	req := authedRequest(t, auth, http.MethodPost, "/api/nodes", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := do(srv, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestApiNodeRemove(t *testing.T) {
	fd := newFakeDaemon()
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodDelete, "/api/nodes/web1", nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(fd.removedNodes) != 1 || fd.removedNodes[0] != "web1" {
		t.Fatalf("removedNodes = %+v", fd.removedNodes)
	}
}

func TestApiNodesRequireAuth(t *testing.T) {
	fd := newFakeDaemon()
	srv, _ := testServer(fd)

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/nodes", nil),
		httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader("{}")),
		httptest.NewRequest(http.MethodDelete, "/api/nodes/web1", nil),
	} {
		rec := do(srv, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: status = %d, body = %s", req.Method, req.URL.Path, rec.Code, rec.Body)
		}
	}
}

// TestApiNodeDrain covers POST /api/nodes/{name}/drain: it proxies
// client.Drain and renders the resulting reconciler.EvacuateResult
// (moved/skipped/failed), matching what `lwd node drain` prints.
func TestApiNodeDrain(t *testing.T) {
	fd := newFakeDaemon()
	fd.drainResult = reconciler.EvacuateResult{
		Moved:   []string{"blog"},
		Skipped: []string{"pinned"},
		Failed:  []reconciler.EvacFailure{{App: "shop", Err: "no capacity"}},
	}
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodPost, "/api/nodes/web1/drain", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got reconciler.EvacuateResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body)
	}
	if len(got.Moved) != 1 || got.Moved[0] != "blog" {
		t.Errorf("Moved = %v", got.Moved)
	}
	if len(got.Skipped) != 1 || got.Skipped[0] != "pinned" {
		t.Errorf("Skipped = %v", got.Skipped)
	}
	if len(got.Failed) != 1 || got.Failed[0].App != "shop" || got.Failed[0].Err != "no capacity" {
		t.Errorf("Failed = %v", got.Failed)
	}
	if len(fd.drainCalls) != 1 || fd.drainCalls[0] != "web1" {
		t.Fatalf("drainCalls = %v", fd.drainCalls)
	}
}

// TestApiNodeDrainNilSlicesNormalized covers that a zero-value
// EvacuateResult's nil Moved/Skipped/Failed are normalized to empty arrays,
// not `null`, matching handleApps/handleNodes/handleHealth/handlePools —
// letting the frontend call .length on them unconditionally.
func TestApiNodeDrainNilSlicesNormalized(t *testing.T) {
	fd := newFakeDaemon() // fd.drainResult is the zero value
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodPost, "/api/nodes/web1/drain", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`"moved":[]`, `"skipped":[]`, `"failed":[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %s, want it to contain %s", body, want)
		}
	}
}

func TestApiNodeDrainError(t *testing.T) {
	fd := newFakeDaemon()
	fd.drainErr = errString("boom")
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodPost, "/api/nodes/web1/drain", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

// TestApiNodeEvacuate covers POST /api/nodes/{name}/evacuate: it proxies
// client.Evacuate (drain minus the cordon) and renders the same
// reconciler.EvacuateResult shape as drain.
func TestApiNodeEvacuate(t *testing.T) {
	fd := newFakeDaemon()
	fd.evacuateResult = reconciler.EvacuateResult{Moved: []string{"blog"}}
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodPost, "/api/nodes/web1/evacuate", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got reconciler.EvacuateResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body)
	}
	if len(got.Moved) != 1 || got.Moved[0] != "blog" {
		t.Errorf("Moved = %v", got.Moved)
	}
	if len(fd.evacuateCalls) != 1 || fd.evacuateCalls[0] != "web1" {
		t.Fatalf("evacuateCalls = %v", fd.evacuateCalls)
	}
}

func TestApiNodeEvacuateError(t *testing.T) {
	fd := newFakeDaemon()
	fd.evacuateErr = errString("boom")
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodPost, "/api/nodes/web1/evacuate", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

// TestApiNodeUncordon covers POST /api/nodes/{name}/uncordon: it proxies
// client.Uncordon (clearing the cordon, touching nothing already deployed)
// and returns 204, matching handleNodeRemove/handleNodeAdd's shape for a
// mutation with no meaningful body.
func TestApiNodeUncordon(t *testing.T) {
	fd := newFakeDaemon()
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodPost, "/api/nodes/web1/uncordon", nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(fd.uncordonCalls) != 1 || fd.uncordonCalls[0] != "web1" {
		t.Fatalf("uncordonCalls = %v", fd.uncordonCalls)
	}
}

func TestApiNodeUncordonError(t *testing.T) {
	fd := newFakeDaemon()
	fd.uncordonErr = errString("boom")
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodPost, "/api/nodes/web1/uncordon", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

// TestApiNodeLifecycleRequireAuth covers that the three node-lifecycle
// routes, like every other /api route, 401 without a valid session cookie.
func TestApiNodeLifecycleRequireAuth(t *testing.T) {
	fd := newFakeDaemon()
	srv, _ := testServer(fd)

	for _, path := range []string{
		"/api/nodes/web1/drain",
		"/api/nodes/web1/evacuate",
		"/api/nodes/web1/uncordon",
	} {
		rec := do(srv, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("POST %s: status = %d, body = %s", path, rec.Code, rec.Body)
		}
	}
}
