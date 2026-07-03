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

// RunSpec is the request to create and start one container.
type RunSpec struct {
	Name   string
	Image  string
	Env    map[string]string
	Labels map[string]string
	Port   int // container port to publish to the host (MVP: same host port)
}

// Container describes a container known to a node.
type Container struct {
	ID       string
	Name     string
	Image    string
	State    string // "running", "exited", etc.
	Labels   map[string]string
	HostPort int // host port the container's Port is published on
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
	RunContainer(ctx context.Context, spec RunSpec) (Container, error)
	RemoveContainer(ctx context.Context, id string) error
	ListContainers(ctx context.Context, labels map[string]string) ([]Container, error)
	ContainerLogs(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
	Health(ctx context.Context, c Container, h HealthSpec) error
}
