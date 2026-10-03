// Package nodeclient is the controller's HTTP client for the `lwd node` API
// (docs/lwd2/DESIGN.md "Node"). It distinguishes "could not talk to the node"
// (ErrUnreachable) from "the node answered with an error" (*HTTPError),
// because the controller reports those differently to operators.
package nodeclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"lwd/internal/bundle"
)

// ErrUnreachable wraps transport failures: refused, DNS, TLS, timeouts.
var ErrUnreachable = errors.New("node unreachable")

// Timeouts. Deploys are synchronous on the node (pull, migrate, readiness,
// smoke) so they get a long budget; everything else should be quick.
const (
	DeployTimeout  = 10 * time.Minute
	DefaultTimeout = 30 * time.Second
)

// HTTPError is a non-2xx answer from the node.
type HTTPError struct {
	Status  int
	Code    string // from a JSON error body, if any
	Message string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("node: %d %s", e.Status, e.Message)
}

// Health is the node's /v1/health answer.
type Health struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
}

// Client talks to one node.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for addr, which is host:port (plain HTTP, the node
// API sits behind a firewall on a private network) or a full http(s) URL.
func New(addr, token string) *Client {
	base := strings.TrimRight(addr, "/")
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	return &Client{base: base, token: token, http: &http.Client{}}
}

// Deploy runs a bundle synchronously and returns the node's result.
func (c *Client) Deploy(ctx context.Context, b *bundle.Bundle) (*bundle.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, DeployTimeout)
	defer cancel()
	body, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	var res bundle.Result
	if err := c.do(ctx, http.MethodPost, "/v1/deploy", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Restart restarts the live containers of app/env (one service if non-empty).
func (c *Client) Restart(ctx context.Context, app, env, service string) error {
	p := "/v1/apps/" + url.PathEscape(app) + "/" + url.PathEscape(env) + "/restart"
	if service != "" {
		p += "?service=" + url.QueryEscape(service)
	}
	return c.quick(ctx, http.MethodPost, p, nil)
}

// Logs returns recent log text of app/env.
func (c *Client) Logs(ctx context.Context, app, env, service string, tail int) (string, error) {
	q := url.Values{}
	if service != "" {
		q.Set("service", service)
	}
	if tail > 0 {
		q.Set("tail", strconv.Itoa(tail))
	}
	p := "/v1/apps/" + url.PathEscape(app) + "/" + url.PathEscape(env) + "/logs"
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	var buf bytes.Buffer
	err := c.quick(ctx, http.MethodGet, p, &buf)
	return buf.String(), err
}

// Status returns the node's /v1/status document verbatim.
func (c *Client) Status(ctx context.Context) (json.RawMessage, error) {
	var raw json.RawMessage
	err := c.quick(ctx, http.MethodGet, "/v1/status", &raw)
	return raw, err
}

// Health returns the node's health answer.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.quick(ctx, http.MethodGet, "/v1/health", &h)
	return h, err
}

// Remove deletes the live project, routes and state of app/env.
func (c *Client) Remove(ctx context.Context, app, env string) error {
	return c.quick(ctx, http.MethodDelete, "/v1/apps/"+url.PathEscape(app)+"/"+url.PathEscape(env), nil)
}

func (c *Client) quick(ctx context.Context, method, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	return c.do(ctx, method, path, nil, out)
}

// do performs a request. out may be nil (discard), *bytes.Buffer (raw body)
// or a JSON target.
func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("%w: reading response: %v", ErrUnreachable, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return parseError(resp.StatusCode, data)
	}
	switch o := out.(type) {
	case nil:
		return nil
	case *bytes.Buffer:
		o.Write(data)
		return nil
	default:
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("node: bad response to %s %s: %v", method, path, err)
		}
		return nil
	}
}

// parseError accepts {"error":{"code","message"}}, {"error":"..."} or text,
// since the node's error shape is not part of the bundle contract.
func parseError(status int, data []byte) error {
	e := &HTTPError{Status: status, Message: strings.TrimSpace(string(data))}
	var structured struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &structured) == nil && len(structured.Error) > 0 {
		var obj struct{ Code, Message string }
		var s string
		switch {
		case json.Unmarshal(structured.Error, &obj) == nil && obj.Message != "":
			e.Code, e.Message = obj.Code, obj.Message
		case json.Unmarshal(structured.Error, &s) == nil:
			e.Message = s
		}
	}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	return e
}
