package node

import (
	"context"
	"fmt"
	"sync"
	"time"
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
// host its registry row carried, and which transport ("agent" or "ssh") was
// selected for it, so a repeat Resolve/ResolveMeta/Reachable for the same
// name doesn't need to re-run lookup or re-probe.
type remoteEntry struct {
	node       Node
	meshAddr   string
	dockerHost string
	transport  string
}

// agentPingTimeout bounds how long ResolveMeta/Reachable wait for a remote
// node's agent to answer /healthz before falling back to (or reporting)
// docker-over-ssh.
const agentPingTimeout = 3 * time.Second

// RegistryResolver is the production Resolver: "" and "local" resolve to a
// fixed local Node, and any other name is resolved via lookup — a closure
// the daemon supplies over its store's nodes registry — so this package
// never imports internal/store (which would create an import cycle, since
// store already depends on nothing here but the daemon wires node and store
// together). A remote Node is cached by name once built: repeated deploys to
// the same node reuse one client (agent HTTP or ssh Docker) rather than
// re-probing or re-dialing on every Resolve.
//
// For a registered node whose lookup row carries an agent_url, ResolveMeta
// prefers the P9b agent transport (NewAgentNode) over the P9a
// docker-over-ssh transport (NewRemoteSSH), but only if the agent actually
// answers its /healthz endpoint; otherwise it falls back to ssh exactly as
// in P9a. meshAddr and dockerHost are unaffected by this choice — they
// describe the node's registry row, not which transport executes its
// container primitives.
type RegistryResolver struct {
	local Node
	token string

	// lookup must return the node's ssh host, mesh address, and agent base
	// URL (agentURL is "" if the node has none registered) and ok=true if
	// name is registered, ok=false (no error) if it is not, or a non-nil
	// error if the lookup itself failed (e.g. a store error) —
	// Resolve/ResolveMeta/Reachable propagate each of these distinctly.
	lookup func(name string) (sshHost, meshAddr, agentURL string, ok bool, err error)

	// newAgent and newSSH build the two remote transports; both are
	// overridden in tests (to return *Fake Nodes) so transport-selection
	// logic can be exercised without a real network or ssh binary. Their
	// defaults, set by NewRegistryResolver, are production behavior.
	newAgent func(baseURL, token string) Node
	newSSH   func(sshHost string) (Node, error)

	mu      sync.Mutex
	remotes map[string]remoteEntry
}

// NewRegistryResolver returns a RegistryResolver. token is the shared agent
// bearer token (the controller's LWD_AGENT_TOKEN) used to authenticate to
// every registered node's agent. lookup must return the node's ssh host,
// mesh address, and agent URL and ok=true if name is registered, ok=false
// (no error) if it is not, or a non-nil error if the lookup itself failed
// (e.g. a store error) — Resolve/ResolveMeta propagate each of these
// distinctly.
func NewRegistryResolver(local Node, token string, lookup func(name string) (sshHost, meshAddr, agentURL string, ok bool, err error)) *RegistryResolver {
	return &RegistryResolver{
		local:   local,
		token:   token,
		lookup:  lookup,
		remotes: map[string]remoteEntry{},
		newAgent: func(baseURL, token string) Node {
			return NewAgentNode(baseURL, token)
		},
		newSSH: func(sshHost string) (Node, error) {
			return NewRemoteSSH(sshHost)
		},
	}
}

// Resolve returns the local node for "" or "local". For any other name, it
// returns a cached remote node if one was already built for that name;
// otherwise it calls lookup once, and on success builds and caches a remote
// Node (preferring the agent transport, see ResolveMeta) before returning
// it. An unregistered name (lookup ok=false) produces an "unknown node"
// error.
func (rr *RegistryResolver) Resolve(nodeName string) (Node, error) {
	n, _, _, _, err := rr.ResolveMeta(nodeName)
	return n, err
}

// ResolveMeta is Resolve plus the resolved node's mesh address, its
// DOCKER_HOST target, and whether it is the local node. See
// Resolver.ResolveMeta.
//
// For a registered (non-local) name, once lookup succeeds: if the row
// carries an agent_url, ResolveMeta builds an agent Node and pings it with a
// short timeout; on a successful ping that agent Node is used (transport
// "agent"). Otherwise — no agent_url, or the ping failed — a
// docker-over-ssh Node is built via newSSH (transport "ssh"), exactly as in
// P9a. meshAddr and dockerHost are unchanged by this choice: meshAddr is the
// looked-up mesh address, and dockerHost is always "ssh://" + sshHost
// regardless of which transport executes the container primitives.
func (rr *RegistryResolver) ResolveMeta(nodeName string) (Node, string, string, bool, error) {
	if nodeName == "" || nodeName == "local" {
		return rr.local, "", "", true, nil
	}

	rr.mu.Lock()
	defer rr.mu.Unlock()

	if e, ok := rr.remotes[nodeName]; ok {
		return e.node, e.meshAddr, e.dockerHost, false, nil
	}

	sshHost, meshAddr, agentURL, ok, err := rr.lookup(nodeName)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("look up node %q: %w", nodeName, err)
	}
	if !ok {
		return nil, "", "", false, fmt.Errorf("unknown node %q", nodeName)
	}

	dockerHost := "ssh://" + sshHost

	n, transport, err := rr.buildTransport(sshHost, agentURL)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("connect to node %q (ssh://%s): %w", nodeName, sshHost, err)
	}

	rr.remotes[nodeName] = remoteEntry{node: n, meshAddr: meshAddr, dockerHost: dockerHost, transport: transport}
	return n, meshAddr, dockerHost, false, nil
}

// pingOK reports whether n answers a Ping within agentPingTimeout, bounding
// ctx so no caller (not even one passing context.Background()) can hang on an
// unresponsive node. It is the single place any resolver path bounds a ping.
func pingOK(ctx context.Context, n Node) bool {
	ctx, cancel := context.WithTimeout(ctx, agentPingTimeout)
	defer cancel()
	return n.Ping(ctx) == nil
}

// buildTransport is the SOLE decision point for agent-vs-ssh: it selects and
// builds the Node for a registered remote node — the agent transport if
// agentURL is set and its /healthz ping succeeds (ping bounded by
// agentPingTimeout via pingOK), otherwise docker-over-ssh via newSSH. There
// is no ssh re-ping: a successful ssh build is usable (P9a semantics). Both
// ResolveMeta and Reachable route through this function so the selection
// logic can never drift between the two.
func (rr *RegistryResolver) buildTransport(sshHost, agentURL string) (Node, string, error) {
	if agentURL != "" {
		an := rr.newAgent(agentURL, rr.token)
		if pingOK(context.Background(), an) {
			return an, "agent", nil
		}
	}
	n, err := rr.newSSH(sshHost)
	if err != nil {
		// transport is still "ssh" (what we attempted) so callers that only
		// want to report the attempted transport, like Reachable, can — while
		// ResolveMeta wraps and propagates err and ignores the transport.
		return nil, "ssh", err
	}
	return n, "ssh", nil
}

// Invalidate evicts the cached remote node for name, if any, so a subsequent
// Resolve/ResolveMeta re-runs lookup and rebuilds (and, if applicable,
// re-probes the agent for) a fresh Node instead of returning a stale cached
// one. Call this whenever a node's registry row changes — added/updated
// (ssh_host, mesh_addr, or agent_url may have changed) or removed — so a
// stale client never lingers. A no-op if nodeName has no cached entry (e.g.
// it was never resolved, or is "local").
func (rr *RegistryResolver) Invalidate(nodeName string) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	delete(rr.remotes, nodeName)
}

// Reachable reports which transport would be used (or was cached) for name
// and whether that transport currently answers a ping — status information
// for the CLI/web UI, distinct from Resolve/ResolveMeta because it must
// never fail a deploy: it always returns an answer, never an error.
//
// "" and "local" always report ("local", true). For any other name: if a
// remote Node is already cached for it, that cached node's transport is
// pinged directly (bounded via pingOK). Otherwise lookup is run fresh
// (without caching the result) — a lookup error or unregistered name reports
// ("", false); the agent-vs-ssh decision is delegated to buildTransport (the
// single source of that logic), and the chosen node is then pinged once via
// pingOK to report reachability. A transport that fails to even build (e.g.
// ssh key setup) is reported as not reachable with the transport it
// attempted, rather than panicking. Every ping here is bounded by
// agentPingTimeout, so Reachable never hangs even when ctx has no deadline.
func (rr *RegistryResolver) Reachable(ctx context.Context, name string) (string, bool) {
	if name == "" || name == "local" {
		return "local", true
	}

	rr.mu.Lock()
	e, cached := rr.remotes[name]
	rr.mu.Unlock()

	if cached {
		return e.transport, pingOK(ctx, e.node)
	}

	sshHost, _, agentURL, ok, err := rr.lookup(name)
	if err != nil || !ok {
		return "", false
	}

	n, transport, berr := rr.buildTransport(sshHost, agentURL)
	if berr != nil {
		// transport reflects what we attempted (buildTransport only errors
		// on the ssh-build path, so this is "ssh"); report not-reachable.
		return transport, false
	}
	return transport, pingOK(ctx, n)
}

// FakeResolver is a Resolver backed by a plain map, for tests: it returns
// the mapped Node for a name, or an "unknown node" error if the name isn't
// present. Unlike RegistryResolver it does not special-case "" or "local" —
// tests that want those to resolve must map them explicitly (as
// spec.App.Node may be "" — Parse preserves an unset node rather than
// defaulting it — tests typically just do FakeResolver{"local": someFake}
// and set Node: "local" explicitly on the App they construct).
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
