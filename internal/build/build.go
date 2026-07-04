// Package build wraps the `docker build` CLI for building app images.
package build

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Builder defines the interface for building Docker images.
type Builder interface {
	// Build builds a Docker image from a Dockerfile, tagging it with tag.
	// contextDir is the build context directory, dockerfile is the path relative
	// to contextDir, and tag is the image tag. On error, the error message includes
	// the docker build command's stderr.
	Build(ctx context.Context, contextDir, dockerfile, tag string) error

	// ImageExists checks if an image with the given tag exists locally.
	// It returns (true, nil) if the image exists, (false, nil) if it doesn't,
	// and (false, err) for unexpected failures (e.g., docker not found).
	ImageExists(ctx context.Context, tag string) (bool, error)
}

// CLI is the real Builder implementation, using the `docker` command.
type CLI struct{}

// NewCLI returns a ready-to-use CLI Builder.
func NewCLI() *CLI {
	return &CLI{}
}

var _ Builder = (*CLI)(nil)

// Build runs `docker build -t <tag> -f <dockerfile> <contextDir>`, capturing
// stderr and returning an error if the command fails.
func (c *CLI) Build(ctx context.Context, contextDir, dockerfile, tag string) error {
	dockerfilePath := filepath.Join(contextDir, dockerfile)
	args := []string{"build", "-t", tag, "-f", dockerfilePath, contextDir}

	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return nil
}

// ImageExists checks if a Docker image with the given tag exists locally by running
// `docker image inspect <tag>`. It returns (true, nil) if the image exists,
// (false, nil) if the image is not found (exit code non-zero), and (false, err)
// for unexpected failures (e.g., docker not found).
func (c *CLI) ImageExists(ctx context.Context, tag string) (bool, error) {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", tag)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		// Image exists
		return true, nil
	}

	// Check if it's an exit error (not found)
	if _, ok := err.(*exec.ExitError); ok {
		// Image not found
		return false, nil
	}

	// Unexpected failure (e.g., docker not found)
	return false, fmt.Errorf("docker image inspect %s: %w", tag, err)
}
