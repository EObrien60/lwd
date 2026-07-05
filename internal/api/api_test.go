package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"lwd/internal/build"
	"lwd/internal/compose"
	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/router"
	"lwd/internal/secrets"
	"lwd/internal/source"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// fakeInvalidator is a NodeCacheInvalidator that records every name it was
// asked to invalidate, so tests can assert POST/DELETE /nodes trigger the
// resolver cache eviction without needing a real RegistryResolver.
type fakeInvalidator struct {
	mu    sync.Mutex
	Calls []string
}

func (f *fakeInvalidator) Invalidate(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, name)
}

// testSecretResolver builds a real (but throwaway) secrets.Store for tests
// that need a reconciler.SecretResolver — the reconciler's tests already
// cover a fake resolver's fail-closed behavior, so wiring the real store
// here exercises the actual integration.
func testSecretResolver(t *testing.T, s *store.Store, dir string) *secrets.Store {
	t.Helper()
	cipher, err := secrets.NewCipher(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("secrets.NewCipher: %v", err)
	}
	return secrets.NewStore(cipher, s)
}

func newTestServer(t *testing.T) (*httptest.Server, *node.Fake) {
	t.Helper()
	f := node.NewFake()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	rt := router.NewFakeRouter()
	secStore := testSecretResolver(t, s, dir)
	srv := New(reconciler.New(node.FakeResolver{"local": f}, rt, s, secStore, compose.NewFake(), source.NewFake(), build.NewFake()), s, f, rt, secStore, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, f
}

// newTestServerWithRouter is like newTestServer but also returns the
// FakeRouter, so tests can assert on route side effects (e.g. rm removing a
// route).
func newTestServerWithRouter(t *testing.T) (*httptest.Server, *node.Fake, *router.FakeRouter) {
	t.Helper()
	f := node.NewFake()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	rt := router.NewFakeRouter()
	secStore := testSecretResolver(t, s, dir)
	srv := New(reconciler.New(node.FakeResolver{"local": f}, rt, s, secStore, compose.NewFake(), source.NewFake(), build.NewFake()), s, f, rt, secStore, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, f, rt
}

// newTestServerWithCompose is like newTestServer but also returns the
// FakeRouter and the compose.Fake, so tests can drive a compose app through
// the API and assert on `docker compose` side effects (e.g. delete calling
// Down).
func newTestServerWithCompose(t *testing.T) (*httptest.Server, *router.FakeRouter, *compose.Fake) {
	t.Helper()
	f := node.NewFake()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	rt := router.NewFakeRouter()
	cf := compose.NewFake()
	secStore := testSecretResolver(t, s, dir)
	srv := New(reconciler.New(node.FakeResolver{"local": f}, rt, s, secStore, cf, source.NewFake(), build.NewFake()), s, f, rt, secStore, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, rt, cf
}

func TestApplyEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	body, _ := json.Marshal(spec.App{Name: "blog", Image: "img:1", Port: 8080, Node: "local"})
	resp, err := http.Post(ts.URL+"/apply", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /apply: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
	var dep store.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&dep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dep.Status != store.StatusRunning {
		t.Errorf("status = %q, want running", dep.Status)
	}
}

func TestApplyEndpointRejectsBadSpec(t *testing.T) {
	ts, _ := newTestServer(t)
	body, _ := json.Marshal(spec.App{Name: "blog"}) // no image/port
	resp, err := http.Post(ts.URL+"/apply", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /apply: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAppsEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	body, _ := json.Marshal(spec.App{Name: "blog", Image: "img:1", Port: 8080, Node: "local"})
	http.Post(ts.URL+"/apply", "application/json", bytes.NewReader(body))

	resp, err := http.Get(ts.URL + "/apps")
	if err != nil {
		t.Fatalf("GET /apps: %v", err)
	}
	defer resp.Body.Close()
	var apps []AppStatus
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "blog" || apps[0].Status != store.StatusRunning {
		t.Fatalf("apps = %+v", apps)
	}
}

func TestRollbackEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	body1, _ := json.Marshal(spec.App{Name: "blog", Image: "img:a", Port: 8080, Node: "local"})
	resp, err := http.Post(ts.URL+"/apply", "application/json", bytes.NewReader(body1))
	if err != nil {
		t.Fatalf("POST /apply v1: %v", err)
	}
	resp.Body.Close()

	body2, _ := json.Marshal(spec.App{Name: "blog", Image: "img:b", Port: 8080, Node: "local"})
	resp, err = http.Post(ts.URL+"/apply", "application/json", bytes.NewReader(body2))
	if err != nil {
		t.Fatalf("POST /apply v2: %v", err)
	}
	resp.Body.Close()

	resp, err = http.Post(ts.URL+"/apps/blog/rollback", "application/json", nil)
	if err != nil {
		t.Fatalf("POST rollback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
	var dep store.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&dep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dep.Image != "img:a" {
		t.Errorf("rollback image = %q, want img:a", dep.Image)
	}

	resp2, err := http.Post(ts.URL+"/apps/unknown/rollback", "application/json", nil)
	if err != nil {
		t.Fatalf("POST rollback unknown: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp2.StatusCode)
	}
}

func TestHistoryEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	body1, _ := json.Marshal(spec.App{Name: "blog", Image: "img:a", Port: 8080, Node: "local"})
	resp, err := http.Post(ts.URL+"/apply", "application/json", bytes.NewReader(body1))
	if err != nil {
		t.Fatalf("POST /apply v1: %v", err)
	}
	resp.Body.Close()

	body2, _ := json.Marshal(spec.App{Name: "blog", Image: "img:b", Port: 8080, Node: "local"})
	resp, err = http.Post(ts.URL+"/apply", "application/json", bytes.NewReader(body2))
	if err != nil {
		t.Fatalf("POST /apply v2: %v", err)
	}
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/apps/blog/history")
	if err != nil {
		t.Fatalf("GET /apps/blog/history: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
	var deps []store.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&deps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(deps) < 2 {
		t.Fatalf("history = %+v, want >= 2 entries", deps)
	}
	if deps[0].Image != "img:b" {
		t.Errorf("deps[0].Image = %q, want img:b (newest first)", deps[0].Image)
	}
	if deps[1].Image != "img:a" {
		t.Errorf("deps[1].Image = %q, want img:a", deps[1].Image)
	}
	for _, d := range deps {
		if d.App != "blog" {
			t.Errorf("deployment app = %q, want blog", d.App)
		}
	}
}

func TestAppsIncludesDomain(t *testing.T) {
	ts, _ := newTestServer(t)
	body, _ := json.Marshal(spec.App{Name: "blog", Image: "img:1", Port: 8080, Node: "local", Domain: "blog.example.com"})
	resp, err := http.Post(ts.URL+"/apply", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /apply: %v", err)
	}
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/apps")
	if err != nil {
		t.Fatalf("GET /apps: %v", err)
	}
	defer resp.Body.Close()
	var apps []AppStatus
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(apps) != 1 || apps[0].Domain != "blog.example.com" {
		t.Fatalf("apps = %+v, want domain blog.example.com", apps)
	}
}

func TestDeleteEndpoint(t *testing.T) {
	ts, f := newTestServer(t)
	body, _ := json.Marshal(spec.App{Name: "blog", Image: "img:1", Port: 8080, Node: "local"})
	http.Post(ts.URL+"/apply", "application/json", bytes.NewReader(body))

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/apps/blog", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	// side effect: containers for the app removed from the node
	got, _ := f.ListContainers(context.Background(), map[string]string{"lwd.app": "blog"})
	if len(got) != 0 {
		t.Fatalf("want no containers after delete, got %+v", got)
	}
	// side effect: deployment retired -> /apps reports it no longer running
	resp2, err := http.Get(ts.URL + "/apps")
	if err != nil {
		t.Fatalf("GET /apps: %v", err)
	}
	defer resp2.Body.Close()
	var apps []AppStatus
	json.NewDecoder(resp2.Body).Decode(&apps)
	for _, a := range apps {
		if a.Name == "blog" && a.Status == store.StatusRunning {
			t.Fatalf("blog still running after delete: %+v", a)
		}
	}
}

func TestDeleteEndpointRemovesRoute(t *testing.T) {
	ts, _, rt := newTestServerWithRouter(t)
	body, _ := json.Marshal(spec.App{Name: "blog", Image: "img:1", Port: 8080, Node: "local", Domain: "blog.example.com"})
	resp, err := http.Post(ts.URL+"/apply", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /apply: %v", err)
	}
	resp.Body.Close()

	if _, ok := rt.Routes["blog.example.com"]; !ok {
		t.Fatalf("expected route for blog.example.com to be set after apply, got %+v", rt.Routes)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/apps/blog", nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != 204 {
		t.Fatalf("status = %d, want 204", delResp.StatusCode)
	}

	if _, ok := rt.Routes["blog.example.com"]; ok {
		t.Fatalf("expected route for blog.example.com to be removed after rm, got %+v", rt.Routes)
	}
}

func TestDeleteComposeAppCallsComposeDown(t *testing.T) {
	ts, rt, cf := newTestServerWithCompose(t)

	composeDir := t.TempDir()
	composePath := filepath.Join(composeDir, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte("services:\n  web:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	cf.ServiceID = "cid-1"
	cf.ServiceName = "lwd-webapp-web-1"
	rt.ProbeStatus = 200

	app := spec.App{
		Name:    "webapp",
		Compose: composePath,
		Service: "web",
		Domain:  "webapp.example.com",
		Port:    8080,
		Node:    "local",
	}
	body, _ := json.Marshal(app)
	resp, err := http.Post(ts.URL+"/apply", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /apply: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("apply status = %d, body = %s", resp.StatusCode, b)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/apps/webapp", nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != 204 {
		b, _ := io.ReadAll(delResp.Body)
		t.Fatalf("status = %d, want 204, body = %s", delResp.StatusCode, b)
	}

	var sawDown bool
	for _, c := range cf.Calls {
		if c == "Down:lwd-webapp" {
			sawDown = true
		}
	}
	if !sawDown {
		t.Fatalf("want compose Down call, calls: %v", cf.Calls)
	}
	if _, ok := rt.Routes["webapp.example.com"]; ok {
		t.Fatalf("want route removed after compose delete, routes: %+v", rt.Routes)
	}
}

func TestSecretSetAndList(t *testing.T) {
	ts, _ := newTestServer(t)

	body, _ := json.Marshal(map[string]string{"key": "DB", "value": "pg://x"})
	resp, err := http.Post(ts.URL+"/apps/blog/secrets", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST secrets: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/apps/blog/secrets")
	if err != nil {
		t.Fatalf("GET secrets: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(b), "pg://x") {
		t.Fatalf("response body leaked secret value: %s", b)
	}
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(names) != 1 || names[0] != "DB" {
		t.Fatalf("names = %+v, want [DB]", names)
	}
}

func TestSecretDelete(t *testing.T) {
	ts, _ := newTestServer(t)

	body, _ := json.Marshal(map[string]string{"key": "DB", "value": "pg://x"})
	resp, err := http.Post(ts.URL+"/apps/blog/secrets", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST secrets: %v", err)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/apps/blog/secrets/DB", nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE secrets: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", delResp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/apps/blog/secrets")
	if err != nil {
		t.Fatalf("GET secrets: %v", err)
	}
	defer resp.Body.Close()
	var names []string
	if err := json.NewDecoder(resp.Body).Decode(&names); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("names = %+v, want empty", names)
	}
}

func TestSecretSetMissingKey(t *testing.T) {
	ts, _ := newTestServer(t)

	body, _ := json.Marshal(map[string]string{"key": "", "value": "x"})
	resp, err := http.Post(ts.URL+"/apps/blog/secrets", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST secrets: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// newTestServerWithInvalidator is like newTestServer but wires a
// fakeInvalidator as the Server's NodeCacheInvalidator, so node tests can
// assert POST/DELETE /nodes trigger cache eviction.
func newTestServerWithInvalidator(t *testing.T) (*httptest.Server, *fakeInvalidator) {
	t.Helper()
	f := node.NewFake()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	rt := router.NewFakeRouter()
	secStore := testSecretResolver(t, s, dir)
	inv := &fakeInvalidator{}
	srv := New(reconciler.New(node.FakeResolver{"local": f}, rt, s, secStore, compose.NewFake(), source.NewFake(), build.NewFake()), s, f, rt, secStore, inv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, inv
}

func TestNodeAddListRemove(t *testing.T) {
	ts, inv := newTestServerWithInvalidator(t)

	body, _ := json.Marshal(map[string]string{"name": "web1", "ssh_host": "deploy@web1", "mesh_addr": "100.64.0.2"})
	resp, err := http.Post(ts.URL+"/nodes", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /nodes: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/nodes")
	if err != nil {
		t.Fatalf("GET /nodes: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var nodes []store.Node
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "web1" || nodes[0].SSHHost != "deploy@web1" || nodes[0].MeshAddr != "100.64.0.2" {
		t.Fatalf("nodes = %+v", nodes)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/nodes/web1", nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /nodes/web1: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", delResp.StatusCode)
	}

	resp2, err := http.Get(ts.URL + "/nodes")
	if err != nil {
		t.Fatalf("GET /nodes after delete: %v", err)
	}
	defer resp2.Body.Close()
	var nodes2 []store.Node
	if err := json.NewDecoder(resp2.Body).Decode(&nodes2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(nodes2) != 0 {
		t.Fatalf("nodes after delete = %+v, want empty", nodes2)
	}

	inv.mu.Lock()
	calls := append([]string(nil), inv.Calls...)
	inv.mu.Unlock()
	if len(calls) != 2 || calls[0] != "web1" || calls[1] != "web1" {
		t.Fatalf("invalidator calls = %+v, want [web1 web1] (POST then DELETE)", calls)
	}
}

func TestNodeAddValidatesFields(t *testing.T) {
	ts, inv := newTestServerWithInvalidator(t)

	cases := []map[string]string{
		{"name": "", "ssh_host": "deploy@web1", "mesh_addr": "100.64.0.2"},
		{"name": "web1", "ssh_host": "", "mesh_addr": "100.64.0.2"},
		{"name": "web1", "ssh_host": "deploy@web1", "mesh_addr": ""},
	}
	for _, c := range cases {
		body, _ := json.Marshal(c)
		resp, err := http.Post(ts.URL+"/nodes", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST /nodes %+v: %v", c, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("case %+v: status = %d, want 400", c, resp.StatusCode)
		}
	}

	inv.mu.Lock()
	calls := len(inv.Calls)
	inv.mu.Unlock()
	if calls != 0 {
		t.Fatalf("invalidator called %d times for rejected requests, want 0", calls)
	}
}

// TestNodeAddRejectsReservedName covers the Phase 9a final-review fix:
// "local" is the implicit, always-present local node (node.Resolver treats
// "" and "local" as the local Docker daemon), so registering a remote node
// under that name would either be silently inert or shadow the real local
// node in confusing ways. handleNodeAdd must reject it (and an empty name)
// with a 400, without touching the store or the cache invalidator.
func TestNodeAddRejectsReservedName(t *testing.T) {
	ts, inv := newTestServerWithInvalidator(t)

	cases := []map[string]string{
		{"name": "local", "ssh_host": "deploy@web1", "mesh_addr": "100.64.0.2"},
		{"name": "", "ssh_host": "deploy@web1", "mesh_addr": "100.64.0.2"},
	}
	for _, c := range cases {
		body, _ := json.Marshal(c)
		resp, err := http.Post(ts.URL+"/nodes", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST /nodes %+v: %v", c, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("case %+v: status = %d, want 400", c, resp.StatusCode)
		}
	}

	resp, err := http.Get(ts.URL + "/nodes")
	if err != nil {
		t.Fatalf("GET /nodes: %v", err)
	}
	defer resp.Body.Close()
	var nodes []store.Node
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(nodes) != 0 {
		t.Fatalf("nodes = %+v, want none registered for a rejected name", nodes)
	}

	inv.mu.Lock()
	calls := len(inv.Calls)
	inv.mu.Unlock()
	if calls != 0 {
		t.Fatalf("invalidator called %d times for rejected requests, want 0", calls)
	}
}

// TestNodeAddValidatesMeshAddrShape covers the Phase 9a final-review fix:
// mesh_addr must look like a plausible IP address or hostname. A valid IP
// and a valid hostname are both accepted; whitespace-containing garbage and
// a value carrying a URL scheme are rejected with a 400 (these would never
// resolve to a working docker-over-ssh host or Caddy upstream).
func TestNodeAddValidatesMeshAddrShape(t *testing.T) {
	ts, inv := newTestServerWithInvalidator(t)

	accepted := []map[string]string{
		{"name": "web1", "ssh_host": "deploy@web1", "mesh_addr": "100.64.0.2"},
		{"name": "web2", "ssh_host": "deploy@web2", "mesh_addr": "web2.internal"},
	}
	for _, c := range accepted {
		body, _ := json.Marshal(c)
		resp, err := http.Post(ts.URL+"/nodes", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST /nodes %+v: %v", c, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("case %+v: status = %d, want 204", c, resp.StatusCode)
		}
	}

	rejected := []map[string]string{
		{"name": "web3", "ssh_host": "deploy@web3", "mesh_addr": "a b"},
		{"name": "web4", "ssh_host": "deploy@web4", "mesh_addr": "http://x"},
	}
	for _, c := range rejected {
		body, _ := json.Marshal(c)
		resp, err := http.Post(ts.URL+"/nodes", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST /nodes %+v: %v", c, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("case %+v: status = %d, want 400", c, resp.StatusCode)
		}
	}

	inv.mu.Lock()
	calls := append([]string(nil), inv.Calls...)
	inv.mu.Unlock()
	if len(calls) != 2 || calls[0] != "web1" || calls[1] != "web2" {
		t.Fatalf("invalidator calls = %+v, want [web1 web2] (only the two accepted adds)", calls)
	}
}
