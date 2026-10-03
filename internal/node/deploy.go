package node

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"lwd/internal/bundle"
	"lwd/internal/router"
)

const (
	defaultReadySeconds = 60
	defaultSmokeSeconds = 10
	keepDeployments     = 5
	maxLogBytes         = 64 << 10
	logTail             = "300"
)

// errBusy reports that the app-env already has an operation in progress.
var errBusy = errors.New("another operation is in progress for this app-env")

// invalidError is a bundle or request the node refuses to execute.
type invalidError struct{ msg string }

func (e *invalidError) Error() string { return e.msg }

func invalidf(format string, a ...any) error { return &invalidError{fmt.Sprintf(format, a...)} }

// readyPathRE accepts an absolute URL path (with optional query) made of URL
// characters only, so it can be appended to a probe URL verbatim.
var readyPathRE = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/?-]*$`)

func appKey(b *bundle.Bundle) string { return b.App + "-" + b.Env }

func readyPath(s bundle.Service) string {
	if s.Ready == "" {
		return "/"
	}
	return s.Ready
}

// validateBundle applies bundle.Validate plus the checks that keep bundle
// values from reaching the Caddyfile, probe URLs or compose files unsafely.
func (n *Node) validateBundle(b *bundle.Bundle) error {
	if err := b.Validate(); err != nil {
		return &invalidError{err.Error()}
	}
	key := appKey(b)
	for _, s := range b.Services {
		if s.Name == migrateService {
			return invalidf("service name %q is reserved", migrateService)
		}
		if s.Port > 0 && !readyPathRE.MatchString(readyPath(s)) {
			return invalidf("service %s: ready path %q must be an absolute URL path", s.Name, s.Ready)
		}
		for _, d := range s.Domains {
			if !router.ValidDomain(d) {
				return invalidf("service %s: invalid domain %q (lower-case DNS name expected)", s.Name, d)
			}
			if owner := n.domainOwner(d); owner != "" && owner != key {
				return invalidf("domain %s is routed by %s on this node", d, owner)
			}
		}
	}
	if _, err := renderEnvironment(b); err != nil {
		return &invalidError{err.Error()}
	}
	return nil
}

// Deploy runs the deploy algorithm for one bundle. It returns errBusy or an
// *invalidError when the bundle is not executed at all; any outcome of an
// executed attempt, including failure, is reported in the Result.
func (n *Node) Deploy(ctx context.Context, b *bundle.Bundle) (*bundle.Result, error) {
	if err := n.validateBundle(b); err != nil {
		return nil, err
	}
	key := appKey(b)
	if !n.tryLock(key) {
		return nil, errBusy
	}
	defer n.unlock(key)

	if err := os.MkdirAll(n.appDir(key), 0o700); err != nil {
		return nil, err
	}
	st, err := loadState(n.statePath(key))
	if err != nil {
		return nil, err
	}
	if st.Live != nil && st.Live.Deployment == b.Deployment {
		return nil, invalidf("deployment %d is already live", b.Deployment)
	}
	st.App, st.Env = b.App, b.Env

	d := &deployRun{
		n: n, b: b, key: key, st: st, prev: st.Live,
		dir: filepath.Join(n.appDir(key), "d"+strconv.FormatInt(b.Deployment, 10)),
		res: &bundle.Result{Events: []bundle.Event{}},
	}
	d.run(ctx)

	st.record(Attempt{Deployment: b.Deployment, Release: b.Release, Status: d.res.Status, Phase: d.res.Phase, At: now()})
	if err := st.save(n.statePath(key)); err != nil {
		d.event(d.res.Phase, "saving state.json: "+err.Error())
	}
	if data, err := json.MarshalIndent(d.res, "", "  "); err == nil {
		os.WriteFile(filepath.Join(d.dir, "result.json"), data, 0o600)
	}
	n.prune(key, st.Live)
	return d.res, nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// deployRun is one in-flight deploy attempt.
type deployRun struct {
	n    *Node
	b    *bundle.Bundle
	key  string
	dir  string
	st   *State
	prev *Live // live deployment before this attempt, nil on first deploy
	cand *Live
	proj project
	res  *bundle.Result
}

func (d *deployRun) event(phase, msg string) {
	d.res.Events = append(d.res.Events, bundle.Event{At: now(), Phase: phase, Message: msg})
}

func (d *deployRun) compose(ctx context.Context, args ...string) ([]byte, error) {
	return d.n.docker.Run(ctx, d.proj.args(args...)...)
}

func (d *deployRun) run(ctx context.Context) {
	b := d.b
	d.event("prepare", fmt.Sprintf("deployment %d of release %d as %s", b.Deployment, b.Release, b.ProjectName()))
	ports, err := d.prepare()
	if err != nil {
		d.finish(bundle.StatusFailed, "prepare", err)
		return
	}
	defer d.n.ports.release(ports)

	d.event("pull", "pulling images")
	if _, err := d.compose(ctx, "pull"); err != nil {
		d.failBeforeLive(ctx, "pull", err, "")
		return
	}

	if b.Migrate != nil {
		d.event("migrate", fmt.Sprintf("running %s with %s's image", strings.Join(b.Migrate.Command, " "), b.Migrate.Service))
		out, err := d.n.docker.Run(ctx, d.proj.args("--profile", migrateProfile, "run", "--rm", "-T", migrateService)...)
		if err != nil {
			d.failBeforeLive(ctx, "migrate", err, string(out))
			return
		}
		d.event("migrate", "migration finished")
	}

	d.event("start", "starting candidate")
	if _, err := d.compose(ctx, "up", "-d"); err != nil {
		d.failBeforeLive(ctx, "start", err, "")
		return
	}

	d.event("ready", "waiting for services")
	if err := d.waitReady(ctx); err != nil {
		d.failBeforeLive(ctx, "ready", err, "")
		return
	}
	d.event("ready", "all services ready")

	d.event("cutover", "switching routes to candidate")
	if err := d.n.setRoutes(ctx, d.key, d.cand); err != nil {
		d.failBeforeLive(ctx, "cutover", err, "")
		return
	}

	d.event("smoke", "checking through Caddy")
	if err := d.smoke(ctx); err != nil {
		d.revert(ctx, err)
		return
	}

	// Persist the new live deployment before retiring the old one: a crash
	// in between then leaves state, routes and running containers agreeing.
	d.st.Live = d.cand
	if err := d.st.save(d.n.statePath(d.key)); err != nil {
		d.event("done", "saving state.json: "+err.Error())
	}
	if d.prev != nil {
		prev := deploymentProject(d.prev.Project, filepath.Join(d.n.appDir(d.key), "d"+strconv.FormatInt(d.prev.Deployment, 10)))
		if _, err := d.n.docker.Run(ctx, prev.args("down")...); err != nil {
			d.event("done", fmt.Sprintf("stopping previous deployment %d: %v", d.prev.Deployment, err))
		} else {
			d.event("done", fmt.Sprintf("stopped previous deployment %d", d.prev.Deployment))
		}
	}
	d.finish(bundle.StatusSucceeded, "done", nil)
}

// prepare writes d<N>/ and allocates host ports for the HTTP services.
func (d *deployRun) prepare() (map[string]int, error) {
	b := d.b
	var httpSvcs []string
	for _, s := range b.Services {
		if s.Port > 0 {
			httpSvcs = append(httpSvcs, s.Name)
		}
	}
	used, err := d.n.usedPorts()
	if err != nil {
		return nil, err
	}
	ports, err := d.n.ports.allocate(httpSvcs, used)
	if err != nil {
		return nil, err
	}
	release := true
	defer func() {
		if release {
			d.n.ports.release(ports)
		}
	}()

	compose, err := renderCompose(b, ports)
	if err != nil {
		return nil, err
	}
	env, err := renderEnvironment(b)
	if err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, err
	}
	// A redelivered deployment id starts from a clean directory.
	if err := os.RemoveAll(d.dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(d.dir, 0o700); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name string
		data []byte
		perm os.FileMode
	}{
		{"bundle.json", raw, 0o600}, // carries resolved secrets
		{environmentFile, env, 0o600},
		{composeFile, compose, 0o644},
	} {
		if err := router.WriteFileAtomic(filepath.Join(d.dir, f.name), f.data, f.perm); err != nil {
			return nil, err
		}
	}

	d.proj = deploymentProject(b.ProjectName(), d.dir)
	d.cand = &Live{
		Deployment: b.Deployment, Release: b.Release, Project: b.ProjectName(), TLS: b.TLS,
		Ports: map[string]int{}, Domains: map[string][]string{}, Ready: map[string]string{},
	}
	for _, s := range b.Services {
		if s.Port == 0 {
			continue
		}
		d.cand.Ports[s.Name] = ports[s.Name]
		d.cand.Ready[s.Name] = readyPath(s)
		if len(s.Domains) > 0 {
			d.cand.Domains[s.Name] = append([]string(nil), s.Domains...)
		}
	}
	d.event("prepare", fmt.Sprintf("wrote %s; host ports %v", d.dir, ports))
	release = false
	return ports, nil
}

// probeClient is for loopback probes: short timeout, redirects not followed
// (a 3xx already proves the app answers).
func probeClient() *http.Client {
	return &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DisableKeepAlives: true},
	}
}

// smokeClient sends every request to Caddy's https listener whatever the URL
// host, so the URL host becomes the SNI and Host header. Certificates are not
// verified: smoke checks routing, not trust.
func (n *Node) smokeClient() *http.Client {
	addr := n.httpsAddr
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, addr)
			},
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			DisableKeepAlives: true,
		},
	}
}

func probe(ctx context.Context, c *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return nil
}

// waitReady polls until every HTTP service answers 2xx/3xx on its loopback
// port and every worker container is running, or the budget runs out.
func (d *deployRun) waitReady(ctx context.Context) error {
	secs := d.b.ReadyTimeoutSeconds
	if secs <= 0 {
		secs = defaultReadySeconds
	}
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	client := probeClient()

	pending := map[string]string{} // service -> last failure
	workers := map[string]bool{}
	for _, s := range d.b.Services {
		pending[s.Name] = "not checked"
		if s.Port == 0 {
			workers[s.Name] = true
		}
	}
	for {
		for _, s := range d.b.Services {
			if _, ok := pending[s.Name]; !ok || s.Port == 0 {
				continue
			}
			url := d.n.loopbackURL(d.cand.Ports[s.Name]) + readyPath(s)
			if err := probe(ctx, client, url); err != nil {
				pending[s.Name] = err.Error()
			} else {
				delete(pending, s.Name)
				d.event("ready", s.Name+" ready")
			}
		}
		if len(workers) > 0 {
			d.checkWorkers(ctx, workers, pending)
		}
		if len(pending) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			names := make([]string, 0, len(pending))
			for name, why := range pending {
				names = append(names, name+" ("+why+")")
			}
			sort.Strings(names)
			return fmt.Errorf("not ready after %ds: %s", secs, strings.Join(names, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.n.readyInterval):
		}
	}
}

func (d *deployRun) checkWorkers(ctx context.Context, workers map[string]bool, pending map[string]string) {
	out, err := d.compose(ctx, "ps", "--all", "--format", "json")
	var cs []Container
	if err == nil {
		cs, err = parsePS(out)
	}
	if err != nil {
		for w := range workers {
			if _, ok := pending[w]; ok {
				pending[w] = err.Error()
			}
		}
		return
	}
	state := map[string]string{}
	for _, c := range cs {
		state[c.Service] = c.State
	}
	for w := range workers {
		if _, ok := pending[w]; !ok {
			continue
		}
		if state[w] == "running" {
			delete(pending, w)
			d.event("ready", w+" running")
		} else if state[w] == "" {
			pending[w] = "no container"
		} else {
			pending[w] = "container " + state[w]
		}
	}
}

// smoke probes every HTTP service once per interval for SmokeSeconds ticks:
// routed services through Caddy, unrouted ones on their loopback port.
func (d *deployRun) smoke(ctx context.Context) error {
	secs := d.b.SmokeSeconds
	if secs <= 0 {
		secs = defaultSmokeSeconds
	}
	viaCaddy, loopback := d.n.smokeClient(), probeClient()
	for i := 0; i < secs; i++ {
		for _, s := range d.b.Services {
			if s.Port == 0 {
				continue
			}
			var urls []string
			for _, dom := range s.Domains {
				urls = append(urls, "https://"+dom+readyPath(s))
			}
			c := viaCaddy
			if len(urls) == 0 {
				urls, c = []string{d.n.loopbackURL(d.cand.Ports[s.Name]) + readyPath(s)}, loopback
			}
			for _, u := range urls {
				if err := probe(ctx, c, u); err != nil {
					return fmt.Errorf("smoke %s: %w", s.Name, err)
				}
			}
		}
		if i < secs-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d.n.smokeInterval):
			}
		}
	}
	d.event("smoke", fmt.Sprintf("passed %d checks", secs))
	return nil
}

// failBeforeLive handles a failure before the candidate served traffic:
// capture logs, remove the candidate, leave the previous deployment alone.
func (d *deployRun) failBeforeLive(ctx context.Context, phase string, cause error, output string) {
	d.event(phase, cause.Error())
	d.retire(ctx, phase, output)
	d.finish(bundle.StatusFailed, phase, cause)
}

// revert handles a smoke failure: routes back to the previous deployment (or
// none on a first deploy), then the candidate is removed.
func (d *deployRun) revert(ctx context.Context, cause error) {
	d.event("smoke", cause.Error())
	if err := d.n.setRoutes(ctx, d.key, d.prev); err != nil {
		cause = fmt.Errorf("%w; restoring previous routes failed: %v", cause, err)
		d.event("smoke", "restoring previous routes failed: "+err.Error())
	} else {
		d.event("smoke", "previous routes restored")
	}
	d.retire(ctx, "smoke", "")
	d.finish(bundle.StatusReverted, "smoke", cause)
}

// retire captures the candidate's logs into failure.log and Result.Logs and
// downs the candidate project.
func (d *deployRun) retire(ctx context.Context, phase, output string) {
	logs, err := d.compose(ctx, "logs", "--no-color", "--tail", logTail)
	text := output
	if len(logs) > 0 {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += string(logs)
	}
	if err != nil {
		d.event(phase, "capturing logs: "+err.Error())
	}
	text = truncateLogs(text)
	d.res.Logs = text
	os.WriteFile(filepath.Join(d.dir, "failure.log"), []byte(text), 0o600)

	if _, err := d.compose(ctx, "down"); err != nil {
		d.event(phase, "removing candidate: "+err.Error())
	} else {
		d.event(phase, "candidate removed")
	}
}

func (d *deployRun) finish(status, phase string, cause error) {
	d.res.Status, d.res.Phase = status, phase
	if cause != nil {
		d.res.Message = cause.Error()
		if status == bundle.StatusFailed && phase == "prepare" {
			d.event(phase, cause.Error())
		}
	} else {
		d.event(phase, "deployment live")
	}
}

// truncateLogs keeps the last maxLogBytes: the end of a log is where the
// failure is.
func truncateLogs(s string) string {
	if len(s) <= maxLogBytes {
		return s
	}
	return "[truncated]\n" + s[len(s)-maxLogBytes:]
}

// prune removes all but the newest keepDeployments d<N> dirs, never the
// live one. It runs after every attempt so failing retries cannot fill disk.
func (n *Node) prune(key string, live *Live) {
	entries, err := os.ReadDir(n.appDir(key))
	if err != nil {
		return
	}
	var ids []int64
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "d") {
			continue
		}
		if id, err := strconv.ParseInt(e.Name()[1:], 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	for i, id := range ids {
		if i < keepDeployments || (live != nil && live.Deployment == id) {
			continue
		}
		os.RemoveAll(filepath.Join(n.appDir(key), "d"+strconv.FormatInt(id, 10)))
	}
}
