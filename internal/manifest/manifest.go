// Package manifest parses and validates lwd.toml, the per-application
// declaration of services, migrations and environments, and resolves it into
// the per-environment service list a deployment bundle is built from.
//
// The [env] table holds both plain variables and environments, told apart by
// type: string values are variables, tables are environments. DESIGN.md's
// `env = { LOG_LEVEL = "info" }` followed by `[env.staging]` is accepted (our
// TOML parser tolerates extending the inline table), but strict TOML tools
// reject that form, so prefer:
//
//	[env]
//	LOG_LEVEL = "info"
//
//	[env.staging]
//	host = "m1"
//	domain = "hello.m1.lwd.internal"
package manifest

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/google/go-containerregistry/pkg/name"

	"lwd/internal/bundle"
)

// Service is one declared service. Image is a repository only; the tag is
// chosen and pinned to a digest when a release is created.
type Service struct {
	Image   string
	Command []string
	Port    int
	Domain  string // "@" or a single DNS label; expanded per environment
	Ready   string // readiness path; defaults to "/" when Port > 0
}

// Job is the optional migration command run before candidates start.
type Job struct {
	Service string
	Command []string
}

// Environment is where and how one environment of the app runs.
type Environment struct {
	Host   string
	Domain string
	TLS    string            // bundle.TLSACME (default) or bundle.TLSInternal
	Vars   map[string]string // overrides the manifest-wide Vars
}

// Manifest is a validated lwd.toml.
type Manifest struct {
	Name         string
	Vars         map[string]string // plain variables for every service and environment
	Secrets      []string          // keys that must be set per environment before deploy
	Services     map[string]Service
	Migrate      *Job
	Environments map[string]Environment
}

// Resolved is a manifest flattened for one environment. Service images are
// still bare repositories; the controller substitutes release digests.
type Resolved struct {
	Env      string
	Host     string
	Domain   string
	TLS      string
	Vars     map[string]string
	Services []bundle.Service // sorted by name
	Migrate  *bundle.Job
}

// Error lists every problem found in a manifest, each prefixed with the
// field path it concerns, so one `lwd app apply` shows them all at once.
type Error struct {
	Problems []string
}

func (e *Error) Error() string {
	return "invalid lwd.toml:\n  " + strings.Join(e.Problems, "\n  ")
}

var (
	nameRE  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	varRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

type rawService struct {
	Image   string   `toml:"image"`
	Command []string `toml:"command"`
	Port    int      `toml:"port"`
	Domain  string   `toml:"domain"`
	Ready   string   `toml:"ready"`
}

type rawJob struct {
	Service string   `toml:"service"`
	Command []string `toml:"command"`
}

type rawEnvironment struct {
	Host   string         `toml:"host"`
	Domain string         `toml:"domain"`
	TLS    string         `toml:"tls"`
	Env    map[string]any `toml:"env"`
}

type rawManifest struct {
	Name     string                    `toml:"name"`
	Env      map[string]toml.Primitive `toml:"env"`
	Secrets  []string                  `toml:"secrets"`
	Services map[string]rawService     `toml:"services"`
	Migrate  *rawJob                   `toml:"migrate"`
	// Reserved for M2: decoded only so we can say so instead of "unknown key".
	Database toml.Primitive `toml:"database"`
	Storage  toml.Primitive `toml:"storage"`
}

// Parse decodes and validates lwd.toml.
func Parse(data []byte) (*Manifest, error) {
	var raw rawManifest
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return nil, fmt.Errorf("invalid lwd.toml: %w", err)
	}

	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }

	m := &Manifest{
		Name:         raw.Name,
		Vars:         map[string]string{},
		Services:     map[string]Service{},
		Environments: map[string]Environment{},
	}

	for _, k := range []string{"database", "storage"} {
		if md.IsDefined(k) {
			add("%s: not supported until M2", k)
		}
	}

	switch {
	case raw.Name == "":
		add("name: required")
	case !nameRE.MatchString(raw.Name):
		add("name: %q must match %s", raw.Name, nameRE)
	}

	seenSecret := map[string]bool{}
	for i, s := range raw.Secrets {
		switch {
		case !varRE.MatchString(s):
			add("secrets[%d]: invalid variable name %q", i, s)
		case seenSecret[s]:
			add("secrets[%d]: duplicate %q", i, s)
		}
		seenSecret[s] = true
		m.Secrets = append(m.Secrets, s)
	}

	if len(raw.Services) == 0 {
		add("services: at least one service is required")
	}
	domainOwner := map[string]string{}
	for _, sn := range slices.Sorted(maps.Keys(raw.Services)) {
		rs := raw.Services[sn]
		path := "services." + sn
		if !nameRE.MatchString(sn) {
			add("%s: invalid name, must match %s", path, nameRE)
		}
		if msg := checkImage(rs.Image); msg != "" {
			add("%s.image: %s", path, msg)
		}
		if rs.Port < 0 || rs.Port > 65535 {
			add("%s.port: %d out of range 1-65535", path, rs.Port)
		}
		for i, c := range rs.Command {
			if c == "" {
				add("%s.command[%d]: empty argument", path, i)
			}
		}
		if rs.Domain != "" {
			switch {
			case rs.Port == 0:
				add("%s.domain: requires port", path)
			case rs.Domain != "@" && !labelRE.MatchString(rs.Domain):
				add("%s.domain: must be \"@\" or a single DNS label, got %q", path, rs.Domain)
			case domainOwner[rs.Domain] != "":
				add("%s.domain: %q already used by services.%s", path, rs.Domain, domainOwner[rs.Domain])
			default:
				domainOwner[rs.Domain] = sn
			}
		}
		ready := rs.Ready
		if ready != "" && rs.Port == 0 {
			add("%s.ready: requires port (workers are checked by running state)", path)
		} else if ready != "" && !strings.HasPrefix(ready, "/") {
			add("%s.ready: must start with /, got %q", path, ready)
		}
		if ready == "" && rs.Port > 0 {
			ready = "/"
		}
		m.Services[sn] = Service{Image: rs.Image, Command: rs.Command, Port: rs.Port, Domain: rs.Domain, Ready: ready}
	}

	if raw.Migrate != nil {
		if _, ok := raw.Services[raw.Migrate.Service]; !ok {
			add("migrate.service: unknown service %q", raw.Migrate.Service)
		}
		if len(raw.Migrate.Command) == 0 {
			add("migrate.command: required")
		}
		m.Migrate = &Job{Service: raw.Migrate.Service, Command: raw.Migrate.Command}
	}

	// [env] mixes plain variables (strings) and environments (tables).
	for _, k := range slices.Sorted(maps.Keys(raw.Env)) {
		path := "env." + k
		switch md.Type("env", k) {
		case "String":
			var v string
			if err := md.PrimitiveDecode(raw.Env[k], &v); err != nil {
				add("%s: %v", path, err)
				continue
			}
			if !varRE.MatchString(k) {
				add("%s: invalid variable name", path)
			}
			m.Vars[k] = v
		case "Hash":
			var re rawEnvironment
			if err := md.PrimitiveDecode(raw.Env[k], &re); err != nil {
				add("%s: %v", path, err)
				continue
			}
			m.Environments[k] = checkEnvironment(path, k, re, add)
		default:
			add("%s: must be a string (variable) or a table (environment)", path)
		}
	}
	if len(m.Environments) == 0 {
		add("env: at least one environment ([env.<name>] with host and domain) is required")
	}

	for _, k := range md.Undecoded() {
		ks := k.String()
		if k[0] == "database" || k[0] == "storage" {
			continue // already reported as reserved
		}
		add("unknown key %s", ks)
	}

	if len(p) > 0 {
		return nil, &Error{Problems: p}
	}
	return m, nil
}

func checkEnvironment(path, name string, re rawEnvironment, add func(string, ...any)) Environment {
	if !nameRE.MatchString(name) {
		add("%s: invalid name, must match %s", path, nameRE)
	}
	if re.Host == "" {
		add("%s.host: required", path)
	} else if !nameRE.MatchString(re.Host) {
		add("%s.host: invalid host name %q", path, re.Host)
	}
	if re.Domain == "" {
		add("%s.domain: required", path)
	} else if !validDomain(re.Domain) {
		add("%s.domain: %q is not a valid lower-case DNS name", path, re.Domain)
	}
	tls := re.TLS
	if tls == "" {
		tls = bundle.TLSACME
	}
	if tls != bundle.TLSACME && tls != bundle.TLSInternal {
		add("%s.tls: must be %q or %q, got %q", path, bundle.TLSACME, bundle.TLSInternal, re.TLS)
	}
	vars := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(re.Env)) {
		s, ok := re.Env[k].(string)
		switch {
		case !varRE.MatchString(k):
			add("%s.env.%s: invalid variable name", path, k)
		case !ok:
			add("%s.env.%s: must be a string", path, k)
		default:
			vars[k] = s
		}
	}
	return Environment{Host: re.Host, Domain: re.Domain, TLS: tls, Vars: vars}
}

// checkImage requires a bare repository: tags and digests are chosen per
// release, so a pinned reference here would silently fight `--tag`.
func checkImage(img string) string {
	if img == "" {
		return "required"
	}
	last := img[strings.LastIndex(img, "/")+1:]
	if strings.Contains(img, "@") || strings.Contains(last, ":") {
		return fmt.Sprintf("must be a repository without tag or digest, got %q", img)
	}
	if _, err := name.NewRepository(img); err != nil {
		return fmt.Sprintf("invalid repository %q: %v", img, err)
	}
	return ""
}

func validDomain(d string) bool {
	if len(d) > 253 {
		return false
	}
	for _, l := range strings.Split(d, ".") {
		if !labelRE.MatchString(l) {
			return false
		}
	}
	return true
}

// EnvironmentNames returns the declared environments in sorted order.
func (m *Manifest) EnvironmentNames() []string {
	return slices.Sorted(maps.Keys(m.Environments))
}

// Resolve flattens the manifest for one environment: domains are expanded
// against the environment domain and variables are merged (environment wins).
// The result shares no mutable state with m.
func (m *Manifest) Resolve(env string) (*Resolved, error) {
	e, ok := m.Environments[env]
	if !ok {
		return nil, fmt.Errorf("environment %q is not declared in the manifest of %s", env, m.Name)
	}
	r := &Resolved{Env: env, Host: e.Host, Domain: e.Domain, TLS: e.TLS, Vars: map[string]string{}}
	maps.Copy(r.Vars, m.Vars)
	maps.Copy(r.Vars, e.Vars)
	for _, sn := range slices.Sorted(maps.Keys(m.Services)) {
		s := m.Services[sn]
		bs := bundle.Service{Name: sn, Image: s.Image, Command: slices.Clone(s.Command), Port: s.Port, Ready: s.Ready}
		switch s.Domain {
		case "":
		case "@":
			bs.Domains = []string{e.Domain}
		default:
			bs.Domains = []string{s.Domain + "." + e.Domain}
		}
		r.Services = append(r.Services, bs)
	}
	if m.Migrate != nil {
		r.Migrate = &bundle.Job{Service: m.Migrate.Service, Command: slices.Clone(m.Migrate.Command)}
	}
	return r, nil
}
