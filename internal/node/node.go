// Package node is the lwd node agent: it executes deployment bundles on the
// local host with Docker Compose and Caddy, and keeps enough on-disk state to
// restore every app after a reboot without the controller. See
// docs/lwd2/DESIGN.md ("Node") and docs/lwd2/NODE.md.
package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"lwd/internal/bundle"
	"lwd/internal/router"
)

// Config is the node's environment-supplied configuration.
type Config struct {
	Addr       string // LWD_NODE_ADDR
	Token      string // LWD_NODE_TOKEN
	Dir        string // LWD_NODE_DIR
	CaddyImage string // LWD_CADDY_IMAGE
	ACMEEmail  string // LWD_ACME_EMAIL, optional
}

// ConfigFromEnv reads Config from the environment, applying defaults.
func ConfigFromEnv() (Config, error) {
	c := Config{
		Addr:       envOr("LWD_NODE_ADDR", "0.0.0.0:7480"),
		Token:      os.Getenv("LWD_NODE_TOKEN"),
		Dir:        envOr("LWD_NODE_DIR", "/srv/lwd"),
		CaddyImage: envOr("LWD_CADDY_IMAGE", "caddy:2"),
		ACMEEmail:  os.Getenv("LWD_ACME_EMAIL"),
	}
	if c.Token == "" {
		return c, errors.New("LWD_NODE_TOKEN is required")
	}
	if !filepath.IsAbs(c.Dir) {
		return c, fmt.Errorf("LWD_NODE_DIR must be absolute, got %q", c.Dir)
	}
	if c.ACMEEmail != "" && !router.ValidEmail(c.ACMEEmail) {
		return c, fmt.Errorf("LWD_ACME_EMAIL %q is not a usable address", c.ACMEEmail)
	}
	return c, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Main runs `lwd node` with the remaining arguments.
func Main(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected arguments %q; lwd node is configured by LWD_NODE_* environment variables", args)
	}
	cfg, err := ConfigFromEnv()
	if err != nil {
		return err
	}
	n := newNode(cfg, execRunner{})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := n.Start(ctx); err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           n.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: /v1/deploy is synchronous and may run for minutes.
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("lwd node listening on %s, dir %s", cfg.Addr, cfg.Dir)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// Let in-flight deploys finish their current step rather than abandoning
	// a half-cut-over app; systemd's stop timeout bounds this.
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// Node executes bundles on this host.
type Node struct {
	cfg     Config
	docker  Runner
	caddy   *router.Caddy
	ports   *portAllocator
	started time.Time

	// Probe plumbing, replaceable in tests.
	loopbackURL   func(port int) string // base URL of a host port on loopback
	httpsAddr     string                // where smoke probes dial: Caddy's :443
	readyInterval time.Duration
	smokeInterval time.Duration
	// How long a newly routed domain may take to get its certificate.
	certTimeoutInternal time.Duration
	certTimeoutACME     time.Duration
	handshake           func(ctx context.Context, domain string) error // replaced in tests
	adminTimeout        time.Duration
	procDir             string // /proc, for host facts

	lockMu sync.Mutex
	busy   map[string]bool // app-env keys with an operation in progress

	// routes is the routing table Caddy is serving, by app-env key. It
	// starts as the persisted live deployments and, during a deploy's smoke
	// window, holds the candidate. Guarded by routeMu, which also serialises
	// Caddyfile writes so concurrent app-envs cannot interleave.
	routeMu sync.Mutex
	routes  map[string]*Live
}

func newNode(cfg Config, docker Runner) *Node {
	n := &Node{
		cfg:    cfg,
		docker: docker,
		caddy: &router.Caddy{
			Path:     filepath.Join(cfg.Dir, "caddy", "etc", "Caddyfile"),
			AdminURL: router.DefaultAdminURL,
		},
		ports:               &portAllocator{min: 20000, max: 29999, free: portBindable},
		started:             time.Now(),
		loopbackURL:         func(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) },
		httpsAddr:           "127.0.0.1:443",
		readyInterval:       500 * time.Millisecond,
		smokeInterval:       time.Second,
		certTimeoutInternal: 30 * time.Second,
		certTimeoutACME:     3 * time.Minute,
		adminTimeout:        60 * time.Second,
		procDir:             "/proc",
		busy:                map[string]bool{},
		routes:              map[string]*Live{},
	}
	n.handshake = n.tlsHandshake
	return n
}

func (n *Node) appsDir() string          { return filepath.Join(n.cfg.Dir, "apps") }
func (n *Node) appDir(key string) string { return filepath.Join(n.appsDir(), key) }
func (n *Node) statePath(key string) string {
	return filepath.Join(n.appDir(key), "state.json")
}

func (n *Node) systemProject() project {
	dir := filepath.Join(n.cfg.Dir, "system")
	return project{name: systemProject, dir: dir, files: []string{filepath.Join(dir, composeFile)}}
}

func (n *Node) ensureDirs() error {
	for _, d := range []string{"system", "caddy/etc", "caddy/data", "caddy/config"} {
		if err := os.MkdirAll(filepath.Join(n.cfg.Dir, d), 0o755); err != nil {
			return err
		}
	}
	// apps/ holds secrets (bundle.json, environment.json).
	return os.MkdirAll(n.appsDir(), 0o700)
}

// Start brings the node to its persisted state: dirs, the lwd-system Caddy
// project, and a Caddyfile rebuilt from every app-env's state.json. Every
// step is idempotent, so a restarted node converges to the same result.
func (n *Node) Start(ctx context.Context) error {
	if err := n.ensureDirs(); err != nil {
		return err
	}
	sys := n.systemProject()
	if err := router.WriteFileAtomic(sys.files[0], renderSystemCompose(n.cfg.Dir, n.cfg.CaddyImage), 0o644); err != nil {
		return err
	}
	states, err := n.loadStates()
	if err != nil {
		return err
	}
	n.routeMu.Lock()
	defer n.routeMu.Unlock()
	n.routes = map[string]*Live{}
	for key, st := range states {
		if st.Live != nil {
			n.routes[key] = st.Live
		}
	}
	content := n.caddyfileLocked()
	// Written before Caddy starts so a fresh container boots straight into
	// the right routes.
	if err := router.WriteFileAtomic(n.caddy.Path, []byte(content), 0o644); err != nil {
		return err
	}
	if _, err := n.docker.Run(ctx, sys.args("up", "-d")...); err != nil {
		return fmt.Errorf("start caddy: %w", err)
	}
	if err := n.caddy.WaitAdmin(ctx, n.adminTimeout); err != nil {
		return err
	}
	// An already-running Caddy kept its old in-memory config across `up -d`.
	return n.caddy.Load(ctx, content)
}

// loadStates reads every apps/<key>/state.json.
func (n *Node) loadStates() (map[string]*State, error) {
	entries, err := os.ReadDir(n.appsDir())
	if err != nil {
		return nil, err
	}
	out := map[string]*State{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := n.statePath(e.Name())
		if _, err := os.Stat(path); err != nil {
			continue
		}
		st, err := loadState(path)
		if err != nil {
			return nil, err
		}
		out[e.Name()] = st
	}
	return out, nil
}

// caddyfileLocked renders the routing table. Caller holds routeMu.
func (n *Node) caddyfileLocked() string {
	var routes []router.Route
	for _, live := range n.routes {
		for svc, domains := range live.Domains {
			for _, d := range domains {
				routes = append(routes, router.Route{
					Domain:      d,
					Upstream:    fmt.Sprintf("127.0.0.1:%d", live.Ports[svc]),
					TLSInternal: live.TLS == bundle.TLSInternal,
				})
			}
		}
	}
	return router.GenerateCaddyfile(router.Global{Admin: router.AdminAddr, Email: n.cfg.ACMEEmail}, routes)
}

// setRoutes points key's domains at live (nil removes them) and applies the
// result to Caddy. If Caddy rejects it, the previous table is restored and
// re-applied, so on error Caddy and the persisted Caddyfile are unchanged.
func (n *Node) setRoutes(ctx context.Context, key string, live *Live) error {
	n.routeMu.Lock()
	defer n.routeMu.Unlock()
	prev, had := n.routes[key]
	n.put(key, live)
	err := n.caddy.Apply(ctx, n.caddyfileLocked())
	if err == nil {
		return nil
	}
	if had {
		n.put(key, prev)
	} else {
		n.put(key, nil)
	}
	if rerr := n.caddy.Apply(ctx, n.caddyfileLocked()); rerr != nil {
		return fmt.Errorf("%w (restoring previous routes also failed: %v)", err, rerr)
	}
	return err
}

func (n *Node) put(key string, live *Live) {
	if live == nil {
		delete(n.routes, key)
	} else {
		n.routes[key] = live
	}
}

// domainOwner returns the app-env currently routing domain, if any.
func (n *Node) domainOwner(domain string) string {
	n.routeMu.Lock()
	defer n.routeMu.Unlock()
	keys := make([]string, 0, len(n.routes))
	for k := range n.routes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, ds := range n.routes[k].Domains {
			for _, d := range ds {
				if d == domain {
					return k
				}
			}
		}
	}
	return ""
}

// usedPorts is every host port held by a routed or persisted live deployment.
// Persisted state matters during a smoke window, when the table holds the
// candidate but the previous deployment is still running.
func (n *Node) usedPorts() (map[int]bool, error) {
	used := map[int]bool{}
	n.routeMu.Lock()
	for _, live := range n.routes {
		for _, p := range live.Ports {
			used[p] = true
		}
	}
	n.routeMu.Unlock()
	states, err := n.loadStates()
	if err != nil {
		return nil, err
	}
	for _, st := range states {
		if st.Live != nil {
			for _, p := range st.Live.Ports {
				used[p] = true
			}
		}
	}
	return used, nil
}

func (n *Node) tryLock(key string) bool {
	n.lockMu.Lock()
	defer n.lockMu.Unlock()
	if n.busy[key] {
		return false
	}
	n.busy[key] = true
	return true
}

func (n *Node) unlock(key string) {
	n.lockMu.Lock()
	defer n.lockMu.Unlock()
	delete(n.busy, key)
}
