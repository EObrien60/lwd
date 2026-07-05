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
	"os/signal"
	"strings"
	"syscall"
	"time"

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
	case "node":
		return runNode(args[1:])
	case "pool":
		return runPool(args[1:])
	case "health":
		return runHealth(args[1:])
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
	agentToken := os.Getenv("LWD_AGENT_TOKEN")
	resolver := node.NewRegistryResolver(n, agentToken, func(name string) (string, string, string, bool, error) {
		rec, err := s.GetNode(name)
		if err != nil {
			return "", "", "", false, err
		}
		if rec == nil {
			return "", "", "", false, nil
		}
		return rec.SSHHost, rec.MeshAddr, rec.AgentURL, true, nil
	})

	// resolver is passed as the api.NodeCacheInvalidator too: POST/DELETE
	// /nodes call resolver.Invalidate so a node add/update/remove never
	// leaves a stale cached docker-over-ssh client behind.
	rec := reconciler.New(resolver, r, s, secStore, compose.NewCLI(), source.NewCLI(), build.NewCLI())
	srv := api.New(rec, s, n, r, secStore, resolver)

	// Phase 10 continuous reconciler loop: rec.reach lets it observe node
	// reachability (*node.RegistryResolver satisfies reconciler.Reachability
	// directly), and nudge lets a successful manual Apply/Rollback wake it
	// early instead of waiting out the ticker.
	rec.SetReachability(resolver)
	nudge := make(chan struct{}, 1)
	rec.SetNudge(nudge)

	// ctx is canceled on SIGINT/SIGTERM: it both stops the reconcile loop
	// below and signals the graceful HTTP shutdown further down.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go reconciler.RunLoop(ctx, rec, config.ReconcileInterval(), nudge)

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

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()

	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "serve:", err)
			return 1
		}
	case <-ctx.Done():
		fmt.Println("lwd daemon shutting down")
		// Bounded, not context.Background(): an in-flight `logs?follow`
		// stream is long-lived by design, and an unbounded Shutdown would
		// wait on it forever instead of letting the process exit.
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		if err := httpSrv.Shutdown(sctx); err != nil {
			fmt.Fprintln(os.Stderr, "shutdown:", err)
			return 1
		}
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

func runNode(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: lwd node <add|ls|rm|capacity|inspect> ...")
		return 2
	}
	switch args[0] {
	case "add":
		return runNodeAdd(args[1:])
	case "ls":
		return runNodeLs()
	case "rm":
		return runNodeRm(args[1:])
	case "capacity":
		return runNodeCapacity()
	case "inspect":
		return runNodeInspect(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown node command %q\n", args[0])
		return 2
	}
}

func runNodeAdd(args []string) int {
	var positional []string
	agentURL := ""
	pool := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--agent":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "usage: lwd node add <name> <ssh-host> <mesh-addr> [--agent <url>] [--pool <name>]")
				return 2
			}
			agentURL = args[i+1]
			i++
		case "--pool":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "usage: lwd node add <name> <ssh-host> <mesh-addr> [--agent <url>] [--pool <name>]")
				return 2
			}
			pool = args[i+1]
			i++
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) < 3 {
		fmt.Fprintln(os.Stderr, "usage: lwd node add <name> <ssh-host> <mesh-addr> [--agent <url>] [--pool <name>]")
		return 2
	}
	name, sshHost, meshAddr := positional[0], positional[1], positional[2]
	if err := newClient().AddNode(context.Background(), name, sshHost, meshAddr, agentURL, pool); err != nil {
		fmt.Fprintln(os.Stderr, "node add:", err)
		return 1
	}
	displayPool := pool
	if displayPool == "" {
		displayPool = "default"
	}
	if agentURL != "" {
		fmt.Printf("added node %s (ssh %s, mesh %s, agent %s, pool %s)\n", name, sshHost, meshAddr, agentURL, displayPool)
	} else {
		fmt.Printf("added node %s (ssh %s, mesh %s, pool %s)\n", name, sshHost, meshAddr, displayPool)
	}
	return 0
}

func runNodeLs() int {
	nodes, err := newClient().Nodes(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "node ls:", err)
		return 1
	}
	fmt.Printf("%-20s %-30s %-15s %-30s %-10s %-10s %s\n", "NAME", "SSH", "MESH", "AGENT", "POOL", "TRANSPORT", "REACHABLE")
	for _, n := range nodes {
		reachable := "no"
		if n.Reachable {
			reachable = "yes"
		}
		fmt.Printf("%-20s %-30s %-15s %-30s %-10s %-10s %s\n", n.Name, n.SSHHost, n.MeshAddr, n.AgentURL, n.Pool, n.Transport, reachable)
	}
	return 0
}

func runPool(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: lwd pool <ls> ...")
		return 2
	}
	switch args[0] {
	case "ls":
		return runPoolLs()
	default:
		fmt.Fprintf(os.Stderr, "unknown pool command %q\n", args[0])
		return 2
	}
}

func runPoolLs() int {
	pools, err := newClient().Pools(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "pool ls:", err)
		return 1
	}
	fmt.Printf("%-20s %s\n", "POOL", "NODES")
	for _, p := range pools {
		fmt.Printf("%-20s %d\n", p.Name, p.Nodes)
	}
	return 0
}

// humanBytes formats a byte count as a short human-readable string (e.g.
// "512M", "2.3G"), using the same binary (1024-based) units spec.ParseSize
// accepts for `requirements.memory` — so a size printed by `lwd node
// capacity`/`lwd node inspect` is valid input if pasted back into an
// lwd.toml. A non-positive count (including the zero value of an
// unmeasured Capacity) renders as "0".
func humanBytes(n int64) string {
	if n <= 0 {
		return "0"
	}
	units := []string{"B", "K", "M", "G", "T"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d%s", int64(f), units[i])
	}
	return fmt.Sprintf("%.1f%s", f, units[i])
}

// runNodeCapacity prints every node's live capacity snapshot (as observed by
// the continuous reconciler, via GET /health) alongside its pool (from GET
// /nodes — capacity and pool live in different daemon responses, see
// client.Health/client.Nodes). A node whose capacity could not be measured
// live (e.g. a briefly-unreachable remote, or an ssh-only node that only
// reports totals) shows "known = no" with CPU/MEM/DISK columns blank rather
// than misleadingly zero.
func runNodeCapacity() int {
	c := newClient()
	ctx := context.Background()

	h, err := c.Health(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "node capacity:", err)
		return 1
	}
	nodes, err := c.Nodes(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "node capacity:", err)
		return 1
	}
	pools := make(map[string]string, len(nodes))
	for _, n := range nodes {
		pools[n.Name] = n.Pool
	}

	fmt.Printf("%-16s %-10s %-14s %-20s %-20s %s\n", "NODE", "POOL", "CPU", "MEM", "DISK", "KNOWN")
	for _, n := range h.Nodes {
		pool := pools[n.Name]
		if pool == "" {
			pool = "default"
		}
		cpu, mem, disk, known := "—", "—", "—", "no"
		if n.Capacity.Known {
			known = "yes"
			cpu = fmt.Sprintf("%.2f/%d", n.Capacity.CPUUsed, n.Capacity.CPUCores)
			mem = fmt.Sprintf("%s/%s", humanBytes(n.Capacity.MemAvailable), humanBytes(n.Capacity.MemTotal))
			disk = fmt.Sprintf("%s/%s", humanBytes(n.Capacity.DiskFree), humanBytes(n.Capacity.DiskTotal))
		}
		fmt.Printf("%-16s %-10s %-14s %-20s %-20s %s\n", n.Name, pool, cpu, mem, disk, known)
	}
	return 0
}

// runNodeInspect prints one node's pool, live capacity, and the surfaces
// currently placed on it. Placement isn't tracked as its own queryable
// field anywhere; it's derived the same way a human would have to: for
// every app, its most recent (current) deployment's recorded spec snapshot
// carries the concrete `Node` it was actually scheduled/pinned to (see
// reconciler.applyImage/applyGit, which resolve placement before recording
// the snapshot) — so an app whose current spec's Node matches name is
// "on" this node.
func runNodeInspect(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: lwd node inspect <name>")
		return 2
	}
	name := args[0]
	c := newClient()
	ctx := context.Background()

	nodes, err := c.Nodes(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "node inspect:", err)
		return 1
	}
	pool := "default"
	for _, n := range nodes {
		if n.Name == name {
			pool = n.Pool
			break
		}
	}

	h, err := c.Health(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "node inspect:", err)
		return 1
	}
	var nh *reconciler.NodeHealth
	for i := range h.Nodes {
		if h.Nodes[i].Name == name {
			nh = &h.Nodes[i]
			break
		}
	}

	fmt.Printf("NODE:      %s\n", name)
	fmt.Printf("POOL:      %s\n", pool)
	if nh == nil {
		fmt.Println("(no live health data for this node yet — it may not be registered, or no reconcile pass has run)")
	} else {
		fmt.Printf("TRANSPORT: %s\n", nh.Transport)
		fmt.Printf("REACHABLE: %v\n", nh.Reachable)
		if nh.Capacity.Known {
			fmt.Printf("CPU:       %.2f/%d cores\n", nh.Capacity.CPUUsed, nh.Capacity.CPUCores)
			fmt.Printf("MEM:       %s/%s\n", humanBytes(nh.Capacity.MemAvailable), humanBytes(nh.Capacity.MemTotal))
			fmt.Printf("DISK:      %s/%s\n", humanBytes(nh.Capacity.DiskFree), humanBytes(nh.Capacity.DiskTotal))
		} else {
			fmt.Println("CAPACITY:  unknown")
		}
	}

	apps, err := c.Apps(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "node inspect:", err)
		return 1
	}

	fmt.Println()
	fmt.Println("SURFACES")
	fmt.Printf("%-20s %-10s %-30s %s\n", "APP", "STATUS", "DOMAIN", "IMAGE")
	var any bool
	for _, a := range apps {
		history, err := c.History(ctx, a.Name)
		if err != nil || len(history) == 0 {
			continue
		}
		var spc spec.App
		if err := json.Unmarshal([]byte(history[0].Spec), &spc); err != nil {
			continue
		}
		if spc.Node != name {
			continue
		}
		any = true
		fmt.Printf("%-20s %-10s %-30s %s\n", a.Name, a.Status, a.Domain, a.Image)
	}
	if !any {
		fmt.Println("(none)")
	}
	return 0
}

// runHealth prints the daemon's current reconciler health snapshot: node
// reachability, edge (Caddy) reachability, and per-app self-heal state. args
// is accepted (and ignored) for consistency with the other run* commands;
// `lwd health` takes no flags or positional arguments.
func runHealth(args []string) int {
	h, err := newClient().Health(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "health:", err)
		return 1
	}

	fmt.Println("NODES")
	fmt.Printf("%-20s %-10s %s\n", "NAME", "TRANSPORT", "REACHABLE")
	for _, n := range h.Nodes {
		reachable := "no"
		if n.Reachable {
			reachable = "yes"
		}
		fmt.Printf("%-20s %-10s %s\n", n.Name, n.Transport, reachable)
	}

	fmt.Println()
	fmt.Println("EDGE")
	edgeReachable := "no"
	if h.Edge.Reachable {
		edgeReachable = "yes"
	}
	fmt.Printf("caddy reachable: %s\n", edgeReachable)

	fmt.Println()
	fmt.Println("APPS")
	fmt.Printf("%-20s %-10s %-14s %s\n", "APP", "STATE", "HEAL ATTEMPTS", "LAST ERROR")
	for _, a := range h.Apps {
		fmt.Printf("%-20s %-10s %-14d %s\n", a.App, a.State, a.HealAttempts, a.LastError)
	}
	return 0
}

func runNodeRm(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: lwd node rm <name>")
		return 2
	}
	if err := newClient().RemoveNode(context.Background(), args[0]); err != nil {
		fmt.Fprintln(os.Stderr, "node rm:", err)
		return 1
	}
	fmt.Println("removed node", args[0])
	return 0
}
