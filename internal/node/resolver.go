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
	// ResolveMeta is Resolve plus placement metadata the reconciler needs to
	// route a remote surface (and target remote backing services): isLocal
	// reports whether nodeName resolved to the local node ("" or "local"),
	// meshAddr is the resolved node's WireGuard mesh address (always "" for
	// the local node; a registered remote node's mesh_addr otherwise), and
	// dockerHost is the `DOCKER_HOST` value that points the `docker compose`
	// CLI at that node's daemon (always "" for the local node — the CLI then
	// inherits the ambient environment unmodified; "ssh://<sshHost>" for a
	// registered remote node).
	ResolveMeta(nodeName string) (n Node, meshAddr string, dockerHost string, isLocal bool, err error)
}

// remoteEntry is a cached remote Node alongside the mesh address and docker
// host its registry row carried, so a repeat Resolve/ResolveMeta for the same
// name doesn't need to re-run lookup.
type remoteEntry struct {
	node       Node
	meshAddr   string
	dockerHost string
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
	lookup func(name string) (sshHost, meshAddr string, ok bool, err error)

	mu      sync.Mutex
	remotes map[string]remoteEntry
}

// NewRegistryResolver returns a RegistryResolver. lookup must return the
// node's ssh host and mesh address and ok=true if name is registered,
// ok=false (no error) if it is not, or a non-nil error if the lookup itself
// failed (e.g. a store error) — Resolve/ResolveMeta propagate each of these
// distinctly.
func NewRegistryResolver(local Node, lookup func(name string) (sshHost, meshAddr string, ok bool, err error)) *RegistryResolver {
	return &RegistryResolver{local: local, lookup: lookup, remotes: map[string]remoteEntry{}}
}

// Resolve returns the local node for "" or "local". For any other name, it
// returns a cached remote node if one was already built for that name;
// otherwise it calls lookup once, and on success builds and caches a
// docker-over-ssh Node via NewRemoteSSH before returning it. An unregistered
// name (lookup ok=false) produces an "unknown node" error.
func (rr *RegistryResolver) Resolve(nodeName string) (Node, error) {
	n, _, _, _, err := rr.ResolveMeta(nodeName)
	return n, err
}

// ResolveMeta is Resolve plus the resolved node's mesh address, its
// DOCKER_HOST target, and whether it is the local node. See
// Resolver.ResolveMeta.
func (rr *RegistryResolver) ResolveMeta(nodeName string) (Node, string, string, bool, error) {
	if nodeName == "" || nodeName == "local" {
		return rr.local, "", "", true, nil
	}

	rr.mu.Lock()
	defer rr.mu.Unlock()

	if e, ok := rr.remotes[nodeName]; ok {
		return e.node, e.meshAddr, e.dockerHost, false, nil
	}

	sshHost, meshAddr, ok, err := rr.lookup(nodeName)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("look up node %q: %w", nodeName, err)
	}
	if !ok {
		return nil, "", "", false, fmt.Errorf("unknown node %q", nodeName)
	}

	n, err := NewRemoteSSH(sshHost)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("connect to node %q (ssh://%s): %w", nodeName, sshHost, err)
	}
	dockerHost := "ssh://" + sshHost
	rr.remotes[nodeName] = remoteEntry{node: n, meshAddr: meshAddr, dockerHost: dockerHost}
	return n, meshAddr, dockerHost, false, nil
}

// Invalidate evicts the cached remote node for name, if any, so a subsequent
// Resolve/ResolveMeta re-runs lookup and builds a fresh docker-over-ssh Node
// instead of returning a stale cached one. Call this whenever a node's
// registry row changes — added/updated (ssh_host or mesh_addr may have
// changed) or removed — so a stale ssh client never lingers. A no-op if
// nodeName has no cached entry (e.g. it was never resolved, or is "local").
func (rr *RegistryResolver) Invalidate(nodeName string) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	delete(rr.remotes, nodeName)
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

// ResolveMeta is Resolve plus placement metadata: isLocal is true iff
// nodeName is "" or "local"; for any other (non-local) name mapped to a
// *Fake, meshAddr is that Fake's MeshAddr field and dockerHost is that
// Fake's DockerHost field — letting reconciler tests exercise remote-surface
// routing and remote-backing DOCKER_HOST targeting without a real registry,
// by simply setting MeshAddr/DockerHost on the *node.Fake they map a
// non-local name to.
func (fr FakeResolver) ResolveMeta(nodeName string) (Node, string, string, bool, error) {
	n, err := fr.Resolve(nodeName)
	if err != nil {
		return nil, "", "", false, err
	}
	isLocal := nodeName == "" || nodeName == "local"
	meshAddr := ""
	dockerHost := ""
	if !isLocal {
		if fk, ok := n.(*Fake); ok {
			meshAddr = fk.MeshAddr
			dockerHost = fk.DockerHost
		}
	}
	return n, meshAddr, dockerHost, isLocal, nil
}

// Compile-time assertions that RegistryResolver and FakeResolver implement
// Resolver.
var _ Resolver = (*RegistryResolver)(nil)
var _ Resolver = (FakeResolver)(nil)
