package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type request struct {
	Method, Path, Query, Body, Actor string
}

// fakeAPI answers /v1 routes with canned bodies keyed "METHOD /path".
type fakeAPI struct {
	mu        sync.Mutex
	requests  []request
	responses map[string]string
	status    map[string]int
	srv       *httptest.Server
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{responses: map[string]string{}, status: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"code":"unauthorized","message":"bad token"}}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, request{r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Get("X-LWD-Actor")})
		key := r.Method + " " + r.URL.Path
		body, ok := f.responses[key]
		code := f.status[key]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"no fake for `+key+`"}}`)
			return
		}
		if code != 0 {
			w.WriteHeader(code)
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) last(t *testing.T) request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no request made")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

type result struct {
	code           int
	stdout, stderr string
}

func (f *fakeAPI) run(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	env := map[string]string{"LWD_URL": f.srv.URL, "LWD_TOKEN": "tok", "USER": "ethan", "HOME": t.TempDir()}
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb, func(k string) string { return env[k] })
	return result{code, out.String(), errb.String()}
}

func TestHelpListsCommands(t *testing.T) {
	f := newFakeAPI(t)
	r := f.run(t, "", "help")
	if r.code != 0 {
		t.Fatalf("code = %d", r.code)
	}
	for _, c := range []string{"controller", "host add", "app apply", "release create", "deploy", "rollback", "restart", "logs", "history", "secret set", "events"} {
		if !strings.Contains(r.stdout, c) {
			t.Errorf("usage missing %q", c)
		}
	}
	if r := f.run(t, ""); r.code != 2 || !strings.Contains(r.stderr, "Usage") {
		t.Errorf("no args: %+v", r)
	}
	if r := f.run(t, "", "frobnicate"); r.code != 2 || !strings.Contains(r.stderr, "unknown command") {
		t.Errorf("unknown: %+v", r)
	}
	if r := f.run(t, "", "deploy", "onlyapp"); r.code != 2 {
		t.Errorf("missing arg should be usage error: %+v", r)
	}
}

const deploymentJSON = `{"id":12,"app":"hello","env":"prod","release":3,"host":"m1","kind":"deploy","status":"%s","phase":"%s","message":"%s","actor":"ethan@box","started_at":"2026-10-01T10:00:00Z","finished_at":"2026-10-01T10:01:00Z","result":{"status":"%s","phase":"%s","events":[{"at":"2026-10-01T10:00:30Z","phase":"ready","message":"web ready"}],"logs":"%s"}}`

// deployment fills deploymentJSON's placeholders in order.
func deployment(status, phase, msg, logs string) string {
	parts := strings.Split(deploymentJSON, "%s")
	vals := []string{status, phase, msg, status, phase, logs}
	var b strings.Builder
	for i, p := range parts {
		b.WriteString(p)
		if i < len(vals) {
			b.WriteString(vals[i])
		}
	}
	return b.String()
}

func TestDeployFlagsAfterPositionals(t *testing.T) {
	f := newFakeAPI(t)
	f.responses["POST /v1/apps/hello/envs/prod/deploy"] = deployment("succeeded", "done", "", "")
	r := f.run(t, "", "deploy", "hello", "prod", "--release", "3")
	if r.code != 0 {
		t.Fatalf("result = %+v", r)
	}
	req := f.last(t)
	if req.Body != `{"release":3}` {
		t.Errorf("body = %s", req.Body)
	}
	if !strings.HasPrefix(req.Actor, "ethan@") {
		t.Errorf("actor = %q", req.Actor)
	}
	if !strings.Contains(r.stdout, "deployment 12") || !strings.Contains(r.stdout, "succeeded") {
		t.Errorf("stdout = %q", r.stdout)
	}
}

func TestDeployFailureExitsNonZeroWithDetail(t *testing.T) {
	f := newFakeAPI(t)
	f.responses["POST /v1/apps/hello/envs/prod/deploy"] = deployment("reverted", "smoke", "smoke failed", "boom-log")
	r := f.run(t, "", "deploy", "hello", "prod")
	if r.code != 1 {
		t.Fatalf("code = %d", r.code)
	}
	for _, want := range []string{"reverted", "smoke failed", "web ready", "boom-log"} {
		if !strings.Contains(r.stdout+r.stderr, want) {
			t.Errorf("output missing %q:\n%s%s", want, r.stdout, r.stderr)
		}
	}
}

func TestAPIErrorIsPrinted(t *testing.T) {
	f := newFakeAPI(t)
	f.responses["POST /v1/apps/hello/envs/prod/rollback"] = `{"error":{"code":"conflict","message":"a deployment of hello/prod is already in progress"}}`
	f.status["POST /v1/apps/hello/envs/prod/rollback"] = 409
	r := f.run(t, "", "rollback", "hello", "prod", "--to", "2")
	if r.code != 1 || !strings.Contains(r.stderr, "already in progress") {
		t.Errorf("result = %+v", r)
	}
	if f.last(t).Body != `{"to":2}` {
		t.Errorf("body = %s", f.last(t).Body)
	}
}

func TestSecretSetReadsStdinTrimmingOneNewline(t *testing.T) {
	f := newFakeAPI(t)
	f.responses["PUT /v1/apps/hello/envs/prod/secrets/SESSION_SECRET"] = `{"key":"SESSION_SECRET","version":4}`
	r := f.run(t, "s3cret\n\n", "secret", "set", "hello", "prod", "SESSION_SECRET")
	if r.code != 0 {
		t.Fatalf("result = %+v", r)
	}
	if b := f.last(t).Body; b != "s3cret\n" {
		t.Errorf("body = %q", b)
	}
	if strings.Contains(r.stdout, "s3cret") || !strings.Contains(r.stdout, "version 4") {
		t.Errorf("stdout = %q", r.stdout)
	}
}

func TestSecretListAndRm(t *testing.T) {
	f := newFakeAPI(t)
	f.responses["GET /v1/apps/hello/envs/prod/secrets"] = `[{"key":"A","version":2,"updated_at":"2026-10-01T10:00:00Z"}]`
	f.responses["DELETE /v1/apps/hello/envs/prod/secrets/A"] = `{"ok":true}`
	r := f.run(t, "", "secret", "list", "hello", "prod")
	if r.code != 0 || !strings.Contains(r.stdout, "KEY") || !strings.Contains(r.stdout, "A") {
		t.Errorf("list = %+v", r)
	}
	r = f.run(t, "", "secret", "rm", "hello", "prod", "A")
	if r.code != 0 || f.last(t).Method != "DELETE" {
		t.Errorf("rm = %+v", r)
	}
}

func TestAppApplyValidatesLocally(t *testing.T) {
	f := newFakeAPI(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lwd.toml"), []byte("name = \"hello\"\nreplicas = 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := f.run(t, "", "app", "apply", dir)
	if r.code != 1 || !strings.Contains(r.stderr, "replicas") || f.count() != 0 {
		t.Fatalf("invalid manifest: %+v (requests %d)", r, f.count())
	}

	good := `name = "hello"
[services.web]
image = "ghcr.io/o/web"
port = 8080
domain = "@"
[env.prod]
host = "m1"
domain = "hello.example.com"
`
	if err := os.WriteFile(filepath.Join(dir, "lwd.toml"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	f.responses["PUT /v1/apps/hello"] = `{"name":"hello","environments":[{"app":"hello","name":"prod","host":"m1","domain":"hello.example.com","tls":"acme"}],"warnings":["environment staging kept"]}`
	r = f.run(t, "", "app", "apply", dir)
	if r.code != 0 {
		t.Fatalf("apply = %+v", r)
	}
	if f.last(t).Body != good {
		t.Errorf("body = %q", f.last(t).Body)
	}
	if !strings.Contains(r.stdout, "hello") || !strings.Contains(r.stderr+r.stdout, "staging kept") {
		t.Errorf("output = %+v", r)
	}
}

func TestReleaseCreateImages(t *testing.T) {
	f := newFakeAPI(t)
	f.responses["POST /v1/apps/hello/releases"] = `{"id":5,"app":"hello","commit":"abc","tag":"v1","images":{"web":"r/web@sha256:aa"},"actor":"e","created_at":"2026-10-01T10:00:00Z"}`
	r := f.run(t, "", "release", "create", "hello", "--tag", "v1", "--commit", "abc", "--image", "web=r/web:v1", "--image", "api=r/api:v2")
	if r.code != 0 {
		t.Fatalf("result = %+v", r)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(f.last(t).Body), &body); err != nil {
		t.Fatal(err)
	}
	imgs, _ := body["images"].(map[string]any)
	if body["tag"] != "v1" || body["commit"] != "abc" || imgs["web"] != "r/web:v1" || imgs["api"] != "r/api:v2" {
		t.Errorf("body = %v", body)
	}
	if !strings.Contains(r.stdout, "release 5") {
		t.Errorf("stdout = %q", r.stdout)
	}
	if r := f.run(t, "", "release", "create", "hello", "--image", "bogus"); r.code != 2 {
		t.Errorf("bad --image should be usage error: %+v", r)
	}
}

func TestHistoryTableAndJSON(t *testing.T) {
	f := newFakeAPI(t)
	f.responses["GET /v1/apps/hello/envs/prod/deployments"] = "[" + deployment("succeeded", "done", "", "") + "]"
	r := f.run(t, "", "history", "hello", "prod")
	if r.code != 0 {
		t.Fatalf("result = %+v", r)
	}
	for _, col := range []string{"ID", "RELEASE", "KIND", "STATUS", "12", "succeeded"} {
		if !strings.Contains(r.stdout, col) {
			t.Errorf("table missing %q:\n%s", col, r.stdout)
		}
	}
	r = f.run(t, "", "history", "hello", "prod", "--json")
	var v []map[string]any
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &v) != nil || len(v) != 1 {
		t.Errorf("json output = %q", r.stdout)
	}
}

func TestHostAddReadsTokenFile(t *testing.T) {
	f := newFakeAPI(t)
	f.responses["POST /v1/hosts"] = `{"name":"m1","addr":"10.0.0.5:7480","created_at":"2026-10-01T10:00:00Z","updated_at":"2026-10-01T10:00:00Z"}`
	tf := filepath.Join(t.TempDir(), "node.token")
	if err := os.WriteFile(tf, []byte("nodetok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := f.run(t, "", "host", "add", "m1", "10.0.0.5:7480", "--token-file", tf)
	if r.code != 0 {
		t.Fatalf("result = %+v", r)
	}
	if f.last(t).Body != `{"name":"m1","addr":"10.0.0.5:7480","token":"nodetok"}` {
		t.Errorf("body = %s", f.last(t).Body)
	}
	if r := f.run(t, "", "host", "add", "m1", "a:1"); r.code != 2 {
		t.Errorf("missing --token-file should be usage error: %+v", r)
	}
}

func TestLogsAndRestart(t *testing.T) {
	f := newFakeAPI(t)
	f.responses["GET /v1/apps/hello/envs/prod/logs"] = "line1\nline2\n"
	f.responses["POST /v1/apps/hello/envs/prod/restart"] = `{"ok":true}`
	r := f.run(t, "", "logs", "hello", "prod", "--service", "web", "--tail", "5")
	if r.code != 0 || r.stdout != "line1\nline2\n" || f.last(t).Query != "service=web&tail=5" {
		t.Errorf("logs = %+v query %q", r, f.last(t).Query)
	}
	r = f.run(t, "", "restart", "hello", "prod", "--service", "api")
	if r.code != 0 || f.last(t).Body != `{"service":"api"}` {
		t.Errorf("restart = %+v body %s", r, f.last(t).Body)
	}
}

func TestStatusAndListCommands(t *testing.T) {
	f := newFakeAPI(t)
	f.responses["GET /v1/hosts"] = `[{"name":"m1","addr":"a:1","created_at":"2026-10-01T10:00:00Z","updated_at":"2026-10-01T10:00:00Z"}]`
	f.responses["GET /v1/hosts/m1"] = `{"name":"m1","addr":"a:1","created_at":"2026-10-01T10:00:00Z","updated_at":"2026-10-01T10:00:00Z","reachable":false,"error":"connection refused"}`
	f.responses["GET /v1/apps"] = `[{"name":"hello","created_at":"2026-10-01T10:00:00Z","updated_at":"2026-10-01T10:00:00Z","environments":[{"app":"hello","name":"prod","host":"m1","domain":"x.y","tls":"acme"}]}]`
	f.responses["GET /v1/apps/hello"] = `{"name":"hello","manifest":"m","created_at":"2026-10-01T10:00:00Z","updated_at":"2026-10-01T10:00:00Z","environments":[{"app":"hello","name":"prod","host":"m1","domain":"x.y","tls":"acme"}]}`
	f.responses["GET /v1/apps/hello/envs/prod/status"] = `{"app":"hello","env":"prod","host":"m1","domain":"x.y","tls":"acme","live":` + deployment("succeeded", "done", "", "") + `,"last":` + deployment("succeeded", "done", "", "") + `,"node":{"containers":[]}}`
	f.responses["GET /v1/apps/hello/releases"] = `[{"id":5,"app":"hello","commit":"abc","tag":"v1","images":{"web":"r/web@sha256:aa"},"actor":"e","created_at":"2026-10-01T10:00:00Z"}]`
	f.responses["GET /v1/events"] = `[{"id":1,"at":"2026-10-01T10:00:00Z","app":"hello","env":"prod","kind":"deploy.succeeded","message":"ok"}]`

	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"host", "list"}, []string{"NAME", "m1", "a:1"}},
		{[]string{"host", "status", "m1"}, []string{"m1", "connection refused"}},
		{[]string{"app", "list"}, []string{"hello", "prod"}},
		{[]string{"app", "status", "hello"}, []string{"prod", "m1", "x.y"}},
		{[]string{"app", "status", "hello", "prod"}, []string{"x.y", "12", "release 3", "containers"}},
		{[]string{"release", "list", "hello"}, []string{"5", "v1", "abc"}},
		{[]string{"events", "hello"}, []string{"deploy.succeeded", "ok"}},
	}
	for _, tc := range cases {
		r := f.run(t, "", tc.args...)
		if r.code != 0 {
			t.Errorf("%v: %+v", tc.args, r)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(r.stdout, w) {
				t.Errorf("%v: output missing %q:\n%s", tc.args, w, r.stdout)
			}
		}
		js := f.run(t, "", append(tc.args, "--json")...)
		if js.code != 0 || !json.Valid([]byte(js.stdout)) {
			t.Errorf("%v --json: %+v", tc.args, js)
		}
	}
	if q := f.last(t).Query; q != "app=hello" {
		t.Errorf("events query = %q", q)
	}
}

func TestMissingTokenIsAnError(t *testing.T) {
	var out, errb bytes.Buffer
	home := t.TempDir()
	code := run([]string{"app", "list"}, strings.NewReader(""), &out, &errb, func(k string) string {
		return map[string]string{"HOME": home}[k]
	})
	if code != 1 || !strings.Contains(errb.String(), "LWD_TOKEN") {
		t.Errorf("code %d stderr %q", code, errb.String())
	}
}
