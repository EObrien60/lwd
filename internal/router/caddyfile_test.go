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
}

func TestGenerateCaddyfileEmpty(t *testing.T) {
	out := GenerateCaddyfile("127.0.0.1:2019", nil)
	if !strings.Contains(out, "admin 127.0.0.1:2019") {
		t.Error("empty config must still set admin")
	}
}
