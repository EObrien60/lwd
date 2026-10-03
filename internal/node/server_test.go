package node

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"lwd/internal/bundle"
)

func (h *harness) do(method, path, token string, body any) (*http.Response, []byte) {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		data, _ := json.Marshal(b)
		rd = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.n.Handler().ServeHTTP(rec, req)
	return rec.Result(), rec.Body.Bytes()
}

func errCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error.Message == "" {
		t.Fatalf("not an error envelope: %s", body)
	}
	return e.Error.Code
}

func TestHealthIsUnauthenticated(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do("GET", "/v1/health", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"ok":true`) || !strings.Contains(string(body), `"version"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestAuthRequired(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/status"},
		{"POST", "/v1/deploy"},
		{"POST", "/v1/apps/hello/staging/restart"},
		{"GET", "/v1/apps/hello/staging/logs"},
		{"DELETE", "/v1/apps/hello/staging"},
	} {
		for _, tok := range []string{"", "wrong", "secretX"} {
			resp, body := h.do(tc.method, tc.path, tok, nil)
			if resp.StatusCode != http.StatusUnauthorized || errCode(t, body) != "unauthorized" {
				t.Errorf("%s %s token=%q: %d %s", tc.method, tc.path, tok, resp.StatusCode, body)
			}
		}
	}
	if len(h.docker.recorded()) != 0 {
		t.Error("unauthenticated request reached docker")
	}
}

func TestDeployEndpoint(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do("POST", "/v1/deploy", "secret", bundleN(1))
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var res bundle.Result
	if err := json.Unmarshal(body, &res); err != nil || res.Status != bundle.StatusSucceeded {
		t.Fatalf("%v %s", err, body)
	}
}

func TestDeployEndpointRejectsInvalid(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do("POST", "/v1/deploy", "secret", "{not json")
	if resp.StatusCode != 400 || errCode(t, body) != "invalid" {
		t.Errorf("bad json: %d %s", resp.StatusCode, body)
	}
	b := bundleN(1)
	b.Services[0].Image = "nginx:latest"
	resp, body = h.do("POST", "/v1/deploy", "secret", b)
	if resp.StatusCode != 400 || errCode(t, body) != "invalid" || !strings.Contains(string(body), "digest") {
		t.Errorf("validate: %d %s", resp.StatusCode, body)
	}
	if len(h.docker.recorded()) != 0 {
		t.Error("invalid bundle reached docker")
	}
}

func TestDeployEndpointBusy(t *testing.T) {
	h := newHarness(t)
	h.deploy(bundleN(1))
	h.n.tryLock("hello-staging")
	resp, body := h.do("POST", "/v1/deploy", "secret", bundleN(2))
	if resp.StatusCode != http.StatusConflict || errCode(t, body) != "conflict" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	// Other app-envs are not blocked.
	other := bundleN(1)
	other.Env = "prod"
	other.Services[0].Domains = []string{"hello.example.com"}
	other.Services[1].Domains = []string{"api.hello.example.com"}
	if resp, body := h.do("POST", "/v1/deploy", "secret", other); resp.StatusCode != 200 {
		t.Fatalf("other env: %d %s", resp.StatusCode, body)
	}
	// Restart and delete share the app-env lock; reading logs does not.
	for _, m := range [][2]string{{"POST", "/v1/apps/hello/staging/restart"}, {"DELETE", "/v1/apps/hello/staging"}} {
		resp, body := h.do(m[0], m[1], "secret", nil)
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("%s %s: %d %s", m[0], m[1], resp.StatusCode, body)
		}
	}
}

func TestRestartLogsDeleteNeedLiveDeployment(t *testing.T) {
	h := newHarness(t)
	for _, m := range [][2]string{{"POST", "/v1/apps/hello/staging/restart"}, {"GET", "/v1/apps/hello/staging/logs"}, {"DELETE", "/v1/apps/hello/staging"}} {
		resp, body := h.do(m[0], m[1], "secret", nil)
		if resp.StatusCode != 404 || errCode(t, body) != "not_found" {
			t.Errorf("%s %s: %d %s", m[0], m[1], resp.StatusCode, body)
		}
	}
}

func TestRestartEndpoint(t *testing.T) {
	h := newHarness(t)
	h.deploy(bundleN(1))
	resp, body := h.do("POST", "/v1/apps/hello/staging/restart?service=api", "secret", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	calls := h.docker.recorded()
	if last := calls[len(calls)-1]; last != "lwd-hello-staging-d1 restart api" {
		t.Errorf("last call = %q", last)
	}
	resp, _ = h.do("POST", "/v1/apps/hello/staging/restart", "secret", nil)
	calls = h.docker.recorded()
	if resp.StatusCode != 200 || calls[len(calls)-1] != "lwd-hello-staging-d1 restart " {
		t.Errorf("restart all: %d %q", resp.StatusCode, calls[len(calls)-1])
	}
	resp, body = h.do("POST", "/v1/apps/hello/staging/restart?service=../x", "secret", nil)
	if resp.StatusCode != 400 {
		t.Errorf("bad service: %d %s", resp.StatusCode, body)
	}
}

func TestLogsEndpoint(t *testing.T) {
	h := newHarness(t)
	h.deploy(bundleN(1))
	var got []string
	h.docker.respond = func(c call) (string, error, bool) {
		if c.Verb == "logs" {
			got = c.Rest
			return "web-1 | hello\n", nil, true
		}
		return "", nil, false
	}
	resp, body := h.do("GET", "/v1/apps/hello/staging/logs?service=web&tail=50", "secret", nil)
	if resp.StatusCode != 200 || string(body) != "web-1 | hello\n" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("%d %q %s", resp.StatusCode, body, resp.Header.Get("Content-Type"))
	}
	if !reflect.DeepEqual(got, []string{"--no-color", "--tail", "50", "web"}) {
		t.Errorf("args = %v", got)
	}
	h.do("GET", "/v1/apps/hello/staging/logs", "secret", nil)
	if !reflect.DeepEqual(got, []string{"--no-color", "--tail", "100"}) {
		t.Errorf("default args = %v", got)
	}
	for _, q := range []string{"tail=abc", "tail=0", "tail=999999", "service=A"} {
		if resp, _ := h.do("GET", "/v1/apps/hello/staging/logs?"+q, "secret", nil); resp.StatusCode != 400 {
			t.Errorf("%s: %d", q, resp.StatusCode)
		}
	}
}

func TestDeleteEndpoint(t *testing.T) {
	h := newHarness(t)
	h.deploy(bundleN(1))
	resp, body := h.do("DELETE", "/v1/apps/hello/staging", "secret", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	calls := h.docker.recorded()
	if calls[len(calls)-1] != "lwd-hello-staging-d1 down " {
		t.Errorf("last call = %q", calls[len(calls)-1])
	}
	if strings.Contains(h.caddy.last(), "hello.lwd.internal") {
		t.Errorf("routes remain:\n%s", h.caddy.last())
	}
	if _, err := os.Stat(filepath.Join(h.dir, "apps", "hello-staging")); !os.IsNotExist(err) {
		t.Errorf("state dir remains: %v", err)
	}
	if resp, _ := h.do("DELETE", "/v1/apps/hello/staging", "secret", nil); resp.StatusCode != 404 {
		t.Errorf("second delete: %d", resp.StatusCode)
	}
}

func TestStatusEndpoint(t *testing.T) {
	h := newHarness(t)
	proc := t.TempDir()
	os.WriteFile(filepath.Join(proc, "loadavg"), []byte("0.50 0.25 0.10 1/100 12345\n"), 0o644)
	os.WriteFile(filepath.Join(proc, "meminfo"), []byte("MemTotal:        2048000 kB\nMemFree:          100 kB\nMemAvailable:    1024000 kB\n"), 0o644)
	h.n.procDir = proc
	h.deploy(bundleN(1))
	h.docker.respond = func(c call) (string, error, bool) {
		if c.Verb == "ps" && c.Project == systemProject {
			return `{"Service":"caddy","State":"running","Status":"Up 1 hour"}`, nil, true
		}
		if c.Verb == "ps" {
			return `[{"Service":"web","State":"running","Health":"healthy"},{"Service":"worker","State":"restarting"}]`, nil, true
		}
		return "", nil, false
	}
	resp, body := h.do("GET", "/v1/status", "secret", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var st NodeStatus
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.Hostname == "" || st.Version == "" || st.Load != [3]float64{0.5, 0.25, 0.1} ||
		st.Memory.TotalBytes != 2048000*1024 || st.Memory.AvailableBytes != 1024000*1024 || st.Disk.TotalBytes == 0 {
		t.Errorf("host facts = %+v", st)
	}
	if st.Caddy.State != "running" {
		t.Errorf("caddy = %+v", st.Caddy)
	}
	if len(st.Apps) != 1 {
		t.Fatalf("apps = %+v", st.Apps)
	}
	a := st.Apps[0]
	if a.App != "hello" || a.Env != "staging" || a.Live == nil || a.Live.Deployment != 1 || a.Live.Release != 7 {
		t.Errorf("app = %+v", a)
	}
	if len(a.Containers) != 2 || a.Containers[0].Health != "healthy" || a.Containers[1].State != "restarting" {
		t.Errorf("containers = %+v", a.Containers)
	}
}

func TestErrorEnvelopeForUnknownRoute(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do("GET", "/v1/nope", "secret", nil)
	if resp.StatusCode != 404 || errCode(t, body) != "not_found" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}
