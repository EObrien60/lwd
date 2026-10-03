package controller

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"lwd/internal/client"
	"lwd/internal/store"
)

// Request body limits: manifests and secrets are small; anything bigger is a
// mistake (or abuse) and is refused rather than buffered.
const (
	maxJSONBody     = 1 << 20
	maxManifestBody = 1 << 20
	maxSecretBody   = 64 << 10
)

// Handler returns the /v1 API. Every route, including unknown ones, requires
// the bearer token.
// Handler serves /v1. token is the admin bearer; readTokens (optional) may
// only make GET/HEAD requests — for observers such as the agentd console.
func (c *Controller) Handler(token string, readTokens ...string) http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, fn func(http.ResponseWriter, *http.Request) error) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if err := fn(w, r); err != nil {
				c.writeError(w, r, err)
			}
		})
	}

	h("POST /v1/hosts", c.postHost)
	h("GET /v1/hosts", func(w http.ResponseWriter, r *http.Request) error {
		hs, err := c.store.ListHosts(r.Context())
		return reply(w, hs, err)
	})
	h("GET /v1/hosts/{name}", func(w http.ResponseWriter, r *http.Request) error {
		st, err := c.HostStatus(r.Context(), r.PathValue("name"))
		return reply(w, st, err)
	})

	h("PUT /v1/apps/{app}", func(w http.ResponseWriter, r *http.Request) error {
		body, err := readBody(r, maxManifestBody)
		if err != nil {
			return err
		}
		a, err := c.ApplyApp(r.Context(), r.PathValue("app"), body, actor(r))
		return reply(w, a, err)
	})
	h("GET /v1/apps", func(w http.ResponseWriter, r *http.Request) error {
		as, err := c.Apps(r.Context())
		return reply(w, as, err)
	})
	h("GET /v1/apps/{app}", func(w http.ResponseWriter, r *http.Request) error {
		a, err := c.App(r.Context(), r.PathValue("app"))
		return reply(w, a, err)
	})

	h("POST /v1/apps/{app}/releases", func(w http.ResponseWriter, r *http.Request) error {
		var req client.ReleaseRequest
		if err := decode(r, &req); err != nil {
			return err
		}
		rel, err := c.CreateRelease(r.Context(), r.PathValue("app"), req, actor(r))
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusCreated, rel)
	})
	h("GET /v1/apps/{app}/releases", func(w http.ResponseWriter, r *http.Request) error {
		app := r.PathValue("app")
		if _, err := c.store.GetApp(r.Context(), app); err != nil {
			return notFound(err, "app %s", app)
		}
		limit, err := intParam(r, "limit", 50, 1000)
		if err != nil {
			return err
		}
		rs, err := c.store.ListReleases(r.Context(), app, limit)
		return reply(w, rs, err)
	})

	// Deploys and rollbacks outlive the HTTP request: if the CLI disconnects
	// (Ctrl-C, flaky VPN) the node is still mid-deploy and the outcome must
	// still be recorded, so they run on a context detached from the request.
	h("POST /v1/apps/{app}/envs/{env}/deploy", func(w http.ResponseWriter, r *http.Request) error {
		var req client.DeployRequest
		if err := decode(r, &req); err != nil {
			return err
		}
		d, err := c.Deploy(context.WithoutCancel(r.Context()), r.PathValue("app"), r.PathValue("env"), req, actor(r))
		return reply(w, d, err)
	})
	h("POST /v1/apps/{app}/envs/{env}/rollback", func(w http.ResponseWriter, r *http.Request) error {
		var req client.RollbackRequest
		if err := decode(r, &req); err != nil {
			return err
		}
		d, err := c.Rollback(context.WithoutCancel(r.Context()), r.PathValue("app"), r.PathValue("env"), req, actor(r))
		return reply(w, d, err)
	})
	h("POST /v1/apps/{app}/envs/{env}/restart", func(w http.ResponseWriter, r *http.Request) error {
		var req client.RestartRequest
		if err := decode(r, &req); err != nil {
			return err
		}
		if err := c.Restart(r.Context(), r.PathValue("app"), r.PathValue("env"), req.Service, actor(r)); err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	h("GET /v1/apps/{app}/envs/{env}/status", func(w http.ResponseWriter, r *http.Request) error {
		st, err := c.Status(r.Context(), r.PathValue("app"), r.PathValue("env"))
		return reply(w, st, err)
	})
	h("GET /v1/apps/{app}/envs/{env}/deployments", func(w http.ResponseWriter, r *http.Request) error {
		app, env := r.PathValue("app"), r.PathValue("env")
		if _, err := c.store.GetEnvironment(r.Context(), app, env); err != nil {
			return notFound(err, "environment %s/%s", app, env)
		}
		limit, err := intParam(r, "limit", 50, 1000)
		if err != nil {
			return err
		}
		ds, err := c.store.ListDeployments(r.Context(), app, env, limit)
		return reply(w, ds, err)
	})
	h("GET /v1/apps/{app}/envs/{env}/logs", func(w http.ResponseWriter, r *http.Request) error {
		tail, err := intParam(r, "tail", 0, 100000)
		if err != nil {
			return err
		}
		logs, err := c.Logs(r.Context(), r.PathValue("app"), r.PathValue("env"), r.URL.Query().Get("service"), tail)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, logs)
		return nil
	})

	h("PUT /v1/apps/{app}/envs/{env}/secrets/{key}", func(w http.ResponseWriter, r *http.Request) error {
		body, err := readBody(r, maxSecretBody)
		if err != nil {
			return err
		}
		m, err := c.SetSecret(r.Context(), r.PathValue("app"), r.PathValue("env"), r.PathValue("key"), body, actor(r))
		return reply(w, m, err)
	})
	h("GET /v1/apps/{app}/envs/{env}/secrets", func(w http.ResponseWriter, r *http.Request) error {
		ms, err := c.ListSecrets(r.Context(), r.PathValue("app"), r.PathValue("env"))
		return reply(w, ms, err)
	})
	h("DELETE /v1/apps/{app}/envs/{env}/secrets/{key}", func(w http.ResponseWriter, r *http.Request) error {
		if err := c.DeleteSecret(r.Context(), r.PathValue("app"), r.PathValue("env"), r.PathValue("key"), actor(r)); err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	h("GET /v1/apps/{app}/envs/{env}/resources", func(w http.ResponseWriter, r *http.Request) error {
		rs, err := c.Resources(r.Context(), r.PathValue("app"), r.PathValue("env"))
		return reply(w, rs, err)
	})
	// Backups and restores, like deploys, finish and are recorded even if
	// the caller disconnects.
	h("POST /v1/apps/{app}/envs/{env}/db/backup", func(w http.ResponseWriter, r *http.Request) error {
		b, err := c.BackupDB(context.WithoutCancel(r.Context()), r.PathValue("app"), r.PathValue("env"), store.BackupManual, actor(r))
		return reply(w, b, err)
	})
	h("GET /v1/apps/{app}/envs/{env}/db/backups", func(w http.ResponseWriter, r *http.Request) error {
		app, env := r.PathValue("app"), r.PathValue("env")
		if _, err := c.database(r.Context(), app, env); err != nil {
			return err
		}
		limit, err := intParam(r, "limit", 50, 1000)
		if err != nil {
			return err
		}
		bs, err := c.store.ListBackups(r.Context(), app, env, limit)
		return reply(w, bs, err)
	})
	h("POST /v1/apps/{app}/envs/{env}/db/restore", func(w http.ResponseWriter, r *http.Request) error {
		var req client.RestoreRequest
		if err := decode(r, &req); err != nil {
			return err
		}
		if req.BackupID <= 0 {
			return errf(client.CodeInvalid, "backup_id is required")
		}
		res, err := c.RestoreDB(context.WithoutCancel(r.Context()), r.PathValue("app"), r.PathValue("env"), req.BackupID, actor(r))
		return reply(w, res, err)
	})
	h("GET /v1/backups", func(w http.ResponseWriter, r *http.Request) error {
		st, err := c.BackupStatus(r.Context())
		return reply(w, st, err)
	})

	h("GET /v1/events", func(w http.ResponseWriter, r *http.Request) error {
		limit, err := intParam(r, "limit", 50, 1000)
		if err != nil {
			return err
		}
		q := r.URL.Query()
		es, err := c.store.ListEvents(r.Context(), q.Get("app"), q.Get("env"), limit)
		return reply(w, es, err)
	})

	h("/", func(w http.ResponseWriter, r *http.Request) error {
		return errf(client.CodeNotFound, "no route for %s %s", r.Method, r.URL.Path)
	})

	return requireToken(token, readTokens, mux)
}

// requireToken checks the bearer token in constant time. Both sides are
// hashed first so the comparison does not leak the token's length either.
// A read-only token is accepted for GET/HEAD only; anything else gets 403.
func requireToken(token string, readTokens []string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	var readOnly [][32]byte
	for _, t := range readTokens {
		if t = strings.TrimSpace(t); t != "" && t != token {
			readOnly = append(readOnly, sha256.Sum256([]byte(t)))
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(got))
		if ok && token != "" && subtle.ConstantTimeCompare(sum[:], want[:]) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		for _, ro := range readOnly {
			if ok && subtle.ConstantTimeCompare(sum[:], ro[:]) == 1 {
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					writeErrorBody(w, http.StatusForbidden, client.CodeUnauthorized, "read-only token cannot "+r.Method)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="lwd"`)
		writeErrorBody(w, http.StatusUnauthorized, client.CodeUnauthorized, "missing or invalid bearer token")
	})
}

// actor identifies the caller for the audit trail. With a single admin token
// this is self-reported (the CLI sends user@host); it is a label, not auth.
func actor(r *http.Request) string {
	a := strings.TrimSpace(r.Header.Get("X-LWD-Actor"))
	a = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, a)
	if len(a) > 100 {
		a = a[:100]
	}
	if a == "" {
		return "api"
	}
	return a
}

func (c *Controller) postHost(w http.ResponseWriter, r *http.Request) error {
	var req client.HostRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	h, err := c.AddHost(r.Context(), req, actor(r))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, h)
}

func decode(r *http.Request, v any) error {
	body, err := readBody(r, maxJSONBody)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil // all request bodies have only optional fields or are validated later
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errf(client.CodeInvalid, "request body: %v", err)
	}
	return nil
}

func readBody(r *http.Request, limit int64) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, limit))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, errf(client.CodeInvalid, "request body larger than %d bytes", limit)
		}
		return nil, errf(client.CodeInvalid, "read body: %v", err)
	}
	return body, nil
}

func intParam(r *http.Request, name string, def, max int) (int, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, errf(client.CodeInvalid, "%s must be a non-negative integer", name)
	}
	return min(n, max), nil
}

func reply(w http.ResponseWriter, v any, err error) error {
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

func (c *Controller) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *Error
	switch {
	case errors.As(err, &ae):
	case errors.Is(err, store.ErrNotFound):
		ae = errf(client.CodeNotFound, "not found")
	case errors.Is(err, store.ErrInvalid):
		ae = errf(client.CodeInvalid, "%v", err)
	default:
		// Internal details go to the log, not the client.
		c.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		ae = errf(client.CodeInternal, "internal error")
	}
	writeErrorBody(w, httpStatus(ae.Code), ae.Code, ae.Message)
}

func writeErrorBody(w http.ResponseWriter, status int, code, msg string) {
	var body client.ErrorBody
	body.Error.Code, body.Error.Message = code, msg
	_ = writeJSON(w, status, body)
}
