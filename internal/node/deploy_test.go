package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"lwd/internal/bundle"
)

func (h *harness) deploy(b bundle.Bundle) bundle.Result {
	h.t.Helper()
	res, err := h.n.Deploy(context.Background(), &b)
	if err != nil {
		h.t.Fatalf("Deploy: %v", err)
	}
	return *res
}

func (h *harness) state() *State {
	h.t.Helper()
	st, err := loadState(filepath.Join(h.dir, "apps", "hello-staging", "state.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

func bundleN(n int64) bundle.Bundle {
	b := testBundle()
	b.Deployment = n
	return b
}

func phases(res bundle.Result) []string {
	var out []string
	for _, e := range res.Events {
		if len(out) == 0 || out[len(out)-1] != e.Phase {
			out = append(out, e.Phase)
		}
	}
	return out
}

func TestDeployHappyPathFirstDeploy(t *testing.T) {
	h := newHarness(t)
	res := h.deploy(bundleN(1))
	if res.Status != bundle.StatusSucceeded || res.Phase != "done" {
		t.Fatalf("result = %+v", res)
	}
	want := []string{"prepare", "pull", "migrate", "start", "ready", "cutover", "smoke", "done"}
	if got := phases(res); !reflect.DeepEqual(got, want) {
		t.Errorf("phases = %v", got)
	}
	for _, e := range res.Events {
		if e.At == "" || e.Message == "" {
			t.Errorf("incomplete event %+v", e)
		}
	}

	const p = "lwd-hello-staging-d1"
	got := h.docker.recorded()
	wantCalls := []string{p + " pull ", p + " run --rm -T " + migrateService, p + " up -d"}
	if len(got) < 3 || !reflect.DeepEqual(got[:3], wantCalls) {
		t.Errorf("docker calls = %q", got)
	}
	if h.docker.has(p, "down") {
		t.Error("successful candidate must not be downed")
	}

	cf := h.caddy.last()
	for _, s := range []string{"hello.lwd.internal {\n\ttls internal\n\treverse_proxy 127.0.0.1:20000\n}", "api.hello.lwd.internal {\n\ttls internal\n\treverse_proxy 127.0.0.1:20001\n}"} {
		if !strings.Contains(cf, s) {
			t.Errorf("Caddyfile missing %q:\n%s", s, cf)
		}
	}
	onDisk, _ := os.ReadFile(filepath.Join(h.dir, "caddy", "etc", "Caddyfile"))
	if string(onDisk) != cf {
		t.Error("persisted Caddyfile differs from the loaded one")
	}

	// Readiness used each service's ready path on its loopback port; smoke
	// went through "Caddy" with SNI for every domain.
	h.app.mu.Lock()
	paths := strings.Join(h.app.paths, " ")
	h.app.mu.Unlock()
	for _, s := range []string{"20000/", "20001/ready", "https://hello.lwd.internal/", "https://api.hello.lwd.internal/ready"} {
		if !strings.Contains(paths, s) {
			t.Errorf("no probe %s in %s", s, paths)
		}
	}

	st := h.state()
	if st.Live == nil || st.Live.Deployment != 1 || st.Live.Project != p ||
		!reflect.DeepEqual(st.Live.Ports, map[string]int{"web": 20000, "api": 20001}) ||
		!reflect.DeepEqual(st.Live.Domains["api"], []string{"api.hello.lwd.internal"}) {
		t.Errorf("live = %+v", st.Live)
	}
	if len(st.History) != 1 || st.History[0].Status != bundle.StatusSucceeded {
		t.Errorf("history = %+v", st.History)
	}

	d := filepath.Join(h.dir, "apps", "hello-staging", "d1")
	for name, perm := range map[string]os.FileMode{"bundle.json": 0o600, environmentFile: 0o600, composeFile: 0o644, "result.json": 0o600} {
		info, err := os.Stat(filepath.Join(d, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if info.Mode().Perm() != perm {
			t.Errorf("%s mode = %v, want %v", name, info.Mode().Perm(), perm)
		}
	}
}

func TestDeploySecondDeployRetiresPrevious(t *testing.T) {
	h := newHarness(t)
	h.deploy(bundleN(1))
	res := h.deploy(bundleN(2))
	if res.Status != bundle.StatusSucceeded {
		t.Fatalf("result = %+v", res)
	}
	if !h.docker.has("lwd-hello-staging-d1", "down") {
		t.Error("previous project not downed")
	}
	if h.docker.has("lwd-hello-staging-d2", "down") {
		t.Error("new project downed")
	}
	st := h.state()
	// Both deployments run side by side during smoke, so ports must differ.
	if !reflect.DeepEqual(st.Live.Ports, map[string]int{"web": 20002, "api": 20003}) {
		t.Errorf("ports = %v", st.Live.Ports)
	}
	if !strings.Contains(h.caddy.last(), "reverse_proxy 127.0.0.1:20002") {
		t.Errorf("routes not switched:\n%s", h.caddy.last())
	}
	if len(st.History) != 2 || st.History[0].Deployment != 2 {
		t.Errorf("history = %+v", st.History)
	}
	// A third deploy may reuse the first deployment's freed ports.
	h.deploy(bundleN(3))
	if p := h.state().Live.Ports; p["web"] != 20000 {
		t.Errorf("third deploy ports = %v", p)
	}
}

func TestDeployReadyFailureLeavesPreviousUntouched(t *testing.T) {
	h := newHarness(t)
	h.deploy(bundleN(1))
	loads := h.caddy.count()
	h.app.set(20003, 503) // candidate api never becomes ready
	h.docker.respond = func(c call) (string, error, bool) {
		if c.Verb == "logs" && c.Project == "lwd-hello-staging-d2" {
			if !reflect.DeepEqual(c.Rest, []string{"--no-color", "--tail", "300"}) {
				t.Errorf("logs args = %v", c.Rest)
			}
			return "api-1 | connection refused to db", nil, true
		}
		return "", nil, false
	}
	b := bundleN(2)
	b.ReadyTimeoutSeconds = 1
	res := h.deploy(b)
	if res.Status != bundle.StatusFailed || res.Phase != "ready" {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Message, "api") {
		t.Errorf("message should name the unready service: %q", res.Message)
	}
	if !strings.Contains(res.Logs, "connection refused to db") {
		t.Errorf("logs = %q", res.Logs)
	}
	fl, _ := os.ReadFile(filepath.Join(h.dir, "apps", "hello-staging", "d2", "failure.log"))
	if !strings.Contains(string(fl), "connection refused to db") {
		t.Errorf("failure.log = %q", fl)
	}
	if !h.docker.has("lwd-hello-staging-d2", "down") {
		t.Error("candidate not downed")
	}
	if h.docker.has("lwd-hello-staging-d1", "down") {
		t.Error("previous deployment touched")
	}
	if h.caddy.count() != loads {
		t.Error("routes changed although the candidate never went live")
	}
	st := h.state()
	if st.Live.Deployment != 1 {
		t.Errorf("live = %+v", st.Live)
	}
	if st.History[0].Status != bundle.StatusFailed || st.History[0].Phase != "ready" {
		t.Errorf("history = %+v", st.History[0])
	}
	// The failed candidate's ports were released.
	h.app.set(20003, 200)
	h.deploy(bundleN(3))
	if p := h.state().Live.Ports; p["web"] != 20002 {
		t.Errorf("ports after failed deploy = %v", p)
	}
}

func TestDeployWorkerNotRunningFailsReady(t *testing.T) {
	h := newHarness(t)
	h.docker.respond = func(c call) (string, error, bool) {
		if c.Verb == "ps" {
			return `[{"Service":"worker","State":"exited","Status":"Exited (1)"}]`, nil, true
		}
		return "", nil, false
	}
	b := bundleN(1)
	b.ReadyTimeoutSeconds = 1
	res := h.deploy(b)
	if res.Status != bundle.StatusFailed || res.Phase != "ready" || !strings.Contains(res.Message, "worker") {
		t.Fatalf("result = %+v", res)
	}
	if st := h.state(); st.Live != nil {
		t.Errorf("failed first deploy left live = %+v", st.Live)
	}
}

func TestDeployMigrateFailure(t *testing.T) {
	h := newHarness(t)
	h.docker.respond = func(c call) (string, error, bool) {
		if c.Verb == "run" {
			return "relation users already exists", errors.New("exit status 1"), true
		}
		return "", nil, false
	}
	res := h.deploy(bundleN(1))
	if res.Status != bundle.StatusFailed || res.Phase != "migrate" {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Logs, "relation users already exists") {
		t.Errorf("migrate output not retained: %q", res.Logs)
	}
	if h.docker.has("lwd-hello-staging-d1", "up") {
		t.Error("candidate started after failed migration")
	}
}

func TestDeployPullFailure(t *testing.T) {
	h := newHarness(t)
	h.docker.respond = func(c call) (string, error, bool) {
		if c.Verb == "pull" {
			return "", errors.New("manifest unknown"), true
		}
		return "", nil, false
	}
	res := h.deploy(bundleN(1))
	if res.Status != bundle.StatusFailed || res.Phase != "pull" || !strings.Contains(res.Message, "manifest unknown") {
		t.Fatalf("result = %+v", res)
	}
}

func TestDeploySmokeFailureRevertsRoutes(t *testing.T) {
	h := newHarness(t)
	h.deploy(bundleN(1))
	before := h.caddy.last()
	// The candidate is ready on loopback but broken through Caddy.
	h.app.setDomain("api.hello.lwd.internal", 502)
	h.docker.respond = func(c call) (string, error, bool) {
		if c.Verb == "logs" {
			return "api-1 | 502 upstream", nil, true
		}
		return "", nil, false
	}
	b := bundleN(2)
	b.SmokeSeconds = 3
	res := h.deploy(b)
	if res.Status != bundle.StatusReverted || res.Phase != "smoke" {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Logs, "502 upstream") {
		t.Errorf("logs not retained: %q", res.Logs)
	}
	if h.caddy.last() != before {
		t.Errorf("routes not restored:\n%s\nwant:\n%s", h.caddy.last(), before)
	}
	if !h.docker.has("lwd-hello-staging-d2", "down") {
		t.Error("candidate not downed")
	}
	if h.docker.has("lwd-hello-staging-d1", "down") {
		t.Error("previous deployment downed")
	}
	st := h.state()
	if st.Live.Deployment != 1 || st.History[0].Status != bundle.StatusReverted {
		t.Errorf("state = %+v / %+v", st.Live, st.History[0])
	}
}

func TestDeploySmokeFailureOnFirstDeployRemovesRoutes(t *testing.T) {
	h := newHarness(t)
	h.app.setDomain("hello.lwd.internal", 500)
	res := h.deploy(bundleN(1))
	if res.Status != bundle.StatusReverted {
		t.Fatalf("result = %+v", res)
	}
	if strings.Contains(h.caddy.last(), "hello.lwd.internal") {
		t.Errorf("routes for a reverted first deploy remain:\n%s", h.caddy.last())
	}
}

func TestDeploySmokeChecksUnroutedServiceOnLoopback(t *testing.T) {
	h := newHarness(t)
	b := bundleN(1)
	b.Services[0].Domains = nil
	b.Services[0].Ready = "/healthz"
	h.app.set(20000, 200)
	res := h.deploy(b)
	if res.Status != bundle.StatusSucceeded {
		t.Fatalf("result = %+v", res)
	}
	if strings.Contains(h.caddy.last(), "20000") {
		t.Errorf("unrouted service got a route:\n%s", h.caddy.last())
	}
	h.app.mu.Lock()
	n := strings.Count(strings.Join(h.app.paths, " "), "20000/healthz")
	h.app.mu.Unlock()
	if n < 2 {
		t.Errorf("unrouted service probed %d times on loopback; want readiness plus smoke", n)
	}
}

func TestDeployCutoverLoadFailure(t *testing.T) {
	h := newHarness(t)
	h.deploy(bundleN(1))
	h.caddy.mu.Lock()
	h.caddy.fail = true
	h.caddy.mu.Unlock()
	res := h.deploy(bundleN(2))
	if res.Status != bundle.StatusFailed || res.Phase != "cutover" {
		t.Fatalf("result = %+v", res)
	}
	if !h.docker.has("lwd-hello-staging-d2", "down") || h.docker.has("lwd-hello-staging-d1", "down") {
		t.Errorf("calls = %q", h.docker.recorded())
	}
	cf, _ := os.ReadFile(filepath.Join(h.dir, "caddy", "etc", "Caddyfile"))
	if !strings.Contains(string(cf), "127.0.0.1:20000") || strings.Contains(string(cf), "127.0.0.1:20002") {
		t.Errorf("persisted Caddyfile not restored to the previous routes:\n%s", cf)
	}
}

func TestDeployBusy(t *testing.T) {
	h := newHarness(t)
	if !h.n.tryLock("hello-staging") {
		t.Fatal("lock")
	}
	b := bundleN(1)
	_, err := h.n.Deploy(context.Background(), &b)
	if !errors.Is(err, errBusy) {
		t.Fatalf("err = %v", err)
	}
	h.n.unlock("hello-staging")
	if res := h.deploy(b); res.Status != bundle.StatusSucceeded {
		t.Fatalf("after unlock: %+v", res)
	}
}

func TestConcurrentDeploysOfDifferentAppEnvs(t *testing.T) {
	h := newHarness(t)
	a, b := bundleN(1), bundleN(2)
	b.Env = "prod"
	b.Services[0].Domains = []string{"hello.example.com"}
	b.Services[1].Domains = []string{"api.hello.example.com"}
	results := make(chan *bundle.Result, 2)
	for _, x := range []bundle.Bundle{a, b} {
		go func() {
			res, err := h.n.Deploy(context.Background(), &x)
			if err != nil {
				t.Error(err)
			}
			results <- res
		}()
	}
	for range 2 {
		if res := <-results; res == nil || res.Status != bundle.StatusSucceeded {
			t.Fatalf("result = %+v", res)
		}
	}
	// Neither cutover may have dropped the other's routes, and no port is
	// shared between the two live deployments.
	cf := h.caddy.last()
	for _, d := range []string{"hello.lwd.internal", "api.hello.lwd.internal", "hello.example.com", "api.hello.example.com"} {
		if !strings.Contains(cf, d+" {") {
			t.Errorf("final Caddyfile lacks %s:\n%s", d, cf)
		}
	}
	seen := map[int]bool{}
	for _, key := range []string{"hello-staging", "hello-prod"} {
		st, _ := loadState(filepath.Join(h.dir, "apps", key, "state.json"))
		for _, p := range st.Live.Ports {
			if seen[p] {
				t.Errorf("port %d allocated twice", p)
			}
			seen[p] = true
		}
	}
}

func TestDeployRejectsInvalidBundles(t *testing.T) {
	h := newHarness(t)
	cases := map[string]func(b *bundle.Bundle){
		"validate":        func(b *bundle.Bundle) { b.Services[0].Image = "nginx:latest" },
		"reserved name":   func(b *bundle.Bundle) { b.Services[2].Name = migrateService },
		"domain":          func(b *bundle.Bundle) { b.Services[0].Domains = []string{"evil.com {\n}"} },
		"upper domain":    func(b *bundle.Bundle) { b.Services[0].Domains = []string{"Hello.lwd.internal"} },
		"ready path":      func(b *bundle.Bundle) { b.Services[0].Ready = "healthz" },
		"ready space":     func(b *bundle.Bundle) { b.Services[0].Ready = "/a b" },
		"var name":        func(b *bundle.Bundle) { b.Vars = map[string]string{"A=B": "x"} },
		"other app owner": func(b *bundle.Bundle) {},
	}
	// A domain already routed by another app-env must not be stolen.
	other := testBundle()
	other.App, other.Deployment = "other", 9
	other.Services = other.Services[:1]
	other.Services[0].Domains = []string{"taken.lwd.internal"}
	other.Migrate = nil
	if res := h.deploy(other); res.Status != bundle.StatusSucceeded {
		t.Fatalf("setup deploy: %+v", res)
	}
	cases["other app owner"] = func(b *bundle.Bundle) { b.Services[0].Domains = []string{"taken.lwd.internal"} }

	for name, mutate := range cases {
		b := bundleN(5)
		mutate(&b)
		_, err := h.n.Deploy(context.Background(), &b)
		var ie *invalidError
		if !errors.As(err, &ie) {
			t.Errorf("%s: err = %v, want invalidError", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(h.dir, "apps", "hello-staging", "d5")); err == nil {
		t.Error("rejected bundle left a deployment dir")
	}
}

func TestDeployRejectsRedeployOfLiveDeployment(t *testing.T) {
	h := newHarness(t)
	h.deploy(bundleN(1))
	b := bundleN(1)
	_, err := h.n.Deploy(context.Background(), &b)
	var ie *invalidError
	if !errors.As(err, &ie) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeployPrunesOldDirsKeepingLive(t *testing.T) {
	h := newHarness(t)
	h.deploy(bundleN(1))
	// Six failed attempts after d1: d1 is live and must survive pruning.
	h.docker.respond = func(c call) (string, error, bool) {
		if c.Verb == "pull" {
			return "", errors.New("nope"), true
		}
		return "", nil, false
	}
	for i := int64(2); i <= 7; i++ {
		h.deploy(bundleN(i))
	}
	for _, d := range []string{"d1", "d3", "d7"} {
		if _, err := os.Stat(filepath.Join(h.dir, "apps", "hello-staging", d)); err != nil {
			t.Errorf("%s pruned while it is live or among the 5 newest", d)
		}
	}
	if _, err := os.Stat(filepath.Join(h.dir, "apps", "hello-staging", "d2")); err == nil {
		t.Error("d2 kept although neither live nor among the 5 newest")
	}
	h.docker.respond = nil
	h.deploy(bundleN(8))
	entries, _ := os.ReadDir(filepath.Join(h.dir, "apps", "hello-staging"))
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	want := []string{"d4", "d5", "d6", "d7", "d8"}
	if !reflect.DeepEqual(dirs, want) {
		t.Fatalf("dirs = %v, want %v", dirs, want)
	}
}

func TestDeployTruncatesLogs(t *testing.T) {
	h := newHarness(t)
	big := strings.Repeat("x", 100<<10) + "THE END"
	h.docker.respond = func(c call) (string, error, bool) {
		if c.Verb == "pull" {
			return "", errors.New("nope"), true
		}
		if c.Verb == "logs" {
			return big, nil, true
		}
		return "", nil, false
	}
	res := h.deploy(bundleN(1))
	if len(res.Logs) > maxLogBytes+100 || !strings.HasSuffix(res.Logs, "THE END") {
		t.Fatalf("logs len %d, suffix %q", len(res.Logs), res.Logs[len(res.Logs)-10:])
	}
}
