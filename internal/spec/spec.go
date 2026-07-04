// Package spec parses and validates lwd.toml app definitions.
// The parsed App is the source of truth the reconciler acts on.
package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
var serviceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
var secretNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
	Name    string            `toml:"name"`
	Image   string            `toml:"image"`
	Domain  string            `toml:"domain"`
	Port    int               `toml:"port"`
	Node    string            `toml:"node"`
	Env     map[string]string `toml:"env"`
	Secrets []string          `toml:"secrets"`
	Health  Health            `toml:"health"`

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

// Parse decodes an lwd.toml document, applies defaults, and returns the App.
func Parse(data []byte) (*App, error) {
	var a App
	if err := toml.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("parse lwd.toml: %w", err)
	}
	if a.Node == "" {
		a.Node = "local"
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
