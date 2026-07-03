package router

import (
	"context"
	"errors"
	"testing"
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
