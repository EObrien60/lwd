package router

import (
	"strings"
	"testing"
)

const golden = `{
	admin 127.0.0.1:2019
	email ops@example.com
}

a.example.com {
	reverse_proxy 127.0.0.1:20001
}

b.lwd.internal {
	tls internal
	reverse_proxy 127.0.0.1:20000
}
`

func TestGenerateCaddyfileGolden(t *testing.T) {
	out := GenerateCaddyfile(Global{Admin: "127.0.0.1:2019", Email: "ops@example.com"}, []Route{
		{Domain: "b.lwd.internal", Upstream: "127.0.0.1:20000", TLSInternal: true},
		{Domain: "a.example.com", Upstream: "127.0.0.1:20001"},
	})
	if out != golden {
		t.Fatalf("got:\n%s\nwant:\n%s", out, golden)
	}
}

func TestGenerateCaddyfileNoRoutesNoEmail(t *testing.T) {
	out := GenerateCaddyfile(Global{Admin: "127.0.0.1:2019"}, nil)
	if out != "{\n\tadmin 127.0.0.1:2019\n}\n" {
		t.Fatalf("got %q", out)
	}
}

func TestGenerateCaddyfileNoPlainHTTPAddress(t *testing.T) {
	// A bare domain address is what makes Caddy redirect HTTP to HTTPS.
	out := GenerateCaddyfile(Global{Admin: "127.0.0.1:2019"}, []Route{{Domain: "x.example.com", Upstream: "127.0.0.1:20000"}})
	if strings.Contains(out, "http://") {
		t.Fatalf("site address must not carry a scheme:\n%s", out)
	}
}

func TestGenerateCaddyfileDeterministic(t *testing.T) {
	routes := []Route{
		{Domain: "c.example.com", Upstream: "127.0.0.1:3"},
		{Domain: "a.example.com", Upstream: "127.0.0.1:1"},
		{Domain: "b.example.com", Upstream: "127.0.0.1:2"},
	}
	first := GenerateCaddyfile(Global{Admin: "a"}, routes)
	routes[0], routes[2] = routes[2], routes[0]
	if second := GenerateCaddyfile(Global{Admin: "a"}, routes); first != second {
		t.Fatalf("output depends on input order:\n%s\n---\n%s", first, second)
	}
}

func TestValidDomain(t *testing.T) {
	for _, d := range []string{"example.com", "a-b.c1.lwd.internal", "localhost", "x.y"} {
		if !ValidDomain(d) {
			t.Errorf("%q should be valid", d)
		}
	}
	for _, d := range []string{"", "Example.com", "a..b", "-a.com", "a-.com", "a.com {", "a.com\n}", "*.a.com", "http://a.com", "a.com:443", strings.Repeat("a", 64) + ".com"} {
		if ValidDomain(d) {
			t.Errorf("%q should be invalid", d)
		}
	}
}

func TestGenerateCaddyfileDNSProvider(t *testing.T) {
	got := GenerateCaddyfile(Global{Admin: AdminAddr, DNS: "cloudflare"}, []Route{{Domain: "app.example.com", Upstream: "127.0.0.1:20000"}})
	if !strings.Contains(got, "\tacme_dns cloudflare {env.CLOUDFLARE_API_TOKEN}\n") {
		t.Fatalf("missing acme_dns:\n%s", got)
	}
	if strings.Contains(GenerateCaddyfile(Global{Admin: AdminAddr, DNS: "bogus"}, nil), "acme_dns") {
		t.Fatal("unknown provider must not be emitted")
	}
}
