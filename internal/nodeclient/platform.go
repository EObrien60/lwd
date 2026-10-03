package nodeclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"lwd/internal/bundle"
)

// Platform calls can be slow: the first provisioning on a host pulls images
// and initialises Postgres; dumps and restores scale with the database.
const (
	ProvisionTimeout = 10 * time.Minute
	BackupTimeout    = 30 * time.Minute
)

func (c *Client) platform(ctx context.Context, timeout time.Duration, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	return c.do(ctx, method, path, body, out)
}

func dbPath(name string) string { return "/v1/platform/databases/" + url.PathEscape(name) }

// ProvisionDatabase creates or updates role and database name.
func (c *Client) ProvisionDatabase(ctx context.Context, name string, req bundle.DatabaseRequest) error {
	return c.platform(ctx, ProvisionTimeout, http.MethodPut, dbPath(name), req, nil)
}

// ProvisionBucket creates or keeps bucket name and its access key.
func (c *Client) ProvisionBucket(ctx context.Context, name string, req bundle.BucketRequest) (bundle.BucketCredentials, error) {
	var creds bundle.BucketCredentials
	err := c.platform(ctx, ProvisionTimeout, http.MethodPut, "/v1/platform/buckets/"+url.PathEscape(name), req, &creds)
	return creds, err
}

// BackupDatabase dumps database name on the node.
func (c *Client) BackupDatabase(ctx context.Context, name string) (bundle.BackupFile, error) {
	var bf bundle.BackupFile
	err := c.platform(ctx, BackupTimeout, http.MethodPost, dbPath(name)+"/backup", nil, &bf)
	return bf, err
}

// ListBackups lists the backups of name kept on the node, newest first.
func (c *Client) ListBackups(ctx context.Context, name string) ([]bundle.BackupFile, error) {
	var bs []bundle.BackupFile
	err := c.platform(ctx, DefaultTimeout, http.MethodGet, dbPath(name)+"/backups", nil, &bs)
	return bs, err
}

// RestoreDatabase replaces database name with a backup.
func (c *Client) RestoreDatabase(ctx context.Context, name string, req bundle.RestoreRequest) (bundle.RestoreResult, error) {
	var res bundle.RestoreResult
	err := c.platform(ctx, BackupTimeout, http.MethodPost, dbPath(name)+"/restore", req, &res)
	return res, err
}
