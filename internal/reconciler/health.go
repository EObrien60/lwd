package reconciler

import (
	"context"
	"time"
)

// SurfaceState is the self-heal lifecycle state of a single app's surface,
// as observed by the continuous reconciler loop (Phase 10).
type SurfaceState string

const (
	// SurfaceHealthy means the app's surface is up and passing its health
	// check (or has no declared health check and is simply running).
	SurfaceHealthy SurfaceState = "healthy"
	// SurfaceDegraded means the surface has been observed unhealthy but a
	// heal attempt has not yet been (or is not currently being) made.
	SurfaceDegraded SurfaceState = "degraded"
	// SurfaceHealing means the reconciler is actively attempting to
	// self-heal a dead/unhealthy surface.
	SurfaceHealing SurfaceState = "healing"
	// SurfaceFailed means self-healing was attempted and exhausted
	// (HealMaxAttempts reached) without the surface becoming healthy again.
	SurfaceFailed SurfaceState = "failed"
)

// AppHealth is the observed health of a single app's surface.
type AppHealth struct {
	App          string       `json:"app"`
	State        SurfaceState `json:"state"`
	LastError    string       `json:"last_error,omitempty"`
	HealAttempts int          `json:"heal_attempts"`
	UpdatedAt    time.Time    `json:"updated_at"`
}

// NodeHealth is the observed reachability of a single registered node.
type NodeHealth struct {
	Name      string    `json:"name"`
	Transport string    `json:"transport"`
	Reachable bool      `json:"reachable"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EdgeHealth is the observed reachability of the shared edge (router/Caddy).
type EdgeHealth struct {
	Reachable bool      `json:"reachable"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Health is a point-in-time snapshot of everything the continuous
// reconciler loop observes: per-node reachability, the shared edge, and
// per-app surface health.
type Health struct {
	Nodes []NodeHealth `json:"nodes"`
	Edge  EdgeHealth   `json:"edge"`
	Apps  []AppHealth  `json:"apps"`
}

// Reachability is the subset of *node.RegistryResolver the reconciler uses
// to observe node health; supplied via SetReachability so New's many call
// sites don't change. *node.RegistryResolver satisfies it.
type Reachability interface {
	Reachable(ctx context.Context, name string) (transport string, ok bool)
}
