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

// Load reads and parses <dir>/lwd.toml.
func Load(dir string) (*App, error) {
	path := filepath.Join(dir, "lwd.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(data)
}

// Validate returns an error if the App cannot be deployed by this version.
func (a *App) Validate() error {
	// Name validation applies to both compose and single-service apps
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

	// Compose app validation
	if a.Compose != "" {
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
		return nil
	}

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
	return nil
}
