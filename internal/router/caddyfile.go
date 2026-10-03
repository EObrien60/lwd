// Package router owns a node's reverse proxy: it renders the Caddyfile for the
// node's live routes and applies it to the local Caddy through its admin API.
// It holds no deployment logic beyond translating routes into Caddy config.
package router

import (
	"regexp"
	"sort"
	"strings"
)

// Global holds the Caddyfile's global options block.
type Global struct {
	Admin string // admin listener, e.g. 127.0.0.1:2019; never reachable from containers
	Email string // optional ACME account email
	// DNS selects the ACME DNS-01 provider for every certificate on this host
	// ("" = Caddy's default HTTP/TLS-ALPN challenges). Private hosts that the
	// internet cannot reach need it to get publicly trusted certificates.
	DNS string
}

// DNS-01 providers a node may use; each needs the matching Caddy plugin and
// reads its credential from the Caddy container's environment.
var dnsProviders = map[string]string{
	"cloudflare": "cloudflare {env.CLOUDFLARE_API_TOKEN}",
}

// ValidDNSProvider reports whether p is a supported DNS-01 provider name.
func ValidDNSProvider(p string) bool { _, ok := dnsProviders[p]; return ok }

// Route sends one domain to one upstream.
type Route struct {
	Domain      string // lower-case FQDN, see ValidDomain
	Upstream    string // host:port, normally 127.0.0.1:<hostport>
	TLSInternal bool   // use Caddy's local CA instead of ACME
}

// GenerateCaddyfile renders a deterministic Caddyfile: routes are sorted by
// domain so identical route sets produce byte-identical files.
//
// Site addresses are bare domains (no scheme) on purpose: that is what makes
// Caddy's automatic HTTPS serve the site on :443 and redirect :80 to it.
// Callers must only pass domains accepted by ValidDomain; the Caddyfile has no
// quoting, so anything else could inject directives.
func GenerateCaddyfile(g Global, routes []Route) string {
	var b strings.Builder
	b.WriteString("{\n\tadmin " + g.Admin + "\n")
	if g.Email != "" {
		b.WriteString("\temail " + g.Email + "\n")
	}
	if line, ok := dnsProviders[g.DNS]; ok {
		b.WriteString("\tacme_dns " + line + "\n")
	}
	b.WriteString("}\n")

	sorted := append([]Route(nil), routes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Domain < sorted[j].Domain })
	for _, r := range sorted {
		b.WriteString("\n" + r.Domain + " {\n")
		if r.TLSInternal {
			b.WriteString("\ttls internal\n")
		}
		b.WriteString("\treverse_proxy " + r.Upstream + "\n}\n")
	}
	return b.String()
}

var labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidDomain reports whether d is a lower-case DNS name safe to place in a
// Caddyfile site address: dot-separated labels of [a-z0-9-], no wildcards,
// schemes, ports or whitespace.
func ValidDomain(d string) bool {
	if d == "" || len(d) > 253 {
		return false
	}
	for _, label := range strings.Split(d, ".") {
		if !labelRE.MatchString(label) {
			return false
		}
	}
	return true
}

var emailRE = regexp.MustCompile(`^[^\s{}"@]+@[^\s{}"@]+$`)

// ValidEmail reports whether e is safe to place in the global email option.
func ValidEmail(e string) bool { return emailRE.MatchString(e) }
