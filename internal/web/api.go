package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"lwd/internal/api"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// appDetail is the wire response for GET /api/apps/{name}.
type appDetail struct {
	Status  *api.AppStatus     `json:"status"`
	History []store.Deployment `json:"history"`
}

func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.client.Apps(r.Context())
	if err != nil {
		writeClientErr(w, err)
		return
	}
	if apps == nil {
		apps = []api.AppStatus{}
	}
	writeJSON(w, http.StatusOK, apps)
}

func (s *Server) handleAppDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	apps, err := s.client.Apps(r.Context())
	if err != nil {
		writeClientErr(w, err)
		return
	}
	var status *api.AppStatus
	for i := range apps {
		if apps[i].Name == name {
			status = &apps[i]
			break
		}
	}

	history, err := s.client.History(r.Context(), name)
	if err != nil {
		writeClientErr(w, err)
		return
	}
	if history == nil {
		history = []store.Deployment{}
	}

	writeJSON(w, http.StatusOK, appDetail{Status: status, History: history})
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	dep, err := s.client.Rollback(r.Context(), name)
	if err != nil {
		writeClientErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dep)
}

// scaleRequest is the wire body for POST /api/apps/{name}/scale.
type scaleRequest struct {
	Replicas int `json:"replicas"`
}

// handleScale proxies the daemon's POST /apps/{name}/scale (Phase 12 Task
// 7): change a running app's replica count, redeploying it set-based via
// client.Scale, and render the resulting store.Deployment — the same shape
// as rollback/redeploy. It is gated by the session Middleware like every
// other /api route; replicas-count validation (>= 1, <= 50, and the
// compose/backing-services guards) all live in the daemon (internal/api's
// own handleScale), so this handler is a thin, unvalidated-body pass-through.
func (s *Server) handleScale(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var req scaleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	dep, err := s.client.Scale(r.Context(), name, req.Replicas)
	if err != nil {
		writeClientErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dep)
}

// handleRedeploy re-applies the newest recorded deployment's spec snapshot
// for the app (e.g. after a config change on the daemon host, or simply to
// restart it). 404 if the app has no deployment history.
func (s *Server) handleRedeploy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	history, err := s.client.History(r.Context(), name)
	if err != nil {
		writeClientErr(w, err)
		return
	}
	if len(history) == 0 {
		writeErr(w, http.StatusNotFound, fmt.Errorf("app %q has no deployment history", name))
		return
	}

	var app spec.App
	if err := json.Unmarshal([]byte(history[0].Spec), &app); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("decode stored spec: %w", err))
		return
	}
	// history[0] is the exact deployment whose Spec we just decoded, so its
	// Scheduled field is that deployment's own placement provenance. A
	// scheduler-placed (unpinned) app's snapshot has a CONCRETE Node baked in
	// (applyImageProvenance sets app.Node = chosen before recording it), so
	// replaying it as-is through client.Apply -> daemon POST /apply would
	// make resolvePlacement see a non-empty Node and misclassify it as
	// pinned — collapsing every replica onto one node and recording
	// Scheduled=false, disabling node-loss failover going forward. Clearing
	// Node when Scheduled re-invokes the scheduler on redeploy, exactly like
	// handleScale's (internal/api) same fix for `lwd scale`. A pinned app
	// (Scheduled == false) is left with its snapshot's Node untouched.
	if history[0].Scheduled {
		app.Node = ""
	}

	dep, err := s.client.Apply(r.Context(), &app)
	if err != nil {
		writeClientErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dep)
}

// handleApply reads the request body as an lwd.toml document, parses and
// validates it, and applies it via the daemon.
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	app, err := spec.Parse(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := app.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	dep, err := s.client.Apply(r.Context(), app)
	if err != nil {
		writeClientErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dep)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.client.Remove(r.Context(), name); err != nil {
		writeClientErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSecretList(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	names, err := s.client.ListSecrets(r.Context(), name)
	if err != nil {
		writeClientErr(w, err)
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, names)
}

func (s *Server) handleSecretSet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	key, value, err := parseSecretRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if key == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("key is required"))
		return
	}

	if err := s.client.SetSecret(r.Context(), name, key, value); err != nil {
		writeClientErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSecretDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	key := r.PathValue("key")
	if err := s.client.DeleteSecret(r.Context(), name, key); err != nil {
		writeClientErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseSecretRequest reads key/value from either a JSON body
// ({"key":...,"value":...}) or a form-encoded body (key=...&value=...),
// depending on Content-Type.
func parseSecretRequest(r *http.Request) (key, value string, err error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		var req struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return "", "", err
		}
		return req.Key, req.Value, nil
	}
	if err := r.ParseForm(); err != nil {
		return "", "", err
	}
	return r.FormValue("key"), r.FormValue("value"), nil
}
