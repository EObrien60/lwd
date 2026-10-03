package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lwd/internal/router"
)

// fakeDocker records docker invocations and answers them from a script.
type fakeDocker struct {
	mu    sync.Mutex
	calls []call
	// respond, if set, answers a call; returning handled=false falls back to
	// the default (success, empty output; ps reports every service running).
	respond func(c call) (out string, err error, handled bool)
}

// call is one docker invocation, split into the compose project and verb.
type call struct {
	Args    []string
	Project string
	Verb    string   // compose subcommand: pull, run, up, ps, logs, down, restart; or exec, network
	Rest    []string // arguments after the verb
	Stdin   string   // what a Stream call was fed
}

func (c call) String() string { return c.Project + " " + c.Verb + " " + strings.Join(c.Rest, " ") }

func parseCall(args []string) call {
	c := call{Args: args}
	i := 0
	if i < len(args) && args[i] == "compose" {
		i++
	}
	for i < len(args) {
		switch args[i] {
		case "-p":
			c.Project = args[i+1]
			i += 2
		case "-f", "--project-directory", "--profile":
			i += 2
		default:
			c.Verb = args[i]
			c.Rest = args[i+1:]
			return c
		}
	}
	return c
}

func (f *fakeDocker) Run(ctx context.Context, args ...string) ([]byte, error) {
	c := parseCall(args)
	f.mu.Lock()
	f.calls = append(f.calls, c)
	respond := f.respond
	f.mu.Unlock()
	if respond != nil {
		if out, err, ok := respond(c); ok {
			return []byte(out), err
		}
	}
	if c.Verb == "ps" {
		return []byte(`{"Service":"worker","State":"running"}` + "\n" + `{"Service":"web","State":"running","Health":"healthy"}`), nil
	}
	return nil, nil
}

// Stream records like Run (with stdin) and writes the answer to stdout.
func (f *fakeDocker) Stream(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	c := parseCall(args)
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		c.Stdin = string(b)
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	respond := f.respond
	f.mu.Unlock()
	if respond != nil {
		if out, err, ok := respond(c); ok {
			io.WriteString(stdout, out)
			return err
		}
	}
	return nil
}

// callsMatching returns the recorded calls for which keep is true.
func (f *fakeDocker) callsMatching(keep func(call) bool) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

var errFake = errors.New("fake docker failure")

func (f *fakeDocker) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.String())
	}
	return out
}

func (f *fakeDocker) has(project, verb string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.Project == project && c.Verb == verb {
			return true
		}
	}
	return false
}

// fakeCaddy is a Caddy admin API recording every /load.
type fakeCaddy struct {
	srv   *httptest.Server
	mu    sync.Mutex
	loads []string
	fail  bool
}

func newFakeCaddy(t *testing.T) *fakeCaddy {
	fc := &fakeCaddy{}
	fc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/load" {
			b, _ := io.ReadAll(r.Body)
			fc.mu.Lock()
			fc.loads = append(fc.loads, string(b))
			fail := fc.fail
			fc.mu.Unlock()
			if fail {
				http.Error(w, "load refused", http.StatusBadRequest)
			}
			return
		}
		w.Write([]byte("{}"))
	}))
	t.Cleanup(fc.srv.Close)
	return fc
}

func (fc *fakeCaddy) last() string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.loads) == 0 {
		return ""
	}
	return fc.loads[len(fc.loads)-1]
}

func (fc *fakeCaddy) count() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return len(fc.loads)
}

// app is a fake application: one handler answering both loopback readiness
// probes (per host port) and smoke probes through "Caddy" (per domain).
type app struct {
	mu sync.Mutex
	// status per host port (loopback) and per domain (through Caddy); default 200.
	byPort   map[int]int
	byDomain map[string]int
	loopback map[int]*httptest.Server
	tls      *httptest.Server
	paths    []string
}

func (a *app) set(port int, status int) {
	a.mu.Lock()
	a.byPort[port] = status
	a.mu.Unlock()
}

func (a *app) setDomain(d string, status int) {
	a.mu.Lock()
	a.byDomain[d] = status
	a.mu.Unlock()
}

type harness struct {
	t      *testing.T
	n      *Node
	docker *fakeDocker
	caddy  *fakeCaddy
	app    *app
	dir    string
}

// newHarness builds a Node over a temp dir with fake docker, fake Caddy and
// fake app servers. Host ports 20000-20009 are routed to per-port loopback
// test servers; https smoke probes reach one TLS test server.
func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	fd := &fakeDocker{}
	fc := newFakeCaddy(t)
	a := &app{byPort: map[int]int{}, byDomain: map[string]int{}, loopback: map[int]*httptest.Server{}}
	for p := 20000; p < 20010; p++ {
		port := p
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			a.mu.Lock()
			st, ok := a.byPort[port]
			a.paths = append(a.paths, fmt.Sprintf("%d%s", port, r.URL.Path))
			a.mu.Unlock()
			if !ok {
				st = 200
			}
			w.WriteHeader(st)
		}))
		t.Cleanup(srv.Close)
		a.loopback[port] = srv
	}
	a.tls = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		st, ok := a.byDomain[r.Host]
		a.paths = append(a.paths, "https://"+r.Host+r.URL.Path)
		a.mu.Unlock()
		if r.TLS == nil || r.TLS.ServerName != r.Host {
			st = 421 // SNI must name the domain
		} else if !ok {
			st = 200
		}
		w.WriteHeader(st)
	}))
	t.Cleanup(a.tls.Close)

	n := newNode(Config{Token: "secret", Dir: dir, CaddyImage: "caddy:2"}, fd)
	n.caddy = &router.Caddy{Path: filepath.Join(dir, "caddy", "etc", "Caddyfile"), AdminURL: fc.srv.URL, PollInterval: time.Millisecond}
	n.ports.min, n.ports.max = 20000, 20009
	n.ports.free = func(int) bool { return true }
	n.loopbackURL = func(port int) string {
		if s, ok := a.loopback[port]; ok {
			return s.URL
		}
		return "http://127.0.0.1:1"
	}
	n.httpsAddr = a.tls.Listener.Addr().String()
	n.readyInterval = 5 * time.Millisecond
	n.smokeInterval = time.Millisecond
	if err := n.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, n: n, docker: fd, caddy: fc, app: a, dir: dir}
}
