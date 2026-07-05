// Package router owns lwd's reverse proxy: it generates the Caddyfile and manages
// the Caddy container that fronts all apps (TLS + domain routing). It holds no app
// logic beyond translating routes into Caddy config.
package router

import (
	"fmt"
	"sort"
	"strings"
)

// Route is one active domain -> container mapping.
type Route struct {
	Domain string
	// Upstream is the reverse_proxy target host: a container name on the
	// shared lwd network for a local surface (Caddy resolves it by Docker
	// DNS), or a remote node's WireGuard mesh IP for a surface placed via
	// node= (Caddy, running on the controller, reaches it over the mesh
	// instead — see internal/reconciler's deployBlueGreenSurface).
	Upstream    string
	Port        int
	TLSInternal bool // use Caddy self-signed certs (local/non-public domains)
}

// GenerateCaddyfile renders a deterministic Caddyfile for the given routes.
func GenerateCaddyfile(adminAddr string, routes []Route) string {
	var b strings.Builder
	fmt.Fprintf(&b, "{\n\tadmin %s\n}\n\n", adminAddr)

	sorted := append([]Route(nil), routes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Domain < sorted[j].Domain })

	for _, r := range sorted {
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
		fmt.Fprintf(&b, "\treverse_proxy %s:%d\n", r.Upstream, r.Port)
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
