// Package cli implements lwd's command dispatch for both the daemon and the
// client subcommands.
package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"

	"lwd/internal/api"
	"lwd/internal/client"
	"lwd/internal/config"
	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/router"
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
	srv := api.New(reconciler.New(n, r, s), s, n)

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
	fmt.Printf("%-20s %-10s %s\n", "APP", "STATUS", "IMAGE")
	for _, a := range apps {
		fmt.Printf("%-20s %-10s %s\n", a.Name, a.Status, a.Image)
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
