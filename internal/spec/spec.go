// Package spec parses and validates lwd.toml app definitions.
// The parsed App is the source of truth the reconciler acts on.
package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
var serviceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
var secretNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var poolNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// gitRefRe matches a safe git ref charset: no leading '-' (which git or a
// shell could interpret as an option) or '.' (rejects e.g. "..", and refs
// are conventionally not dot-prefixed), and no whitespace/control
// characters. Allows branch names ("feature/x"), tags ("v1.2.3"), and full
// commit SHAs.
var gitRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// gitScpLikeRe matches the scp-like scp syntax git accepts for ssh remotes,
// e.g. "git@github.com:me/app.git" or "user@host:path/to/repo" — no "://"
// scheme, just a user@host:path form.
var gitScpLikeRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+@[A-Za-z0-9_.-]+:[^:].*$`)

// gitAllowedSchemes are the URL schemes lwd permits for a git remote. This
// intentionally excludes command-executing transports like ext:: and fd::,
// which git would otherwise use to run an arbitrary host command supplied
// via the url (the ext:: transport runs its argument through a shell) —
// closing the host-RCE path at the earliest possible gate, before any clone
// is attempted, for both file- and web-generated apps.
var gitAllowedSchemes = map[string]bool{
	"http":  true,
	"https": true,
	"git":   true,
	"ssh":   true,
	"file":  true,
}

// Git describes a git source for building from a repository.
type Git struct {
	URL  string `toml:"url"`
	Ref  string `toml:"ref"`
	Path string `toml:"path"`
}

// Service describes a backing service (e.g., database, cache).
type Service struct {
	Name    string            `toml:"name"`
	Image   string            `toml:"image"`
	Command string            `toml:"command"`
	Env     map[string]string `toml:"env"`
	Secrets []string          `toml:"secrets"`
	Volume  string            `toml:"volume"`
}

// App is a single deployable application as declared in lwd.toml.
type App struct {
	Name   string `toml:"name"`
	Image  string `toml:"image"`
	Domain string `toml:"domain"`
	Port   int    `toml:"port"`
	// Node selects where this app runs: "" means unset — let the scheduler
	// place it; "local" pins it to the local node; any other value pins it
	// to that registered node name. Parse does NOT default "" to "local"
	// (Phase 11a) — resolvers (node.RegistryResolver/FakeResolver) and the
	// compose guard below still treat "" and "local" as equivalent to local.
	Node    string            `toml:"node"`
	Pool    string            `toml:"pool"`
	Env     map[string]string `toml:"env"`
	Secrets []string          `toml:"secrets"`
	Health  Health            `toml:"health"`

	// Replicas is the number of surface replicas to run for this app (Phase
	// 12). Parse defaults an unset (0) value to 1, so a bare struct literal
	// built without going through Parse must set this explicitly to pass
	// Validate. Load-balanced across replicas via the router (N=1 degrades
	// to today's single-container behavior byte-for-byte). Not supported for
	// compose apps.
	Replicas int `toml:"replicas"`

	// Requirements declares resource needs used by the scheduler to pick a
	// node/pool when Node is unset. Nil means no requirements declared.
	Requirements *Requirements `toml:"requirements"`

	// Compose apps
	Compose string `toml:"compose"`
	Service string `toml:"service"`

	// Git apps
	Git *Git `toml:"git"`

	// Backing services
	Services []Service `toml:"services"`

	// Not yet supported — parsed so we can reject them explicitly.
	Build    *Build   `toml:"build"`
	Surfaces []string `toml:"surfaces"`
}

// Health describes how the reconciler decides a container is up.
type Health struct {
	Path       string        `toml:"path"`
	Timeout    time.Duration `toml:"-"`
	RawTimeout string        `toml:"timeout"`
}

// Build describes build-from-source (not yet supported in this plan).
type Build struct {
	Context    string `toml:"context"`
	Dockerfile string `toml:"dockerfile"`
}

// Requirements declares an app's resource needs, used by the scheduler
// (Phase 11a) to pick a node/pool when App.Node is unset ("").
type Requirements struct {
	CPU    float64 `toml:"cpu"`
	Memory string  `toml:"memory"`
}

// sizeRe matches an optional integer/decimal magnitude followed by an
// optional binary-unit suffix (K/M/G/T, optionally with a trailing "i", e.g.
// "512M" or "512Mi"), case-insensitive, with optional surrounding
// whitespace.
var sizeRe = regexp.MustCompile(`(?i)^\s*([0-9]+(?:\.[0-9]+)?)\s*(ki|mi|gi|ti|k|m|g|t)?\s*$`)

// sizeUnitFactor maps a (lowercased) size suffix to its multiplier. lwd uses
// binary units throughout: "K"/"Ki" both mean 1024, "M"/"Mi" both mean
// 1024^2, and so on — there is no decimal (1000-based) interpretation of
// these suffixes.
var sizeUnitFactor = map[string]int64{
	"":   1,
	"k":  1024,
	"ki": 1024,
	"m":  1024 * 1024,
	"mi": 1024 * 1024,
	"g":  1024 * 1024 * 1024,
	"gi": 1024 * 1024 * 1024,
	"t":  1024 * 1024 * 1024 * 1024,
	"ti": 1024 * 1024 * 1024 * 1024,
}

// ParseSize parses a memory-size string like "512M", "2G", "1Ki", or a plain
// byte count like "1024", into a byte count. An empty string means "unset"
// and returns (0, nil). Units are binary (K/Ki/M/Mi/G/Gi/T/Ti all use a 1024
// base, e.g. "1M" == 1048576 bytes) — lwd does not support decimal
// (1000-based) size suffixes. Negative values and unrecognized formats
// return an error.
func ParseSize(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("size %q is invalid: want a number optionally followed by K/M/G/T (or Ki/Mi/Gi/Ti)", s)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("size %q is invalid: %w", s, err)
	}
	factor := sizeUnitFactor[strings.ToLower(m[2])]
	bytes := n * float64(factor)
	if bytes < 0 {
		return 0, fmt.Errorf("size %q is invalid: must not be negative", s)
	}
	return int64(bytes), nil
}

// Parse decodes an lwd.toml document, applies defaults, and returns the App.
func Parse(data []byte) (*App, error) {
	var a App
	if err := toml.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("parse lwd.toml: %w", err)
	}
	if a.Git != nil && a.Git.Ref == "" {
		a.Git.Ref = "main"
	}
	if a.Health.RawTimeout != "" {
		d, err := time.ParseDuration(a.Health.RawTimeout)
		if err != nil {
			return nil, fmt.Errorf("health.timeout %q: %w", a.Health.RawTimeout, err)
		}
		a.Health.Timeout = d
	}
	if a.Health.Timeout == 0 {
		a.Health.Timeout = 30 * time.Second
	}
	if a.Replicas == 0 {
		a.Replicas = 1
	}
	return &a, nil
}

// Load reads and parses <dir>/lwd.toml. For a compose app, a relative
// Compose path is resolved against dir into an absolute path, so that
// daemon-side code (which runs with a different working directory than the
// CLI invocation) can still os.ReadFile it. An already-absolute Compose path
// is left untouched. Single-service apps (Compose == "") are unaffected.
func Load(dir string) (*App, error) {
	path := filepath.Join(dir, "lwd.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	a, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if a.Compose != "" && !filepath.IsAbs(a.Compose) {
		a.Compose = filepath.Join(dir, a.Compose)
	}
	return a, nil
}

// Validate returns an error if the App cannot be deployed by this version.
func (a *App) Validate() error {
	// Name validation applies to all app types
	if a.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !nameRe.MatchString(a.Name) {
		return fmt.Errorf("name %q is invalid: must match [a-zA-Z0-9][a-zA-Z0-9_.-]*", a.Name)
	}

	// Secret names are injected as environment variables (and, for backing
	// services, spliced into an unescapable `${NAME}` compose-interpolation
	// ref) so they must be validated as env-var identifiers.
	for _, name := range a.Secrets {
		if !secretNameRe.MatchString(name) {
			return fmt.Errorf("secret name %q is invalid: must match [A-Za-z_][A-Za-z0-9_]*", name)
		}
	}

	// Surfaces are never supported
	if len(a.Surfaces) > 0 {
		return fmt.Errorf("surfaces are not supported yet")
	}

	// Pool validation applies to all app types
	if a.Pool != "" && !poolNameRe.MatchString(a.Pool) {
		return fmt.Errorf("pool %q is invalid: must match [A-Za-z0-9][A-Za-z0-9_-]*", a.Pool)
	}

	// Requirements validation applies to all app types
	if a.Requirements != nil {
		if a.Requirements.CPU < 0 {
			return fmt.Errorf("requirements.cpu %v is invalid: must not be negative", a.Requirements.CPU)
		}
		if _, err := ParseSize(a.Requirements.Memory); err != nil {
			return fmt.Errorf("requirements.memory: %w", err)
		}
	}

	// Git app validation
	if a.Git != nil {
		if a.Git.URL == "" {
			return fmt.Errorf("git url is required")
		}
		if err := validateGitURL(a.Git.URL); err != nil {
			return err
		}
		if err := validateGitRef(a.Git.Ref); err != nil {
			return err
		}
		if err := validateRelativeNoTraversal("git path", a.Git.Path); err != nil {
			return err
		}
		if a.Build == nil {
			return fmt.Errorf("build is required for git apps")
		}
		if err := validateRelativeNoTraversal("build context", a.Build.Context); err != nil {
			return err
		}
		if err := validateRelativeNoTraversal("build dockerfile", a.Build.Dockerfile); err != nil {
			return err
		}
		if a.Image != "" {
			return fmt.Errorf("cannot mix git and image")
		}
		if a.Compose != "" {
			return fmt.Errorf("cannot mix git and compose")
		}
		if a.Domain == "" {
			return fmt.Errorf("domain is required for git apps")
		}
		if a.Port == 0 {
			return fmt.Errorf("port is required for git apps")
		}
	} else if a.Compose != "" {
		// Compose app validation
		if a.Service == "" {
			return fmt.Errorf("service is required for compose apps")
		}
		if a.Domain == "" {
			return fmt.Errorf("domain is required for compose apps")
		}
		if a.Port == 0 {
			return fmt.Errorf("port is required for compose apps")
		}
		if a.Image != "" {
			return fmt.Errorf("cannot mix compose and image")
		}
		if a.Build != nil {
			return fmt.Errorf("cannot mix compose and build")
		}
		// A compose app's Up/routing (applyCompose) always runs against the
		// controller's own `docker compose` process and never threads a
		// resolved node's DOCKER_HOST through — placing one on a registered
		// remote node would silently deploy it on the controller instead of
		// the node the user asked for. image/git apps ARE supported on remote
		// nodes (via node.Resolver + docker-over-ssh); only the compose-file
		// shape is guarded here. "" (unset/schedule) and "local" are the only
		// node values Validate treats as local — anything else is remote.
		if a.Node != "" && a.Node != "local" {
			return fmt.Errorf("compose apps on remote nodes are not supported yet (place on local, or use image/git apps with [[services]])")
		}
	} else {
		// Single-service app validation
		if a.Build != nil {
			return fmt.Errorf("build-from-source is not supported yet")
		}
		if a.Image == "" {
			return fmt.Errorf("image is required")
		}
		if a.Port == 0 {
			return fmt.Errorf("port is required")
		}
	}

	// Replicas validation applies to all app types, checked after the
	// shape-specific block above so a compose app that's already invalid for
	// another reason (missing service/domain/port, remote node, ...) reports
	// that error rather than a replicas one. Parse defaults an unset (0)
	// Replicas to 1 before Validate normally sees it, but a hand-built App
	// (bypassing Parse — e.g. a JSON-decoded API request) can still reach
	// Validate with Replicas == 0, so 0 is rejected here just like any other
	// value below the floor.
	if a.Replicas < 1 {
		return fmt.Errorf("replicas must be >= 1")
	}
	if a.Replicas > 50 {
		return fmt.Errorf("replicas must be <= 50")
	}
	if a.Replicas > 1 && a.Compose != "" {
		return fmt.Errorf("replicas not supported for compose apps")
	}

	// Services validation (allowed on image and git apps, not on compose)
	if len(a.Services) > 0 {
		if a.Compose != "" {
			return fmt.Errorf("services are not allowed on compose apps")
		}

		// Track service names for uniqueness
		seenNames := make(map[string]bool)

		for _, svc := range a.Services {
			if svc.Name == "" {
				return fmt.Errorf("service name is required")
			}
			if !serviceNameRe.MatchString(svc.Name) {
				return fmt.Errorf("service name %q is invalid: must match [a-z0-9][a-z0-9-]*", svc.Name)
			}
			if seenNames[svc.Name] {
				return fmt.Errorf("duplicate service name %q", svc.Name)
			}
			seenNames[svc.Name] = true

			if svc.Image == "" {
				return fmt.Errorf("service %q requires an image", svc.Name)
			}
			for _, name := range svc.Secrets {
				if !secretNameRe.MatchString(name) {
					return fmt.Errorf("secret name %q is invalid: must match [A-Za-z_][A-Za-z0-9_]*", name)
				}
			}
		}
	}

	return nil
}

// validateGitURL rejects git remote URLs that could reach a command-executing
// git transport (ext::, fd::) or be parsed as a git command-line option
// instead of a positional URL argument. This is the gate for BOTH
// file-authored lwd.toml apps and web-generated ones, and runs before any
// clone is attempted.
//
// It stays permissive for real remotes: standard scheme:// URLs with an
// allowed scheme (http, https, git, ssh, file), and scp-like ssh syntax
// (user@host:path, e.g. "git@github.com:me/app.git") are both accepted.
func validateGitURL(url string) error {
	if strings.HasPrefix(url, "-") {
		return fmt.Errorf("git url %q is invalid: must not start with -", url)
	}

	if i := strings.Index(url, "://"); i >= 0 {
		scheme := url[:i]
		if !gitAllowedSchemes[strings.ToLower(scheme)] {
			return fmt.Errorf("git url %q is invalid: unsupported scheme %q (allowed: http, https, git, ssh, file, or scp-like user@host:path)", url, scheme)
		}
		return nil
	}

	// No "://" scheme. Reject any other "::"-style transport prefix (e.g.
	// ext::sh -c ..., fd::5) outright: these run arbitrary host commands or
	// read from arbitrary file descriptors rather than fetching from a
	// remote, and have no legitimate use in an lwd.toml.
	if strings.Contains(url, "::") {
		return fmt.Errorf("git url %q is invalid: command-executing transports are not allowed", url)
	}

	// Otherwise the only other form git accepts is scp-like ssh syntax
	// (user@host:path) or a plain local filesystem path. Plain local paths
	// are not needed by lwd (use file:// instead) and are rejected here to
	// keep the allowed surface small and explicit.
	if gitScpLikeRe.MatchString(url) {
		return nil
	}

	return fmt.Errorf("git url %q is invalid: must be a scheme://... URL (http, https, git, ssh, file) or scp-like user@host:path", url)
}

// validateGitRef rejects a git ref (branch, tag, or commit SHA) that doesn't
// match a safe charset — no leading '-' (option injection) or leading '.',
// and no whitespace/control/shell-metacharacters. This is checked before ref
// ever reaches `git clone --branch` or `git checkout`. An empty ref is
// accepted here (it means "unset"; Parse defaults it to "main" — Validate
// may also run directly against an App built without going through Parse).
func validateGitRef(ref string) error {
	if ref == "" {
		return nil
	}
	if !gitRefRe.MatchString(ref) {
		return fmt.Errorf("git ref %q is invalid: must match [A-Za-z0-9][A-Za-z0-9._/-]*", ref)
	}
	return nil
}

// validateRelativeNoTraversal rejects a path (git.path, build.context, or
// build.dockerfile) that is absolute or escapes the directory it's
// interpreted relative to (a git clone's root), via a ".." path segment.
// label identifies the field in the returned error. An empty path is
// accepted (it means "unset"; callers apply their own defaults).
func validateRelativeNoTraversal(label, path string) error {
	if path == "" {
		return nil
	}
	if filepath.IsAbs(path) {
		return fmt.Errorf("%s %q is invalid: must be a relative path", label, path)
	}
	cleaned := filepath.ToSlash(filepath.Clean(path))
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == ".." {
			return fmt.Errorf("%s %q is invalid: must not escape the clone root (no .. segments)", label, path)
		}
	}
	return nil
}
