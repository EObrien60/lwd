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
	"os"
	"strings"

	"lwd/internal/api"
	"lwd/internal/config"
	"lwd/internal/reconciler"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// Client talks to the lwd daemon, either over a local unix socket (New) or
// over TCP with bearer-token auth (NewHTTP) — see FromEnv for how the
// CLI/web frontends pick between the two.
type Client struct {
	http *http.Client
	base string
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
		base: "http://lwd",
	}
}

// bearerTransport wraps an http.RoundTripper, adding an Authorization:
// Bearer <token> header to every outgoing request. It clones the request
// before mutating it, per http.RoundTripper's contract that RoundTrip must
// not modify the request.
type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(req)
}

// NewHTTP returns a Client that talks to the daemon's TCP API listener at
// baseURL, authenticating with token (sent as "Authorization: Bearer
// <token>" on every request). baseURL may be a bare host:port (e.g.
// "10.0.0.5:8077"), in which case "http://" is assumed, or a full
// "http(s)://..." URL; any trailing slash is trimmed. If token is empty, no
// Authorization header is sent (matching an unauthenticated daemon).
//
// No client-wide timeout is set — same as New — so a long-lived streaming
// request (e.g. Logs with follow=true) is bounded only by its context, never
// killed out from under it by an http.Client deadline.
func NewHTTP(baseURL, token string) *Client {
	base := strings.TrimSuffix(baseURL, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}

	var transport http.RoundTripper = &http.Transport{}
	if token != "" {
		transport = &bearerTransport{base: transport, token: token}
	}

	return &Client{
		http: &http.Client{Transport: transport},
		base: base,
	}
}

// firstNonEmpty returns the first non-empty string among vs, or "" if all
// are empty.
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// FromEnv builds a Client from environment configuration, preferring a
// remote TCP daemon over the local unix socket:
//
//   - LWD_DAEMON set: dial it over TCP via NewHTTP, authenticating with
//     LWD_API_TOKEN (may be empty for an unauthenticated daemon).
//   - LWD_DAEMON unset: dial the local unix socket via New, using
//     LWD_SOCKET if set, else the default socket path from internal/config.
func FromEnv() *Client {
	if daemon := os.Getenv("LWD_DAEMON"); daemon != "" {
		return NewHTTP(daemon, os.Getenv("LWD_API_TOKEN"))
	}
	return New(firstNonEmpty(os.Getenv("LWD_SOCKET"), config.SocketPath()))
}

func (c *Client) url(path string) string { return c.base + path }

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

// Scale changes name's replica count and redeploys it set-based, returning
// the resulting deployment. It reuses the app's current spec snapshot
// (image/domain/port/etc unchanged) — only Replicas changes.
func (c *Client) Scale(ctx context.Context, name string, replicas int) (*store.Deployment, error) {
	body, err := json.Marshal(map[string]int{"replicas": replicas})
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/apps/"+name+"/scale"), bytes.NewReader(body))
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

// Uncordon clears a node's cordon, making it eligible for scheduler
// placement again. It never touches anything already deployed on it.
func (c *Client) Uncordon(ctx context.Context, name string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/nodes/"+name+"/uncordon"), nil)
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

// Evacuate moves every scheduler-placed surface off name onto some other
// fitting node, without changing name's schedulable bit.
func (c *Client) Evacuate(ctx context.Context, name string) (reconciler.EvacuateResult, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/nodes/"+name+"/evacuate"), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return reconciler.EvacuateResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return reconciler.EvacuateResult{}, decodeErr(resp)
	}
	var res reconciler.EvacuateResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return reconciler.EvacuateResult{}, err
	}
	return res, nil
}

// Drain cordons name (excluding it from future scheduler placement) THEN
// evacuates every scheduler-placed surface off it.
func (c *Client) Drain(ctx context.Context, name string) (reconciler.EvacuateResult, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/nodes/"+name+"/drain"), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return reconciler.EvacuateResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return reconciler.EvacuateResult{}, decodeErr(resp)
	}
	var res reconciler.EvacuateResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return reconciler.EvacuateResult{}, err
	}
	return res, nil
}
