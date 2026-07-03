// Package node abstracts container operations behind the Node interface.
// This is lwd's federation seam: today the only implementation is the local
// Docker daemon, but the reconciler is written against Node so a remote agent
// can be dropped in later without changing the core.
package node

import (
	"context"
	"io"
	"time"
)

// PortMapping is one host<->container TCP port publication.
type PortMapping struct {
	HostPort      int
	ContainerPort int
}

// RunSpec is the request to create and start one container.
type RunSpec struct {
	Name    string
	Image   string
	Env     map[string]string
	Labels  map[string]string
	Port    int           // app's primary container port; exposed on the network but NOT auto-published to the host
	Network string        // network to attach to; "" = default (no explicit network)
	Publish []PortMapping // host<->container ports to publish; nil = publish nothing
}

// Container describes a container known to a node.
type Container struct {
	ID       string
	Name     string
	Image    string
	State    string // "running", "exited", etc.
	Labels   map[string]string
	HostPort int    // host port the container's Port is published on, if any
	IP       string // address on the primary network, when known
}

// HealthSpec describes how to decide a container is healthy.
type HealthSpec struct {
	Path    string // HTTP path; empty means TCP-connect check only
	Timeout time.Duration
}

// Node is the set of operations lwd performs on a deployment target.
// image refs are the only cross-node currency; a Node never assumes locality.
type Node interface {
	EnsureImage(ctx context.Context, imageRef string) error
	// EnsureNetwork makes sure a private bridge network named name exists,
	// creating it if absent. Idempotent.
	EnsureNetwork(ctx context.Context, name string) error
	// RunContainer creates and starts a container. It no longer auto-publishes
	// Port to the host: Port is only exposed on the network (and attached to
	// spec.Network, if set); host ports are published only for entries in
	// spec.Publish.
	RunContainer(ctx context.Context, spec RunSpec) (Container, error)
	RemoveContainer(ctx context.Context, id string) error
	ListContainers(ctx context.Context, labels map[string]string) ([]Container, error)
	ContainerLogs(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
	Health(ctx context.Context, c Container, h HealthSpec) error
	// ContainerHealth inspects a container and returns its Docker state
	// (running/exited/...) and, if the image declares a HEALTHCHECK, the
	// Docker health status (starting/healthy/unhealthy); "" if none.
	ContainerHealth(ctx context.Context, id string) (state string, dockerHealth string, err error)
}
