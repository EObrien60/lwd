// Package compose wraps the `docker compose` CLI plugin so lwd can delegate
// orchestration of multi-service apps to it rather than reimplementing
// compose semantics. lwd shells out to `docker compose` for up/down/service
// resolution; it never parses or interprets the compose file itself.
package compose

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// UpSpec describes a compose project to bring up.
type UpSpec struct {
	// Project is the compose project name (`-p`).
	Project string
	// File is the path to the compose file (`-f`).
	File string
	// Env is set on the `docker compose` process environment (in addition to
	// the current process environment), as KEY=VAL pairs. Compose exposes
	// these to variable interpolation in the compose file and passes them
	// through to containers that reference them.
	Env map[string]string
}

// DownSpec describes a compose project to tear down. It exists mainly so
// Fake can record what a Down call was passed (mirroring UpSpec/LastUp) for
// tests to assert on.
type DownSpec struct {
	// Project is the compose project name (`-p`).
	Project string
	// File is the path to the compose file (`-f`).
	File string
	// Env is set on the `docker compose` process environment, same as
	// UpSpec.Env.
	Env map[string]string
}

// Composer brings up, tears down, and inspects docker-compose-managed
// stacks. The real implementation (CLI) shells out to the `docker compose`
// CLI plugin; Fake is an in-memory stand-in for tests.
type Composer interface {
	// Up brings spec's project up (`up -d --remove-orphans`), creating or
	// recreating any services whose config or image changed. Unchanged
	// services are left running.
	Up(ctx context.Context, spec UpSpec) error
	// Down tears down project's containers and its default network. env is
	// set on the `docker compose` process environment (in addition to the
	// current process environment), same as UpSpec.Env — used to pass
	// DOCKER_HOST when project's containers live on a remote node's Docker
	// daemon rather than the local one. nil/empty means no extra env (the
	// local-node case).
	Down(ctx context.Context, project, file string, env map[string]string) error
	// ServiceContainer resolves the running container for service within
	// project, returning its ID and name. It errors if the service has no
	// running container.
	ServiceContainer(ctx context.Context, project, service string) (id, name string, err error)
}

// CLI is the real Composer, implemented by shelling out to the `docker
// compose` CLI plugin.
type CLI struct{}

// NewCLI returns a ready-to-use CLI Composer.
func NewCLI() *CLI {
	return &CLI{}
}

var _ Composer = (*CLI)(nil)

// Up runs `docker compose -p <project> -f <file> up -d --remove-orphans`,
// with spec.Env added to the process environment so compose's variable
// interpolation (and any pass-through to containers) can see it.
func (c *CLI) Up(ctx context.Context, spec UpSpec) error {
	args := []string{"compose", "-p", spec.Project, "-f", spec.File, "up", "-d", "--remove-orphans"}
	env := os.Environ()
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	if _, err := run(ctx, env, "docker", args...); err != nil {
		return fmt.Errorf("compose up %s: %w", spec.Project, err)
	}
	return nil
}

// Down runs `docker compose -p <project> -f <file> down`, removing the
// project's containers and its default network, with env added to the
// process environment (same merge behavior as Up) so a remote node's
// backing project is torn down against its own Docker daemon rather than
// the controller's.
func (c *CLI) Down(ctx context.Context, project, file string, env map[string]string) error {
	args := []string{"compose", "-p", project, "-f", file, "down"}
	procEnv := os.Environ()
	for k, v := range env {
		procEnv = append(procEnv, k+"="+v)
	}
	if _, err := run(ctx, procEnv, "docker", args...); err != nil {
		return fmt.Errorf("compose down %s: %w", project, err)
	}
	return nil
}

// ServiceContainer resolves the running container ID for service in
// project via `docker compose -p <project> ps -q <service>`, then resolves
// its name via `docker inspect`.
func (c *CLI) ServiceContainer(ctx context.Context, project, service string) (id, name string, err error) {
	out, err := run(ctx, nil, "docker", "compose", "-p", project, "ps", "-q", service)
	if err != nil {
		return "", "", fmt.Errorf("compose ps %s/%s: %w", project, service, err)
	}

	id = firstLine(out)
	if id == "" {
		return "", "", fmt.Errorf("no running container for service %s in project %s", service, project)
	}

	nameOut, err := run(ctx, nil, "docker", "inspect", "-f", "{{.Name}}", id)
	if err != nil {
		return "", "", fmt.Errorf("inspect container %s: %w", id, err)
	}
	name = strings.TrimPrefix(strings.TrimSpace(firstLine(nameOut)), "/")

	return id, name, nil
}

// run executes name with args, using env as the process environment (nil
// means inherit os.Environ() unmodified), and returns trimmed stdout. On a
// non-zero exit it returns an error including captured stderr.
func run(ctx context.Context, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(stdout.String()), nil
}

// firstLine returns the first non-empty line of s.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}
