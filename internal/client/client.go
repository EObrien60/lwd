// Package client is the Go client for the controller's /v1 API and defines
// its composite wire types. Row-shaped responses (hosts, releases,
// deployments, secrets metadata, events) reuse the store types, whose JSON
// form deliberately omits node tokens and secret values.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"lwd/internal/store"
)

// DefaultURL is where `lwd controller` listens by default.
const DefaultURL = "http://127.0.0.1:7470"

// Error codes carried in {"error":{"code","message"}}.
const (
	CodeConflict        = "conflict"
	CodeNotFound        = "not_found"
	CodeInvalid         = "invalid"
	CodeNodeUnreachable = "node_unreachable"
	CodeUnauthorized    = "unauthorized"
	CodeInternal        = "internal"
)

// ErrorBody is the JSON shape of every /v1 error response.
type ErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Error is a /v1 error response.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// HostRequest registers (or re-registers) a node.
type HostRequest struct {
	Name  string `json:"name"`
	Addr  string `json:"addr"`
	Token string `json:"token"`
}

// HostStatus is a host plus its node's live /v1/status.
type HostStatus struct {
	store.Host
	Reachable bool            `json:"reachable"`
	Node      json.RawMessage `json:"node,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// AppDetail is an app, its environments and, after apply, any warnings.
type AppDetail struct {
	store.App
	Environments []store.Environment `json:"environments"`
	Warnings     []string            `json:"warnings,omitempty"`
}

// ReleaseRequest creates a release. Each service's image is images[svc] if
// given, else "<manifest image>:<tag>".
type ReleaseRequest struct {
	Commit string            `json:"commit,omitempty"`
	Tag    string            `json:"tag,omitempty"`
	Images map[string]string `json:"images,omitempty"`
}

// DeployRequest deploys a release (0 = newest).
type DeployRequest struct {
	Release int64  `json:"release,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// RollbackRequest rolls back to a release (0 = previous live release).
type RollbackRequest struct {
	To     int64  `json:"to,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// RestartRequest restarts one service, or all when empty.
type RestartRequest struct {
	Service string `json:"service,omitempty"`
}

// EnvStatus merges the controller's view of an app environment with the
// node's live view of it.
type EnvStatus struct {
	App       string            `json:"app"`
	Env       string            `json:"env"`
	Host      string            `json:"host"`
	Domain    string            `json:"domain"`
	TLS       string            `json:"tls"`
	Live      *store.Deployment `json:"live,omitempty"`
	Last      *store.Deployment `json:"last,omitempty"`
	Node      json.RawMessage   `json:"node,omitempty"`
	NodeError string            `json:"node_error,omitempty"`
}

// Client calls one controller.
type Client struct {
	BaseURL string
	Token   string
	Actor   string // sent as X-LWD-Actor for the audit trail
	HTTP    *http.Client
}

// New returns a client for baseURL authenticating with token.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{}}
}

// FromEnv configures a client from LWD_URL and LWD_TOKEN, falling back to
// the token file $XDG_CONFIG_HOME/lwd/token (default ~/.config/lwd/token).
func FromEnv(getenv func(string) string) (*Client, error) {
	base := getenv("LWD_URL")
	if base == "" {
		base = DefaultURL
	}
	token := strings.TrimSpace(getenv("LWD_TOKEN"))
	if token == "" {
		path := filepath.Join(ConfigDir(getenv), "token")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("no API token: set LWD_TOKEN or write it to %s", path)
		}
		token = strings.TrimSpace(string(data))
		if token == "" {
			return nil, fmt.Errorf("no API token: %s is empty", path)
		}
	}
	return New(base, token), nil
}

// ConfigDir is $XDG_CONFIG_HOME/lwd, or ~/.config/lwd.
func ConfigDir(getenv func(string) string) string {
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "lwd")
	}
	return filepath.Join(getenv("HOME"), ".config", "lwd")
}

func p(parts ...string) string {
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return "/v1/" + strings.Join(parts, "/")
}

// AddHost registers a node.
func (c *Client) AddHost(ctx context.Context, r HostRequest) (store.Host, error) {
	var h store.Host
	err := c.call(ctx, http.MethodPost, "/v1/hosts", r, &h)
	return h, err
}

// ListHosts lists nodes.
func (c *Client) ListHosts(ctx context.Context) ([]store.Host, error) {
	var hs []store.Host
	err := c.call(ctx, http.MethodGet, "/v1/hosts", nil, &hs)
	return hs, err
}

// GetHost returns a host with live node status.
func (c *Client) GetHost(ctx context.Context, name string) (HostStatus, error) {
	var h HostStatus
	err := c.call(ctx, http.MethodGet, p("hosts", name), nil, &h)
	return h, err
}

// ApplyApp creates or updates an app from lwd.toml text.
func (c *Client) ApplyApp(ctx context.Context, name string, manifest []byte) (AppDetail, error) {
	var a AppDetail
	err := c.call(ctx, http.MethodPut, p("apps", name), rawBody(manifest), &a)
	return a, err
}

// ListApps lists apps.
func (c *Client) ListApps(ctx context.Context) ([]AppDetail, error) {
	var as []AppDetail
	err := c.call(ctx, http.MethodGet, "/v1/apps", nil, &as)
	return as, err
}

// GetApp returns an app with its manifest and environments.
func (c *Client) GetApp(ctx context.Context, name string) (AppDetail, error) {
	var a AppDetail
	err := c.call(ctx, http.MethodGet, p("apps", name), nil, &a)
	return a, err
}

// CreateRelease pins images and records a release.
func (c *Client) CreateRelease(ctx context.Context, app string, r ReleaseRequest) (store.Release, error) {
	var rel store.Release
	err := c.call(ctx, http.MethodPost, p("apps", app, "releases"), r, &rel)
	return rel, err
}

// ListReleases lists an app's releases, newest first.
func (c *Client) ListReleases(ctx context.Context, app string) ([]store.Release, error) {
	var rs []store.Release
	err := c.call(ctx, http.MethodGet, p("apps", app, "releases"), nil, &rs)
	return rs, err
}

// Deploy deploys and waits for the outcome.
func (c *Client) Deploy(ctx context.Context, app, env string, r DeployRequest) (store.Deployment, error) {
	var d store.Deployment
	err := c.call(ctx, http.MethodPost, p("apps", app, "envs", env, "deploy"), r, &d)
	return d, err
}

// Rollback rolls back and waits for the outcome.
func (c *Client) Rollback(ctx context.Context, app, env string, r RollbackRequest) (store.Deployment, error) {
	var d store.Deployment
	err := c.call(ctx, http.MethodPost, p("apps", app, "envs", env, "rollback"), r, &d)
	return d, err
}

// Restart restarts live containers.
func (c *Client) Restart(ctx context.Context, app, env, service string) error {
	return c.call(ctx, http.MethodPost, p("apps", app, "envs", env, "restart"), RestartRequest{Service: service}, nil)
}

// Status returns the merged status of an app environment.
func (c *Client) Status(ctx context.Context, app, env string) (EnvStatus, error) {
	var s EnvStatus
	err := c.call(ctx, http.MethodGet, p("apps", app, "envs", env, "status"), nil, &s)
	return s, err
}

// Deployments returns deployment history, newest first.
func (c *Client) Deployments(ctx context.Context, app, env string) ([]store.Deployment, error) {
	var ds []store.Deployment
	err := c.call(ctx, http.MethodGet, p("apps", app, "envs", env, "deployments"), nil, &ds)
	return ds, err
}

// Logs returns recent logs as text.
func (c *Client) Logs(ctx context.Context, app, env, service string, tail int) (string, error) {
	q := url.Values{}
	if service != "" {
		q.Set("service", service)
	}
	if tail > 0 {
		q.Set("tail", strconv.Itoa(tail))
	}
	path := p("apps", app, "envs", env, "logs")
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var buf bytes.Buffer
	err := c.call(ctx, http.MethodGet, path, nil, &buf)
	return buf.String(), err
}

// SetSecret stores a new version of a secret.
func (c *Client) SetSecret(ctx context.Context, app, env, key string, value []byte) (store.SecretMeta, error) {
	var m store.SecretMeta
	err := c.call(ctx, http.MethodPut, p("apps", app, "envs", env, "secrets", key), rawBody(value), &m)
	return m, err
}

// ListSecrets lists secret names and versions.
func (c *Client) ListSecrets(ctx context.Context, app, env string) ([]store.SecretMeta, error) {
	var ms []store.SecretMeta
	err := c.call(ctx, http.MethodGet, p("apps", app, "envs", env, "secrets"), nil, &ms)
	return ms, err
}

// DeleteSecret removes a secret.
func (c *Client) DeleteSecret(ctx context.Context, app, env, key string) error {
	return c.call(ctx, http.MethodDelete, p("apps", app, "envs", env, "secrets", key), nil, nil)
}

// Events lists events, newest first; app/env may be empty, limit 0 = server default.
func (c *Client) Events(ctx context.Context, app, env string, limit int) ([]store.Event, error) {
	q := url.Values{}
	if app != "" {
		q.Set("app", app)
	}
	if env != "" {
		q.Set("env", env)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/events"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var es []store.Event
	err := c.call(ctx, http.MethodGet, path, nil, &es)
	return es, err
}

// rawBody marks a request body to be sent verbatim rather than as JSON.
type rawBody []byte

// call sends in (JSON, rawBody or nil) and decodes the response into out
// (JSON target, *bytes.Buffer for text, or nil).
func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	ctype := ""
	switch v := in.(type) {
	case nil:
	case rawBody:
		body, ctype = bytes.NewReader(v), "application/octet-stream"
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		body, ctype = bytes.NewReader(b), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if c.Actor != "" {
		req.Header.Set("X-LWD-Actor", c.Actor)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("controller %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var eb ErrorBody
		if json.Unmarshal(data, &eb) == nil && eb.Error.Code != "" {
			return &Error{Status: resp.StatusCode, Code: eb.Error.Code, Message: eb.Error.Message}
		}
		return &Error{Status: resp.StatusCode, Code: CodeInternal, Message: strings.TrimSpace(string(data))}
	}
	switch o := out.(type) {
	case nil:
		return nil
	case *bytes.Buffer:
		o.Write(data)
		return nil
	default:
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s %s: %w", method, path, err)
		}
		return nil
	}
}

// IsCode reports whether err is a /v1 error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
