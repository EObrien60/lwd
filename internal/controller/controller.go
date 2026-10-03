// Package controller implements `lwd controller`: the /v1 API over the
// Postgres store, release creation (digest pinning), and deploy/rollback
// orchestration. A deploy builds a self-contained bundle.Bundle and hands it
// to the target node, which does all Docker and Caddy work; the controller
// only records what happened. See docs/lwd2/DESIGN.md "Controller".
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"lwd/internal/bundle"
	"lwd/internal/client"
	"lwd/internal/manifest"
	"lwd/internal/nodeclient"
	"lwd/internal/registry"
	"lwd/internal/secrets"
	"lwd/internal/store"
)

// Budgets handed to the node in every bundle.
const (
	ReadyTimeoutSeconds = 60
	SmokeSeconds        = 10
)

var (
	nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	keyRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Error is an API error: an HTTP status plus a stable code.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// httpStatus maps an error code to its HTTP status.
func httpStatus(code string) int {
	switch code {
	case client.CodeConflict:
		return http.StatusConflict
	case client.CodeNotFound:
		return http.StatusNotFound
	case client.CodeInvalid:
		return http.StatusBadRequest
	case client.CodeNodeUnreachable:
		return http.StatusBadGateway
	case client.CodeUnauthorized:
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}

// notFound turns store.ErrNotFound into a not_found API error naming what.
func notFound(err error, what string, args ...any) error {
	if errors.Is(err, store.ErrNotFound) {
		return errf(client.CodeNotFound, what+" not found", args...)
	}
	return err
}

// Controller holds the controller's dependencies.
type Controller struct {
	store    *store.Store
	cipher   *secrets.Cipher
	resolver registry.Resolver
	log      *slog.Logger
}

// New returns a controller.
func New(st *store.Store, c *secrets.Cipher, r registry.Resolver, log *slog.Logger) *Controller {
	return &Controller{store: st, cipher: c, resolver: r, log: log}
}

// event appends to the audit log. Failures are logged, not returned: the
// action it describes has already happened.
func (c *Controller) event(ctx context.Context, e store.Event) {
	if err := c.store.AddEvent(ctx, e); err != nil {
		c.log.Error("record event", "kind", e.Kind, "err", err)
	}
}

func jsonData(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// --- hosts ---

// AddHost registers a node, encrypting its token.
func (c *Controller) AddHost(ctx context.Context, r client.HostRequest, actor string) (store.Host, error) {
	switch {
	case !nameRE.MatchString(r.Name):
		return store.Host{}, errf(client.CodeInvalid, "host name %q must match %s", r.Name, nameRE)
	case strings.TrimSpace(r.Addr) == "":
		return store.Host{}, errf(client.CodeInvalid, "addr is required")
	case strings.TrimSpace(r.Token) == "":
		return store.Host{}, errf(client.CodeInvalid, "token is required")
	}
	enc, err := c.cipher.Encrypt([]byte(strings.TrimSpace(r.Token)))
	if err != nil {
		return store.Host{}, err
	}
	if err := c.store.PutHost(ctx, store.Host{Name: r.Name, Addr: strings.TrimSpace(r.Addr), TokenEnc: enc}); err != nil {
		return store.Host{}, err
	}
	c.event(ctx, store.Event{Kind: "host.added", Message: fmt.Sprintf("host %s at %s added by %s", r.Name, r.Addr, actor),
		Data: jsonData(map[string]string{"host": r.Name, "addr": r.Addr, "actor": actor})})
	return c.store.GetHost(ctx, r.Name)
}

// node returns a client for the named host.
func (c *Controller) node(ctx context.Context, host string) (*nodeclient.Client, store.Host, error) {
	h, err := c.store.GetHost(ctx, host)
	if err != nil {
		return nil, h, notFound(err, "host %s", host)
	}
	tok, err := c.cipher.Decrypt(h.TokenEnc)
	if err != nil {
		return nil, h, fmt.Errorf("host %s token: %w", host, err)
	}
	return nodeclient.New(h.Addr, string(tok)), h, nil
}

// HostStatus returns a host and its node's live status.
func (c *Controller) HostStatus(ctx context.Context, name string) (client.HostStatus, error) {
	nc, h, err := c.node(ctx, name)
	if err != nil {
		return client.HostStatus{}, err
	}
	out := client.HostStatus{Host: h}
	st, err := nc.Status(ctx)
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	out.Reachable, out.Node = true, st
	return out, nil
}

// nodeErr converts a node call failure into an API error.
func nodeErr(host string, err error) error {
	if errors.Is(err, nodeclient.ErrUnreachable) {
		return errf(client.CodeNodeUnreachable, "host %s: %v", host, err)
	}
	var he *nodeclient.HTTPError
	if errors.As(err, &he) {
		switch he.Status {
		case http.StatusConflict:
			return errf(client.CodeConflict, "host %s: %s", host, he.Message)
		case http.StatusNotFound:
			return errf(client.CodeNotFound, "host %s: %s", host, he.Message)
		case http.StatusBadRequest:
			return errf(client.CodeInvalid, "host %s: %s", host, he.Message)
		}
		return errf(client.CodeInternal, "host %s: %s", host, he.Message)
	}
	return err
}

// --- apps ---

// ApplyApp validates lwd.toml and stores it, syncing environments.
func (c *Controller) ApplyApp(ctx context.Context, name string, text []byte, actor string) (client.AppDetail, error) {
	m, err := manifest.Parse(text)
	if err != nil {
		return client.AppDetail{}, errf(client.CodeInvalid, "%v", err)
	}
	if m.Name != name {
		return client.AppDetail{}, errf(client.CodeInvalid, "manifest name %q does not match app %q", m.Name, name)
	}
	var envs []store.Environment
	for _, en := range m.EnvironmentNames() {
		e := m.Environments[en]
		if _, err := c.store.GetHost(ctx, e.Host); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return client.AppDetail{}, errf(client.CodeInvalid, "env.%s.host: unknown host %q (register it with: lwd host add)", en, e.Host)
			}
			return client.AppDetail{}, err
		}
		envs = append(envs, store.Environment{App: name, Name: en, Host: e.Host, Domain: e.Domain, TLS: e.TLS})
	}
	kept, err := c.store.PutApp(ctx, store.App{Name: name, Manifest: string(text)}, envs)
	if err != nil {
		return client.AppDetail{}, err
	}
	out, err := c.App(ctx, name)
	if err != nil {
		return out, err
	}
	out.Manifest = ""
	for _, k := range kept {
		out.Warnings = append(out.Warnings, fmt.Sprintf("environment %s is no longer in the manifest but has deployment history; kept (not deployable until re-declared)", k))
	}
	c.event(ctx, store.Event{App: name, Kind: "app.applied", Message: fmt.Sprintf("manifest applied by %s", actor),
		Data: jsonData(map[string]any{"environments": m.EnvironmentNames(), "kept": kept, "actor": actor})})
	return out, nil
}

// App returns an app with manifest and environments.
func (c *Controller) App(ctx context.Context, name string) (client.AppDetail, error) {
	a, err := c.store.GetApp(ctx, name)
	if err != nil {
		return client.AppDetail{}, notFound(err, "app %s", name)
	}
	envs, err := c.store.ListEnvironments(ctx, name)
	if err != nil {
		return client.AppDetail{}, err
	}
	return client.AppDetail{App: a, Environments: envs}, nil
}

// Apps lists apps with their environments.
func (c *Controller) Apps(ctx context.Context) ([]client.AppDetail, error) {
	apps, err := c.store.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]client.AppDetail, 0, len(apps))
	for _, a := range apps {
		envs, err := c.store.ListEnvironments(ctx, a.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, client.AppDetail{App: a, Environments: envs})
	}
	return out, nil
}

// --- releases ---

// CreateRelease pins every service image to a digest and records the
// release with a snapshot of the current manifest.
func (c *Controller) CreateRelease(ctx context.Context, app string, r client.ReleaseRequest, actor string) (store.Release, error) {
	a, err := c.store.GetApp(ctx, app)
	if err != nil {
		return store.Release{}, notFound(err, "app %s", app)
	}
	m, err := manifest.Parse([]byte(a.Manifest))
	if err != nil {
		return store.Release{}, fmt.Errorf("stored manifest of %s: %w", app, err)
	}
	for svc := range r.Images {
		if _, ok := m.Services[svc]; !ok {
			return store.Release{}, errf(client.CodeInvalid, "images: %q is not a service of %s", svc, app)
		}
	}
	refs := map[string]string{} // service -> unresolved ref
	for _, svc := range slices.Sorted(maps.Keys(m.Services)) {
		if ref, ok := r.Images[svc]; ok {
			refs[svc] = ref
			continue
		}
		if r.Tag == "" {
			return store.Release{}, errf(client.CodeInvalid, "tag is required (service %s has no explicit image)", svc)
		}
		refs[svc] = m.Services[svc].Image + ":" + r.Tag
	}
	// Services often share an image (api + worker); resolve each ref once so
	// they are guaranteed the same digest.
	pinned := map[string]string{}
	for _, ref := range refs {
		if _, done := pinned[ref]; done {
			continue
		}
		d, err := c.resolver.Resolve(ctx, ref)
		if err != nil {
			return store.Release{}, errf(client.CodeInvalid, "%v", err)
		}
		pinned[ref] = d
	}
	rel := store.Release{App: app, Commit: r.Commit, Tag: r.Tag, Manifest: a.Manifest, Images: map[string]string{}, Actor: actor}
	for svc, ref := range refs {
		rel.Images[svc] = pinned[ref]
	}
	if err := c.store.CreateRelease(ctx, &rel); err != nil {
		return store.Release{}, err
	}
	c.event(ctx, store.Event{App: app, Kind: "release.created",
		Message: fmt.Sprintf("release %d (tag %s, commit %s) created by %s", rel.ID, r.Tag, r.Commit, actor),
		Data:    jsonData(map[string]any{"release": rel.ID, "images": rel.Images, "actor": actor})})
	rel.Manifest = ""
	return rel, nil
}

// --- deploy / rollback ---

// Deploy deploys release (0 = newest) to app/env and waits for the result.
func (c *Controller) Deploy(ctx context.Context, app, env string, req client.DeployRequest, actor string) (store.Deployment, error) {
	return c.locked(ctx, app, env, func(ctx context.Context, e store.Environment) (store.Deployment, error) {
		var rel store.Release
		var err error
		if req.Release == 0 {
			rel, err = c.store.LatestRelease(ctx, app)
			if errors.Is(err, store.ErrNotFound) {
				err = errf(client.CodeNotFound, "%s has no releases (create one with: lwd release create %s --tag T)", app, app)
			}
		} else {
			rel, err = c.store.GetRelease(ctx, app, req.Release)
			err = notFound(err, "release %d of %s", req.Release, app)
		}
		if err != nil {
			return store.Deployment{}, err
		}
		return c.execute(ctx, e, rel, store.KindDeploy, actor, req.Reason)
	})
}

// Rollback redeploys an earlier release. By default the target is the
// release of the newest succeeded deployment that differs from what is live.
func (c *Controller) Rollback(ctx context.Context, app, env string, req client.RollbackRequest, actor string) (store.Deployment, error) {
	return c.locked(ctx, app, env, func(ctx context.Context, e store.Environment) (store.Deployment, error) {
		var relID int64
		if req.To != 0 {
			relID = req.To
		} else {
			live, err := c.store.LiveDeployment(ctx, app, env)
			if errors.Is(err, store.ErrNotFound) {
				return store.Deployment{}, errf(client.CodeInvalid, "nothing is live in %s/%s; nothing to roll back", app, env)
			} else if err != nil {
				return store.Deployment{}, err
			}
			target, err := c.store.RollbackTarget(ctx, app, env, live.Release)
			if errors.Is(err, store.ErrNotFound) {
				return store.Deployment{}, errf(client.CodeInvalid, "%s/%s has no earlier successfully deployed release than %d; use --to", app, env, live.Release)
			} else if err != nil {
				return store.Deployment{}, err
			}
			relID = target.Release
		}
		rel, err := c.store.GetRelease(ctx, app, relID)
		if err != nil {
			return store.Deployment{}, notFound(err, "release %d of %s", relID, app)
		}
		return c.execute(ctx, e, rel, store.KindRollback, actor, req.Reason)
	})
}

// locked runs fn under the app-env deploy lock. Decisions that depend on
// history (rollback target) are made inside it, so they cannot race.
func (c *Controller) locked(ctx context.Context, app, env string, fn func(context.Context, store.Environment) (store.Deployment, error)) (store.Deployment, error) {
	e, err := c.store.GetEnvironment(ctx, app, env)
	if err != nil {
		return store.Deployment{}, notFound(err, "environment %s/%s", app, env)
	}
	var d store.Deployment
	err = c.store.WithLock(ctx, app, env, func(ctx context.Context) error {
		var err error
		d, err = fn(ctx, e)
		return err
	})
	if errors.Is(err, store.ErrBusy) {
		return d, errf(client.CodeConflict, "a deployment of %s/%s is already in progress", app, env)
	}
	return d, err
}

// execute builds the bundle, records the deployment and runs it on the node.
func (c *Controller) execute(ctx context.Context, e store.Environment, rel store.Release, kind, actor, reason string) (store.Deployment, error) {
	b, err := c.buildBundle(ctx, e, rel)
	if err != nil {
		return store.Deployment{}, err
	}
	nc, _, err := c.node(ctx, e.Host)
	if err != nil {
		return store.Deployment{}, err
	}
	d := store.Deployment{App: e.App, Env: e.Name, Release: rel.ID, Host: e.Host, Kind: kind, Actor: actor, Reason: reason}
	if err := c.store.CreateDeployment(ctx, &d); err != nil {
		return d, err
	}
	b.Deployment = d.ID
	c.event(ctx, store.Event{App: e.App, Env: e.Name, Deployment: &d.ID, Kind: "deploy.started",
		Message: fmt.Sprintf("%s of release %d to %s/%s on %s started by %s", kind, rel.ID, e.App, e.Name, e.Host, actor),
		Data:    jsonData(map[string]any{"kind": kind, "release": rel.ID, "host": e.Host, "actor": actor, "reason": reason})})
	c.log.Info("deploy started", "deployment", d.ID, "app", e.App, "env", e.Name, "release", rel.ID, "kind", kind)

	res, err := nc.Deploy(ctx, b)
	if err != nil {
		apiErr := nodeErr(e.Host, err)
		msg := apiErr.Error()
		var ae *Error
		if errors.As(apiErr, &ae) {
			msg = ae.Message
			if ae.Code == client.CodeNodeUnreachable {
				msg = "node unreachable: " + msg
			}
		}
		c.finish(ctx, d, store.StatusFailed, "submit", msg, nil, kind)
		if ae != nil {
			ae.Message = fmt.Sprintf("deployment %d failed: %s", d.ID, ae.Message)
			return d, ae
		}
		return d, apiErr
	}
	status := res.Status
	msg := res.Message
	if status != bundle.StatusSucceeded && status != bundle.StatusFailed && status != bundle.StatusReverted {
		msg = fmt.Sprintf("node reported unknown status %q: %s", status, msg)
		status = store.StatusFailed
	}
	c.finish(ctx, d, status, res.Phase, msg, jsonData(res), kind)
	return c.store.GetDeployment(ctx, d.ID)
}

// finish records a deployment outcome and its event.
func (c *Controller) finish(ctx context.Context, d store.Deployment, status, phase, msg string, result json.RawMessage, kind string) {
	if err := c.store.FinishDeployment(ctx, d.ID, status, phase, msg, result); err != nil {
		c.log.Error("record deployment result", "deployment", d.ID, "err", err)
	}
	text := fmt.Sprintf("%s %d of release %d %s", kind, d.ID, d.Release, status)
	if msg != "" {
		text += ": " + msg
	}
	c.event(ctx, store.Event{App: d.App, Env: d.Env, Deployment: &d.ID, Kind: "deploy." + status, Message: text,
		Data: jsonData(map[string]any{"kind": kind, "release": d.Release, "phase": phase})})
	c.log.Info("deploy finished", "deployment", d.ID, "status", status, "phase", phase, "message", msg)
}

// buildBundle assembles the node bundle. Services, variables and the
// migration come from the release's manifest snapshot (releases are
// immutable); host, domain and TLS come from the environment's current row,
// so a rollback lands where the environment lives now.
func (c *Controller) buildBundle(ctx context.Context, e store.Environment, rel store.Release) (*bundle.Bundle, error) {
	m, err := manifest.Parse([]byte(rel.Manifest))
	if err != nil {
		return nil, fmt.Errorf("release %d manifest: %w", rel.ID, err)
	}
	pe := m.Environments[e.Name] // zero value if the release predates this env
	pe.Host, pe.Domain, pe.TLS = e.Host, e.Domain, e.TLS
	m.Environments[e.Name] = pe
	r, err := m.Resolve(e.Name)
	if err != nil {
		return nil, err
	}
	for i, s := range r.Services {
		img, ok := rel.Images[s.Name]
		if !ok {
			return nil, errf(client.CodeInvalid, "release %d has no image for service %s", rel.ID, s.Name)
		}
		r.Services[i].Image = img
	}

	enc, err := c.store.SecretValues(ctx, e.App, e.Name)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, k := range m.Secrets {
		if _, ok := enc[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, errf(client.CodeInvalid, "missing secrets for %s/%s: %s (set with: lwd secret set %s %s KEY)",
			e.App, e.Name, strings.Join(missing, ", "), e.App, e.Name)
	}
	for k, v := range enc {
		pt, err := c.cipher.Decrypt(v)
		if err != nil {
			return nil, fmt.Errorf("decrypt secret %s: %w", k, err)
		}
		r.Vars[k] = string(pt)
	}

	b := &bundle.Bundle{
		App: e.App, Env: e.Name, Release: rel.ID,
		Services: r.Services, Migrate: r.Migrate, Vars: r.Vars, TLS: r.TLS,
		ReadyTimeoutSeconds: ReadyTimeoutSeconds, SmokeSeconds: SmokeSeconds,
	}
	// Validate before a deployment row exists; the id is filled in later.
	b.Deployment = 1
	if err := b.Validate(); err != nil {
		return nil, errf(client.CodeInvalid, "bundle: %v", err)
	}
	b.Deployment = 0
	return b, nil
}

// --- node proxies ---

// Restart restarts live containers of app/env on its node.
func (c *Controller) Restart(ctx context.Context, app, env, service, actor string) error {
	e, err := c.store.GetEnvironment(ctx, app, env)
	if err != nil {
		return notFound(err, "environment %s/%s", app, env)
	}
	nc, _, err := c.node(ctx, e.Host)
	if err != nil {
		return err
	}
	if err := nc.Restart(ctx, app, env, service); err != nil {
		return nodeErr(e.Host, err)
	}
	what := "all services"
	if service != "" {
		what = "service " + service
	}
	c.event(ctx, store.Event{App: app, Env: env, Kind: "restart", Message: fmt.Sprintf("%s restarted by %s", what, actor),
		Data: jsonData(map[string]string{"service": service, "actor": actor})})
	return nil
}

// Logs fetches recent logs of app/env from its node.
func (c *Controller) Logs(ctx context.Context, app, env, service string, tail int) (string, error) {
	e, err := c.store.GetEnvironment(ctx, app, env)
	if err != nil {
		return "", notFound(err, "environment %s/%s", app, env)
	}
	nc, _, err := c.node(ctx, e.Host)
	if err != nil {
		return "", err
	}
	logs, err := nc.Logs(ctx, app, env, service, tail)
	if err != nil {
		return "", nodeErr(e.Host, err)
	}
	return logs, nil
}

// Status merges the DB view of app/env with the node's live view of it.
func (c *Controller) Status(ctx context.Context, app, env string) (client.EnvStatus, error) {
	e, err := c.store.GetEnvironment(ctx, app, env)
	if err != nil {
		return client.EnvStatus{}, notFound(err, "environment %s/%s", app, env)
	}
	out := client.EnvStatus{App: app, Env: env, Host: e.Host, Domain: e.Domain, TLS: e.TLS}
	if d, err := c.store.LiveDeployment(ctx, app, env); err == nil {
		out.Live = &d
	} else if !errors.Is(err, store.ErrNotFound) {
		return out, err
	}
	if d, err := c.store.LastDeployment(ctx, app, env); err == nil {
		out.Last = &d
	} else if !errors.Is(err, store.ErrNotFound) {
		return out, err
	}
	nc, _, err := c.node(ctx, e.Host)
	if err != nil {
		out.NodeError = err.Error()
		return out, nil
	}
	raw, err := nc.Status(ctx)
	if err != nil {
		out.NodeError = err.Error()
		return out, nil
	}
	out.Node = extractAppEnv(raw, app, env)
	return out, nil
}

// extractAppEnv picks app/env's entry out of a node /v1/status document.
// DESIGN.md fixes the content ("every app-env's live deployment + container
// states") but not the shape, so accept the plausible ones: "apps" as a list
// of objects carrying app and env fields, or as a map keyed "<app>-<env>" or
// "<app>/<env>". Returns nil if not found.
func extractAppEnv(raw json.RawMessage, app, env string) json.RawMessage {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	apps := doc["apps"]
	var list []json.RawMessage
	if json.Unmarshal(apps, &list) == nil {
		for _, item := range list {
			var id struct {
				App string `json:"app"`
				Env string `json:"env"`
			}
			if json.Unmarshal(item, &id) == nil && id.App == app && id.Env == env {
				return item
			}
		}
		return nil
	}
	var byKey map[string]json.RawMessage
	if json.Unmarshal(apps, &byKey) == nil {
		for _, k := range []string{app + "-" + env, app + "/" + env} {
			if v, ok := byKey[k]; ok {
				return v
			}
		}
	}
	return nil
}

// --- secrets ---

// SetSecret encrypts and stores a new version of key for app/env.
func (c *Controller) SetSecret(ctx context.Context, app, env, key string, value []byte, actor string) (store.SecretMeta, error) {
	if !keyRE.MatchString(key) {
		return store.SecretMeta{}, errf(client.CodeInvalid, "secret key %q must match %s", key, keyRE)
	}
	if _, err := c.store.GetEnvironment(ctx, app, env); err != nil {
		return store.SecretMeta{}, notFound(err, "environment %s/%s", app, env)
	}
	enc, err := c.cipher.Encrypt(value)
	if err != nil {
		return store.SecretMeta{}, err
	}
	v, err := c.store.SetSecret(ctx, app, env, key, enc)
	if err != nil {
		return store.SecretMeta{}, err
	}
	c.event(ctx, store.Event{App: app, Env: env, Kind: "secret.set", Message: fmt.Sprintf("secret %s set to version %d by %s", key, v, actor),
		Data: jsonData(map[string]any{"key": key, "version": v, "actor": actor})})
	return store.SecretMeta{Key: key, Version: v}, nil
}

// ListSecrets lists names and versions only.
func (c *Controller) ListSecrets(ctx context.Context, app, env string) ([]store.SecretMeta, error) {
	if _, err := c.store.GetEnvironment(ctx, app, env); err != nil {
		return nil, notFound(err, "environment %s/%s", app, env)
	}
	return c.store.ListSecrets(ctx, app, env)
}

// DeleteSecret removes key from app/env (future deploys only).
func (c *Controller) DeleteSecret(ctx context.Context, app, env, key, actor string) error {
	if err := c.store.DeleteSecret(ctx, app, env, key); err != nil {
		return notFound(err, "secret %s in %s/%s", key, app, env)
	}
	c.event(ctx, store.Event{App: app, Env: env, Kind: "secret.deleted", Message: fmt.Sprintf("secret %s deleted by %s", key, actor),
		Data: jsonData(map[string]string{"key": key, "actor": actor})})
	return nil
}
