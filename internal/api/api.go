// Package api exposes the daemon's HTTP API. The CLI and (later) the web UI are
// its only clients. It holds no business logic beyond request/response mapping.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/router"
	"lwd/internal/secrets"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// hostnamePattern matches a plausible bare hostname (a single DNS label or
// dotted sequence of them — e.g. "web1", "web1.internal", "10.0.0.1" also
// happens to match but that's fine, IPs are valid mesh addresses too). It
// deliberately rejects whitespace, a URL scheme, or other garbage that could
// never be dialed as a docker-over-ssh host.
var hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// poolNamePattern matches a valid pool name: it must start with an
// alphanumeric character and may otherwise contain alphanumerics, "_", or
// "-". This keeps pool names safe to use as path segments / labels
// elsewhere without further escaping.
var poolNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// NodeResolver lets the API evict a resolver's cached remote node entry when
// a node's registry row changes — added, updated, or removed via POST/DELETE
// /nodes — so a stale docker-over-ssh client never lingers past the change,
// and lets it report a node's live reachability for GET /nodes.
// *node.RegistryResolver satisfies this in production; tests may pass nil
// (checked before use) or a fake that records calls.
type NodeResolver interface {
	Invalidate(name string)
	// Reachable reports which transport would be used for name ("agent",
	// "ssh", or "local") and whether that transport currently answers a
	// ping. It never errors — an unknown name or a failed lookup reports
	// ("", false).
	Reachable(ctx context.Context, name string) (transport string, ok bool)
}

// Server wires HTTP routes to the reconciler, store, node, router, and
// secrets store.
type Server struct {
	rec      *reconciler.Reconciler
	store    *store.Store
	node     node.Node
	router   router.Router
	secrets  *secrets.Store
	resolver NodeResolver
}

// AppStatus is the wire representation of an app's current state.
type AppStatus struct {
	Name        string `json:"name"`
	Image       string `json:"image"`
	ContainerID string `json:"container_id"`
	Status      string `json:"status"`
	Domain      string `json:"domain"`
}

// New returns a Server. inv may be nil, in which case node registry changes
// (POST/DELETE /nodes) skip cache invalidation, and GET /nodes reports every
// node's transport as "" and reachable as false (no ping) — fine for tests
// that don't exercise a resolver, but the daemon must pass its real
// RegistryResolver.
func New(r *reconciler.Reconciler, s *store.Store, n node.Node, rt router.Router, sec *secrets.Store, inv NodeResolver) *Server {
	return &Server{rec: r, store: s, node: n, router: rt, secrets: sec, resolver: inv}
}

// Handler returns the HTTP handler for all routes.
func (srv *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /apply", srv.handleApply)
	mux.HandleFunc("GET /apps", srv.handleApps)
	mux.HandleFunc("GET /apps/{name}/logs", srv.handleLogs)
	mux.HandleFunc("GET /apps/{name}/history", srv.handleHistory)
	mux.HandleFunc("POST /apps/{name}/rollback", srv.handleRollback)
	mux.HandleFunc("DELETE /apps/{name}", srv.handleDelete)
	mux.HandleFunc("POST /apps/{name}/secrets", srv.handleSecretSet)
	mux.HandleFunc("GET /apps/{name}/secrets", srv.handleSecretList)
	mux.HandleFunc("DELETE /apps/{name}/secrets/{key}", srv.handleSecretDelete)
	mux.HandleFunc("POST /nodes", srv.handleNodeAdd)
	mux.HandleFunc("GET /nodes", srv.handleNodeList)
	mux.HandleFunc("DELETE /nodes/{name}", srv.handleNodeDelete)
	mux.HandleFunc("GET /pools", srv.handlePools)
	mux.HandleFunc("GET /health", srv.handleHealth)
	return mux
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (srv *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var app spec.App
	if err := json.NewDecoder(r.Body).Decode(&app); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := app.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	dep, err := srv.rec.Apply(r.Context(), &app)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, dep)
}

func (srv *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	dep, err := srv.rec.Rollback(r.Context(), name)
	if err != nil {
		if strings.Contains(err.Error(), "no previous deployment") {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, dep)
}

func (srv *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	names, err := srv.store.ListApps()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]AppStatus, 0, len(names))
	for _, name := range names {
		cur, err := srv.store.CurrentDeployment(name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		st := AppStatus{Name: name, Status: store.StatusRetired}
		if cur != nil {
			st.Image = cur.Image
			st.ContainerID = cur.ContainerID
			st.Status = cur.Status
			var a spec.App
			if err := json.Unmarshal([]byte(cur.Spec), &a); err == nil {
				st.Domain = a.Domain
			}
		}
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, out)
}

func (srv *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cur, err := srv.store.CurrentDeployment(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if cur == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("app %q not found", name))
		return
	}
	follow := r.URL.Query().Get("follow") == "true"
	rc, err := srv.node.ContainerLogs(r.Context(), cur.ContainerID, follow)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func (srv *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	deps, err := srv.store.DeploymentsForApp(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, deps)
}

func (srv *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := srv.rec.Remove(r.Context(), name); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// secretRequest is the wire body for POST /apps/{name}/secrets. Values never
// appear in any response — only in this request body.
type secretRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (srv *Server) handleSecretSet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req secretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Key == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("key is required"))
		return
	}
	if err := srv.secrets.Set(name, req.Key, req.Value); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) handleSecretList(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	names, err := srv.secrets.List(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, names)
}

func (srv *Server) handleSecretDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	key := r.PathValue("key")
	if err := srv.secrets.Delete(name, key); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// nodeRequest is the wire body for POST /nodes.
type nodeRequest struct {
	Name     string `json:"name"`
	SSHHost  string `json:"ssh_host"`
	MeshAddr string `json:"mesh_addr"`
	AgentURL string `json:"agent_url"`
	Pool     string `json:"pool"`
}

// nodeStatusResponse is the wire shape for entries in the GET /nodes
// response: a registered node plus its live reachability, as reported by the
// Server's NodeResolver. It mirrors client.NodeStatus, which decodes it —
// the round-trip between the two is covered by api tests.
type nodeStatusResponse struct {
	store.Node
	Transport string `json:"transport"`
	Reachable bool   `json:"reachable"`
}

// invalidateNode evicts the resolver's cached remote node for name, if the
// Server was given a resolver. Called after every registry mutation
// (add/update via POST, remove via DELETE) so a stale docker-over-ssh client
// from before the change never lingers.
func (srv *Server) invalidateNode(name string) {
	if srv.resolver != nil {
		srv.resolver.Invalidate(name)
	}
}

// handleNodeAdd registers (or updates, upsert-by-name) a node in the store's
// registry. All three fields are required — an empty name, ssh_host, or
// mesh_addr each produce a 400 with a clear error, since a partially
// specified node can't be resolved into a working docker-over-ssh Node
// later. "local" is rejected too: it is the implicit, always-present local
// node (node.Resolver treats "" and "local" as the local Docker daemon), so
// registering a remote node under that name would either be silently inert
// or shadow the real local node in confusing ways. mesh_addr is checked
// against a plausible IP-or-hostname shape (net.ParseIP or hostnamePattern)
// to catch whitespace, a URL scheme, or other garbage early, before it ever
// reaches a docker-over-ssh dial or a Caddy upstream.
func (srv *Server) handleNodeAdd(w http.ResponseWriter, r *http.Request) {
	var req nodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("name is required"))
		return
	}
	if req.Name == "local" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("node name %q is reserved for the implicit local node", req.Name))
		return
	}
	if req.SSHHost == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("ssh_host is required"))
		return
	}
	if req.MeshAddr == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("mesh_addr is required"))
		return
	}
	if net.ParseIP(req.MeshAddr) == nil && !hostnamePattern.MatchString(req.MeshAddr) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("mesh_addr %q is not a valid IP address or hostname", req.MeshAddr))
		return
	}
	if req.AgentURL != "" {
		u, err := url.Parse(req.AgentURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("agent_url %q is not a valid http(s) URL", req.AgentURL))
			return
		}
	}
	if req.Pool != "" && !poolNamePattern.MatchString(req.Pool) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("pool %q is invalid: must match %s", req.Pool, poolNamePattern.String()))
		return
	}
	n := store.Node{Name: req.Name, SSHHost: req.SSHHost, MeshAddr: req.MeshAddr, AgentURL: req.AgentURL, Pool: req.Pool, CreatedAt: time.Now()}
	if err := srv.store.AddNode(n); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// The add may be an update to an existing node's ssh_host/mesh_addr —
	// invalidate any cached remote Node for this name so the next deploy to
	// it re-resolves against the new values instead of reusing a stale ssh
	// client built from the old ones.
	srv.invalidateNode(req.Name)
	w.WriteHeader(http.StatusNoContent)
}

// handleNodeList lists registered nodes along with each one's live
// reachability. Reachability is probed in parallel, one goroutine per node,
// so an N-node list never serializes N pings (each bounded to a few seconds
// by the resolver) into an N*ping-timeout response. If the Server has no
// resolver (nil — test servers that don't exercise one), every node reports
// transport "" and reachable false without any ping.
func (srv *Server) handleNodeList(w http.ResponseWriter, r *http.Request) {
	nodes, err := srv.store.ListNodes()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]nodeStatusResponse, len(nodes))
	for i, n := range nodes {
		out[i] = nodeStatusResponse{Node: n}
	}
	if srv.resolver != nil {
		var wg sync.WaitGroup
		for i, n := range nodes {
			wg.Add(1)
			go func(i int, name string) {
				defer wg.Done()
				transport, ok := srv.resolver.Reachable(r.Context(), name)
				out[i].Transport = transport
				out[i].Reachable = ok
			}(i, n.Name)
		}
		wg.Wait()
	}
	writeJSON(w, http.StatusOK, out)
}

func (srv *Server) handleNodeDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := srv.store.DeleteNode(name); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	srv.invalidateNode(name)
	w.WriteHeader(http.StatusNoContent)
}

// poolInfo is the wire shape for entries in the GET /pools response: a pool
// name and the number of registered nodes in it.
type poolInfo struct {
	Name  string `json:"name"`
	Nodes int    `json:"nodes"`
}

// handlePools lists every pool with a registered node in it, plus the count
// of nodes in each. "default" is always included, seeded at 1 — the
// implicit "local" node (the controller's own Docker, never stored in the
// registry) is always a member of "default" — before adding registered
// store nodes on top.
func (srv *Server) handlePools(w http.ResponseWriter, r *http.Request) {
	nodes, err := srv.store.ListNodes()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	counts := map[string]int{store.DefaultPool: 1}
	for _, n := range nodes {
		counts[n.Pool]++
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]poolInfo, len(names))
	for i, name := range names {
		out[i] = poolInfo{Name: name, Nodes: counts[name]}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleHealth serves the reconciler's current in-memory Health snapshot
// (node reachability, edge/router reachability, and per-app surface health)
// as populated by the continuous reconciler loop's most recent pass. It
// carries no secret values — just reachability booleans and heal
// bookkeeping — so it's safe to expose read-only with no additional
// filtering.
//
// Nodes/Apps are normalized to empty (rather than null) slices before
// serializing — matching handleApps/handleNodeList's own non-nil-slice
// convention — since both are legitimately nil in steady state (no
// Reachability configured, or no image/git app deployed yet; see
// reconciler.probeNodes and reconcileApp), not just during an initial
// bootstrap race.
func (srv *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h := srv.rec.HealthSnapshot()
	if h.Nodes == nil {
		h.Nodes = []reconciler.NodeHealth{}
	}
	if h.Apps == nil {
		h.Apps = []reconciler.AppHealth{}
	}
	writeJSON(w, http.StatusOK, h)
}
