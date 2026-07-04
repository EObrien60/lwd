// Package source provides git clone functionality for deploying apps from
// repositories. It shells out to the box's git and returns the resolved
// commit SHA.
package source

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
//
// url and ref are expected to have already passed spec.Validate's stricter
// charset/transport checks (the gate for both file- and web-generated apps),
// but this function defends in depth anyway: it never runs git without
// GIT_ALLOW_PROTOCOL restricting transports to the handful lwd actually
// needs (blocking the command-executing ext:: / fd:: transports), always
// separates flags from positional arguments with `--` so a hostile value
// can't be parsed as a git option, and refuses to run at all if ref still
// starts with `-` (which would otherwise be interpretable as a flag).
func (c *CLI) Clone(ctx context.Context, url, ref, dir string) (sha string, err error) {
	if strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("git ref %q is invalid: must not start with -", ref)
	}

	// Try shallow clone with --branch.
	_, err = run(ctx, "git", "clone", "--depth", "1", "--branch", ref, "--", url, dir)
	if err != nil {
		// Shallow clone failed; try full clone then checkout.
		_, err = run(ctx, "git", "clone", "--", url, dir)
		if err != nil {
			return "", err
		}
		// No `--` here: `git checkout -- <ref>` would treat ref as a
		// pathspec rather than a branch/tag/sha, breaking checkout
		// semantics. Positional-option-injection is instead closed by the
		// leading-dash rejection above (ref is guaranteed not to start with
		// `-` by the time we get here) plus spec.Validate's ref-charset gate.
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

// gitEnv returns the environment for a git subprocess: the current process
// environment plus GIT_ALLOW_PROTOCOL, which restricts git to the transports
// lwd actually needs (http/https for normal remotes, git/ssh for those
// protocols, and file for local clones — including this repo's own git e2e
// test, which clones a file:// repo). Deliberately excludes ext:: (and any
// other transport not listed), which git would otherwise use to run an
// arbitrary host command supplied via the url — the core of the host-RCE
// finding this hardens against.
func gitEnv() []string {
	return append(os.Environ(), "GIT_ALLOW_PROTOCOL=https:git:ssh:file")
}

// run executes name with args using exec.CommandContext, returning trimmed
// stdout. On a non-zero exit it returns an error including the command and
// captured stderr. Every invocation runs with gitEnv() so GIT_ALLOW_PROTOCOL
// is always in force, regardless of call site.
func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = gitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(stdout.String()), nil
}
