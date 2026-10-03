package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("LWD_NODE_TOKEN", "")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("missing token accepted")
	}
	t.Setenv("LWD_NODE_TOKEN", "tok")
	t.Setenv("LWD_NODE_ADDR", "")
	t.Setenv("LWD_NODE_DIR", "")
	t.Setenv("LWD_CADDY_IMAGE", "")
	t.Setenv("LWD_ACME_EMAIL", "")
	c, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c != (Config{Addr: "0.0.0.0:7480", Token: "tok", Dir: "/srv/lwd", CaddyImage: "caddy:2"}) {
		t.Errorf("defaults = %+v", c)
	}
	t.Setenv("LWD_ACME_EMAIL", "ops@example.com\n}")
	if _, err := ConfigFromEnv(); err == nil {
		t.Error("injectable email accepted")
	}
	t.Setenv("LWD_ACME_EMAIL", "")
	t.Setenv("LWD_NODE_DIR", "relative")
	if _, err := ConfigFromEnv(); err == nil {
		t.Error("relative dir accepted")
	}
}

func TestMainRequiresToken(t *testing.T) {
	t.Setenv("LWD_NODE_TOKEN", "")
	if err := Main(nil); err == nil || !strings.Contains(err.Error(), "LWD_NODE_TOKEN") {
		t.Fatalf("err = %v", err)
	}
}

func TestStartRebuildsFromPersistedState(t *testing.T) {
	h := newHarness(t)
	h.n.cfg.ACMEEmail = "ops@example.com"
	h.deploy(bundleN(1))
	want := h.caddy.last()

	// A fresh process over the same dir: lose the Caddyfile, keep state.
	os.Remove(filepath.Join(h.dir, "caddy", "etc", "Caddyfile"))
	fresh := newHarness(t)
	n := fresh.n
	n.cfg.Dir, fresh.dir = h.dir, h.dir
	n.cfg.ACMEEmail = "ops@example.com"
	n.caddy.Path = filepath.Join(h.dir, "caddy", "etc", "Caddyfile")

	for i := 0; i < 2; i++ { // idempotent
		if err := n.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := fresh.caddy.last(); got != want {
			t.Fatalf("start %d loaded:\n%s\nwant:\n%s", i, got, want)
		}
		onDisk, _ := os.ReadFile(n.caddy.Path)
		if string(onDisk) != want {
			t.Fatalf("persisted Caddyfile:\n%s", onDisk)
		}
	}
	if !strings.Contains(want, "email ops@example.com") || !strings.Contains(want, "reverse_proxy 127.0.0.1:20000") {
		t.Errorf("unexpected Caddyfile:\n%s", want)
	}
	sys, err := os.ReadFile(filepath.Join(h.dir, "system", composeFile))
	if err != nil || !strings.Contains(string(sys), `"network_mode": "host"`) {
		t.Fatalf("system compose: %v\n%s", err, sys)
	}
	calls := fresh.docker.recorded()
	if len(calls) != 2 || calls[0] != systemProject+" up -d" {
		t.Errorf("calls = %q", calls)
	}
	// The rebuilt routing table is live: a new deploy keeps the old app's
	// ports reserved and retires its project.
	fresh.deploy(bundleN(2))
	if p := fresh.state().Live.Ports; p["web"] != 20002 {
		t.Errorf("ports after restart = %v", p)
	}
}

func TestStartFailsWhenCaddyDoesNotStart(t *testing.T) {
	h := newHarness(t)
	h.docker.respond = func(c call) (string, error, bool) {
		if c.Project == systemProject && c.Verb == "up" {
			return "", errors.New("image not found"), true
		}
		return "", nil, false
	}
	if err := h.n.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "image not found") {
		t.Fatalf("err = %v", err)
	}
}
