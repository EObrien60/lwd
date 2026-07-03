package router

import (
	"context"
	"sync"
)

// FakeRouter is an in-memory Router for tests. It records call names in
// Calls and lets tests force failures/probe results via the exported knobs.
type FakeRouter struct {
	mu sync.Mutex

	// Routes and Staging are inspectable by tests after calling the Router
	// methods. Routes is keyed by domain, Staging by host (presence = set).
	Routes  map[string]Route
	Staging map[string]bool
	Calls   []string

	// ProbeStatus/ProbeErr configure ProbeThroughCaddy's return values.
	ProbeStatus int
	ProbeErr    error

	// EnsureErr, if set, is returned by EnsureUp.
	EnsureErr error

	// SetRouteErr, if set, is returned by SetRoute instead of installing the
	// route into Routes.
	SetRouteErr error
}

// NewFakeRouter returns a ready-to-use FakeRouter.
func NewFakeRouter() *FakeRouter {
	return &FakeRouter{
		Routes:      map[string]Route{},
		Staging:     map[string]bool{},
		ProbeStatus: 200,
	}
}

var _ Router = (*FakeRouter)(nil)

func (f *FakeRouter) record(name string) {
	f.Calls = append(f.Calls, name)
}

// EnsureUp records the call and returns EnsureErr.
func (f *FakeRouter) EnsureUp(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("EnsureUp")
	return f.EnsureErr
}

// SetRoute installs r into Routes, unless SetRouteErr is set, in which case
// it returns that error without touching Routes.
func (f *FakeRouter) SetRoute(ctx context.Context, r Route) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("SetRoute:" + r.Domain)
	if f.SetRouteErr != nil {
		return f.SetRouteErr
	}
	f.Routes[r.Domain] = r
	return nil
}

// RemoveRoute deletes domain from Routes.
func (f *FakeRouter) RemoveRoute(ctx context.Context, domain string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RemoveRoute:" + domain)
	delete(f.Routes, domain)
	return nil
}

// SetStaging marks host as staged in Staging.
func (f *FakeRouter) SetStaging(ctx context.Context, host, upstream string, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("SetStaging:" + host)
	f.Staging[host] = true
	return nil
}

// RemoveStaging clears host from Staging.
func (f *FakeRouter) RemoveStaging(ctx context.Context, host string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RemoveStaging:" + host)
	delete(f.Staging, host)
	return nil
}

// ProbeThroughCaddy returns the configured ProbeStatus/ProbeErr.
func (f *FakeRouter) ProbeThroughCaddy(ctx context.Context, host, path string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ProbeThroughCaddy:" + host)
	return f.ProbeStatus, f.ProbeErr
}

// Reload records the call and always succeeds.
func (f *FakeRouter) Reload(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Reload")
	return nil
}

// SeedRoutes populates Routes with each of routes, keyed by Domain, and
// records the call. It does not reload anything (the FakeRouter has nothing
// to reload); it exists so callers can be tested against the same interface
// as CaddyRouter.SeedRoutes.
func (f *FakeRouter) SeedRoutes(routes []Route) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("SeedRoutes")
	for _, r := range routes {
		f.Routes[r.Domain] = r
	}
}
