package node

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Fake is an in-memory Node for tests. It records call names in Calls and lets
// tests force failures via the *Err fields.
type Fake struct {
	mu    sync.Mutex
	seq   int
	items map[string]Container // keyed by container ID
	Calls []string

	// LastRunSpec captures the RunSpec passed to the most recent RunContainer
	// call, so tests can assert on things RunContainer doesn't otherwise
	// surface via Container (e.g. injected env vars).
	LastRunSpec RunSpec

	EnsureErr error
	HealthErr error
	RunErr    error

	// HealthState and DockerHealth are returned by ContainerHealth for any
	// container ID. HealthState defaults to "running" for created containers
	// unless overridden.
	HealthState  string
	DockerHealth string

	// DockerHealthSeq, if non-empty, overrides DockerHealth: each call to
	// ContainerHealth consumes the next entry, holding on the last entry once
	// exhausted. Lets tests simulate a Docker HEALTHCHECK that starts out
	// "starting" and later flips to "healthy" (or "unhealthy").
	DockerHealthSeq  []string
	dockerHealthCall int
}

// NewFake returns a ready-to-use Fake node.
func NewFake() *Fake {
	return &Fake{items: map[string]Container{}}
}

func (f *Fake) record(name string) {
	f.Calls = append(f.Calls, name)
}

func (f *Fake) EnsureImage(ctx context.Context, imageRef string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("EnsureImage:" + imageRef)
	return f.EnsureErr
}

// EnsureNetwork records the call and always succeeds; the Fake has no real
// networking to create.
func (f *Fake) EnsureNetwork(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("EnsureNetwork:" + name)
	return nil
}

func (f *Fake) RunContainer(ctx context.Context, spec RunSpec) (Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RunContainer:" + spec.Name)
	f.LastRunSpec = spec
	if f.RunErr != nil {
		return Container{}, f.RunErr
	}
	f.seq++
	var hostPort int
	for _, pm := range spec.Publish {
		if pm.ContainerPort == spec.Port {
			hostPort = pm.HostPort
			break
		}
	}
	if hostPort == 0 && len(spec.Publish) > 0 {
		hostPort = spec.Publish[0].HostPort
	}
	var ip string
	if spec.Network != "" {
		ip = fmt.Sprintf("10.42.0.%d", (f.seq%254)+1)
	}
	c := Container{
		ID:       fmt.Sprintf("fake-%d", f.seq),
		Name:     spec.Name,
		Image:    spec.Image,
		State:    "running",
		Labels:   spec.Labels,
		HostPort: hostPort,
		IP:       ip,
	}
	f.items[c.ID] = c
	return c, nil
}

func (f *Fake) RemoveContainer(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RemoveContainer:" + id)
	delete(f.items, id)
	return nil
}

func (f *Fake) ListContainers(ctx context.Context, labels map[string]string) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListContainers")
	var out []Container
	for _, c := range f.items {
		if matches(c.Labels, labels) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *Fake) ContainerLogs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ContainerLogs:" + id)
	return io.NopCloser(strings.NewReader("fake log line\n")), nil
}

func (f *Fake) Health(ctx context.Context, c Container, h HealthSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Health:" + c.ID)
	return f.HealthErr
}

// ContainerHealth returns the configured HealthState/DockerHealth for any
// container ID, defaulting HealthState to "running" for containers the Fake
// created.
func (f *Fake) ContainerHealth(ctx context.Context, id string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ContainerHealth:" + id)
	state := f.HealthState
	if state == "" {
		if _, ok := f.items[id]; ok {
			state = "running"
		}
	}

	dockerHealth := f.DockerHealth
	if len(f.DockerHealthSeq) > 0 {
		idx := f.dockerHealthCall
		if idx >= len(f.DockerHealthSeq) {
			idx = len(f.DockerHealthSeq) - 1
		}
		dockerHealth = f.DockerHealthSeq[idx]
		f.dockerHealthCall++
	}
	return state, dockerHealth, nil
}

// ConnectContainerToNetwork records the call and always succeeds; the Fake has
// no real networking to manage. It is idempotent by design.
func (f *Fake) ConnectContainerToNetwork(ctx context.Context, containerID, network string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ConnectContainerToNetwork:" + containerID + ":" + network)
	return nil
}

func matches(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
