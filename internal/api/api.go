// Package api exposes the daemon's HTTP API. The CLI and (later) the web UI are
// its only clients. It holds no business logic beyond request/response mapping.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/router"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// Server wires HTTP routes to the reconciler, store, node, and router.
type Server struct {
	rec    *reconciler.Reconciler
	store  *store.Store
	node   node.Node
	router router.Router
}

// AppStatus is the wire representation of an app's current state.
type AppStatus struct {
	Name        string `json:"name"`
	Image       string `json:"image"`
	ContainerID string `json:"container_id"`
	Status      string `json:"status"`
	Domain      string `json:"domain"`
}

// New returns a Server.
func New(r *reconciler.Reconciler, s *store.Store, n node.Node, rt router.Router) *Server {
	return &Server{rec: r, store: s, node: n, router: rt}
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
	if err := srv.removeApp(r.Context(), name); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) removeApp(ctx context.Context, name string) error {
	cur, err := srv.store.CurrentDeployment(name)
	if err != nil {
		return err
	}
	var domain string
	if cur != nil && cur.Spec != "" {
		var a spec.App
		if err := json.Unmarshal([]byte(cur.Spec), &a); err == nil {
			domain = a.Domain
		}
	}

	containers, err := srv.node.ListContainers(ctx, map[string]string{"lwd.app": name})
	if err != nil {
		return err
	}
	for _, c := range containers {
		if err := srv.node.RemoveContainer(ctx, c.ID); err != nil {
			return err
		}
	}

	if domain != "" {
		// Best-effort: a failure here shouldn't stop the app from being
		// retired (its containers are already gone), but it does mean the
		// domain may keep 502ing until a later reload/rm fixes it up.
		_ = srv.router.RemoveRoute(ctx, domain)
	}

	if cur != nil {
		return srv.store.SetStatus(cur.ID, store.StatusRetired)
	}
	return nil
}
