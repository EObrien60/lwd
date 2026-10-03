package controller

import (
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"lwd/internal/client"
	"lwd/internal/store"
	"lwd/internal/store/storetest"
)

func TestServeLifecycle(t *testing.T) {
	dbURL := storetest.URL(t)

	// Leave a deployment "running" as if a previous controller crashed.
	ctx := context.Background()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.PutHost(ctx, store.Host{Name: "m1", Addr: "x:1", TokenEnc: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutApp(ctx, store.App{Name: "a", Manifest: "m"}, []store.Environment{{Name: "p", Host: "m1", Domain: "a.b", TLS: "acme"}}); err != nil {
		t.Fatal(err)
	}
	rel := store.Release{App: "a", Manifest: "m", Images: map[string]string{}}
	if err := st.CreateRelease(ctx, &rel); err != nil {
		t.Fatal(err)
	}
	d := store.Deployment{App: "a", Env: "p", Release: rel.ID, Host: "m1", Kind: "deploy"}
	if err := st.CreateDeployment(ctx, &d); err != nil {
		t.Fatal(err)
	}
	st.Close()

	cfg := Config{DatabaseURL: dbURL, APIToken: "tok", SecretKeyFile: filepath.Join(t.TempDir(), "k", "secret.key")}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- Serve(runCtx, cfg, ln, slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	c := client.New("http://"+ln.Addr().String(), "tok")
	var hist []store.Deployment
	deadline := time.Now().Add(10 * time.Second)
	for {
		hist, err = c.Deployments(ctx, "a", "p")
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Status != store.StatusFailed {
		t.Errorf("interrupted deployment not failed at startup: %+v", hist)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not shut down")
	}
}

func TestServeRefusesWithoutToken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	err = Serve(context.Background(), Config{DatabaseURL: "postgres://unused"}, ln, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("expected refusal without API token")
	}
}
