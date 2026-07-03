package router

import (
	"strings"
	"testing"
)

func TestGenerateCaddyfileSortedWithTLS(t *testing.T) {
	out := GenerateCaddyfile("127.0.0.1:2019", []Route{
		{Domain: "b.example.com", Upstream: "lwd-b-2", Port: 8080},
		{Domain: "a.localhost", Upstream: "lwd-a-1", Port: 3000, TLSInternal: true},
	})
	if !strings.Contains(out, "admin 127.0.0.1:2019") {
		t.Error("missing admin directive")
	}
	// deterministic order: a.localhost block appears before b.example.com
	ai := strings.Index(out, "a.localhost")
	bi := strings.Index(out, "b.example.com")
	if ai == -1 || bi == -1 || ai > bi {
		t.Fatalf("blocks not sorted: a=%d b=%d\n%s", ai, bi, out)
	}
	if !strings.Contains(out, "reverse_proxy lwd-b-2:8080") {
		t.Error("missing reverse_proxy for b")
	}
	if !strings.Contains(out, "tls internal") {
		t.Error("a.localhost should use internal TLS")
	}
	// Every route must claim BOTH the plain-http and https addresses for its
	// domain (not a bare-domain block), so Caddy's automatic HTTP->HTTPS
	// redirect never kicks in — see the comment in GenerateCaddyfile. A
	// regression to a bare "b.example.com {" block would silently reintroduce
	// the redirect and break plain-HTTP health probes; this substring check
	// catches that here, without needing the Docker-gated e2e test.
	if !strings.Contains(out, "http://b.example.com, https://b.example.com {") {
		t.Error("b.example.com route must bind both http:// and https:// addresses")
	}
	if !strings.Contains(out, "http://a.localhost, https://a.localhost {") {
		t.Error("a.localhost route must bind both http:// and https:// addresses")
	}
}

func TestGenerateCaddyfileEmpty(t *testing.T) {
	out := GenerateCaddyfile("127.0.0.1:2019", nil)
	if !strings.Contains(out, "admin 127.0.0.1:2019") {
		t.Error("empty config must still set admin")
	}
}
