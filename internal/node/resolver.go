package node

import (
	"fmt"
	"sync"
)

// Resolver maps a node name — as declared by an app's `node = "..."` field in
// lwd.toml (spec.App.Node) — to the Node implementation that should run its
// containers. "" and "local" always resolve to the local Docker daemon;
// every other name is a registered remote node.
type Resolver interface {
	Resolve(nodeName string) (Node, error)
}

// RegistryResolver is the production Resolver: "" and "local" resolve to a
// fixed local Node, and any other name is resolved via lookup — a closure
// the daemon supplies over its store's nodes registry — so this package
// never imports internal/store (which would create an import cycle, since
// store already depends on nothing here but the daemon wires node and store
// together). A remote Node, once built via NewRemoteSSH, is cached by name:
// repeated deploys to the same node reuse one Docker client rather than
// dialing ssh again on every Resolve.
type RegistryResolver struct {
	local  Node
	lookup func(name string) (sshHost string, ok bool, err error)

	mu      sync.Mutex
	remotes map[string]Node
}

// NewRegistryResolver returns a RegistryResolver. lookup must return the
// node's ssh host and ok=true if name is registered, ok=false (no error) if
// it is not, or a non-nil error if the lookup itself failed (e.g. a store
// error) — Resolve propagates each of these distinctly.
func NewRegistryResolver(local Node, lookup func(name string) (sshHost string, ok bool, err error)) *RegistryResolver {
	return &RegistryResolver{local: local, lookup: lookup, remotes: map[string]Node{}}
}

// Resolve returns the local node for "" or "local". For any other name, it
// returns a cached remote node if one was already built for that name;
// otherwise it calls lookup once, and on success builds and caches a
// docker-over-ssh Node via NewRemoteSSH before returning it. An unregistered
// name (lookup ok=false) produces an "unknown node" error.
func (rr *RegistryResolver) Resolve(nodeName string) (Node, error) {
	if nodeName == "" || nodeName == "local" {
		return rr.local, nil
	}

	rr.mu.Lock()
	defer rr.mu.Unlock()

	if n, ok := rr.remotes[nodeName]; ok {
		return n, nil
	}

	sshHost, ok, err := rr.lookup(nodeName)
	if err != nil {
		return nil, fmt.Errorf("look up node %q: %w", nodeName, err)
	}
	if !ok {
		return nil, fmt.Errorf("unknown node %q", nodeName)
	}

	n, err := NewRemoteSSH(sshHost)
	if err != nil {
		return nil, fmt.Errorf("connect to node %q (ssh://%s): %w", nodeName, sshHost, err)
	}
	rr.remotes[nodeName] = n
	return n, nil
}

// FakeResolver is a Resolver backed by a plain map, for tests: it returns
// the mapped Node for a name, or an "unknown node" error if the name isn't
// present. Unlike RegistryResolver it does not special-case "" or "local" —
// tests that want those to resolve must map them explicitly (as
// spec.App.Node is always normalized to "local" by spec.Parse, tests
// typically just do FakeResolver{"local": someFake}).
type FakeResolver map[string]Node

// Resolve looks nodeName up in the map.
func (fr FakeResolver) Resolve(nodeName string) (Node, error) {
	if n, ok := fr[nodeName]; ok {
		return n, nil
	}
	return nil, fmt.Errorf("unknown node %q", nodeName)
}

// Compile-time assertions that RegistryResolver and FakeResolver implement
// Resolver.
var _ Resolver = (*RegistryResolver)(nil)
var _ Resolver = (FakeResolver)(nil)
