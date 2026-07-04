// Package spec parses and validates lwd.toml app definitions.
// The parsed App is the source of truth the reconciler acts on.
package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/BurntSushi/toml"
)

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
var serviceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

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

	// Surfaces are never supported
	if len(a.Surfaces) > 0 {
		return fmt.Errorf("surfaces are not supported yet")
	}

	// Git app validation
	if a.Git != nil {
		if a.Git.URL == "" {
			return fmt.Errorf("git url is required")
		}
		if a.Build == nil {
			return fmt.Errorf("build is required for git apps")
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
		}
	}

	return nil
}
