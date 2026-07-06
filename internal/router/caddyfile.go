// Package router owns lwd's reverse proxy: it generates the Caddyfile and manages
// the Caddy container that fronts all apps (TLS + domain routing). It holds no app
// logic beyond translating routes into Caddy config.
package router

import (
	"fmt"
	"sort"
	"strings"
)

// Upstream is a single reverse_proxy target: a container name on the shared
// lwd network for a local surface (Caddy resolves it by Docker DNS), or a
// remote node's WireGuard mesh IP for a surface placed via node= (Caddy,
// running on the controller, reaches it over the mesh instead — see
// internal/reconciler's deployReplicaSet).
type Upstream struct {
	Host string
	Port int
}

// Route is one active domain -> upstream-set mapping. A single-element
// Upstreams reproduces today's one-container-per-domain behavior; more than
// one enables Caddy round-robin load balancing across replicas.
type Route struct {
	Domain      string
	Upstreams   []Upstream
	TLSInternal bool // use Caddy self-signed certs (local/non-public domains)
}

// GenerateCaddyfile renders a deterministic Caddyfile for the given routes.
func GenerateCaddyfile(adminAddr string, routes []Route) string {
	var b strings.Builder
	fmt.Fprintf(&b, "{\n\tadmin %s\n}\n\n", adminAddr)

	sorted := append([]Route(nil), routes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Domain < sorted[j].Domain })

	for _, r := range sorted {
		if len(r.Upstreams) == 0 {
			// A live route should always carry at least one upstream; skip
			// rather than emit a broken/empty reverse_proxy block.
			continue
		}

		// Every route is bound to BOTH the plain-http and https addresses for
		// its domain. Caddy's automatic HTTPS only auto-generates an
		// HTTP->HTTPS redirect for a domain when nothing else is explicitly
		// bound to it on port 80; listing "http://domain" ourselves claims
		// that slot so the reverse_proxy handler serves plain HTTP directly
		// (no redirect) on :80, in addition to TLS on :443 — required so
		// lwd's health probes and the CLI/API's "through Caddy" checks, which
		// all talk plain HTTP to 127.0.0.1:80 with a Host header, see the
		// app's real response instead of a 3xx redirect to a host that has
		// no DNS entry.
		fmt.Fprintf(&b, "http://%s, https://%s {\n", r.Domain, r.Domain)
		if r.TLSInternal {
			b.WriteString("\ttls internal\n")
		}
		if len(r.Upstreams) == 1 {
			u := r.Upstreams[0]
			fmt.Fprintf(&b, "\treverse_proxy %s:%d\n", u.Host, u.Port)
		} else {
			targets := make([]string, len(r.Upstreams))
			for i, u := range r.Upstreams {
				targets[i] = fmt.Sprintf("%s:%d", u.Host, u.Port)
			}
			b.WriteString("\treverse_proxy {\n")
			fmt.Fprintf(&b, "\t\tto %s\n", strings.Join(targets, " "))
			b.WriteString("\t\tlb_policy round_robin\n")
			b.WriteString("\t\tfail_duration 30s\n")
			b.WriteString("\t}\n")
		}
		b.WriteString("}\n\n")
	}
	return b.String()
}

// UseInternalTLS reports whether a domain should use Caddy's self-signed certs
// rather than public ACME (local dev, .localhost, or bare hostnames).
func UseInternalTLS(domain string) bool {
	if domain == "" {
		return true
	}
	if strings.HasSuffix(domain, ".localhost") || domain == "localhost" {
		return true
	}
	// No dot => not a public FQDN (e.g. "myapp"); treat as internal.
	return !strings.Contains(domain, ".")
}
