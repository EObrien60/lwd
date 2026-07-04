// Package source provides git clone functionality for deploying apps from
// repositories. It shells out to the box's git and returns the resolved
// commit SHA.
package source

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Git defines the interface for cloning git repositories.
type Git interface {
	// Clone clones the repository at url with ref (branch, tag, or commit SHA)
	// into dir, returning the resolved commit SHA. If Clone succeeds, sha is a
	// 40-character commit hash. On error, sha is empty and err includes the
	// git command's stderr.
	Clone(ctx context.Context, url, ref, dir string) (sha string, err error)
}

// CLI is the real Git implementation, using the box's git command.
type CLI struct{}

// NewCLI returns a ready-to-use CLI Git client.
func NewCLI() *CLI {
	return &CLI{}
}

var _ Git = (*CLI)(nil)

// Clone clones the repository at url with ref into dir, returning the resolved
// commit SHA. It attempts a shallow clone with --branch first; if that fails
// (e.g., because ref is a commit SHA), it falls back to a full clone and
// checkout the ref.
func (c *CLI) Clone(ctx context.Context, url, ref, dir string) (sha string, err error) {
	// Try shallow clone with --branch.
	_, err = run(ctx, "git", "clone", "--depth", "1", "--branch", ref, url, dir)
	if err != nil {
		// Shallow clone failed; try full clone then checkout.
		_, err = run(ctx, "git", "clone", url, dir)
		if err != nil {
			return "", err
		}
		_, err = run(ctx, "git", "-C", dir, "checkout", ref)
		if err != nil {
			return "", err
		}
	}

	// Get the resolved commit SHA.
	sha, err = run(ctx, "git", "-C", dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}

	return sha, nil
}

// run executes name with args using exec.CommandContext, returning trimmed
// stdout. On a non-zero exit it returns an error including the command and
// captured stderr.
func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(stdout.String()), nil
}
