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

// adminAddr is the address Caddy's admin API listens on, bound to loopback
// only (see node.RunContainer's host-port binding rules for non-80/443 ports).
const adminAddr = "127.0.0.1:2019"

// caddyAdminBaseURL is the base URL for Caddy's admin API.
const caddyAdminBaseURL = "http://127.0.0.1:2019"

// caddyProxyBaseURL is the base URL for traffic through Caddy's HTTP listener.
const caddyProxyBaseURL = "http://127.0.0.1:80"

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
	// SetStaging exposes upstream:port at host for out-of-band health probing
	// before it becomes the active route (used by blue-green staging).
	SetStaging(ctx context.Context, host, upstream string, port int) error
	// RemoveStaging removes a staging route for host, if any.
	RemoveStaging(ctx context.Context, host string) error
	// ProbeThroughCaddy issues an HTTP GET through Caddy's public listener
	// with the given Host header, returning the response status code.
	ProbeThroughCaddy(ctx context.Context, host, path string) (status int, err error)
	// Reload regenerates the Caddyfile from current state and reloads Caddy.
	Reload(ctx context.Context) error
}

// CaddyRouter is the real Router implementation: it drives a Caddy container
// via the Node interface and reloads config through Caddy's admin API.
type CaddyRouter struct {
	node    node.Node
	dataDir string

	httpClient *http.Client

	mu      sync.Mutex
	routes  map[string]Route // domain -> active route
	staging map[string]Route // host -> staging route
}

// NewCaddyRouter returns a CaddyRouter that manages the Caddy container via n
// and persists its generated Caddyfile under dataDir.
func NewCaddyRouter(n node.Node, dataDir string) *CaddyRouter {
	return &CaddyRouter{
		node:       n,
		dataDir:    dataDir,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		routes:     map[string]Route{},
		staging:    map[string]Route{},
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
		_, err := c.node.RunContainer(ctx, node.RunSpec{
			Name:    caddyContainerName,
			Image:   "caddy:2",
			Network: networkName,
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

	return c.Reload(ctx)
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

// SetRoute installs or replaces the active route for r.Domain and reloads Caddy.
func (c *CaddyRouter) SetRoute(ctx context.Context, r Route) error {
	c.mu.Lock()
	c.routes[r.Domain] = r
	c.mu.Unlock()
	return c.Reload(ctx)
}

// RemoveRoute removes the active route for domain, if any, and reloads Caddy.
func (c *CaddyRouter) RemoveRoute(ctx context.Context, domain string) error {
	c.mu.Lock()
	delete(c.routes, domain)
	c.mu.Unlock()
	return c.Reload(ctx)
}

// SetStaging exposes upstream:port at host so it can be probed through Caddy
// ahead of cutover, and reloads Caddy.
func (c *CaddyRouter) SetStaging(ctx context.Context, host, upstream string, port int) error {
	c.mu.Lock()
	c.staging[host] = Route{Domain: host, Upstream: upstream, Port: port, TLSInternal: true}
	c.mu.Unlock()
	return c.Reload(ctx)
}

// RemoveStaging removes a staging route for host, if any, and reloads Caddy.
func (c *CaddyRouter) RemoveStaging(ctx context.Context, host string) error {
	c.mu.Lock()
	delete(c.staging, host)
	c.mu.Unlock()
	return c.Reload(ctx)
}

// Reload regenerates the Caddyfile from the current route+staging state,
// writes it to disk, and POSTs it to Caddy's admin API. On failure the
// in-memory state (and on-disk file) from before this call are left intact.
func (c *CaddyRouter) Reload(ctx context.Context) error {
	c.mu.Lock()
	routes := make([]Route, 0, len(c.routes)+len(c.staging))
	for _, r := range c.routes {
		routes = append(routes, r)
	}
	for _, r := range c.staging {
		routes = append(routes, r)
	}
	c.mu.Unlock()

	caddyfile := GenerateCaddyfile(adminAddr, routes)

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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, caddyAdminBaseURL+"/load", bytes.NewReader([]byte(caddyfile)))
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, caddyProxyBaseURL+path, nil)
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
