package router

import (
	"strings"
	"testing"
)

func TestGenerateCaddyfileSortedWithTLS(t *testing.T) {
	out := GenerateCaddyfile("127.0.0.1:2019", []Route{
		{Domain: "b.example.com", Upstreams: []Upstream{{Host: "lwd-b-2", Port: 8080}}},
		{Domain: "a.localhost", Upstreams: []Upstream{{Host: "lwd-a-1", Port: 3000}}, TLSInternal: true},
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

// singleUpstreamGolden is the exact expected output for a single-domain,
// single-upstream, TLS-internal route. It is the non-regression anchor for
// Phase 12's multi-upstream generalization: a 1-element Upstreams slice must
// render byte-identical to what a bare Upstream/Port pair rendered before
// this change.
const singleUpstreamGolden = "{\n\tadmin 127.0.0.1:2019\n}\n\nhttp://a.localhost, https://a.localhost {\n\ttls internal\n\treverse_proxy lwd-a-1:3000\n}\n\n"

func TestGenerateCaddyfileSingleUpstreamUnchanged(t *testing.T) {
	out := GenerateCaddyfile("127.0.0.1:2019", []Route{
		{Domain: "a.localhost", Upstreams: []Upstream{{Host: "lwd-a-1", Port: 3000}}, TLSInternal: true},
	})
	if out != singleUpstreamGolden {
		t.Fatalf("single-upstream output changed (non-regression break):\ngot:\n%q\nwant:\n%q", out, singleUpstreamGolden)
	}
}

func TestGenerateCaddyfileMultiUpstreamRoundRobin(t *testing.T) {
	out := GenerateCaddyfile("127.0.0.1:2019", []Route{
		{
			Domain: "app.example.com",
			Upstreams: []Upstream{
				{Host: "h1", Port: 1001},
				{Host: "h2", Port: 1002},
				{Host: "h3", Port: 1003},
			},
		},
	})
	if !strings.Contains(out, "to h1:1001 h2:1002 h3:1003") {
		t.Errorf("missing 'to' directive with all upstreams:\n%s", out)
	}
	if !strings.Contains(out, "lb_policy round_robin") {
		t.Errorf("missing lb_policy round_robin:\n%s", out)
	}
	if !strings.Contains(out, "fail_duration 30s") {
		t.Errorf("missing fail_duration 30s:\n%s", out)
	}
	// Must not fall into the single-upstream one-liner form.
	if strings.Contains(out, "reverse_proxy h1:1001\n") {
		t.Errorf("multi-upstream route rendered as single-upstream one-liner:\n%s", out)
	}
}
