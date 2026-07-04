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
