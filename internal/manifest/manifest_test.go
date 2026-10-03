package manifest

import (
	"reflect"
	"strings"
	"testing"

	"lwd/internal/bundle"
)

// designExample is the manifest from docs/lwd2/DESIGN.md, written with an
// [env] table for plain variables (the strictly valid TOML form).
const designExample = `
name = "hello"

secrets = ["SESSION_SECRET"]

[services.web]
image  = "ghcr.io/obhsoftware/lwd-hello-web"
port   = 8080
domain = "@"
ready  = "/"

[services.api]
image  = "ghcr.io/obhsoftware/lwd-hello-api"
port   = 3000
domain = "api"
ready  = "/ready"

[services.worker]
image   = "ghcr.io/obhsoftware/lwd-hello-api"
command = ["node", "dist/worker.js"]

[migrate]
service = "api"
command = ["node", "dist/migrate.js"]

[env]
LOG_LEVEL = "info"

[env.staging]
host   = "m1"
domain = "hello.m1.lwd.internal"
tls    = "internal"
env    = { LOG_LEVEL = "debug" }

[env.prod]
host   = "prod-01"
domain = "hello.example.com"
`

func TestParseDesignExample(t *testing.T) {
	m, err := Parse([]byte(designExample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Name != "hello" {
		t.Errorf("name = %q", m.Name)
	}
	if !reflect.DeepEqual(m.Vars, map[string]string{"LOG_LEVEL": "info"}) {
		t.Errorf("vars = %v", m.Vars)
	}
	if !reflect.DeepEqual(m.Secrets, []string{"SESSION_SECRET"}) {
		t.Errorf("secrets = %v", m.Secrets)
	}
	if len(m.Services) != 3 {
		t.Fatalf("services = %v", m.Services)
	}
	if got := m.Services["worker"].Command; !reflect.DeepEqual(got, []string{"node", "dist/worker.js"}) {
		t.Errorf("worker command = %v", got)
	}
	if m.Migrate == nil || m.Migrate.Service != "api" {
		t.Errorf("migrate = %+v", m.Migrate)
	}
	st := m.Environments["staging"]
	if st.Host != "m1" || st.Domain != "hello.m1.lwd.internal" || st.TLS != "internal" || st.Vars["LOG_LEVEL"] != "debug" {
		t.Errorf("staging = %+v", st)
	}
	if p := m.Environments["prod"]; p.TLS != "acme" {
		t.Errorf("prod tls default = %q, want acme", p.TLS)
	}
	if r := m.Services["web"].Ready; r != "/" {
		t.Errorf("web ready = %q", r)
	}
}

func TestParseDesignLiteralInlineEnv(t *testing.T) {
	// DESIGN.md writes plain vars as an inline table next to [env.<name>]
	// sections. Strict TOML forbids extending an inline table, but our parser
	// accepts it and the docs use this form, so keep it working.
	src := `name = "hello"
env = { LOG_LEVEL = "info" }
[services.web]
image = "r/web"
[env.staging]
host = "m1"
domain = "a.b"
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if m.Vars["LOG_LEVEL"] != "info" || m.Environments["staging"].Host != "m1" {
		t.Errorf("manifest = %+v", m)
	}
}

func TestReadyDefaultsOnlyForHTTPServices(t *testing.T) {
	m, err := Parse([]byte(`name = "a"
[services.web]
image = "r/web"
port = 80
[services.worker]
image = "r/worker"
[env.prod]
host = "h"
domain = "a.example.com"
`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Services["web"].Ready != "/" {
		t.Errorf("web ready = %q", m.Services["web"].Ready)
	}
	if m.Services["worker"].Ready != "" {
		t.Errorf("worker ready = %q", m.Services["worker"].Ready)
	}
}

func TestParseErrors(t *testing.T) {
	base := func(extra string) string {
		return `name = "a"
[services.web]
image = "ghcr.io/o/web"
port = 8080
domain = "@"
[env.prod]
host = "h1"
domain = "a.example.com"
` + extra
	}
	cases := []struct {
		name string
		src  string
		want string // substring of the error
	}{
		{"bad toml", `name = `, "lwd.toml"},
		{"missing name", strings.Replace(base(""), `name = "a"`, ``, 1), "name: required"},
		{"bad name", strings.Replace(base(""), `name = "a"`, `name = "Hello"`, 1), "name: "},
		{"long name", strings.Replace(base(""), `name = "a"`, `name = "a234567890123456789012345678901x"`, 1), "name: "},
		{"no services", `name = "a"
[env.prod]
host = "h"
domain = "a.b"
`, "services: at least one service is required"},
		{"bad service name", base(`[services.Web]
image = "r/x"
`), `services.Web: invalid name`},
		{"missing image", base(`[services.w]
port = 1
`), "services.w.image: required"},
		{"image with tag", base(`[services.w]
image = "ghcr.io/o/w:1.2"
`), "services.w.image: must be a repository without tag or digest"},
		{"image with digest", base(`[services.w]
image = "ghcr.io/o/w@sha256:0000000000000000000000000000000000000000000000000000000000000000"
`), "services.w.image: must be a repository without tag or digest"},
		{"image invalid", base(`[services.w]
image = "UPPER/case"
`), "services.w.image"},
		{"port too big", base(`[services.w]
image = "r/w"
port = 70000
`), "services.w.port"},
		{"port negative", base(`[services.w]
image = "r/w"
port = -1
`), "services.w.port"},
		{"domain without port", base(`[services.w]
image = "r/w"
domain = "x"
`), "services.w.domain: requires port"},
		{"ready without port", base(`[services.w]
image = "r/w"
ready = "/x"
`), "services.w.ready: requires port"},
		{"ready no slash", base(`[services.w]
image = "r/w"
port = 1
ready = "x"
`), "services.w.ready: must start with /"},
		{"domain multi label", base(`[services.w]
image = "r/w"
port = 1
domain = "a.b"
`), `services.w.domain: must be "@" or a single DNS label`},
		{"domain bad label", base(`[services.w]
image = "r/w"
port = 1
domain = "-x"
`), "services.w.domain"},
		{"duplicate domain", base(`[services.w]
image = "r/w"
port = 1
domain = "@"
`), "domain"},
		{"empty command element", base(`[services.w]
image = "r/w"
command = ["", "x"]
`), "services.w.command"},
		{"migrate unknown service", base(`[migrate]
service = "nope"
command = ["x"]
`), "migrate.service: unknown service"},
		{"migrate no command", base(`[migrate]
service = "web"
`), "migrate.command: required"},
		{"no envs", `name = "a"
[services.w]
image = "r/w"
`, "env: at least one environment"},
		{"env no host", base(`[env.stage]
domain = "x.y"
`), "env.stage.host: required"},
		{"env no domain", base(`[env.stage]
host = "h"
`), "env.stage.domain: required"},
		{"env bad domain", base(`[env.stage]
host = "h"
domain = "not a domain"
`), "env.stage.domain"},
		{"env bad tls", base(`[env.stage]
host = "h"
domain = "x.y"
tls = "off"
`), `env.stage.tls: must be "acme" or "internal"`},
		{"env bad name", base(`[env.Stage]
host = "h"
domain = "x.y"
`), "env.Stage: invalid name"},
		{"env bad var name", base(`[env.stage]
host = "h"
domain = "x.y"
env = { "1X" = "y" }
`), "env.stage.env.1X: invalid variable name"},
		{"top var bad name", base(`[env]
"A-B" = "x"
`), "env.A-B: invalid variable name"},
		{"top var wrong type", base(`[env]
A = 1
`), "env.A: must be a string"},
		{"secret bad name", strings.Replace(base(""), `name = "a"`, "name = \"a\"\nsecrets = [\"a b\"]", 1), "secrets[0]: invalid variable name"},
		{"secret dup", strings.Replace(base(""), `name = "a"`, "name = \"a\"\nsecrets = [\"A\", \"A\"]", 1), "secrets[1]: duplicate"},
		{"database", strings.Replace(base(""), `name = "a"`, "name = \"a\"\n[database]\nengine = \"postgres\"", 1), "database: not supported until M2"},
		{"storage", strings.Replace(base(""), `name = "a"`, "name = \"a\"\nstorage = true", 1), "storage: not supported until M2"},
		{"unknown top key", strings.Replace(base(""), `name = "a"`, "name = \"a\"\nreplicas = 3", 1), "unknown key replicas"},
		{"unknown service key", base(`[services.w]
image = "r/w"
replicas = 2
`), "unknown key services.w.replicas"},
		{"unknown env key", base(`[env.stage]
host = "h"
domain = "x.y"
pool = "p"
`), "unknown key env.stage.pool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestParseReportsAllProblems(t *testing.T) {
	_, err := Parse([]byte(`name = "A"
[services.w]
port = 1
`))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"name:", "services.w.image: required", "env: at least one"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %q", want, err)
		}
	}
}

func TestResolve(t *testing.T) {
	m, err := Parse([]byte(designExample))
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.Resolve("staging")
	if err != nil {
		t.Fatal(err)
	}
	if r.Host != "m1" || r.TLS != bundle.TLSInternal || r.Domain != "hello.m1.lwd.internal" {
		t.Errorf("placement = %+v", r)
	}
	if !reflect.DeepEqual(r.Vars, map[string]string{"LOG_LEVEL": "debug"}) {
		t.Errorf("vars = %v", r.Vars)
	}
	want := []bundle.Service{
		{Name: "api", Image: "ghcr.io/obhsoftware/lwd-hello-api", Port: 3000, Domains: []string{"api.hello.m1.lwd.internal"}, Ready: "/ready"},
		{Name: "web", Image: "ghcr.io/obhsoftware/lwd-hello-web", Port: 8080, Domains: []string{"hello.m1.lwd.internal"}, Ready: "/"},
		{Name: "worker", Image: "ghcr.io/obhsoftware/lwd-hello-api", Command: []string{"node", "dist/worker.js"}},
	}
	if !reflect.DeepEqual(r.Services, want) {
		t.Errorf("services =\n%+v\nwant\n%+v", r.Services, want)
	}
	if r.Migrate == nil || r.Migrate.Service != "api" || !reflect.DeepEqual(r.Migrate.Command, []string{"node", "dist/migrate.js"}) {
		t.Errorf("migrate = %+v", r.Migrate)
	}
	// Resolving must not alias manifest state: mutating the result is safe.
	r.Vars["X"] = "y"
	r.Services[2].Command[0] = "mutated"
	if _, ok := m.Vars["X"]; ok || m.Services["worker"].Command[0] != "node" {
		t.Error("Resolve aliases manifest data")
	}

	if _, err := m.Resolve("nope"); err == nil {
		t.Error("expected error for unknown environment")
	}
}

func TestResolveProdUsesBaseVars(t *testing.T) {
	m, err := Parse([]byte(designExample))
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.Resolve("prod")
	if err != nil {
		t.Fatal(err)
	}
	if r.Vars["LOG_LEVEL"] != "info" || r.TLS != bundle.TLSACME {
		t.Errorf("prod = %+v", r)
	}
}

func TestEnvironmentNamesSorted(t *testing.T) {
	m, err := Parse([]byte(designExample))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.EnvironmentNames(); !reflect.DeepEqual(got, []string{"prod", "staging"}) {
		t.Errorf("names = %v", got)
	}
}
