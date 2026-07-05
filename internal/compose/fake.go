package compose

import (
	"context"
	"sync"
)

// Fake is an in-memory Composer for tests. It records call names in Calls
// and lets tests force failures/probe results via the exported knobs.
type Fake struct {
	mu sync.Mutex

	Calls []string

	// LastUp captures the UpSpec passed to the most recent Up call.
	LastUp UpSpec

	// UpErr, if set, is returned by Up.
	UpErr error

	// LastDown captures the project/file/env passed to the most recent Down
	// call, so tests can assert on it (e.g. DOCKER_HOST for a remote
	// backing teardown).
	LastDown DownSpec

	// DownErr, if set, is returned by Down.
	DownErr error

	// ServiceID/ServiceName are returned by ServiceContainer. ServiceErr, if
	// set, is returned instead (ServiceID/ServiceName are ignored).
	ServiceID   string
	ServiceName string
	ServiceErr  error
}

// NewFake returns a ready-to-use Fake Composer.
func NewFake() *Fake {
	return &Fake{}
}

var _ Composer = (*Fake)(nil)

func (f *Fake) record(name string) {
	f.Calls = append(f.Calls, name)
}

// Up records the call and spec, and returns UpErr.
func (f *Fake) Up(ctx context.Context, spec UpSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Up:" + spec.Project)
	f.LastUp = spec
	return f.UpErr
}

// Down records the call and spec, and returns DownErr.
func (f *Fake) Down(ctx context.Context, project, file string, env map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Down:" + project)
	f.LastDown = DownSpec{Project: project, File: file, Env: env}
	return f.DownErr
}

// ServiceContainer records the call and returns the configured
// ServiceID/ServiceName, or ServiceErr if set.
func (f *Fake) ServiceContainer(ctx context.Context, project, service string) (id, name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ServiceContainer:" + project + ":" + service)
	if f.ServiceErr != nil {
		return "", "", f.ServiceErr
	}
	return f.ServiceID, f.ServiceName, nil
}
