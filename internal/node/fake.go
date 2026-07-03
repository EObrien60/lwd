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

	EnsureErr error
	HealthErr error
	RunErr    error
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

func (f *Fake) RunContainer(ctx context.Context, spec RunSpec) (Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RunContainer:" + spec.Name)
	if f.RunErr != nil {
		return Container{}, f.RunErr
	}
	f.seq++
	c := Container{
		ID:       fmt.Sprintf("fake-%d", f.seq),
		Name:     spec.Name,
		Image:    spec.Image,
		State:    "running",
		Labels:   spec.Labels,
		HostPort: spec.Port,
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

func matches(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
