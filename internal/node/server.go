package node

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"lwd/internal/bundle"
	"lwd/internal/version"
)

const maxBundleBytes = 8 << 20

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// Handler returns the node's /v1 API.
func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", n.handleHealth)
	mux.Handle("GET /v1/status", n.auth(n.handleStatus))
	mux.Handle("POST /v1/deploy", n.auth(n.handleDeploy))
	mux.Handle("POST /v1/apps/{app}/{env}/restart", n.auth(n.handleRestart))
	mux.Handle("GET /v1/apps/{app}/{env}/logs", n.auth(n.handleLogs))
	mux.Handle("DELETE /v1/apps/{app}/{env}", n.auth(n.handleDelete))
	mux.Handle("/", n.auth(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path)
	}))
	return mux
}

func (n *Node) auth(h http.HandlerFunc) http.Handler {
	want := []byte("Bearer " + n.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		h(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// writeErr maps node errors onto the API's error codes.
func writeErr(w http.ResponseWriter, err error) {
	var ie *invalidError
	switch {
	case errors.As(err, &ie):
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, errBusy):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	default:
		log.Printf("internal error: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

var errNotFound = errors.New("no live deployment for this app-env")

func (n *Node) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version.String})
}

func (n *Node) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var b bundle.Bundle
	// Unknown fields are tolerated so a newer controller can talk to an older
	// node during a rolling upgrade.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBundleBytes)).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "decode bundle: "+err.Error())
		return
	}
	// A deploy must run to a consistent end even if the controller hangs up
	// mid-cutover; the outcome is on disk in result.json and state.json.
	res, err := n.Deploy(context.WithoutCancel(r.Context()), &b)
	if err != nil {
		writeErr(w, err)
		return
	}
	log.Printf("deploy %s-%s d%d: %s at %s %s", b.App, b.Env, b.Deployment, res.Status, res.Phase, res.Message)
	writeJSON(w, http.StatusOK, res)
}

// target resolves {app}/{env} to its key and live deployment.
func (n *Node) target(r *http.Request) (key string, live *Live, err error) {
	app, env := r.PathValue("app"), r.PathValue("env")
	if !nameRE.MatchString(app) || !nameRE.MatchString(env) {
		return "", nil, invalidf("invalid app/env %q/%q", app, env)
	}
	key = app + "-" + env
	st, err := loadState(n.statePath(key))
	if err != nil {
		return "", nil, err
	}
	if st.Live == nil {
		return key, nil, errNotFound
	}
	return key, st.Live, nil
}

func (n *Node) liveProject(key string, live *Live) project {
	return deploymentProject(live.Project, filepath.Join(n.appDir(key), "d"+strconv.FormatInt(live.Deployment, 10)))
}

func serviceArg(r *http.Request) ([]string, error) {
	s := r.URL.Query().Get("service")
	if s == "" {
		return nil, nil
	}
	if !nameRE.MatchString(s) {
		return nil, invalidf("invalid service %q", s)
	}
	return []string{s}, nil
}

func (n *Node) handleRestart(w http.ResponseWriter, r *http.Request) {
	svc, err := serviceArg(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	key, live, err := n.lockedTarget(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer n.unlock(key)
	if _, err := n.docker.Run(r.Context(), n.liveProject(key, live).args(append([]string{"restart"}, svc...)...)...); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// lockedTarget is target plus the app-env lock; the caller must unlock key.
func (n *Node) lockedTarget(r *http.Request) (string, *Live, error) {
	key, _, err := n.target(r)
	if err != nil {
		return "", nil, err
	}
	if !n.tryLock(key) {
		return "", nil, errBusy
	}
	// Re-read under the lock: a deploy may have just finished.
	_, live, err := n.target(r)
	if err != nil {
		n.unlock(key)
		return "", nil, err
	}
	return key, live, nil
}

func (n *Node) handleLogs(w http.ResponseWriter, r *http.Request) {
	svc, err := serviceArg(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	tail := 100
	if t := r.URL.Query().Get("tail"); t != "" {
		tail, err = strconv.Atoi(t)
		if err != nil || tail < 1 || tail > 10000 {
			writeErr(w, invalidf("tail must be 1-10000"))
			return
		}
	}
	key, live, err := n.target(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	args := append([]string{"logs", "--no-color", "--tail", strconv.Itoa(tail)}, svc...)
	out, err := n.docker.Run(r.Context(), n.liveProject(key, live).args(args...)...)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(out)
}

func (n *Node) handleDelete(w http.ResponseWriter, r *http.Request) {
	key, live, err := n.lockedTarget(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer n.unlock(key)
	// Routes go first so Caddy never proxies to a stopped project; if down
	// then fails, state stays and the delete can be retried.
	if err := n.setRoutes(r.Context(), key, nil); err != nil {
		writeErr(w, fmt.Errorf("remove routes: %w", err))
		return
	}
	if _, err := n.docker.Run(r.Context(), n.liveProject(key, live).args("down")...); err != nil {
		writeErr(w, err)
		return
	}
	if err := os.RemoveAll(n.appDir(key)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
