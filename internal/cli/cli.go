// Package cli implements lwd's command dispatch for both the daemon and the
// client subcommands.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"lwd/internal/api"
	"lwd/internal/build"
	"lwd/internal/client"
	"lwd/internal/compose"
	"lwd/internal/config"
	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/router"
	"lwd/internal/secrets"
	"lwd/internal/source"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// Run dispatches a subcommand and returns a process exit code.
func Run(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "missing command")
		return 2
	}
	switch args[0] {
	case "daemon":
		return runDaemon()
	case "apply":
		return runApply(args[1:])
	case "ls":
		return runLs()
	case "logs":
		return runLogs(args[1:])
	case "rm":
		return runRm(args[1:])
	case "rollback":
		return runRollback(args[1:])
	case "history":
		return runHistory(args[1:])
	case "secret":
		return runSecret(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		return 2
	}
}

func runDaemon() int {
	if err := os.MkdirAll(config.DataDir(), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir data dir:", err)
		return 1
	}
	n, err := node.NewLocal()
	if err != nil {
		fmt.Fprintln(os.Stderr, "docker:", err)
		return 1
	}
	s, err := store.Open(config.DBPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "store:", err)
		return 1
	}
	defer s.Close()

	r := router.NewCaddyRouter(n, config.DataDir())

	// Seed the in-memory route set from reality BEFORE the startup reload: if
	// lwd-caddy is still running from a prior daemon process, EnsureUp's
	// Reload would otherwise POST a route-less Caddyfile (CaddyRouter.routes
	// starts empty on every process start) and drop every app's live route
	// for the duration of the reload. Seeding first means that reload's
	// atomic /load installs the full correct set instead, with no empty
	// window. A failure here is logged and tolerated: EnsureUp still runs, so
	// the daemon comes up (just without routes restored) rather than failing
	// to start entirely.
	if routes, err := buildInitialRoutes(context.Background(), n, s); err != nil {
		fmt.Fprintln(os.Stderr, "router: failed to build initial routes:", err)
	} else {
		r.SeedRoutes(routes)
	}

	if err := r.EnsureUp(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "router: failed to bring up Caddy:", err)
		return 1
	}

	cipher, err := secrets.NewCipher(config.KeyPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "secrets: failed to load encryption key:", err)
		return 1
	}
	secStore := secrets.NewStore(cipher, s)

	// The reconciler places each app's containers on the node its spec
	// declares via a Resolver: "" and "local" always resolve to n, the
	// daemon's own local Docker; any other name is looked up in the store's
	// nodes registry (populated by `lwd node add`).
	resolver := node.NewRegistryResolver(n, func(name string) (string, bool, error) {
		rec, err := s.GetNode(name)
		if err != nil {
			return "", false, err
		}
		if rec == nil {
			return "", false, nil
		}
		return rec.SSHHost, true, nil
	})

	srv := api.New(reconciler.New(resolver, r, s, secStore, compose.NewCLI(), source.NewCLI(), build.NewCLI()), s, n, r, secStore)

	sock := config.SocketPath()
	_ = os.Remove(sock) // clean stale socket
	ln, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		return 1
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "chmod socket:", err)
		return 1
	}
	fmt.Println("lwd daemon listening on", sock)
	httpSrv := &http.Server{Handler: srv.Handler()}
	if err := httpSrv.Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		return 1
	}
	return 0
}

// buildInitialRoutes reconstructs the route set that should already be live
// in a still-running Caddy container, from persisted deployment state, so a
// restarting daemon can seed its in-memory Router (see Router.SeedRoutes)
// before its startup reload runs. It considers every running "surface"
// container, keeping only the one that matches the store's recorded current
// (StatusRunning) deployment for its app — by container ID, not name, since
// that's the durable link between a container and the deployment row that
// produced it. Containers left over from an old, superseded deployment (e.g.
// one the reconciler failed to remove) are skipped, as are apps with no
// current deployment or no Domain configured. Routes are deduped by domain
// (a domain can only have one current deployment at a time, so this is
// mostly defensive).
func buildInitialRoutes(ctx context.Context, n node.Node, s *store.Store) ([]router.Route, error) {
	containers, err := n.ListContainers(ctx, map[string]string{"lwd.role": "surface"})
	if err != nil {
		return nil, fmt.Errorf("list surface containers: %w", err)
	}

	routes := make(map[string]router.Route)
	for _, c := range containers {
		if c.State != "running" {
			continue
		}
		app := c.Labels["lwd.app"]
		if app == "" {
			continue
		}
		cur, err := s.CurrentDeployment(app)
		if err != nil || cur == nil {
			continue
		}
		if c.ID != cur.ContainerID {
			// Not the current deployment's container (e.g. a stale surface
			// left over from a failed swap) — skip it so we never seed a
			// route pointing at the wrong container.
			continue
		}

		var a spec.App
		if err := json.Unmarshal([]byte(cur.Spec), &a); err != nil {
			continue
		}
		if a.Domain == "" {
			continue
		}

		routes[a.Domain] = router.Route{
			Domain:      a.Domain,
			Upstream:    c.Name,
			Port:        a.Port,
			TLSInternal: router.UseInternalTLS(a.Domain),
		}
	}

	out := make([]router.Route, 0, len(routes))
	for _, r := range routes {
		out = append(out, r)
	}
	return out, nil
}

func newClient() *client.Client { return client.New(config.SocketPath()) }

func runApply(args []string) int {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}
	app, err := spec.Load(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	dep, err := newClient().Apply(context.Background(), app)
	if err != nil {
		fmt.Fprintln(os.Stderr, "apply:", err)
		return 1
	}
	fmt.Printf("deployed %s (%s) container %s\n", dep.App, dep.Image, dep.ContainerID)
	return 0
}

func runLs() int {
	apps, err := newClient().Apps(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "ls:", err)
		return 1
	}
	fmt.Printf("%-20s %-10s %-30s %s\n", "APP", "STATUS", "DOMAIN", "IMAGE")
	for _, a := range apps {
		fmt.Printf("%-20s %-10s %-30s %s\n", a.Name, a.Status, a.Domain, a.Image)
	}
	return 0
}

func runHistory(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: lwd history <app>")
		return 2
	}
	deps, err := newClient().History(context.Background(), args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "history:", err)
		return 1
	}
	fmt.Printf("%-6s %-10s %-30s %s\n", "ID", "STATUS", "IMAGE", "CREATED")
	for _, d := range deps {
		fmt.Printf("%-6d %-10s %-30s %s\n", d.ID, d.Status, d.Image, d.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	return 0
}

func runLogs(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: lwd logs <app> [-f]")
		return 2
	}
	name := args[0]
	follow := false
	for _, a := range args[1:] {
		if a == "-f" || a == "--follow" {
			follow = true
		}
	}
	if err := newClient().Logs(context.Background(), name, follow, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "logs:", err)
		return 1
	}
	return 0
}

func runRollback(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: lwd rollback <app>")
		return 2
	}
	dep, err := newClient().Rollback(context.Background(), args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "rollback:", err)
		return 1
	}
	fmt.Printf("rolled back %s to %s (container %s)\n", dep.App, dep.Image, dep.ContainerID)
	return 0
}

func runRm(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: lwd rm <app>")
		return 2
	}
	if err := newClient().Remove(context.Background(), args[0]); err != nil {
		fmt.Fprintln(os.Stderr, "rm:", err)
		return 1
	}
	fmt.Println("removed", args[0])
	return 0
}

func runSecret(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: lwd secret <set|ls|rm> ...")
		return 2
	}
	switch args[0] {
	case "set":
		return runSecretSet(args[1:])
	case "ls":
		return runSecretLs(args[1:])
	case "rm":
		return runSecretRm(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown secret command %q\n", args[0])
		return 2
	}
}

func runSecretSet(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: lwd secret set <app> <KEY> (value read from stdin)")
		return 2
	}
	app, key := args[0], args[1]
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "secret set:", err)
		return 1
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if err := newClient().SetSecret(context.Background(), app, key, value); err != nil {
		fmt.Fprintln(os.Stderr, "secret set:", err)
		return 1
	}
	fmt.Printf("secret %s set for %s; redeploy to apply\n", key, app)
	return 0
}

func runSecretLs(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: lwd secret ls <app>")
		return 2
	}
	names, err := newClient().ListSecrets(context.Background(), args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "secret ls:", err)
		return 1
	}
	for _, n := range names {
		fmt.Println(n)
	}
	return 0
}

func runSecretRm(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: lwd secret rm <app> <KEY>")
		return 2
	}
	app, key := args[0], args[1]
	if err := newClient().DeleteSecret(context.Background(), app, key); err != nil {
		fmt.Fprintln(os.Stderr, "secret rm:", err)
		return 1
	}
	fmt.Printf("removed secret %s from %s\n", key, app)
	return 0
}
