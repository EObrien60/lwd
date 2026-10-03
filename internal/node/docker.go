package node

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Runner runs the docker CLI. It is the node's only way to touch Docker, so
// tests substitute a fake.
type Runner interface {
	// Run executes `docker args...` and returns its stdout. On failure the
	// error carries the exit status and the tail of stderr.
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// execRunner runs the real docker binary.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 2048 {
			msg = "..." + msg[len(msg)-2048:]
		}
		return stdout.Bytes(), fmt.Errorf("docker %s: %v: %s", verbOf(args), err, msg)
	}
	return stdout.Bytes(), nil
}

// verbOf names a docker invocation for error messages without echoing paths.
func verbOf(args []string) string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "compose":
			continue
		case "-p", "-f", "--project-directory", "--profile":
			i++
			continue
		}
		return args[i]
	}
	return ""
}

// project is one compose project on this node.
type project struct {
	name  string
	dir   string   // project directory; "" when its files are gone
	files []string // compose files, merged in order
}

// deploymentProject is the compose project of deployment dir d.
func deploymentProject(name, dir string) project {
	p := project{name: name}
	// Pruned or hand-removed dirs still have containers; compose can operate
	// on a project by name alone for down/ps/logs/restart.
	if _, err := os.Stat(filepath.Join(dir, composeFile)); err == nil {
		p.dir = dir
		p.files = []string{filepath.Join(dir, composeFile), filepath.Join(dir, environmentFile)}
	}
	return p
}

// args builds `compose -p NAME [--project-directory D -f F...] extra...`.
func (p project) args(extra ...string) []string {
	a := []string{"compose", "-p", p.name}
	if p.dir != "" {
		a = append(a, "--project-directory", p.dir)
	}
	for _, f := range p.files {
		a = append(a, "-f", f)
	}
	return append(a, extra...)
}
