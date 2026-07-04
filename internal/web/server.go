package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"

	"lwd/internal/api"
	"lwd/internal/client"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// DaemonClient is the subset of *client.Client that the web layer uses. Its
// method signatures must match *client.Client exactly so that value satisfies
// this interface; a fake implements it for handler tests.
type DaemonClient interface {
	Apps(ctx context.Context) ([]api.AppStatus, error)
	History(ctx context.Context, name string) ([]store.Deployment, error)
	Logs(ctx context.Context, name string, follow bool, w io.Writer) error
	Apply(ctx context.Context, app *spec.App) (*store.Deployment, error)
	Rollback(ctx context.Context, name string) (*store.Deployment, error)
	Remove(ctx context.Context, name string) error
	SetSecret(ctx context.Context, app, key, value string) error
	ListSecrets(ctx context.Context, app string) ([]string, error)
	DeleteSecret(ctx context.Context, app, key string) error
}

// The real daemon client must satisfy DaemonClient. This assertion fails the
// build if internal/client drifts from the interface lwd-web depends on.
var _ DaemonClient = (*client.Client)(nil)

// Server wires the browser-facing JSON API to a DaemonClient, gated by an
// Authenticator's session middleware.
type Server struct {
	client DaemonClient
	auth   *Authenticator
}

// NewServer returns a Server.
func NewServer(c DaemonClient, a *Authenticator) *Server {
	return &Server{client: c, auth: a}
}

// Handler returns the full HTTP handler for lwd-web: login/logout and the
// browser JSON API, wrapped by the session-authentication middleware.
// Embedded static assets (the dashboard UI) are wired in a later task; for
// now "/" serves a minimal placeholder so the route (and the auth
// middleware's allowlist) are already in place.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /login", s.auth.Login)
	mux.HandleFunc("POST /logout", s.auth.Logout)

	mux.HandleFunc("GET /api/apps", s.handleApps)
	mux.HandleFunc("GET /api/apps/{name}", s.handleAppDetail)
	mux.HandleFunc("POST /api/apps/{name}/rollback", s.handleRollback)
	mux.HandleFunc("POST /api/apps/{name}/redeploy", s.handleRedeploy)
	mux.HandleFunc("POST /api/apply", s.handleApply)
	mux.HandleFunc("DELETE /api/apps/{name}", s.handleDelete)
	mux.HandleFunc("GET /api/apps/{name}/secrets", s.handleSecretList)
	mux.HandleFunc("POST /api/apps/{name}/secrets", s.handleSecretSet)
	mux.HandleFunc("DELETE /api/apps/{name}/secrets/{key}", s.handleSecretDelete)
	mux.HandleFunc("GET /api/apps/{name}/logs", s.handleLogs)

	// Embedded static assets (placeholder UI; Task 5 replaces the contents
	// of internal/web/assets/ with the crafted design). "/", "/login", and
	// "/static/" are public per the auth middleware's allowlist: the app
	// shell's JS calls the authed /api endpoints and redirects to /login
	// itself on a 401.
	mux.HandleFunc("GET /login", s.loginPageHandler)
	mux.Handle("GET /static/", s.staticHandler())
	mux.HandleFunc("GET /", s.indexHandler)

	return s.auth.Middleware(mux)
}

// writeJSON writes v as a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr writes {"error": err.Error()} with the given status code,
// mirroring the daemon API's error shape.
func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// writeClientErr classifies an error returned from a DaemonClient call.
// Network-level failures (the daemon's unix socket is unreachable) become
// 502 so the browser can distinguish "daemon down" from an application
// error; anything else is a 500.
func writeClientErr(w http.ResponseWriter, err error) {
	var netErr net.Error
	var urlErr *url.Error
	if errors.As(err, &netErr) || errors.As(err, &urlErr) {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}
