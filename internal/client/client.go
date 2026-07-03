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
