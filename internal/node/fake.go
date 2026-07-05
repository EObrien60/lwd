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
	SaveErr   error
	LoadErr   error

	// ImagePresentErr, if set, is returned by ImagePresent instead of
	// consulting Images — used to test the hard-fail path of
	// reconciler.ensureImageOnNode (an inspect failure, as opposed to a
	// clean "not present" answer).
	ImagePresentErr error

	// Images is the set of image refs considered present on this fake node.
	Images map[string]bool

	// MeshAddr is this fake node's simulated WireGuard mesh address, read by
	// FakeResolver.ResolveMeta for a non-local name mapped to this Fake.
	MeshAddr string

	// Loaded is set true by a successful LoadImage call.
	Loaded bool
	// LastLoaded captures the bytes passed to the most recent successful
	// LoadImage call, so tests can assert on the transferred content.
	LastLoaded []byte

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
	var primaryRequested bool
	for _, pm := range spec.Publish {
		if pm.ContainerPort == spec.Port {
			hostPort = pm.HostPort
			primaryRequested = true
			break
		}
	}
	if !primaryRequested && len(spec.Publish) > 0 {
		hostPort = spec.Publish[0].HostPort
	}
	if hostPort == 0 && len(spec.Publish) > 0 {
		// Simulate Docker assigning an ephemeral host port for a
		// HostPort:0 publish request (used for remote surfaces).
		hostPort = 30000 + f.seq
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

// ImagePresent reports whether ref is in the Images set, or returns
// ImagePresentErr if set (simulating an inspect failure distinct from a
// clean "not present" answer).
func (f *Fake) ImagePresent(ctx context.Context, ref string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ImagePresent:" + ref)
	if f.ImagePresentErr != nil {
		return false, f.ImagePresentErr
	}
	return f.Images[ref], nil
}

// fakeImageTarPrefix marks the canned stream SaveImage returns as carrying a
// specific ref, so a subsequent LoadImage on another Fake (simulating the
// transfer path) can mark that same ref present — mirroring how a real
// `docker save`/`docker load` round-trip carries the image identity in the
// tar itself.
const fakeImageTarPrefix = "faketar:"

// SaveImage returns a canned tar-like stream unless SaveErr is set.
func (f *Fake) SaveImage(ctx context.Context, ref string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("SaveImage:" + ref)
	if f.SaveErr != nil {
		return nil, f.SaveErr
	}
	return io.NopCloser(strings.NewReader(fakeImageTarPrefix + ref)), nil
}

// LoadImage drains r and marks Loaded, unless LoadErr is set. If r carries a
// fakeImageTarPrefix-tagged ref (as produced by another Fake's SaveImage),
// that ref is added to Images, so a subsequent ImagePresent on the loaded ref
// reports true — completing the simulated save|load transfer round-trip.
func (f *Fake) LoadImage(ctx context.Context, r io.Reader) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("LoadImage")
	if f.LoadErr != nil {
		return f.LoadErr
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.LastLoaded = b
	f.Loaded = true
	if ref, ok := strings.CutPrefix(string(b), fakeImageTarPrefix); ok {
		if f.Images == nil {
			f.Images = map[string]bool{}
		}
		f.Images[ref] = true
	}
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
