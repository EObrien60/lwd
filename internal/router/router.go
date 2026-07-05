package router

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"lwd/internal/node"
)

// networkName is the private Docker network all routed containers join.
const networkName = "lwd"

// caddyContainerName is the name of the system Caddy container lwd manages.
const caddyContainerName = "lwd-caddy"

// adminAddr is the address Caddy's admin API listens on INSIDE the container.
// It must bind all interfaces (not 127.0.0.1): Docker's port publishing
// delivers host-published traffic to the container's external-facing
// address, not to a loopback-bound socket in the container's network
// namespace, so a loopback bind here would make the admin API unreachable
// even from the host despite the port being published. Loopback-only access
// from the host is instead enforced one layer up, by node.RunContainer's
// host-port binding rules for non-80/443 ports (see defaultCaddyAdminBaseURL
// below, which the host process itself connects through).
//
// SECURITY NOTE: binding all interfaces also means the admin API is reachable
// on the "lwd" Docker network by every app container attached to it, not just
// from the host. Any deployed app can therefore reach 2019 on the Caddy
// container's network IP and rewrite routing for every other app. This is
// acceptable only under the single-operator/trusted-apps assumption for this
// milestone (see README's "Known limitations"); isolating the admin API from
// app containers (e.g. a second, app-inaccessible network for Caddy<->host,
// or an auth-gated admin listener) is a later hardening step.
const adminAddr = "0.0.0.0:2019"

// defaultCaddyAdminBaseURL is the default base URL for Caddy's admin API.
const defaultCaddyAdminBaseURL = "http://127.0.0.1:2019"

// defaultCaddyProxyBaseURL is the default base URL for traffic through
// Caddy's HTTP listener.
const defaultCaddyProxyBaseURL = "http://127.0.0.1:80"

// adminReadyTimeout bounds how long EnsureUp will poll Caddy's admin API
// waiting for it to accept connections after starting the container.
const adminReadyTimeout = 10 * time.Second

// adminReadyPollInterval is the delay between admin-readiness polls.
const adminReadyPollInterval = 250 * time.Millisecond

// Router owns the reverse proxy fronting all apps: it keeps a Caddyfile in
// sync with the desired route set and reloads Caddy via its admin API.
type Router interface {
	// EnsureUp makes sure the lwd network and the Caddy container are running,
	// and that Caddy is loaded with the current route set.
	EnsureUp(ctx context.Context) error
	// SetRoute installs or replaces the active route for r.Domain.
	SetRoute(ctx context.Context, r Route) error
	// RemoveRoute removes the active route for domain, if any.
	RemoveRoute(ctx context.Context, domain string) error
	// SetStaging exposes upstreams at host for out-of-band health probing
	// before it becomes the active route (used by blue-green staging).
	SetStaging(ctx context.Context, host string, upstreams []Upstream) error
	// RemoveStaging removes a staging route for host, if any.
	RemoveStaging(ctx context.Context, host string) error
	// ProbeThroughCaddy issues an HTTP GET through Caddy's public listener
	// with the given Host header, returning the response status code.
	ProbeThroughCaddy(ctx context.Context, host, path string) (status int, err error)
	// Reload regenerates the Caddyfile from current state and reloads Caddy.
	Reload(ctx context.Context) error
	// SeedRoutes replaces the in-memory active route set with routes, keyed by
	// Domain, WITHOUT reloading Caddy. It exists so a daemon restart can seed
	// reality (routes a still-running lwd-caddy container already has live)
	// before the startup EnsureUp->Reload runs, so that reload's atomic /load
	// installs the full correct set instead of a route-less one that would
	// otherwise drop every app's live route for the reload's duration.
	SeedRoutes(routes []Route)
	// Healthy reports whether Caddy is currently reachable/administrable via
	// its admin API. It performs no mutation; it is a point-in-time probe for
	// callers (e.g. a reconciler health report) to surface edge health.
	Healthy(ctx context.Context) bool
}

// CaddyRouter is the real Router implementation: it drives a Caddy container
// via the Node interface and reloads config through Caddy's admin API.
type CaddyRouter struct {
	node    node.Node
	dataDir string

	// adminBaseURL and proxyBaseURL are injectable so tests can point them at
	// an httptest.Server; they default to Caddy's real addresses.
	adminBaseURL string
	proxyBaseURL string

	httpClient *http.Client

	mu      sync.Mutex
	routes  map[string]Route // domain -> active route
	staging map[string]Route // host -> staging route
}

// NewCaddyRouter returns a CaddyRouter that manages the Caddy container via n
// and persists its generated Caddyfile under dataDir.
func NewCaddyRouter(n node.Node, dataDir string) *CaddyRouter {
	return &CaddyRouter{
		node:         n,
		dataDir:      dataDir,
		adminBaseURL: defaultCaddyAdminBaseURL,
		proxyBaseURL: defaultCaddyProxyBaseURL,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		routes:       map[string]Route{},
		staging:      map[string]Route{},
	}
}

var _ Router = (*CaddyRouter)(nil)

// EnsureUp ensures the lwd network exists and the lwd-caddy container is
// running, then reloads Caddy with the current route set.
func (c *CaddyRouter) EnsureUp(ctx context.Context) error {
	if err := c.node.EnsureNetwork(ctx, networkName); err != nil {
		return fmt.Errorf("router: ensure network: %w", err)
	}

	running, err := c.caddyRunning(ctx)
	if err != nil {
		return fmt.Errorf("router: check caddy container: %w", err)
	}
	if !running {
		if err := c.node.EnsureImage(ctx, "caddy:2"); err != nil {
			return fmt.Errorf("router: ensure caddy image: %w", err)
		}
		// The stock caddy:2 image boots with its own baked-in default
		// Caddyfile, whose admin API binds to "localhost:2019" — reachable
		// only from inside the container's own network namespace. Docker's
		// port publishing delivers host traffic to the container's
		// external-facing address, not to that loopback socket, so with the
		// image's default config the admin API would be unreachable from the
		// host even though its port is published — a chicken-and-egg problem,
		// since reaching the admin API is normally how we'd fix its bind
		// address. To break that, the container is started with a command
		// that first writes a minimal bootstrap Caddyfile (admin already
		// bound to adminAddr, i.e. all interfaces) to disk, then execs caddy
		// against it — so the admin API is reachable via the published port
		// from the very first instant the container runs, before Reload ever
		// POSTs the real route config to it.
		bootstrap := GenerateCaddyfile(adminAddr, nil)
		_, err := c.node.RunContainer(ctx, node.RunSpec{
			Name:    caddyContainerName,
			Image:   "caddy:2",
			Network: networkName,
			Env:     map[string]string{"LWD_BOOTSTRAP_CADDYFILE": bootstrap},
			Cmd: []string{"sh", "-c",
				`printf '%s' "$LWD_BOOTSTRAP_CADDYFILE" > /etc/caddy/Caddyfile && exec caddy run --config /etc/caddy/Caddyfile --adapter caddyfile`},
			Publish: []node.PortMapping{
				{HostPort: 80, ContainerPort: 80},
				{HostPort: 443, ContainerPort: 443},
				{HostPort: 2019, ContainerPort: 2019},
			},
			Labels: map[string]string{"lwd.role": "system"},
		})
		if err != nil {
			return fmt.Errorf("router: run caddy container: %w", err)
		}
	}

	if err := c.waitForAdminReady(ctx); err != nil {
		return err
	}

	return c.Reload(ctx)
}

// waitForAdminReady polls Caddy's admin API until it accepts connections or
// adminReadyTimeout elapses, returning a clear error if it never comes up.
// Caddy's admin listener may take a moment to bind after the container
// starts, so the first reload must not race it.
func (c *CaddyRouter) waitForAdminReady(ctx context.Context) error {
	deadline := time.Now().Add(adminReadyTimeout)
	var lastErr error
	for {
		if err := c.pingAdmin(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("router: caddy admin API at %s did not become ready within %s: %w", c.adminBaseURL, adminReadyTimeout, lastErr)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(adminReadyPollInterval):
		}
	}
}

// pingAdmin issues a single GET against the admin API's /config/ endpoint to
// check whether it is accepting connections. Any response, including a
// non-2xx one, counts as "up"; only transport-level failures are errors.
func (c *CaddyRouter) pingAdmin(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.adminBaseURL+"/config/", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// caddyRunning reports whether the lwd-caddy container already exists and is
// running.
func (c *CaddyRouter) caddyRunning(ctx context.Context) (bool, error) {
	containers, err := c.node.ListContainers(ctx, map[string]string{"lwd.role": "system"})
	if err != nil {
		return false, err
	}
	for _, ct := range containers {
		if ct.Name == caddyContainerName && ct.State == "running" {
			return true, nil
		}
	}
	return false, nil
}

// SetRoute installs or replaces the active route for r.Domain and reloads
// Caddy. The in-memory route map is only updated if the reload succeeds; on
// failure c.routes is left exactly as it was before the call.
func (c *CaddyRouter) SetRoute(ctx context.Context, r Route) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	routes := copyRoutes(c.routes)
	routes[r.Domain] = r

	if err := c.applyConfig(ctx, routes, c.staging); err != nil {
		return err
	}
	c.routes = routes
	return nil
}

// RemoveRoute removes the active route for domain, if any, and reloads
// Caddy. The in-memory route map is only updated if the reload succeeds; on
// failure c.routes is left exactly as it was before the call.
func (c *CaddyRouter) RemoveRoute(ctx context.Context, domain string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	routes := copyRoutes(c.routes)
	delete(routes, domain)

	if err := c.applyConfig(ctx, routes, c.staging); err != nil {
		return err
	}
	c.routes = routes
	return nil
}

// SetStaging exposes upstreams at host so they can be probed through Caddy
// ahead of cutover, and reloads Caddy. The in-memory staging map is only
// updated if the reload succeeds; on failure c.staging is left exactly as it
// was before the call.
func (c *CaddyRouter) SetStaging(ctx context.Context, host string, upstreams []Upstream) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	staging := copyRoutes(c.staging)
	staging[host] = Route{Domain: host, Upstreams: upstreams, TLSInternal: true}

	if err := c.applyConfig(ctx, c.routes, staging); err != nil {
		return err
	}
	c.staging = staging
	return nil
}

// RemoveStaging removes a staging route for host, if any, and reloads Caddy.
// The in-memory staging map is only updated if the reload succeeds; on
// failure c.staging is left exactly as it was before the call.
func (c *CaddyRouter) RemoveStaging(ctx context.Context, host string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	staging := copyRoutes(c.staging)
	delete(staging, host)

	if err := c.applyConfig(ctx, c.routes, staging); err != nil {
		return err
	}
	c.staging = staging
	return nil
}

// SeedRoutes replaces the committed in-memory route set with routes, keyed by
// Domain, without touching Caddy or the on-disk Caddyfile. Callers are
// responsible for reloading afterward (typically via EnsureUp/Reload) so the
// seeded set actually takes effect.
func (c *CaddyRouter) SeedRoutes(routes []Route) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := make(map[string]Route, len(routes))
	for _, r := range routes {
		m[r.Domain] = r
	}
	c.routes = m
}

// copyRoutes returns a shallow copy of a domain/host -> Route map, so
// mutators can build a candidate state without touching the committed one
// until a reload succeeds.
func copyRoutes(m map[string]Route) map[string]Route {
	out := make(map[string]Route, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Reload regenerates the Caddyfile from the current COMMITTED route+staging
// state and reloads Caddy with it. It does not change c.routes/c.staging —
// there is nothing to commit, since the state reloaded is whatever was
// already committed. On failure, that committed state (and the on-disk
// Caddyfile from the last successful reload) are left untouched.
func (c *CaddyRouter) Reload(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.applyConfig(ctx, c.routes, c.staging)
}

// applyConfig assembles routes+staging into a Caddyfile, POSTs it to Caddy's
// admin API, and only then writes it to disk. It never mutates c.routes or
// c.staging; callers decide whether/how to commit the candidate maps once
// applyConfig returns nil. Callers must hold c.mu.
func (c *CaddyRouter) applyConfig(ctx context.Context, routes map[string]Route, staging map[string]Route) error {
	list := make([]Route, 0, len(routes)+len(staging))
	for _, r := range routes {
		list = append(list, r)
	}
	for _, r := range staging {
		list = append(list, r)
	}

	caddyfile := GenerateCaddyfile(adminAddr, list)

	if err := c.postConfig(ctx, caddyfile); err != nil {
		return fmt.Errorf("router: reload caddy: %w", err)
	}

	if c.dataDir != "" {
		if err := os.MkdirAll(c.dataDir, 0o755); err != nil {
			return fmt.Errorf("router: create data dir: %w", err)
		}
		path := filepath.Join(c.dataDir, "Caddyfile")
		if err := os.WriteFile(path, []byte(caddyfile), 0o644); err != nil {
			return fmt.Errorf("router: write caddyfile: %w", err)
		}
	}

	return nil
}

// postConfig sends caddyfile to Caddy's admin /load endpoint. On a non-2xx
// response it returns an error without mutating any state (the caller has
// not yet committed anything on failure).
func (c *CaddyRouter) postConfig(ctx context.Context, caddyfile string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.adminBaseURL+"/load", bytes.NewReader([]byte(caddyfile)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/caddyfile")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("caddy admin /load returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// ProbeThroughCaddy issues a GET through Caddy's public HTTP listener with
// the given Host header, returning the response status code. err is only
// set on a transport failure, not on a non-2xx response.
func (c *CaddyRouter) ProbeThroughCaddy(ctx context.Context, host, path string) (int, error) {
	if path == "" {
		path = "/"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.proxyBaseURL+path, nil)
	if err != nil {
		return 0, err
	}
	req.Host = host

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// Healthy reports whether Caddy's admin API is reachable and returning a 2xx
// response right now. Any transport error or non-2xx status is treated as
// unhealthy; it performs no mutation.
func (c *CaddyRouter) Healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.adminBaseURL+"/config/", nil)
	if err != nil {
		return false
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
