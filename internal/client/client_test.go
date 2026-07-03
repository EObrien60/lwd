package client

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"lwd/internal/api"
	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// startUnixServer runs the real api.Server on a unix socket for the test.
func startUnixServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "lwd.sock")
	f := node.NewFake()
	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	srv := api.New(reconciler.New(f, s), s, f)

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	httpSrv := &http.Server{Handler: srv.Handler()}
	go httpSrv.Serve(ln)
	t.Cleanup(func() {
		httpSrv.Close()
		s.Close()
		os.Remove(sock)
	})
	return sock
}

func TestClientApplyAndApps(t *testing.T) {
	sock := startUnixServer(t)
	c := New(sock)
	ctx := context.Background()

	dep, err := c.Apply(ctx, &spec.App{Name: "blog", Image: "img:1", Port: 8080, Node: "local"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if dep.Status != store.StatusRunning {
		t.Errorf("status = %q, want running", dep.Status)
	}

	apps, err := c.Apps(ctx)
	if err != nil {
		t.Fatalf("Apps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "blog" {
		t.Fatalf("apps = %+v", apps)
	}
}
