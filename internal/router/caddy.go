package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AdminAddr is where the node's Caddy admin API listens. Caddy runs with host
// networking, so loopback is the host's loopback and containers on bridge
// networks cannot reach it.
const (
	AdminAddr       = "127.0.0.1:2019"
	DefaultAdminURL = "http://" + AdminAddr
)

// Caddy applies Caddyfiles to a running Caddy.
type Caddy struct {
	Path         string        // Caddyfile on the host, inside the directory mounted at /etc/caddy
	AdminURL     string        // e.g. DefaultAdminURL
	Client       *http.Client  // nil = a client with a 30s timeout
	PollInterval time.Duration // WaitAdmin poll interval; 0 = 500ms
}

func (c *Caddy) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Apply persists content as the Caddyfile and then loads it. The file is
// written first so a Caddy restart always boots the newest config; it is
// replaced by rename inside the mounted directory, so Caddy never reads a
// partial file (renaming over a single bind-mounted file would not be visible
// in the container, which is why the directory is mounted).
func (c *Caddy) Apply(ctx context.Context, content string) error {
	if err := WriteFileAtomic(c.Path, []byte(content), 0o644); err != nil {
		return err
	}
	return c.Load(ctx, content)
}

// Load posts content to the admin /load endpoint without touching disk.
func (c *Caddy) Load(ctx context.Context, content string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AdminURL+"/load", strings.NewReader(content))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/caddyfile")
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("caddy load: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("caddy load: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// WaitAdmin polls the admin API until it answers 200 or timeout elapses.
func (c *Caddy) WaitAdmin(ctx context.Context, timeout time.Duration) error {
	interval := c.PollInterval
	if interval == 0 {
		interval = 500 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.AdminURL+"/config/", nil)
		resp, err := c.client().Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("status %s", resp.Status)
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("caddy admin %s not ready after %s: %v", c.AdminURL, timeout, last)
		case <-time.After(interval):
		}
	}
}

// WriteFileAtomic writes data to a temp file in path's directory, syncs it and
// renames it over path.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
