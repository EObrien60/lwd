package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"lwd/internal/bundle"
	"lwd/internal/client"
	"lwd/internal/registry"
	"lwd/internal/secrets"
	"lwd/internal/store"
	"lwd/internal/store/storetest"
)

const (
	adminToken = "admin-token-123"
	nodeToken  = "node-token-456"
	secretVal  = "super-secret-value"
)

const helloManifest = `
name = "hello"
secrets = ["SESSION_SECRET"]

[services.web]
image  = "ghcr.io/o/web"
port   = 8080
domain = "@"

[services.api]
image  = "ghcr.io/o/api"
port   = 3000
domain = "api"
ready  = "/ready"

[services.worker]
image   = "ghcr.io/o/api"
command = ["node", "worker.js"]

[migrate]
service = "api"
command = ["node", "migrate.js"]

[env]
LOG_LEVEL = "info"

[env.staging]
host   = "m1"
domain = "hello.test"
tls    = "internal"
env    = { LOG_LEVEL = "debug" }

[env.prod]
host   = "m1"
domain = "hello.example.com"
`

func digest(c byte) string { return "@sha256:" + strings.Repeat(string(c), 64) }

// countingResolver counts lookups per ref so tests can check each distinct
// reference is resolved once.
type countingResolver struct {
	mu    sync.Mutex
	fake  registry.Fake
	calls map[string]int
}

func (r *countingResolver) Resolve(ctx context.Context, ref string) (string, error) {
	r.mu.Lock()
	r.calls[ref]++
	r.mu.Unlock()
	return r.fake.Resolve(ctx, ref)
}

// fakeNode implements the node HTTP API from DESIGN.md "Node".
type fakeNode struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	bundles  []bundle.Bundle
	restarts []string
	result   bundle.Result // returned by /v1/deploy
	httpCode int           // if non-zero, /v1/deploy fails with this code
	entered  chan struct{} // if non-nil, deploy signals entry...
	release  chan struct{} // ...and waits for this before answering
	status   string
}

func newFakeNode(t *testing.T) *fakeNode {
	n := &fakeNode{t: t, result: bundle.Result{Status: bundle.StatusSucceeded, Phase: "done"}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/deploy", func(w http.ResponseWriter, r *http.Request) {
		var b bundle.Bundle
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("node: decode bundle: %v", err)
		}
		if err := b.Validate(); err != nil {
			t.Errorf("node: controller sent invalid bundle: %v", err)
		}
		n.mu.Lock()
		n.bundles = append(n.bundles, b)
		entered, release, code, res := n.entered, n.release, n.httpCode, n.result
		n.mu.Unlock()
		if entered != nil {
			entered <- struct{}{}
			<-release
		}
		if code != 0 {
			http.Error(w, `{"error":{"code":"conflict","message":"node busy"}}`, code)
			return
		}
		res.Events = []bundle.Event{{At: time.Now().Format(time.RFC3339), Phase: res.Phase, Message: "x"}}
		_ = json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		_, _ = io.WriteString(w, n.status)
	})
	mux.HandleFunc("POST /v1/apps/{app}/{env}/restart", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.restarts = append(n.restarts, r.PathValue("app")+"/"+r.PathValue("env")+"?"+r.URL.RawQuery)
		n.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/apps/{app}/{env}/logs", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "logs for "+r.PathValue("app")+"/"+r.PathValue("env")+" "+r.URL.RawQuery+"\n")
	})
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+nodeToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(n.srv.Close)
	n.status = `{"host":{"name":"m1"},"apps":[{"app":"hello","env":"prod","deployment":1,"containers":[]},{"app":"other","env":"prod"}]}`
	return n
}

func (n *fakeNode) set(f func(*fakeNode)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	f(n)
}

func (n *fakeNode) setResult(r bundle.Result) { n.set(func(n *fakeNode) { n.result = r }) }

// block makes the next deploys signal on entered and wait on release.
func (n *fakeNode) block() {
	n.set(func(n *fakeNode) { n.entered, n.release = make(chan struct{}), make(chan struct{}) })
}

func (n *fakeNode) getRestarts() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.restarts...)
}

func (n *fakeNode) addr() string { return strings.TrimPrefix(n.srv.URL, "http://") }

func (n *fakeNode) lastBundle() bundle.Bundle {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.bundles) == 0 {
		n.t.Fatal("node received no bundle")
	}
	return n.bundles[len(n.bundles)-1]
}

type harness struct {
	t     *testing.T
	ctx   context.Context
	c     *client.Client
	node  *fakeNode
	res   *countingResolver
	store *store.Store
	srv   *httptest.Server
	// bodies captures every controller response body for leak checks.
	bodies *strings.Builder
}

func setup(t *testing.T) *harness {
	t.Helper()
	st := storetest.New(t)
	ciph, err := secrets.NewCipher(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	res := &countingResolver{calls: map[string]int{}, fake: registry.Fake{
		"ghcr.io/o/web:v1":  "ghcr.io/o/web" + digest('a'),
		"ghcr.io/o/api:v1":  "ghcr.io/o/api" + digest('b'),
		"ghcr.io/o/web:v2":  "ghcr.io/o/web" + digest('c'),
		"ghcr.io/o/api:v2":  "ghcr.io/o/api" + digest('d'),
		"ghcr.io/o/api:fix": "ghcr.io/o/api" + digest('e'),
	}}
	ctrl := New(st, ciph, res, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := &harness{t: t, ctx: context.Background(), node: newFakeNode(t), res: res, store: st, bodies: &strings.Builder{}}
	inner := ctrl.Handler(adminToken)
	var mu sync.Mutex
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &teeWriter{ResponseWriter: w}
		inner.ServeHTTP(rec, r)
		mu.Lock()
		h.bodies.Write(rec.buf)
		mu.Unlock()
	}))
	t.Cleanup(h.srv.Close)
	h.c = client.New(h.srv.URL, adminToken)
	h.c.Actor = "tester@box"
	return h
}

type teeWriter struct {
	http.ResponseWriter
	buf []byte
}

func (w *teeWriter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	return w.ResponseWriter.Write(b)
}

// ready registers the node, applies the app, sets the secret for both
// environments and creates release v1.
func (h *harness) ready() store.Release {
	h.t.Helper()
	if _, err := h.c.AddHost(h.ctx, client.HostRequest{Name: "m1", Addr: h.node.addr(), Token: nodeToken}); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.c.ApplyApp(h.ctx, "hello", []byte(helloManifest)); err != nil {
		h.t.Fatal(err)
	}
	for _, env := range []string{"staging", "prod"} {
		if _, err := h.c.SetSecret(h.ctx, "hello", env, "SESSION_SECRET", []byte(secretVal)); err != nil {
			h.t.Fatal(err)
		}
	}
	rel, err := h.c.CreateRelease(h.ctx, "hello", client.ReleaseRequest{Commit: "abc123", Tag: "v1"})
	if err != nil {
		h.t.Fatal(err)
	}
	return rel
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if !client.IsCode(err, code) {
		t.Fatalf("err = %v, want code %s", err, code)
	}
}

func TestAuthRequired(t *testing.T) {
	h := setup(t)
	for _, tok := range []string{"", "wrong", adminToken + "x"} {
		_, err := client.New(h.srv.URL, tok).ListApps(h.ctx)
		wantCode(t, err, client.CodeUnauthorized)
	}
	// Even unknown routes under /v1 must not reveal anything without auth.
	resp, err := http.Get(h.srv.URL + "/v1/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown route without auth = %d", resp.StatusCode)
	}
	if _, err := h.c.ListApps(h.ctx); err != nil {
		t.Fatalf("authorised ListApps: %v", err)
	}
}

func TestUnknownRouteIsJSONNotFound(t *testing.T) {
	h := setup(t)
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/nope", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var eb client.ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil || resp.StatusCode != 404 || eb.Error.Code != "not_found" {
		t.Errorf("status %d body %+v err %v", resp.StatusCode, eb, err)
	}
}

func TestDeployEndToEnd(t *testing.T) {
	h := setup(t)
	rel := h.ready()
	if rel.Images["web"] != "ghcr.io/o/web"+digest('a') || rel.Images["api"] != "ghcr.io/o/api"+digest('b') || rel.Images["worker"] != rel.Images["api"] {
		t.Fatalf("release images = %v", rel.Images)
	}
	if h.res.calls["ghcr.io/o/api:v1"] != 1 {
		t.Errorf("api:v1 resolved %d times, want once", h.res.calls["ghcr.io/o/api:v1"])
	}

	d, err := h.c.Deploy(h.ctx, "hello", "staging", client.DeployRequest{Reason: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != store.StatusSucceeded || d.Kind != store.KindDeploy || d.Release != rel.ID || d.Actor != "tester@box" || d.Reason != "first" || d.FinishedAt == nil {
		t.Fatalf("deployment = %+v", d)
	}
	var res bundle.Result
	if err := json.Unmarshal(d.Result, &res); err != nil || res.Phase != "done" || len(res.Events) != 1 {
		t.Errorf("result = %s (%v)", d.Result, err)
	}

	b := h.node.lastBundle()
	if b.App != "hello" || b.Env != "staging" || b.Release != rel.ID || b.Deployment != d.ID || b.TLS != bundle.TLSInternal {
		t.Errorf("bundle header = %+v", b)
	}
	if b.ReadyTimeoutSeconds != 60 || b.SmokeSeconds != 10 {
		t.Errorf("timeouts = %d/%d", b.ReadyTimeoutSeconds, b.SmokeSeconds)
	}
	wantVars := map[string]string{"LOG_LEVEL": "debug", "SESSION_SECRET": secretVal}
	if !reflect.DeepEqual(b.Vars, wantVars) {
		t.Errorf("vars = %v", b.Vars)
	}
	wantSvcs := []bundle.Service{
		{Name: "api", Image: "ghcr.io/o/api" + digest('b'), Port: 3000, Domains: []string{"api.hello.test"}, Ready: "/ready"},
		{Name: "web", Image: "ghcr.io/o/web" + digest('a'), Port: 8080, Domains: []string{"hello.test"}, Ready: "/"},
		{Name: "worker", Image: "ghcr.io/o/api" + digest('b'), Command: []string{"node", "worker.js"}},
	}
	if !reflect.DeepEqual(b.Services, wantSvcs) {
		t.Errorf("services = %+v", b.Services)
	}
	if b.Migrate == nil || b.Migrate.Service != "api" {
		t.Errorf("migrate = %+v", b.Migrate)
	}

	evs, err := h.c.Events(h.ctx, "hello", "staging", 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if len(kinds) < 2 || kinds[0] != "deploy.succeeded" || kinds[1] != "deploy.started" {
		t.Errorf("event kinds = %v", kinds)
	}

	hist, err := h.c.Deployments(h.ctx, "hello", "staging")
	if err != nil || len(hist) != 1 || hist[0].ID != d.ID {
		t.Errorf("history = %+v, %v", hist, err)
	}
	h.assertNoLeaks()
}

func (h *harness) assertNoLeaks() {
	h.t.Helper()
	all := h.bodies.String()
	for _, s := range []string{secretVal, nodeToken, adminToken} {
		if strings.Contains(all, s) {
			h.t.Errorf("a response body leaked %q", s)
		}
	}
}

func TestDeployDefaultsToNewestReleaseAndAcceptsExplicit(t *testing.T) {
	h := setup(t)
	r1 := h.ready()
	r2, err := h.c.CreateRelease(h.ctx, "hello", client.ReleaseRequest{Tag: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := h.c.Deploy(h.ctx, "hello", "prod", client.DeployRequest{})
	if err != nil || d.Release != r2.ID {
		t.Fatalf("default deploy = %+v, %v", d, err)
	}
	d, err = h.c.Deploy(h.ctx, "hello", "prod", client.DeployRequest{Release: r1.ID})
	if err != nil || d.Release != r1.ID {
		t.Fatalf("explicit deploy = %+v, %v", d, err)
	}
	_, err = h.c.Deploy(h.ctx, "hello", "prod", client.DeployRequest{Release: 9999})
	wantCode(t, err, client.CodeNotFound)
	_, err = h.c.Deploy(h.ctx, "hello", "nope", client.DeployRequest{})
	wantCode(t, err, client.CodeNotFound)
}

func TestDeployMissingSecretIsInvalid(t *testing.T) {
	h := setup(t)
	h.ready()
	if err := h.c.DeleteSecret(h.ctx, "hello", "prod", "SESSION_SECRET"); err != nil {
		t.Fatal(err)
	}
	_, err := h.c.Deploy(h.ctx, "hello", "prod", client.DeployRequest{})
	wantCode(t, err, client.CodeInvalid)
	if !strings.Contains(err.Error(), "SESSION_SECRET") {
		t.Errorf("error should name the missing secret: %v", err)
	}
	hist, _ := h.c.Deployments(h.ctx, "hello", "prod")
	if len(hist) != 0 {
		t.Errorf("no deployment should be recorded, got %+v", hist)
	}
}

func TestDeployFailedAndRevertedAreRecorded(t *testing.T) {
	h := setup(t)
	h.ready()
	for _, status := range []string{bundle.StatusFailed, bundle.StatusReverted} {
		h.node.setResult(bundle.Result{Status: status, Phase: "ready", Message: "web not ready", Logs: "boom"})
		d, err := h.c.Deploy(h.ctx, "hello", "prod", client.DeployRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if d.Status != status || d.Phase != "ready" || d.Message != "web not ready" {
			t.Errorf("deployment = %+v", d)
		}
		evs, _ := h.c.Events(h.ctx, "hello", "prod", 1)
		if len(evs) != 1 || evs[0].Kind != "deploy."+status {
			t.Errorf("events = %+v", evs)
		}
	}
	st, err := h.c.Status(h.ctx, "hello", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if st.Live != nil || st.Last == nil || st.Last.Status != bundle.StatusReverted {
		t.Errorf("status live=%+v last=%+v", st.Live, st.Last)
	}
}

func TestDeployNodeUnreachable(t *testing.T) {
	h := setup(t)
	h.ready()
	h.node.srv.Close()
	_, err := h.c.Deploy(h.ctx, "hello", "prod", client.DeployRequest{})
	wantCode(t, err, client.CodeNodeUnreachable)
	hist, _ := h.c.Deployments(h.ctx, "hello", "prod")
	if len(hist) != 1 || hist[0].Status != store.StatusFailed || !strings.Contains(hist[0].Message, "unreachable") {
		t.Fatalf("history = %+v", hist)
	}
	evs, _ := h.c.Events(h.ctx, "hello", "prod", 1)
	if len(evs) != 1 || evs[0].Kind != "deploy.failed" {
		t.Errorf("events = %+v", evs)
	}
}

func TestDeployNodeRejects(t *testing.T) {
	h := setup(t)
	h.ready()
	h.node.set(func(n *fakeNode) { n.httpCode = http.StatusConflict })
	_, err := h.c.Deploy(h.ctx, "hello", "prod", client.DeployRequest{})
	wantCode(t, err, client.CodeConflict)
	hist, _ := h.c.Deployments(h.ctx, "hello", "prod")
	if len(hist) != 1 || hist[0].Status != store.StatusFailed {
		t.Fatalf("history = %+v", hist)
	}
}

func TestConcurrentDeployIsConflict(t *testing.T) {
	h := setup(t)
	h.ready()
	h.node.block()
	done := make(chan error)
	go func() {
		_, err := h.c.Deploy(h.ctx, "hello", "prod", client.DeployRequest{})
		done <- err
	}()
	<-h.node.entered
	_, err := h.c.Deploy(h.ctx, "hello", "prod", client.DeployRequest{})
	wantCode(t, err, client.CodeConflict)
	_, err = h.c.Rollback(h.ctx, "hello", "prod", client.RollbackRequest{})
	wantCode(t, err, client.CodeConflict)

	// Another environment is not blocked: staging reaches the node while
	// prod is still held there. Then release both (either may wake first).
	go func() {
		<-h.node.entered
		h.node.release <- struct{}{}
		h.node.release <- struct{}{}
	}()
	if _, err := h.c.Deploy(h.ctx, "hello", "staging", client.DeployRequest{}); err != nil {
		t.Errorf("staging deploy: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestClientDisconnectDoesNotAbandonDeployment(t *testing.T) {
	h := setup(t)
	h.ready()
	h.node.block()
	ctx, cancel := context.WithCancel(h.ctx)
	go func() { _, _ = h.c.Deploy(ctx, "hello", "prod", client.DeployRequest{}) }()
	<-h.node.entered
	cancel()
	time.Sleep(50 * time.Millisecond)
	h.node.release <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		hist, _ := h.c.Deployments(h.ctx, "hello", "prod")
		if len(hist) == 1 && hist[0].Status == store.StatusSucceeded {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("deployment was not completed after the client went away")
}

func TestRollback(t *testing.T) {
	h := setup(t)
	r1 := h.ready()
	_, err := h.c.Rollback(h.ctx, "hello", "prod", client.RollbackRequest{})
	wantCode(t, err, client.CodeInvalid) // nothing live

	r2, err := h.c.CreateRelease(h.ctx, "hello", client.ReleaseRequest{Tag: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.Deploy(h.ctx, "hello", "prod", client.DeployRequest{Release: r1.ID}); err != nil {
		t.Fatal(err)
	}
	_, err = h.c.Rollback(h.ctx, "hello", "prod", client.RollbackRequest{})
	wantCode(t, err, client.CodeInvalid) // only one release ever succeeded

	if _, err := h.c.Deploy(h.ctx, "hello", "prod", client.DeployRequest{Release: r2.ID}); err != nil {
		t.Fatal(err)
	}
	// A failed attempt of r1 in between must not confuse the target choice.
	h.node.setResult(bundle.Result{Status: bundle.StatusFailed, Phase: "pull"})
	if _, err := h.c.Deploy(h.ctx, "hello", "prod", client.DeployRequest{Release: r1.ID}); err != nil {
		t.Fatal(err)
	}
	h.node.setResult(bundle.Result{Status: bundle.StatusSucceeded, Phase: "done"})

	d, err := h.c.Rollback(h.ctx, "hello", "prod", client.RollbackRequest{Reason: "bad deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != store.KindRollback || d.Release != r1.ID || d.Status != store.StatusSucceeded || d.Reason != "bad deploy" {
		t.Fatalf("rollback = %+v", d)
	}
	if b := h.node.lastBundle(); b.Release != r1.ID || b.Services[0].Image != "ghcr.io/o/api"+digest('b') {
		t.Errorf("rollback bundle = %+v", b)
	}
	// Rolling back again goes forward to r2 (previous live release).
	d, err = h.c.Rollback(h.ctx, "hello", "prod", client.RollbackRequest{})
	if err != nil || d.Release != r2.ID {
		t.Fatalf("second rollback = %+v, %v", d, err)
	}
	d, err = h.c.Rollback(h.ctx, "hello", "prod", client.RollbackRequest{To: r1.ID})
	if err != nil || d.Release != r1.ID || d.Kind != store.KindRollback {
		t.Fatalf("rollback --to = %+v, %v", d, err)
	}
	_, err = h.c.Rollback(h.ctx, "hello", "prod", client.RollbackRequest{To: 424242})
	wantCode(t, err, client.CodeNotFound)
}

func TestReleaseCreate(t *testing.T) {
	h := setup(t)
	h.ready()
	rel, err := h.c.CreateRelease(h.ctx, "hello", client.ReleaseRequest{Tag: "v2", Images: map[string]string{"worker": "ghcr.io/o/api:fix"}})
	if err != nil {
		t.Fatal(err)
	}
	if rel.Images["worker"] != "ghcr.io/o/api"+digest('e') || rel.Images["api"] != "ghcr.io/o/api"+digest('d') || rel.Tag != "v2" {
		t.Errorf("images = %v", rel.Images)
	}
	if rel.Manifest != "" {
		t.Errorf("manifest snapshot need not be returned")
	}
	stored, err := h.store.GetRelease(h.ctx, "hello", rel.ID)
	if err != nil || stored.Manifest != helloManifest {
		t.Errorf("snapshot = %q, %v", stored.Manifest, err)
	}

	cases := []client.ReleaseRequest{
		{Tag: "latest"},
		{},            // no tag and no images
		{Tag: "nope"}, // unknown tag
		{Tag: "v1", Images: map[string]string{"ghost": "ghcr.io/o/api:v1"}},
	}
	for _, rr := range cases {
		_, err := h.c.CreateRelease(h.ctx, "hello", rr)
		wantCode(t, err, client.CodeInvalid)
	}
	_, err = h.c.CreateRelease(h.ctx, "ghost", client.ReleaseRequest{Tag: "v1"})
	wantCode(t, err, client.CodeNotFound)

	rels, err := h.c.ListReleases(h.ctx, "hello")
	if err != nil || len(rels) != 2 || rels[0].ID != rel.ID {
		t.Errorf("releases = %+v, %v", rels, err)
	}
}

func TestApplyApp(t *testing.T) {
	h := setup(t)
	// Unknown host.
	_, err := h.c.ApplyApp(h.ctx, "hello", []byte(helloManifest))
	wantCode(t, err, client.CodeInvalid)
	if !strings.Contains(err.Error(), "m1") {
		t.Errorf("error should name the host: %v", err)
	}
	if _, err := h.c.AddHost(h.ctx, client.HostRequest{Name: "m1", Addr: h.node.addr(), Token: nodeToken}); err != nil {
		t.Fatal(err)
	}
	_, err = h.c.ApplyApp(h.ctx, "other", []byte(helloManifest))
	wantCode(t, err, client.CodeInvalid) // name mismatch
	_, err = h.c.ApplyApp(h.ctx, "hello", []byte("name = \"hello\"\nreplicas = 2\n"))
	wantCode(t, err, client.CodeInvalid)
	if !strings.Contains(err.Error(), "replicas") {
		t.Errorf("error should list manifest problems: %v", err)
	}

	a, err := h.c.ApplyApp(h.ctx, "hello", []byte(helloManifest))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Environments) != 2 {
		t.Errorf("environments = %+v", a.Environments)
	}
	got, err := h.c.GetApp(h.ctx, "hello")
	if err != nil || got.Manifest != helloManifest || len(got.Environments) != 2 {
		t.Errorf("app = %+v, %v", got, err)
	}
	apps, err := h.c.ListApps(h.ctx)
	if err != nil || len(apps) != 1 || apps[0].Name != "hello" || len(apps[0].Environments) != 2 {
		t.Errorf("apps = %+v, %v", apps, err)
	}
	_, err = h.c.GetApp(h.ctx, "ghost")
	wantCode(t, err, client.CodeNotFound)
}

func TestApplyKeepsEnvironmentWithHistory(t *testing.T) {
	h := setup(t)
	h.ready()
	if _, err := h.c.Deploy(h.ctx, "hello", "staging", client.DeployRequest{}); err != nil {
		t.Fatal(err)
	}
	trimmed := helloManifest[:strings.Index(helloManifest, "[env.staging]")] + `[env.prod]
host   = "m1"
domain = "hello.example.com"
`
	a, err := h.c.ApplyApp(h.ctx, "hello", []byte(trimmed))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Warnings) != 1 || !strings.Contains(a.Warnings[0], "staging") {
		t.Errorf("warnings = %v", a.Warnings)
	}
}

func TestHosts(t *testing.T) {
	h := setup(t)
	_, err := h.c.AddHost(h.ctx, client.HostRequest{Name: "Bad Name", Addr: "x:1", Token: "t"})
	wantCode(t, err, client.CodeInvalid)
	_, err = h.c.AddHost(h.ctx, client.HostRequest{Name: "m1", Addr: "x:1"})
	wantCode(t, err, client.CodeInvalid) // token required
	if _, err := h.c.AddHost(h.ctx, client.HostRequest{Name: "m1", Addr: h.node.addr(), Token: nodeToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.AddHost(h.ctx, client.HostRequest{Name: "dead", Addr: "127.0.0.1:1", Token: "x"}); err != nil {
		t.Fatal(err)
	}
	hs, err := h.c.ListHosts(h.ctx)
	if err != nil || len(hs) != 2 {
		t.Fatalf("hosts = %+v, %v", hs, err)
	}
	st, err := h.c.GetHost(h.ctx, "m1")
	if err != nil || !st.Reachable || !strings.Contains(string(st.Node), `"apps"`) {
		t.Errorf("m1 = %+v, %v", st, err)
	}
	st, err = h.c.GetHost(h.ctx, "dead")
	if err != nil || st.Reachable || st.Error == "" {
		t.Errorf("dead = %+v, %v", st, err)
	}
	_, err = h.c.GetHost(h.ctx, "ghost")
	wantCode(t, err, client.CodeNotFound)
	h.assertNoLeaks()
}

func TestStatusMergesNodeView(t *testing.T) {
	h := setup(t)
	h.ready()
	d, err := h.c.Deploy(h.ctx, "hello", "prod", client.DeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := h.c.Status(h.ctx, "hello", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if st.Host != "m1" || st.Domain != "hello.example.com" || st.TLS != "acme" || st.Live == nil || st.Live.ID != d.ID || st.Last.ID != d.ID {
		t.Errorf("status = %+v", st)
	}
	var node map[string]any
	if err := json.Unmarshal(st.Node, &node); err != nil || node["app"] != "hello" || node["env"] != "prod" {
		t.Errorf("node view = %s (%v)", st.Node, err)
	}
	// Node down: DB view still returned, with the error.
	h.node.srv.Close()
	st, err = h.c.Status(h.ctx, "hello", "prod")
	if err != nil || st.NodeError == "" || st.Live == nil {
		t.Errorf("status with node down = %+v, %v", st, err)
	}
}

func TestRestartAndLogsProxy(t *testing.T) {
	h := setup(t)
	h.ready()
	if err := h.c.Restart(h.ctx, "hello", "prod", "web"); err != nil {
		t.Fatal(err)
	}
	if r := h.node.getRestarts(); len(r) != 1 || r[0] != "hello/prod?service=web" {
		t.Errorf("restarts = %v", r)
	}
	logs, err := h.c.Logs(h.ctx, "hello", "prod", "api", 20)
	if err != nil || logs != "logs for hello/prod service=api&tail=20\n" {
		t.Errorf("logs = %q, %v", logs, err)
	}
	h.node.srv.Close()
	wantCode(t, h.c.Restart(h.ctx, "hello", "prod", ""), client.CodeNodeUnreachable)
	_, err = h.c.Logs(h.ctx, "hello", "prod", "", 0)
	wantCode(t, err, client.CodeNodeUnreachable)
}

func TestSecrets(t *testing.T) {
	h := setup(t)
	h.ready()
	m, err := h.c.SetSecret(h.ctx, "hello", "prod", "SESSION_SECRET", []byte("v2"))
	if err != nil || m.Version != 2 || m.Key != "SESSION_SECRET" {
		t.Fatalf("set = %+v, %v", m, err)
	}
	_, err = h.c.SetSecret(h.ctx, "hello", "prod", "bad-key", []byte("x"))
	wantCode(t, err, client.CodeInvalid)
	_, err = h.c.SetSecret(h.ctx, "hello", "nope", "K", []byte("x"))
	wantCode(t, err, client.CodeNotFound)
	list, err := h.c.ListSecrets(h.ctx, "hello", "prod")
	if err != nil || len(list) != 1 || list[0].Version != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	wantCode(t, h.c.DeleteSecret(h.ctx, "hello", "prod", "NOPE"), client.CodeNotFound)
	if err := h.c.DeleteSecret(h.ctx, "hello", "prod", "SESSION_SECRET"); err != nil {
		t.Fatal(err)
	}
	list, _ = h.c.ListSecrets(h.ctx, "hello", "prod")
	if len(list) != 0 {
		t.Errorf("after delete = %+v", list)
	}
	h.assertNoLeaks()
}

func TestEventsFilter(t *testing.T) {
	h := setup(t)
	h.ready()
	all, err := h.c.Events(h.ctx, "", "", 0)
	if err != nil || len(all) < 4 {
		t.Fatalf("events = %+v, %v", all, err)
	}
	kinds := map[string]bool{}
	for _, e := range all {
		kinds[e.Kind] = true
	}
	for _, k := range []string{"host.added", "app.applied", "secret.set", "release.created"} {
		if !kinds[k] {
			t.Errorf("missing event %s in %v", k, kinds)
		}
	}
	two, _ := h.c.Events(h.ctx, "", "", 2)
	if len(two) != 2 {
		t.Errorf("limit ignored: %d", len(two))
	}
	prod, _ := h.c.Events(h.ctx, "hello", "prod", 0)
	for _, e := range prod {
		if e.App != "hello" || e.Env != "prod" {
			t.Errorf("filter leaked %+v", e)
		}
	}
	h.assertNoLeaks()
}
