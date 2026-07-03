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
	Domain      string
	Upstream    string // container name on the lwd network
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
		fmt.Fprintf(&b, "%s {\n", r.Domain)
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
