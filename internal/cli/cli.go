// Package cli implements the lwd command line: `lwd controller` and every
// client command from docs/lwd2/DESIGN.md "CLI". Client commands talk to the
// controller's /v1 API (LWD_URL, LWD_TOKEN or ~/.config/lwd/token).
//
// Flags may follow positional arguments (`lwd deploy hello prod --release 3`);
// each subcommand has its own flag set. Read commands accept --json to print
// the API's JSON instead of a table.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"lwd/internal/bundle"
	"lwd/internal/client"
	"lwd/internal/controller"
	"lwd/internal/manifest"
	"lwd/internal/store"
	"lwd/internal/version"
)

const usageText = `Usage: lwd <command> [arguments]

Server:
  lwd controller                         run the controller (env: LWD_DATABASE_URL, LWD_API_TOKEN, ...)
  lwd node                               run the node agent on a workload host
  lwd version

Hosts:
  lwd host add NAME ADDR --token-file F  register a node (ADDR is host:port of its API)
  lwd host list
  lwd host status NAME

Apps:
  lwd app apply [DIR]                    validate and upload DIR/lwd.toml (default .)
  lwd app list
  lwd app status APP [ENV]

Releases and deploys:
  lwd release create APP --tag T [--commit C] [--image svc=ref]...
  lwd release list APP
  lwd deploy APP ENV [--release N]
  lwd rollback APP ENV [--to N]
  lwd restart APP ENV [--service S]
  lwd logs APP ENV [--service S] [--tail N]
  lwd history APP ENV

Secrets:
  lwd secret set APP ENV KEY             value is read from stdin
  lwd secret list APP ENV
  lwd secret rm APP ENV KEY

Audit:
  lwd events [APP]

Read commands accept --json. Client config: LWD_URL (default http://127.0.0.1:7470),
LWD_TOKEN or ~/.config/lwd/token.
`

// usageError means the command line was wrong: print usage, exit 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

// errReported means the command already printed why it failed; exit 1.
var errReported = errors.New("reported")

// Run executes an lwd subcommand and returns the process exit code.
func Run(args []string) int {
	return run(args, os.Stdin, os.Stdout, os.Stderr, os.Getenv)
}

type cli struct {
	ctx    context.Context
	in     io.Reader
	out    io.Writer
	err    io.Writer
	getenv func(string) string
}

type command struct {
	name string // "deploy" or "host add"
	run  func(c *cli, args []string) error
}

var commands = []command{
	{"controller", (*cli).controller},
	{"version", func(c *cli, _ []string) error { fmt.Fprintln(c.out, "lwd", version.String); return nil }},
	{"host add", (*cli).hostAdd},
	{"host list", (*cli).hostList},
	{"host status", (*cli).hostStatus},
	{"app apply", (*cli).appApply},
	{"app list", (*cli).appList},
	{"app status", (*cli).appStatus},
	{"release create", (*cli).releaseCreate},
	{"release list", (*cli).releaseList},
	{"deploy", (*cli).deploy},
	{"rollback", (*cli).rollback},
	{"restart", (*cli).restart},
	{"logs", (*cli).logs},
	{"history", (*cli).history},
	{"secret set", (*cli).secretSet},
	{"secret list", (*cli).secretList},
	{"secret rm", (*cli).secretRm},
	{"events", (*cli).events},
}

func run(args []string, in io.Reader, out, errw io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprint(errw, usageText)
		return 2
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		fmt.Fprint(out, usageText)
		return 0
	}
	var cmd *command
	var rest []string
	for i := range commands {
		words := strings.Fields(commands[i].name)
		if len(args) >= len(words) && strings.Join(args[:len(words)], " ") == commands[i].name {
			cmd, rest = &commands[i], args[len(words):]
			break
		}
	}
	if cmd == nil {
		fmt.Fprintf(errw, "lwd: unknown command %q\n\n%s", strings.Join(args[:min(2, len(args))], " "), usageText)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := &cli{ctx: ctx, in: in, out: out, err: errw, getenv: getenv}
	err := cmd.run(c, rest)
	var ue usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ue):
		fmt.Fprintf(errw, "lwd %s: %s\nRun 'lwd help' for usage.\n", cmd.name, ue.msg)
		return 2
	case errors.Is(err, errReported):
		return 1
	default:
		fmt.Fprintf(errw, "lwd %s: %v\n", cmd.name, err)
		return 1
	}
}

// parse parses flags anywhere among args and checks the positional count.
func parse(fs *flag.FlagSet, args []string, minPos, maxPos int) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError{err.Error()}
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) < minPos || len(pos) > maxPos {
		return nil, usagef("wrong number of arguments")
	}
	return pos, nil
}

func newFlags(name string) *flag.FlagSet { return flag.NewFlagSet(name, flag.ContinueOnError) }

func (c *cli) client() (*client.Client, error) {
	cl, err := client.FromEnv(c.getenv)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	user := c.getenv("USER")
	if user == "" {
		user = "unknown"
	}
	cl.Actor = user + "@" + host
	return cl, nil
}

func (c *cli) printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(c.out, string(b))
	return err
}

func (c *cli) table(header string, rows func(w io.Writer)) {
	tw := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	rows(tw)
	tw.Flush()
}

func ts(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// --- controller ---

func (c *cli) controller(args []string) error {
	if _, err := parse(newFlags("controller"), args, 0, 0); err != nil {
		return err
	}
	cfg, err := controller.ConfigFromEnv(c.getenv)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(c.err, nil))
	// After the first signal, restore default handling so a second Ctrl-C
	// kills the process instead of waiting out in-flight deploys.
	ctx, stop := signal.NotifyContext(c.ctx, os.Interrupt, syscall.SIGTERM)
	go func() { <-ctx.Done(); stop() }()
	return controller.Run(ctx, cfg, log)
}

// --- hosts ---

func (c *cli) hostAdd(args []string) error {
	fs := newFlags("host add")
	tokenFile := fs.String("token-file", "", "file holding the node's LWD_NODE_TOKEN")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	if *tokenFile == "" {
		return usagef("--token-file is required")
	}
	tok, err := os.ReadFile(*tokenFile)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	h, err := cl.AddHost(c.ctx, client.HostRequest{Name: pos[0], Addr: pos[1], Token: strings.TrimSpace(string(tok))})
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "host %s registered at %s\n", h.Name, h.Addr)
	return nil
}

func (c *cli) hostList(args []string) error {
	fs := newFlags("host list")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	hs, err := cl.ListHosts(c.ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(hs)
	}
	c.table("NAME\tADDR\tADDED", func(w io.Writer) {
		for _, h := range hs {
			fmt.Fprintf(w, "%s\t%s\t%s\n", h.Name, h.Addr, ts(h.CreatedAt))
		}
	})
	return nil
}

func (c *cli) hostStatus(args []string) error {
	fs := newFlags("host status")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	h, err := cl.GetHost(c.ctx, pos[0])
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(h)
	}
	fmt.Fprintf(c.out, "host:      %s\naddr:      %s\nreachable: %v\n", h.Name, h.Addr, h.Reachable)
	if h.Error != "" {
		fmt.Fprintf(c.out, "error:     %s\n", h.Error)
	}
	if len(h.Node) > 0 {
		fmt.Fprintf(c.out, "node:\n%s\n", indentJSON(h.Node))
	}
	return nil
}

func indentJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.MarshalIndent(v, "  ", "  ")
	return "  " + string(b)
}

// --- apps ---

func (c *cli) appApply(args []string) error {
	pos, err := parse(newFlags("app apply"), args, 0, 1)
	if err != nil {
		return err
	}
	dir := "."
	if len(pos) == 1 {
		dir = pos[0]
	}
	data, err := os.ReadFile(filepath.Join(dir, "lwd.toml"))
	if err != nil {
		return err
	}
	// Validate locally first: faster feedback, and no round trip for typos.
	m, err := manifest.Parse(data)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	a, err := cl.ApplyApp(c.ctx, m.Name, data)
	if err != nil {
		return err
	}
	var envs []string
	for _, e := range a.Environments {
		envs = append(envs, e.Name)
	}
	fmt.Fprintf(c.out, "applied %s (environments: %s)\n", a.Name, strings.Join(envs, ", "))
	for _, w := range a.Warnings {
		fmt.Fprintf(c.err, "warning: %s\n", w)
	}
	return nil
}

func (c *cli) appList(args []string) error {
	fs := newFlags("app list")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	as, err := cl.ListApps(c.ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(as)
	}
	c.table("APP\tENVIRONMENTS\tUPDATED", func(w io.Writer) {
		for _, a := range as {
			var envs []string
			for _, e := range a.Environments {
				envs = append(envs, e.Name)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", a.Name, dash(strings.Join(envs, ",")), ts(a.UpdatedAt))
		}
	})
	return nil
}

func (c *cli) appStatus(args []string) error {
	fs := newFlags("app status")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args, 1, 2)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	if len(pos) == 2 {
		st, err := cl.Status(c.ctx, pos[0], pos[1])
		if err != nil {
			return err
		}
		if *asJSON {
			return c.printJSON(st)
		}
		c.printEnvStatus(st)
		return nil
	}
	a, err := cl.GetApp(c.ctx, pos[0])
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(a)
	}
	c.table("ENV\tHOST\tDOMAIN\tTLS\tLIVE RELEASE\tLAST DEPLOYMENT", func(w io.Writer) {
		for _, e := range a.Environments {
			live, last := "-", "-"
			if st, err := cl.Status(c.ctx, a.Name, e.Name); err == nil {
				if st.Live != nil {
					live = strconv.FormatInt(st.Live.Release, 10)
				}
				if st.Last != nil {
					last = fmt.Sprintf("%d %s (%s)", st.Last.ID, st.Last.Status, ts(st.Last.StartedAt))
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Name, e.Host, e.Domain, e.TLS, live, last)
		}
	})
	return nil
}

func (c *cli) printEnvStatus(st client.EnvStatus) {
	fmt.Fprintf(c.out, "app:    %s\nenv:    %s\nhost:   %s\ndomain: %s (tls %s)\n", st.App, st.Env, st.Host, st.Domain, st.TLS)
	if st.Live != nil {
		fmt.Fprintf(c.out, "live:   deployment %d, release %d (%s, %s)\n", st.Live.ID, st.Live.Release, st.Live.Kind, ts(st.Live.StartedAt))
	} else {
		fmt.Fprintln(c.out, "live:   nothing deployed")
	}
	if st.Last != nil && (st.Live == nil || st.Last.ID != st.Live.ID) {
		fmt.Fprintf(c.out, "last:   deployment %d, release %d %s at %s: %s\n", st.Last.ID, st.Last.Release, st.Last.Status, dash(st.Last.Phase), dash(st.Last.Message))
	}
	switch {
	case st.NodeError != "":
		fmt.Fprintf(c.out, "node:   unavailable: %s\n", st.NodeError)
	case len(st.Node) > 0 && string(st.Node) != "null":
		fmt.Fprintf(c.out, "node:\n%s\n", indentJSON(st.Node))
	default:
		fmt.Fprintln(c.out, "node:   no live deployment reported")
	}
}

// --- releases ---

// mapFlag collects repeated key=value flags.
type mapFlag map[string]string

func (m mapFlag) String() string { return "" }
func (m mapFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" || v == "" {
		return fmt.Errorf("want svc=ref, got %q", s)
	}
	m[k] = v
	return nil
}

func (c *cli) releaseCreate(args []string) error {
	fs := newFlags("release create")
	tag := fs.String("tag", "", "image tag for every service")
	commit := fs.String("commit", "", "source commit")
	images := mapFlag{}
	fs.Var(images, "image", "svc=ref override (repeatable)")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	req := client.ReleaseRequest{Tag: *tag, Commit: *commit}
	if len(images) > 0 {
		req.Images = images
	}
	r, err := cl.CreateRelease(c.ctx, pos[0], req)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(r)
	}
	fmt.Fprintf(c.out, "release %d of %s created\n", r.ID, r.App)
	c.table("SERVICE\tIMAGE", func(w io.Writer) {
		for _, svc := range slices.Sorted(maps.Keys(r.Images)) {
			fmt.Fprintf(w, "%s\t%s\n", svc, r.Images[svc])
		}
	})
	return nil
}

func (c *cli) releaseList(args []string) error {
	fs := newFlags("release list")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	rs, err := cl.ListReleases(c.ctx, pos[0])
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(rs)
	}
	c.table("ID\tTAG\tCOMMIT\tCREATED\tBY", func(w io.Writer) {
		for _, r := range rs {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", r.ID, dash(r.Tag), dash(r.Commit), ts(r.CreatedAt), dash(r.Actor))
		}
	})
	return nil
}

// --- deploy, rollback, restart, logs, history ---

func (c *cli) deploy(args []string) error {
	fs := newFlags("deploy")
	rel := fs.Int64("release", 0, "release id (default newest)")
	asJSON := fs.Bool("json", false, "print the deployment as JSON")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	fmt.Fprintf(c.err, "deploying %s/%s (this waits for readiness and smoke checks)...\n", pos[0], pos[1])
	d, err := cl.Deploy(c.ctx, pos[0], pos[1], client.DeployRequest{Release: *rel})
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printDeploymentJSON(d)
	}
	return c.reportDeployment(d)
}

func (c *cli) rollback(args []string) error {
	fs := newFlags("rollback")
	to := fs.Int64("to", 0, "release id (default: previous live release)")
	asJSON := fs.Bool("json", false, "print the deployment as JSON")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	fmt.Fprintf(c.err, "rolling back %s/%s...\n", pos[0], pos[1])
	d, err := cl.Rollback(c.ctx, pos[0], pos[1], client.RollbackRequest{To: *to})
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printDeploymentJSON(d)
	}
	return c.reportDeployment(d)
}

// reportDeployment prints the outcome; anything but success exits 1 with the
// node's steps and log excerpt, which is usually all one needs to debug.
func (c *cli) reportDeployment(d store.Deployment) error {
	fmt.Fprintf(c.out, "deployment %d %s: %s of release %d to %s/%s on %s (phase %s)\n",
		d.ID, d.Status, d.Kind, d.Release, d.App, d.Env, d.Host, dash(d.Phase))
	if d.Status == store.StatusSucceeded {
		return nil
	}
	if d.Message != "" {
		fmt.Fprintf(c.out, "  %s\n", d.Message)
	}
	var res bundle.Result
	if len(d.Result) > 0 && json.Unmarshal(d.Result, &res) == nil {
		for _, e := range res.Events {
			fmt.Fprintf(c.out, "  %s  %-8s %s\n", e.At, e.Phase, e.Message)
		}
		if res.Logs != "" {
			fmt.Fprintf(c.out, "--- logs ---\n%s\n", strings.TrimRight(res.Logs, "\n"))
		}
	}
	return errReported
}

func (c *cli) restart(args []string) error {
	fs := newFlags("restart")
	svc := fs.String("service", "", "only this service")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	if err := cl.Restart(c.ctx, pos[0], pos[1], *svc); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "restarted %s/%s\n", pos[0], pos[1])
	return nil
}

func (c *cli) logs(args []string) error {
	fs := newFlags("logs")
	svc := fs.String("service", "", "only this service")
	tail := fs.Int("tail", 0, "lines per service (node default if 0)")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	text, err := cl.Logs(c.ctx, pos[0], pos[1], *svc, *tail)
	if err != nil {
		return err
	}
	_, err = io.WriteString(c.out, text)
	return err
}

func (c *cli) history(args []string) error {
	fs := newFlags("history")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	ds, err := cl.Deployments(c.ctx, pos[0], pos[1])
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(ds)
	}
	c.table("ID\tRELEASE\tKIND\tSTATUS\tPHASE\tSTARTED\tDURATION\tBY\tMESSAGE", func(w io.Writer) {
		for _, d := range ds {
			dur := "-"
			if d.FinishedAt != nil {
				dur = d.FinishedAt.Sub(d.StartedAt).Round(time.Second).String()
			}
			fmt.Fprintf(w, "%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				d.ID, d.Release, d.Kind, d.Status, dash(d.Phase), ts(d.StartedAt), dur, dash(d.Actor), oneLine(d.Message))
		}
	})
	return nil
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return dash(s)
}

// --- secrets ---

func (c *cli) secretSet(args []string) error {
	pos, err := parse(newFlags("secret set"), args, 3, 3)
	if err != nil {
		return err
	}
	// Read from stdin so values stay out of shell history and process lists.
	// `echo x | lwd secret set` adds one newline, which is not part of the value.
	val, err := io.ReadAll(io.LimitReader(c.in, 1<<20))
	if err != nil {
		return err
	}
	val = []byte(strings.TrimSuffix(string(val), "\n"))
	cl, err := c.client()
	if err != nil {
		return err
	}
	m, err := cl.SetSecret(c.ctx, pos[0], pos[1], pos[2], val)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "secret %s set for %s/%s (version %d); takes effect on next deploy\n", m.Key, pos[0], pos[1], m.Version)
	return nil
}

func (c *cli) secretList(args []string) error {
	fs := newFlags("secret list")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	ms, err := cl.ListSecrets(c.ctx, pos[0], pos[1])
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(ms)
	}
	c.table("KEY\tVERSION\tUPDATED", func(w io.Writer) {
		for _, m := range ms {
			fmt.Fprintf(w, "%s\t%d\t%s\n", m.Key, m.Version, ts(m.UpdatedAt))
		}
	})
	return nil
}

func (c *cli) secretRm(args []string) error {
	pos, err := parse(newFlags("secret rm"), args, 3, 3)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	if err := cl.DeleteSecret(c.ctx, pos[0], pos[1], pos[2]); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "secret %s removed from %s/%s; takes effect on next deploy\n", pos[2], pos[0], pos[1])
	return nil
}

// --- events ---

func (c *cli) events(args []string) error {
	fs := newFlags("events")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	app := ""
	if len(pos) == 1 {
		app = pos[0]
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	es, err := cl.Events(c.ctx, app, "", 0)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(es)
	}
	c.table("TIME\tAPP/ENV\tKIND\tMESSAGE", func(w io.Writer) {
		for _, e := range es {
			where := "-"
			if e.App != "" {
				where = e.App
				if e.Env != "" {
					where += "/" + e.Env
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", ts(e.At), where, e.Kind, oneLine(e.Message))
		}
	})
	return nil
}

// printDeploymentJSON prints d as JSON; like reportDeployment, anything but
// success exits 1 so scripts can branch on the exit code or the body.
func (c *cli) printDeploymentJSON(d store.Deployment) error {
	if err := c.printJSON(d); err != nil {
		return err
	}
	if d.Status != store.StatusSucceeded {
		return errReported
	}
	return nil
}
