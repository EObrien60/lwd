package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/router"
	"lwd/internal/spec"
	"lwd/internal/store"
)

func newTestServer(t *testing.T) (*httptest.Server, *node.Fake) {
	t.Helper()
	f := node.NewFake()
	s, err := store.Open(filepath.Join(t.TempDir(), "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	srv := New(reconciler.New(f, router.NewFakeRouter(), s), s, f)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, f
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
