package node

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
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
	return newLocalWithClient(cli), nil
}

// NewRemoteSSH connects to a Docker daemon on a remote host over SSH (the
// Docker SDK's ssh connection helper; lwd manages no ssh credentials of its
// own, relying on the host's ssh config/agent). The returned *Local behaves
// identically to a locally-connected one — every method just talks to
// whichever daemon its client is pointed at.
func NewRemoteSSH(sshHost string) (*Local, error) {
	cli, err := client.NewClientWithOpts(
		client.WithHost("ssh://"+sshHost),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client (ssh://%s): %w", sshHost, err)
	}
	return newLocalWithClient(cli), nil
}

func newLocalWithClient(cli *client.Client) *Local {
	return &Local{cli: cli}
}

// Ping reports whether the Docker daemon is reachable.
func (l *Local) Ping(ctx context.Context) error {
	_, err := l.cli.Ping(ctx)
	return err
}

// EnsureImage makes the image available for RunContainer. Pinned digests
// (@sha256:...) are immutable, so a present copy is used as-is. For mutable
// refs (tags like :latest) it re-pulls so a moved tag is picked up; if the
// pull fails but a local copy exists (e.g. a locally-built image), that copy
// is used rather than failing the deploy.
func (l *Local) EnsureImage(ctx context.Context, imageRef string) error {
	_, _, inspectErr := l.cli.ImageInspectWithRaw(ctx, imageRef)
	present := inspectErr == nil
	if present && strings.Contains(imageRef, "@sha256:") {
		return nil
	}
	rc, err := l.cli.ImagePull(ctx, imageRef, image.PullOptions{})
	if err != nil {
		if present {
			return nil // registry unreachable but we have a local copy
		}
		return fmt.Errorf("pull %s: %w", imageRef, err)
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, rc)
	return nil
}

// EnsureNetwork makes sure a private bridge network named name exists,
// creating it if absent. Idempotent.
func (l *Local) EnsureNetwork(ctx context.Context, name string) error {
	if _, err := l.cli.NetworkInspect(ctx, name, network.InspectOptions{}); err == nil {
		return nil
	} else if !client.IsErrNotFound(err) {
		return fmt.Errorf("inspect network %s: %w", name, err)
	}
	if _, err := l.cli.NetworkCreate(ctx, name, network.CreateOptions{Driver: "bridge"}); err != nil {
		// Another caller may have created it concurrently; treat that as success.
		if _, inspectErr := l.cli.NetworkInspect(ctx, name, network.InspectOptions{}); inspectErr == nil {
			return nil
		}
		return fmt.Errorf("create network %s: %w", name, err)
	}
	return nil
}

// RunContainer creates and starts a container. It exposes Port on the
// network, attaches to spec.Network (if set), and publishes only the host
// ports listed in spec.Publish. Ports 80/443 bind to 0.0.0.0; every other
// published host port binds to 127.0.0.1 only.
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
	if len(spec.Cmd) > 0 {
		cfg.Cmd = spec.Cmd
	}
	hostCfg := &container.HostConfig{}
	cfg.ExposedPorts = nat.PortSet{}
	if spec.Port != 0 {
		p := nat.Port(strconv.Itoa(spec.Port) + "/tcp")
		cfg.ExposedPorts[p] = struct{}{}
	}

	var primaryHostPort int
	if len(spec.Publish) > 0 {
		portBindings := nat.PortMap{}
		for _, pm := range spec.Publish {
			hostIP := pm.HostIP
			if hostIP == "" {
				hostIP = "127.0.0.1"
				if pm.HostPort == 80 || pm.HostPort == 443 {
					hostIP = "0.0.0.0"
				}
			}
			cp := nat.Port(strconv.Itoa(pm.ContainerPort) + "/tcp")
			cfg.ExposedPorts[cp] = struct{}{}
			hostPortStr := ""
			if pm.HostPort != 0 {
				hostPortStr = strconv.Itoa(pm.HostPort)
			}
			portBindings[cp] = append(portBindings[cp], nat.PortBinding{
				HostIP:   hostIP,
				HostPort: hostPortStr,
			})
			if pm.ContainerPort == spec.Port {
				primaryHostPort = pm.HostPort
			}
		}
		if primaryHostPort == 0 && spec.Publish[0].ContainerPort != spec.Port {
			primaryHostPort = spec.Publish[0].HostPort
		}
		hostCfg.PortBindings = portBindings
	}

	var netCfg *network.NetworkingConfig
	if spec.Network != "" {
		netCfg = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				spec.Network: {},
			},
		}
	}

	created, err := l.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, spec.Name)
	if err != nil {
		return Container{}, fmt.Errorf("create container: %w", err)
	}
	if err := l.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = l.cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
		return Container{}, fmt.Errorf("start container: %w", err)
	}

	ip := ""
	if inspect, inspectErr := l.cli.ContainerInspect(ctx, created.ID); inspectErr == nil && inspect.NetworkSettings != nil {
		if spec.Network != "" {
			if ep, ok := inspect.NetworkSettings.Networks[spec.Network]; ok {
				ip = ep.IPAddress
			}
		}
		if ip == "" {
			for _, ep := range inspect.NetworkSettings.Networks {
				if ep.IPAddress != "" {
					ip = ep.IPAddress
					break
				}
			}
		}

		// Read back the actual published host port for the primary port,
		// rather than trusting the requested value: a HostPort of 0 asks
		// Docker to assign an ephemeral port, and the daemon is always the
		// authority on what actually got bound.
		if spec.Port != 0 {
			cp := nat.Port(strconv.Itoa(spec.Port) + "/tcp")
			if bindings, ok := inspect.NetworkSettings.Ports[cp]; ok && len(bindings) > 0 {
				if hp, perr := strconv.Atoi(bindings[0].HostPort); perr == nil {
					primaryHostPort = hp
				}
			}
		}
	}

	return Container{
		ID:       created.ID,
		Name:     spec.Name,
		Image:    spec.Image,
		State:    "running",
		Labels:   spec.Labels,
		HostPort: primaryHostPort,
		IP:       ip,
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

// Health polls the container until healthy or the timeout elapses, honoring
// ctx cancellation. With a Path it expects an HTTP 2xx on 127.0.0.1:HostPort;
// otherwise it does a TCP connect.
func (l *Local) Health(ctx context.Context, c Container, h HealthSpec) error {
	if c.HostPort == 0 {
		return nil // nothing to probe
	}
	if h.Timeout <= 0 {
		return fmt.Errorf("health check: non-positive timeout %v", h.Timeout)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(c.HostPort))
	deadline := time.Now().Add(h.Timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if h.Path != "" {
			lastErr = probeHTTP(ctx, "http://"+addr+h.Path)
		} else {
			lastErr = probeTCP(ctx, addr)
		}
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("health check timed out: %w", lastErr)
}

func probeHTTP(ctx context.Context, url string) error {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func probeTCP(ctx context.Context, addr string) error {
	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// ContainerHealth inspects a container and returns its Docker state and, if
// the image declares a HEALTHCHECK, the Docker health status.
func (l *Local) ContainerHealth(ctx context.Context, id string) (string, string, error) {
	inspect, err := l.cli.ContainerInspect(ctx, id)
	if err != nil {
		return "", "", fmt.Errorf("inspect container %s: %w", id, err)
	}
	if inspect.State == nil {
		return "", "", nil
	}
	dockerHealth := ""
	if inspect.State.Health != nil {
		dockerHealth = inspect.State.Health.Status
	}
	return inspect.State.Status, dockerHealth, nil
}

// ConnectContainerToNetwork attaches a container to a network. Idempotent: if
// the container is already on the network, the "already exists" or "endpoint
// already exists in network" error is treated as success.
func (l *Local) ConnectContainerToNetwork(ctx context.Context, containerID, network string) error {
	err := l.cli.NetworkConnect(ctx, network, containerID, nil)
	if err == nil {
		return nil
	}
	// Treat "endpoint already exists in this network" as idempotent success.
	errMsg := err.Error()
	if strings.Contains(errMsg, "already exists") || strings.Contains(errMsg, "endpoint already exists") {
		return nil
	}
	return fmt.Errorf("connect container %s to network %s: %w", containerID, network, err)
}

// ImagePresent reports whether ref is present in this node's local Docker
// image store, without pulling or otherwise fetching it. Mirrors the
// present/absent/error distinction used by EnsureImage and the build
// package's ImageExists: (true, nil) present, (false, nil) not found,
// (false, err) unexpected failure.
func (l *Local) ImagePresent(ctx context.Context, ref string) (bool, error) {
	_, _, err := l.cli.ImageInspectWithRaw(ctx, ref)
	if err == nil {
		return true, nil
	}
	if client.IsErrNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("inspect image %s: %w", ref, err)
}

// SaveImage returns a tar stream of the image (docker save). The caller must
// Close the returned reader.
func (l *Local) SaveImage(ctx context.Context, ref string) (io.ReadCloser, error) {
	rc, err := l.cli.ImageSave(ctx, []string{ref})
	if err != nil {
		return nil, fmt.Errorf("save image %s: %w", ref, err)
	}
	return rc, nil
}

// LoadImage loads a tar stream produced by SaveImage (docker load), draining
// and closing the daemon's response body.
func (l *Local) LoadImage(ctx context.Context, r io.Reader) error {
	resp, err := l.cli.ImageLoad(ctx, r, true)
	if err != nil {
		return fmt.Errorf("load image: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Compile-time assertion that Local implements Node.
var _ Node = (*Local)(nil)
