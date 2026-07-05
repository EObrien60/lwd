package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"lwd/internal/node"
)

func TestFakeRouterSetAndRemoveRoute(t *testing.T) {
	ctx := context.Background()
	fr := NewFakeRouter()

	if err := fr.SetRoute(ctx, Route{Domain: "app.example.com", Upstream: "lwd-app-1", Port: 8080}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	r, ok := fr.Routes["app.example.com"]
	if !ok {
		t.Fatal("expected route to be recorded")
	}
	if r.Upstream != "lwd-app-1" || r.Port != 8080 {
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

	if err := fr.SetStaging(ctx, "staging.local", "lwd-app-2", 3000); err != nil {
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
	_ = fr.SetRoute(ctx, Route{Domain: "a.example.com", Upstream: "lwd-a-1", Port: 80})
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

	err := c.SetRoute(ctx, Route{Domain: "bad.example.com", Upstream: "lwd-bad-1", Port: 8080})
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

	if err := c.SetRoute(ctx, Route{Domain: "good.example.com", Upstream: "lwd-good-1", Port: 9090}); err != nil {
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

	if err := c.SetRoute(ctx, Route{Domain: "app.example.com", Upstream: "lwd-app-1", Port: 8080}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	r, ok := c.routes["app.example.com"]
	if !ok {
		t.Fatal("expected route to be committed")
	}
	if r.Upstream != "lwd-app-1" || r.Port != 8080 {
		t.Fatalf("unexpected route: %+v", r)
	}

	if err := c.RemoveRoute(ctx, "app.example.com"); err != nil {
		t.Fatalf("RemoveRoute: %v", err)
	}
	if _, ok := c.routes["app.example.com"]; ok {
		t.Fatal("expected route to be removed after successful reload")
	}
}
