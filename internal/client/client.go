// Package client is an HTTP client for the daemon's unix-socket API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"lwd/internal/api"
	"lwd/internal/reconciler"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// Client talks to the lwd daemon over a unix socket.
type Client struct {
	http *http.Client
}

// New returns a Client that dials the given unix socket path. The host in URLs
// is a dummy; the dialer always connects to the socket.
func New(socketPath string) *Client {
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

func (c *Client) url(path string) string { return "http://lwd" + path }

func decodeErr(resp *http.Response) error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Error == "" {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return fmt.Errorf("%s", e.Error)
}

// Apply deploys an app and returns the resulting deployment.
func (c *Client) Apply(ctx context.Context, app *spec.App) (*store.Deployment, error) {
	body, err := json.Marshal(app)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/apply"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp)
	}
	var dep store.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&dep); err != nil {
		return nil, err
	}
	return &dep, nil
}

// Rollback redeploys the previous deployment for name and returns the
// resulting deployment.
func (c *Client) Rollback(ctx context.Context, name string) (*store.Deployment, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/apps/"+name+"/rollback"), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp)
	}
	var dep store.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&dep); err != nil {
		return nil, err
	}
	return &dep, nil
}

// Apps lists apps and their statuses.
func (c *Client) Apps(ctx context.Context) ([]api.AppStatus, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/apps"), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp)
	}
	var apps []api.AppStatus
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return nil, err
	}
	return apps, nil
}

// History returns all recorded deployments for name, newest first.
func (c *Client) History(ctx context.Context, name string) ([]store.Deployment, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/apps/"+name+"/history"), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp)
	}
	var deps []store.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&deps); err != nil {
		return nil, err
	}
	return deps, nil
}

// Logs streams an app's logs to w.
func (c *Client) Logs(ctx context.Context, name string, follow bool, w io.Writer) error {
	u := c.url("/apps/" + name + "/logs")
	if follow {
		u += "?follow=true"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeErr(resp)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// Remove stops and deregisters an app.
func (c *Client) Remove(ctx context.Context, name string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, c.url("/apps/"+name), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return decodeErr(resp)
	}
	return nil
}

// SetSecret sets (or overwrites) a secret value for app. The value never
// appears in any response.
func (c *Client) SetSecret(ctx context.Context, app, key, value string) error {
	body, err := json.Marshal(map[string]string{"key": key, "value": value})
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/apps/"+app+"/secrets"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return decodeErr(resp)
	}
	return nil
}

// ListSecrets returns the secret names set for app (never values).
func (c *Client) ListSecrets(ctx context.Context, app string) ([]string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/apps/"+app+"/secrets"), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp)
	}
	var names []string
	if err := json.NewDecoder(resp.Body).Decode(&names); err != nil {
		return nil, err
	}
	return names, nil
}

// DeleteSecret removes a secret from app.
func (c *Client) DeleteSecret(ctx context.Context, app, key string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, c.url("/apps/"+app+"/secrets/"+key), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return decodeErr(resp)
	}
	return nil
}

// NodeStatus is a registered node plus its live reachability, as reported by
// the daemon's GET /nodes. It mirrors the api package's private
// nodeStatusResponse — the HTTP round-trip between the two is covered by api
// tests.
type NodeStatus struct {
	store.Node
	Transport string `json:"transport"`
	Reachable bool   `json:"reachable"`
}

// AddNode registers (or updates) a node in the daemon's registry. agentURL
// may be empty (no lwd-agent registered for this node); if non-empty, the
// daemon validates it is a well-formed http(s) URL. pool may be empty, in
// which case the daemon defaults it to "default".
func (c *Client) AddNode(ctx context.Context, name, sshHost, meshAddr, agentURL, pool string) error {
	body, err := json.Marshal(map[string]string{
		"name": name, "ssh_host": sshHost, "mesh_addr": meshAddr, "agent_url": agentURL, "pool": pool,
	})
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/nodes"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return decodeErr(resp)
	}
	return nil
}

// Nodes lists all registered nodes along with their live reachability.
func (c *Client) Nodes(ctx context.Context) ([]NodeStatus, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/nodes"), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp)
	}
	var nodes []NodeStatus
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

// Pool is a node pool and the number of registered nodes in it, as reported
// by the daemon's GET /pools.
type Pool struct {
	Name  string `json:"name"`
	Nodes int    `json:"nodes"`
}

// Pools lists every pool with a registered node in it, plus the count of
// nodes in each. "default" is always present, even with zero registered
// nodes, since the implicit local node lives there.
func (c *Client) Pools(ctx context.Context) ([]Pool, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/pools"), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp)
	}
	var pools []Pool
	if err := json.NewDecoder(resp.Body).Decode(&pools); err != nil {
		return nil, err
	}
	return pools, nil
}

// Health returns the daemon's current reconciler health snapshot: per-node
// reachability, the shared edge (router), and per-app surface health.
func (c *Client) Health(ctx context.Context) (reconciler.Health, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/health"), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return reconciler.Health{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return reconciler.Health{}, decodeErr(resp)
	}
	var h reconciler.Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return reconciler.Health{}, err
	}
	return h, nil
}

// RemoveNode deregisters a node from the daemon's registry.
func (c *Client) RemoveNode(ctx context.Context, name string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, c.url("/nodes/"+name), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return decodeErr(resp)
	}
	return nil
}
