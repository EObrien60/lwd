package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lwd/internal/client"
	"lwd/internal/store"
)

func TestApiNodesList(t *testing.T) {
	fd := newFakeDaemon()
	fd.nodes = []client.NodeStatus{
		{Node: store.Node{Name: "web1", SSHHost: "deploy@web1.example.com", MeshAddr: "100.64.0.2", AgentURL: "http://100.64.0.2:8078"}, Transport: "agent", Reachable: true},
		{Node: store.Node{Name: "web2", SSHHost: "deploy@web2.example.com", MeshAddr: "100.64.0.3"}, Transport: "ssh", Reachable: false},
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
	if got[0].Name != "web1" || got[0].Transport != "agent" || !got[0].Reachable {
		t.Errorf("unexpected node[0]: %+v", got[0])
	}
	if got[1].Name != "web2" || got[1].Transport != "ssh" || got[1].Reachable {
		t.Errorf("unexpected node[1]: %+v", got[1])
	}
}

func TestApiNodeAdd(t *testing.T) {
	fd := newFakeDaemon()
	srv, auth := testServer(fd)

	body := `{"name":"web1","ssh_host":"deploy@web1.example.com","mesh_addr":"100.64.0.2","agent_url":"http://100.64.0.2:8078"}`
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
	if got.Name != "web1" || got.SSHHost != "deploy@web1.example.com" || got.MeshAddr != "100.64.0.2" || got.AgentURL != "http://100.64.0.2:8078" {
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
