package node

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"lwd/internal/bundle"
)

// Compose files are emitted as JSON, which is valid YAML: encoding/json does
// all the quoting, so no bundle value can change the document's structure.
//
// Vars are not written to a dotenv `.env`/env_file: compose's dotenv dialect
// has no way to quote a value containing both quote kinds and newlines, and a
// file named `.env` in the project directory is also parsed by compose for
// interpolation. Instead each deployment gets two compose files merged with
// `-f compose.yaml -f environment.json`: compose.yaml (structure, no secrets)
// and environment.json (0600, an `environment:` mapping per service). In both,
// `$` is escaped as `$$` so compose interpolation never rewrites a value.

const (
	composeFile     = "compose.yaml"
	environmentFile = "environment.json"
	systemProject   = "lwd-system"

	// migrateService is the compose service that runs bundle.Migrate. Bundles
	// may not use the name for their own services (see validateBundle).
	migrateService = "lwd-migrate"
	migrateProfile = "migrate"
)

type composeDoc struct {
	Name     string                     `json:"name"`
	Services map[string]*composeService `json:"services"`
}

type composeService struct {
	Image       string            `json:"image,omitempty"`
	Command     []string          `json:"command,omitempty"`
	Restart     string            `json:"restart,omitempty"`
	Profiles    []string          `json:"profiles,omitempty"`
	Ports       []string          `json:"ports,omitempty"`
	NetworkMode string            `json:"network_mode,omitempty"`
	Volumes     []string          `json:"volumes,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Logging     *composeLogging   `json:"logging,omitempty"`
}

type composeLogging struct {
	Driver  string            `json:"driver"`
	Options map[string]string `json:"options"`
}

func defaultLogging() *composeLogging {
	return &composeLogging{Driver: "json-file", Options: map[string]string{"max-size": "10m", "max-file": "3"}}
}

// esc protects a literal value from compose variable interpolation.
func esc(s string) string { return strings.ReplaceAll(s, "$", "$$") }

func escAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = esc(s)
	}
	return out
}

func labels(b *bundle.Bundle, service string) map[string]string {
	return map[string]string{
		"lwd.app":        b.App,
		"lwd.env":        b.Env,
		"lwd.release":    strconv.FormatInt(b.Release, 10),
		"lwd.deployment": strconv.FormatInt(b.Deployment, 10),
		"lwd.service":    service,
	}
}

// renderCompose renders a deployment's compose.yaml. ports maps each HTTP
// service to its allocated loopback host port.
func renderCompose(b *bundle.Bundle, ports map[string]int) ([]byte, error) {
	doc := composeDoc{Name: b.ProjectName(), Services: map[string]*composeService{}}
	images := map[string]string{}
	for _, s := range b.Services {
		svc := &composeService{
			Image:   esc(s.Image),
			Command: escAll(s.Command),
			Restart: "unless-stopped",
			Labels:  labels(b, s.Name),
			Logging: defaultLogging(),
		}
		if s.Port > 0 {
			hp, ok := ports[s.Name]
			if !ok {
				return nil, fmt.Errorf("no host port allocated for service %s", s.Name)
			}
			svc.Ports = []string{fmt.Sprintf("127.0.0.1:%d:%d", hp, s.Port)}
		}
		doc.Services[s.Name] = svc
		images[s.Name] = s.Image
	}
	if m := b.Migrate; m != nil {
		doc.Services[migrateService] = &composeService{
			Image:    esc(images[m.Service]),
			Command:  escAll(m.Command),
			Restart:  "no",
			Profiles: []string{migrateProfile},
			Labels:   labels(b, migrateService),
			Logging:  defaultLogging(),
		}
	}
	return json.MarshalIndent(doc, "", "  ")
}

// renderEnvironment renders environment.json: the bundle's vars for every
// service, including the migrate job.
func renderEnvironment(b *bundle.Bundle) ([]byte, error) {
	env := make(map[string]string, len(b.Vars))
	for k, v := range b.Vars {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return nil, fmt.Errorf("invalid variable name %q", k)
		}
		if strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("variable %s contains a NUL byte", k)
		}
		env[k] = esc(v)
	}
	// Platform identity, so apps can report what they are running as. These
	// win over any same-named var: they describe the deployment, not config.
	for k, v := range map[string]string{
		"LWD_APP":        b.App,
		"LWD_ENV":        b.Env,
		"LWD_RELEASE":    strconv.FormatInt(b.Release, 10),
		"LWD_DEPLOYMENT": strconv.FormatInt(b.Deployment, 10),
	} {
		env[k] = v
	}
	doc := composeDoc{Name: b.ProjectName(), Services: map[string]*composeService{}}
	for _, s := range b.Services {
		doc.Services[s.Name] = &composeService{Environment: env}
	}
	if b.Migrate != nil {
		doc.Services[migrateService] = &composeService{Environment: env}
	}
	return json.MarshalIndent(doc, "", "  ")
}

// renderSystemCompose renders system/compose.yaml for the node's Caddy.
func renderSystemCompose(root, image string) []byte {
	doc := composeDoc{Name: systemProject, Services: map[string]*composeService{
		"caddy": {
			Image:       esc(image),
			Command:     []string{"caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"},
			Restart:     "unless-stopped",
			NetworkMode: "host",
			Volumes: []string{
				filepath.Join(root, "caddy", "etc") + ":/etc/caddy",
				filepath.Join(root, "caddy", "data") + ":/data",
				filepath.Join(root, "caddy", "config") + ":/config",
			},
			Logging: defaultLogging(),
		},
	}}
	out, _ := json.MarshalIndent(doc, "", "  ")
	return out
}
