// Package api exposes the daemon's HTTP API. The CLI and (later) the web UI are
// its only clients. It holds no business logic beyond request/response mapping.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/router"
	"lwd/internal/secrets"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// NodeCacheInvalidator lets the API evict a resolver's cached remote node
// entry when a node's registry row changes — added, updated, or removed via
// POST/DELETE /nodes — so a stale docker-over-ssh client never lingers past
// the change. *node.RegistryResolver satisfies this in production; tests may
// pass nil (checked before use) or a fake that records calls.
type NodeCacheInvalidator interface {
	Invalidate(name string)
}

// Server wires HTTP routes to the reconciler, store, node, router, and
// secrets store.
type Server struct {
	rec         *reconciler.Reconciler
	store       *store.Store
	node        node.Node
	router      router.Router
	secrets     *secrets.Store
	invalidator NodeCacheInvalidator
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
// (POST/DELETE /nodes) skip cache invalidation — fine for tests that don't
// exercise a resolver, but the daemon must pass its real RegistryResolver.
func New(r *reconciler.Reconciler, s *store.Store, n node.Node, rt router.Router, sec *secrets.Store, inv NodeCacheInvalidator) *Server {
	return &Server{rec: r, store: s, node: n, router: rt, secrets: sec, invalidator: inv}
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
}

// invalidateNode evicts the resolver's cached remote node for name, if the
// Server was given an invalidator. Called after every registry mutation
// (add/update via POST, remove via DELETE) so a stale docker-over-ssh client
// from before the change never lingers.
func (srv *Server) invalidateNode(name string) {
	if srv.invalidator != nil {
		srv.invalidator.Invalidate(name)
	}
}

// handleNodeAdd registers (or updates, upsert-by-name) a node in the store's
// registry. All three fields are required — an empty name, ssh_host, or
// mesh_addr each produce a 400 with a clear error, since a partially
// specified node can't be resolved into a working docker-over-ssh Node
// later.
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
	if req.SSHHost == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("ssh_host is required"))
		return
	}
	if req.MeshAddr == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("mesh_addr is required"))
		return
	}
	n := store.Node{Name: req.Name, SSHHost: req.SSHHost, MeshAddr: req.MeshAddr, CreatedAt: time.Now()}
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

func (srv *Server) handleNodeList(w http.ResponseWriter, r *http.Request) {
	nodes, err := srv.store.ListNodes()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, nodes)
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
