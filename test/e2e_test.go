package e2e

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lwd/internal/api"
	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/router"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// Full stack against real Docker over a unix socket.
// Run with: LWD_DOCKER_TEST=1 go test ./test/ -v
func TestEndToEndDeploy(t *testing.T) {
	if os.Getenv("LWD_DOCKER_TEST") == "" {
		t.Skip("set LWD_DOCKER_TEST=1 to run the end-to-end test")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "lwd.sock")

	n, err := node.NewLocal()
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	rtr := router.NewCaddyRouter(n, dir)
	srv := api.New(reconciler.New(n, rtr, s), s, n)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: srv.Handler()}
	go httpSrv.Serve(ln)
	defer httpSrv.Close()

	// lwd publishes container port == host port, so the app must be told to
	// listen on that port. traefik/whoami defaults to :80; point it at :9280
	// via its WHOAMI_PORT_NUMBER env var to match.
	app := &spec.App{
		Name:  "e2e-whoami",
		Image: "traefik/whoami:latest",
		Port:  9280,
		Node:  "local",
		Env:   map[string]string{"WHOAMI_PORT_NUMBER": "9280"},
	}
	app.Health.Path = "/"
	app.Health.Timeout = 30 * time.Second

	rec := reconciler.New(n, rtr, s)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dep, err := rec.Apply(ctx, app)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	defer n.RemoveContainer(context.Background(), dep.ContainerID)

	resp, err := http.Get("http://127.0.0.1:9280/")
	if err != nil {
		t.Fatalf("GET app: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("app status = %d", resp.StatusCode)
	}
}
