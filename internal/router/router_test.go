package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lwd/internal/node"
)

func TestFakeRouterSetAndRemoveRoute(t *testing.T) {
	ctx := context.Background()
	fr := NewFakeRouter()

	if err := fr.SetRoute(ctx, Route{Domain: "app.example.com", Upstreams: []Upstream{{Host: "lwd-app-1", Port: 8080}}}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	r, ok := fr.Routes["app.example.com"]
	if !ok {
		t.Fatal("expected route to be recorded")
	}
	if len(r.Upstreams) != 1 || r.Upstreams[0].Host != "lwd-app-1" || r.Upstreams[0].Port != 8080 {
		t.Fatalf("unexpected route: %+v", r)
	}

	if err := fr.RemoveRoute(ctx, "app.example.com"); err != nil {
		t.Fatalf("RemoveRoute: %v", err)
	}
	if _, ok := fr.Routes["app.example.com"]; ok {
		t.Fatal("expected route to be removed")
	}
}

func TestFakeRouterSetAndRemoveStaging(t *testing.T) {
	ctx := context.Background()
	fr := NewFakeRouter()

	if err := fr.SetStaging(ctx, "staging.local", []Upstream{{Host: "lwd-app-2", Port: 3000}}); err != nil {
		t.Fatalf("SetStaging: %v", err)
	}
	if !fr.Staging["staging.local"] {
		t.Fatal("expected staging host to be recorded")
	}

	if err := fr.RemoveStaging(ctx, "staging.local"); err != nil {
		t.Fatalf("RemoveStaging: %v", err)
	}
	if fr.Staging["staging.local"] {
		t.Fatal("expected staging host to be removed")
	}
}

func TestFakeRouterProbeThroughCaddy(t *testing.T) {
	ctx := context.Background()
	fr := NewFakeRouter()
	fr.ProbeStatus = 204

	status, err := fr.ProbeThroughCaddy(ctx, "app.example.com", "/healthz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != 204 {
		t.Fatalf("expected status 204, got %d", status)
	}

	fr.ProbeErr = errors.New("boom")
	if _, err := fr.ProbeThroughCaddy(ctx, "app.example.com", "/healthz"); err == nil {
		t.Fatal("expected configured error")
	}
}

func TestFakeRouterEnsureUpErr(t *testing.T) {
	ctx := context.Background()
	fr := NewFakeRouter()
	fr.EnsureErr = errors.New("no docker")

	if err := fr.EnsureUp(ctx); err == nil {
		t.Fatal("expected EnsureErr to propagate")
	}
}

func TestFakeRouterRecordsCalls(t *testing.T) {
	ctx := context.Background()
	fr := NewFakeRouter()

	_ = fr.EnsureUp(ctx)
	_ = fr.SetRoute(ctx, Route{Domain: "a.example.com", Upstreams: []Upstream{{Host: "lwd-a-1", Port: 80}}})
	_ = fr.Reload(ctx)

	if len(fr.Calls) != 3 {
		t.Fatalf("expected 3 recorded calls, got %d: %v", len(fr.Calls), fr.Calls)
	}
}

// stubAdminStatus returns an httptest.Server that responds to every request
// with the given status code, so tests can simulate a failing or succeeding
// Caddy admin API.
func stubAdminStatus(status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
}

func TestCaddyRouterSetRouteRollsBackOnReloadFailure(t *testing.T) {
	ctx := context.Background()

	admin := stubAdminStatus(http.StatusBadRequest)
	defer admin.Close()

	c := NewCaddyRouter(node.NewFake(), t.TempDir())
	c.adminBaseURL = admin.URL

	err := c.SetRoute(ctx, Route{Domain: "bad.example.com", Upstreams: []Upstream{{Host: "lwd-bad-1", Port: 8080}}})
	if err == nil {
		t.Fatal("expected SetRoute to fail when admin API returns 400")
	}
	if len(c.routes) != 0 {
		t.Fatalf("expected no routes committed after failed reload, got %d: %+v", len(c.routes), c.routes)
	}

	// Point the stub at a server that succeeds, and verify a different,
	// good domain commits cleanly with no trace of the failed attempt.
	good := stubAdminStatus(http.StatusOK)
	defer good.Close()
	c.adminBaseURL = good.URL

	if err := c.SetRoute(ctx, Route{Domain: "good.example.com", Upstreams: []Upstream{{Host: "lwd-good-1", Port: 9090}}}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	if len(c.routes) != 1 {
		t.Fatalf("expected exactly 1 route committed, got %d: %+v", len(c.routes), c.routes)
	}
	if _, ok := c.routes["good.example.com"]; !ok {
		t.Fatalf("expected good.example.com to be committed, got %+v", c.routes)
	}
	if _, ok := c.routes["bad.example.com"]; ok {
		t.Fatal("expected bad.example.com to NOT be committed (poisoned entry leaked)")
	}
}

// TestEnsureUpAdoptsRunningCaddy verifies that a running lwd-caddy container
// found by name (regardless of labels) is reused as-is: EnsureUp must not
// attempt to create a second one (which would race the running container for
// host ports 80/443/2019 and fail with a confusing "port already allocated"
// error), and must still proceed to reload it.
func TestEnsureUpAdoptsRunningCaddy(t *testing.T) {
	ctx := context.Background()

	admin := stubAdminStatus(http.StatusOK)
	defer admin.Close()

	n := node.NewFake()
	n.SeedContainer(node.Container{Name: caddyContainerName, State: "running"})

	c := NewCaddyRouter(n, t.TempDir())
	c.adminBaseURL = admin.URL

	if err := c.EnsureUp(ctx); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}

	for _, call := range n.Calls {
		if call == "RunContainer:"+caddyContainerName {
			t.Fatalf("expected no RunContainer call for an already-running lwd-caddy, got calls: %v", n.Calls)
		}
	}
}

// TestEnsureUpRemovesStaleCaddy verifies that a non-running lwd-caddy (e.g. an
// exited leftover from a prior run, or an older build's container) is removed
// before a fresh one is created, so the create doesn't fail with a
// container-name conflict.
func TestEnsureUpRemovesStaleCaddy(t *testing.T) {
	ctx := context.Background()

	admin := stubAdminStatus(http.StatusOK)
	defer admin.Close()

	n := node.NewFake()
	stale := n.SeedContainer(node.Container{Name: caddyContainerName, State: "exited"})

	c := NewCaddyRouter(n, t.TempDir())
	c.adminBaseURL = admin.URL

	if err := c.EnsureUp(ctx); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}

	removeIdx, runIdx := -1, -1
	for i, call := range n.Calls {
		if call == "RemoveContainer:"+stale.ID {
			removeIdx = i
		}
		if call == "RunContainer:"+caddyContainerName {
			runIdx = i
		}
	}
	if removeIdx == -1 {
		t.Fatalf("expected RemoveContainer call for the stale lwd-caddy, got calls: %v", n.Calls)
	}
	if runIdx == -1 {
		t.Fatalf("expected a RunContainer call to create a fresh lwd-caddy, got calls: %v", n.Calls)
	}
	if removeIdx > runIdx {
		t.Fatalf("expected stale container removal before create, got calls: %v", n.Calls)
	}
}

// TestEnsureUpPortConflictFriendlyError verifies that when no lwd-caddy exists
// yet and Docker's create fails because host port 80/443 is already bound by
// something else, EnsureUp returns a clear, actionable error instead of
// leaking Docker's raw bind-failure message.
func TestEnsureUpPortConflictFriendlyError(t *testing.T) {
	ctx := context.Background()

	n := node.NewFake()
	n.RunErr = errors.New(`Bind for 0.0.0.0:80 failed: port is already allocated`)

	c := NewCaddyRouter(n, t.TempDir())

	err := c.EnsureUp(ctx)
	if err == nil {
		t.Fatal("expected EnsureUp to return an error on a port conflict")
	}
	if !strings.Contains(err.Error(), "host port 80 or 443 is already in use") {
		t.Fatalf("expected a friendly port-conflict error, got: %v", err)
	}
}

func TestCaddyRouterHealthy(t *testing.T) {
	ctx := context.Background()

	t.Run("2xx is healthy", func(t *testing.T) {
		admin := stubAdminStatus(http.StatusOK)
		defer admin.Close()

		c := NewCaddyRouter(node.NewFake(), t.TempDir())
		c.adminBaseURL = admin.URL

		if !c.Healthy(ctx) {
			t.Fatal("expected Healthy to be true when admin API returns 200")
		}
	})

	t.Run("non-2xx is unhealthy", func(t *testing.T) {
		admin := stubAdminStatus(http.StatusInternalServerError)
		defer admin.Close()

		c := NewCaddyRouter(node.NewFake(), t.TempDir())
		c.adminBaseURL = admin.URL

		if c.Healthy(ctx) {
			t.Fatal("expected Healthy to be false when admin API returns 500")
		}
	})

	t.Run("unreachable is unhealthy", func(t *testing.T) {
		admin := stubAdminStatus(http.StatusOK)
		admin.Close() // close immediately so the server is unreachable

		c := NewCaddyRouter(node.NewFake(), t.TempDir())
		c.adminBaseURL = admin.URL

		if c.Healthy(ctx) {
			t.Fatal("expected Healthy to be false when admin API is unreachable")
		}
	})
}

func TestCaddyRouterSetAndRemoveRouteCommitOnSuccess(t *testing.T) {
	ctx := context.Background()

	admin := stubAdminStatus(http.StatusOK)
	defer admin.Close()

	c := NewCaddyRouter(node.NewFake(), t.TempDir())
	c.adminBaseURL = admin.URL

	if err := c.SetRoute(ctx, Route{Domain: "app.example.com", Upstreams: []Upstream{{Host: "lwd-app-1", Port: 8080}}}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	r, ok := c.routes["app.example.com"]
	if !ok {
		t.Fatal("expected route to be committed")
	}
	if len(r.Upstreams) != 1 || r.Upstreams[0].Host != "lwd-app-1" || r.Upstreams[0].Port != 8080 {
		t.Fatalf("unexpected route: %+v", r)
	}

	if err := c.RemoveRoute(ctx, "app.example.com"); err != nil {
		t.Fatalf("RemoveRoute: %v", err)
	}
	if _, ok := c.routes["app.example.com"]; ok {
		t.Fatal("expected route to be removed after successful reload")
	}
}

// TestRouteUpstreamsRoundTrip verifies that a multi-element Upstreams set
// survives SetRoute -> committed-route lookup unchanged, in order, for both
// the FakeRouter and the real CaddyRouter — proving Route.Upstreams is
// treated as a set, not collapsed/reordered/truncated anywhere in the path.
func TestRouteUpstreamsRoundTrip(t *testing.T) {
	ctx := context.Background()
	want := []Upstream{
		{Host: "lwd-app-1", Port: 8080},
		{Host: "lwd-app-2", Port: 8081},
		{Host: "lwd-app-3", Port: 8082},
	}

	t.Run("FakeRouter", func(t *testing.T) {
		fr := NewFakeRouter()
		if err := fr.SetRoute(ctx, Route{Domain: "multi.example.com", Upstreams: want}); err != nil {
			t.Fatalf("SetRoute: %v", err)
		}
		got := fr.Routes["multi.example.com"].Upstreams
		if len(got) != len(want) {
			t.Fatalf("got %d upstreams, want %d: %+v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("upstream[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("CaddyRouter", func(t *testing.T) {
		admin := stubAdminStatus(http.StatusOK)
		defer admin.Close()

		c := NewCaddyRouter(node.NewFake(), t.TempDir())
		c.adminBaseURL = admin.URL

		if err := c.SetRoute(ctx, Route{Domain: "multi.example.com", Upstreams: want}); err != nil {
			t.Fatalf("SetRoute: %v", err)
		}
		got := c.routes["multi.example.com"].Upstreams
		if len(got) != len(want) {
			t.Fatalf("got %d upstreams, want %d: %+v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("upstream[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})
}
