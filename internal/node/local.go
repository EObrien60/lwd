package node

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

// Local implements Node against the host's Docker daemon.
type Local struct {
	cli *client.Client
}

// NewLocal connects to the local Docker daemon using the environment.
func NewLocal() (*Local, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Local{cli: cli}, nil
}

// EnsureImage pulls the image if it is not already present locally.
func (l *Local) EnsureImage(ctx context.Context, imageRef string) error {
	_, _, err := l.cli.ImageInspectWithRaw(ctx, imageRef)
	if err == nil {
		return nil // already present
	}
	rc, err := l.cli.ImagePull(ctx, imageRef, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", imageRef, err)
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, rc) // drain to completion
	return nil
}

// RunContainer creates and starts a container, publishing Port to the same host port.
func (l *Local) RunContainer(ctx context.Context, spec RunSpec) (Container, error) {
	var env []string
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}

	cfg := &container.Config{
		Image:  spec.Image,
		Env:    env,
		Labels: spec.Labels,
	}
	hostCfg := &container.HostConfig{}
	if spec.Port != 0 {
		p := nat.Port(strconv.Itoa(spec.Port) + "/tcp")
		cfg.ExposedPorts = nat.PortSet{p: struct{}{}}
		hostCfg.PortBindings = nat.PortMap{
			p: []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(spec.Port)}},
		}
	}

	created, err := l.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, spec.Name)
	if err != nil {
		return Container{}, fmt.Errorf("create container: %w", err)
	}
	if err := l.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return Container{}, fmt.Errorf("start container: %w", err)
	}
	return Container{
		ID:       created.ID,
		Name:     spec.Name,
		Image:    spec.Image,
		State:    "running",
		Labels:   spec.Labels,
		HostPort: spec.Port,
	}, nil
}

// RemoveContainer stops (with a short timeout) and force-removes a container.
func (l *Local) RemoveContainer(ctx context.Context, id string) error {
	timeout := 10
	_ = l.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})
	if err := l.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("remove container %s: %w", id, err)
	}
	return nil
}

// ListContainers returns containers matching all given labels (running or not).
func (l *Local) ListContainers(ctx context.Context, labels map[string]string) ([]Container, error) {
	args := filters.NewArgs()
	for k, v := range labels {
		args.Add("label", k+"="+v)
	}
	list, err := l.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	out := make([]Container, 0, len(list))
	for _, c := range list {
		name := ""
		if len(c.Names) > 0 {
			name = c.Names[0]
			if len(name) > 0 && name[0] == '/' {
				name = name[1:]
			}
		}
		var hostPort int
		for _, p := range c.Ports {
			if p.PublicPort != 0 {
				hostPort = int(p.PublicPort)
				break
			}
		}
		out = append(out, Container{
			ID: c.ID, Name: name, Image: c.Image, State: c.State,
			Labels: c.Labels, HostPort: hostPort,
		})
	}
	return out, nil
}

// ContainerLogs streams a container's combined stdout/stderr.
func (l *Local) ContainerLogs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	return l.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Follow: follow, Tail: "200",
	})
}

// Health polls the container until healthy or the timeout elapses. With a Path
// it expects an HTTP 2xx on 127.0.0.1:HostPort; otherwise it does a TCP connect.
func (l *Local) Health(ctx context.Context, c Container, h HealthSpec) error {
	if c.HostPort == 0 {
		return nil // nothing to probe
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(c.HostPort))
	deadline := time.Now().Add(h.Timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if h.Path != "" {
			lastErr = probeHTTP("http://" + addr + h.Path)
		} else {
			lastErr = probeTCP(addr)
		}
		if lastErr == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("health check timed out: %w", lastErr)
}

func probeHTTP(url string) error {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func probeTCP(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// Compile-time assertion that Local implements Node.
var _ Node = (*Local)(nil)
