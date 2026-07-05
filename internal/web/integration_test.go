package web

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lwd/internal/api"
	"lwd/internal/build"
	"lwd/internal/client"
	"lwd/internal/compose"
	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/router"
	"lwd/internal/secrets"
	"lwd/internal/source"
	"lwd/internal/store"
)

// startFakeDaemon runs a real daemon api.Server on a temp unix socket, backed
// by the fake node/router/compose stack (no Docker) plus a real temp SQLite
// store and a real secrets.Store. This mirrors internal/client/client_test.go's
// startUnixServer helper so this test can exercise the full
// web -> client -> daemon path without Docker.
func startFakeDaemon(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "lwd.sock")

	f := node.NewFake()
	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	rt := router.NewFakeRouter()
	cipher, err := secrets.NewCipher(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("secrets.NewCipher: %v", err)
	}
	secStore := secrets.NewStore(cipher, s)
	daemon := api.New(reconciler.New(node.FakeResolver{"local": f}, rt, s, secStore, compose.NewFake(), source.NewFake(), build.NewFake()), s, f, rt, secStore)

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	httpSrv := &http.Server{Handler: daemon.Handler()}
	go httpSrv.Serve(ln)
	t.Cleanup(func() {
		httpSrv.Close()
		s.Close()
		os.Remove(sock)
	})
	return sock
}

// TestIntegrationWebClientDaemon drives lwd-web's real HTTP handler (Server,
// backed by a real *client.Client) against a real daemon api.Server on a
// temp unix socket. No Docker is involved: the daemon uses the fake
// node/router/compose stack. This proves the full
// browser -> lwd-web -> internal/client -> daemon chain works end to end.
func TestIntegrationWebClientDaemon(t *testing.T) {
	sock := startFakeDaemon(t)
	c := client.New(sock)
	auth := NewAuthenticator([]byte("integration-test-signing-key"), "testpass")
	srv := NewServer(c, auth)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// An unauthenticated request to the JSON API is rejected.
	resp, err := http.Get(ts.URL + "/api/apps")
	if err != nil {
		t.Fatalf("GET /api/apps (unauthed): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthed GET /api/apps = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	// Log in and let the cookie jar capture the signed session cookie.
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	httpClient := &http.Client{Jar: jar}

	resp, err = httpClient.PostForm(ts.URL+"/login", url.Values{"password": {"testpass"}})
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	resp.Body.Close()

	base, _ := url.Parse(ts.URL)
	var sessionCookie *http.Cookie
	for _, ck := range jar.Cookies(base) {
		if ck.Name == sessionCookieName {
			sessionCookie = ck
		}
	}
	if sessionCookie == nil {
		t.Fatalf("no %s cookie set after login", sessionCookieName)
	}

	// Authenticated, the overview starts empty.
	apps := getApps(t, httpClient, ts.URL)
	if len(apps) != 0 {
		t.Fatalf("apps before apply = %+v, want empty", apps)
	}

	// Apply a single-service app from a pasted lwd.toml document, exactly as
	// the "Deploy" modal in the UI would.
	tomlDoc := `
name = "blog"
image = "ghcr.io/example/blog:latest"
domain = "blog.localhost"
port = 8080
`
	resp, err = httpClient.Post(ts.URL+"/api/apply", "application/toml", strings.NewReader(tomlDoc))
	if err != nil {
		t.Fatalf("POST /api/apply: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/apply = %d, body = %s", resp.StatusCode, body)
	}
	var dep store.Deployment
	if err := json.Unmarshal(body, &dep); err != nil {
		t.Fatalf("unmarshal apply response: %v (body %s)", err, body)
	}
	if dep.Status != store.StatusRunning {
		t.Fatalf("apply status = %q, want %q", dep.Status, store.StatusRunning)
	}

	// The app now shows up in the overview.
	apps = getApps(t, httpClient, ts.URL)
	if len(apps) != 1 || apps[0].Name != "blog" {
		t.Fatalf("apps after apply = %+v", apps)
	}

	// The detail endpoint reports both current status and history.
	resp, err = httpClient.Get(ts.URL + "/api/apps/blog")
	if err != nil {
		t.Fatalf("GET /api/apps/blog: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/apps/blog = %d, body = %s", resp.StatusCode, body)
	}
	var detail appDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("unmarshal detail: %v (body %s)", err, body)
	}
	if detail.Status == nil || detail.Status.Name != "blog" {
		t.Fatalf("detail.Status = %+v", detail.Status)
	}
	if len(detail.History) != 1 {
		t.Fatalf("detail.History = %+v, want 1 entry", detail.History)
	}
}

// getApps performs an authenticated GET /api/apps and decodes the response.
func getApps(t *testing.T, c *http.Client, base string) []api.AppStatus {
	t.Helper()
	resp, err := c.Get(base + "/api/apps")
	if err != nil {
		t.Fatalf("GET /api/apps: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /api/apps body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/apps = %d, body = %s", resp.StatusCode, body)
	}
	var apps []api.AppStatus
	if err := json.Unmarshal(body, &apps); err != nil {
		t.Fatalf("unmarshal apps: %v (body %s)", err, body)
	}
	return apps
}

// TestApiUnreachable502 points a real *client.Client at a socket path with no
// listener and asserts the web layer surfaces the daemon-unreachable case as
// 502 (not 500), so the browser can distinguish "daemon down" from an
// application error.
func TestApiUnreachable502(t *testing.T) {
	bogusSock := filepath.Join(t.TempDir(), "does-not-exist.sock")
	c := client.New(bogusSock)
	auth := NewAuthenticator([]byte("integration-test-signing-key"), "testpass")
	srv := NewServer(c, auth)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	httpClient := &http.Client{Jar: jar}

	resp, err := httpClient.PostForm(ts.URL+"/login", url.Values{"password": {"testpass"}})
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	resp.Body.Close()

	resp, err = httpClient.Get(ts.URL + "/api/apps")
	if err != nil {
		t.Fatalf("GET /api/apps: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("GET /api/apps (daemon unreachable) = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
}
