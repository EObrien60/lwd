// Package bundle defines the contract between the lwd controller and an lwd
// node: everything a node needs to make one deployment of one app-environment
// exist, and the result it reports back. The node never consults the
// controller's database; the bundle is self-contained.
package bundle

import (
	"fmt"
	"regexp"
	"strings"
)

// TLS modes for an environment's domains.
const (
	TLSACME     = "acme"     // public certificates via Let's Encrypt/ZeroSSL
	TLSInternal = "internal" // Caddy's local CA (private hosts, M1 VM)
)

// Deployment statuses reported by a node.
const (
	StatusSucceeded = "succeeded" // new deployment is live and passed smoke
	StatusFailed    = "failed"    // failed before going live; previous untouched
	StatusReverted  = "reverted"  // went live, failed smoke, previous restored
)

// Bundle is one deployment of one app-environment onto one node.
type Bundle struct {
	App        string            `json:"app"`
	Env        string            `json:"env"`
	Release    int64             `json:"release"`
	Deployment int64             `json:"deployment"` // unique per deploy attempt; names the compose project
	Services   []Service         `json:"services"`
	Migrate    *Job              `json:"migrate,omitempty"`
	Vars       map[string]string `json:"vars"` // plain env + resolved secrets, injected into every service
	TLS        string            `json:"tls"`  // TLSACME or TLSInternal
	// Platform attaches every service and the migrate job to the external
	// docker network PlatformNetwork, in addition to the project's default
	// network, so they can reach the host's platform services
	// (lwd-postgres, lwd-s3). Set when the app declares a resource.
	Platform bool `json:"platform,omitempty"`

	ReadyTimeoutSeconds int `json:"ready_timeout_seconds"` // per-service readiness budget before going live
	SmokeSeconds        int `json:"smoke_seconds"`         // post-cutover window checked through Caddy
}

// Service is one container in the deployment. A service with Port > 0 is an
// HTTP service: it is readiness-gated and blue-green switched. A service with
// no port (a worker) is simply started and must stay running.
type Service struct {
	Name    string   `json:"name"`
	Image   string   `json:"image"` // must be digest-pinned: repo@sha256:...
	Command []string `json:"command,omitempty"`
	Port    int      `json:"port,omitempty"`
	Domains []string `json:"domains,omitempty"` // fully qualified; empty = not routed
	Ready   string   `json:"ready,omitempty"`   // readiness path, e.g. "/ready"; default "/"
}

// Job is a one-off command run to completion before any candidate starts.
type Job struct {
	Service string   `json:"service"` // whose image (and vars) to run with
	Command []string `json:"command"`
}

// Result is what a node reports after a deploy attempt.
type Result struct {
	Status  string  `json:"status"`            // StatusSucceeded | StatusFailed | StatusReverted
	Phase   string  `json:"phase"`             // phase reached or failed in: pull, migrate, start, ready, cutover, smoke, done
	Message string  `json:"message,omitempty"` // human-readable cause on failure
	Events  []Event `json:"events"`            // ordered steps taken
	Logs    string  `json:"logs,omitempty"`    // log excerpt retained on failure
}

// Event is one timestamped step inside a deploy.
type Event struct {
	At      string `json:"at"` // RFC3339
	Phase   string `json:"phase"`
	Message string `json:"message"`
}

var (
	nameRE   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	digestRE = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)
)

// ProjectName is the compose project name for this deployment.
func (b *Bundle) ProjectName() string {
	return fmt.Sprintf("lwd-%s-%s-d%d", b.App, b.Env, b.Deployment)
}

// Validate rejects bundles a node must not execute.
func (b *Bundle) Validate() error {
	if !nameRE.MatchString(b.App) || !nameRE.MatchString(b.Env) {
		return fmt.Errorf("invalid app/env name %q/%q", b.App, b.Env)
	}
	if b.Release <= 0 || b.Deployment <= 0 {
		return fmt.Errorf("release and deployment ids are required")
	}
	if b.TLS != TLSACME && b.TLS != TLSInternal {
		return fmt.Errorf("tls must be %q or %q", TLSACME, TLSInternal)
	}
	if len(b.Services) == 0 {
		return fmt.Errorf("bundle has no services")
	}
	seen := map[string]bool{}
	domains := map[string]bool{}
	for _, s := range b.Services {
		if !nameRE.MatchString(s.Name) {
			return fmt.Errorf("invalid service name %q", s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("duplicate service %q", s.Name)
		}
		seen[s.Name] = true
		if !digestRE.MatchString(s.Image) {
			return fmt.Errorf("service %s: image %q is not digest-pinned", s.Name, s.Image)
		}
		if s.Port < 0 || s.Port > 65535 {
			return fmt.Errorf("service %s: bad port %d", s.Name, s.Port)
		}
		if len(s.Domains) > 0 && s.Port == 0 {
			return fmt.Errorf("service %s: domains require a port", s.Name)
		}
		for _, d := range s.Domains {
			d = strings.ToLower(d)
			if domains[d] {
				return fmt.Errorf("domain %s used twice", d)
			}
			domains[d] = true
		}
	}
	if b.Migrate != nil {
		if !seen[b.Migrate.Service] || len(b.Migrate.Command) == 0 {
			return fmt.Errorf("migrate must name an existing service and a command")
		}
	}
	return nil
}
