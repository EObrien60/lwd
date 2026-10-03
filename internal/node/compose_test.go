package node

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"lwd/internal/bundle"
)

const testDigest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testBundle() bundle.Bundle {
	return bundle.Bundle{
		App: "hello", Env: "staging", Release: 7, Deployment: 42, TLS: bundle.TLSInternal,
		Services: []bundle.Service{
			{Name: "web", Image: "ghcr.io/obh/web" + testDigest, Port: 8080, Domains: []string{"hello.lwd.internal"}, Ready: "/"},
			{Name: "api", Image: "ghcr.io/obh/api" + testDigest, Port: 3000, Domains: []string{"api.hello.lwd.internal"}, Ready: "/ready"},
			{Name: "worker", Image: "ghcr.io/obh/api" + testDigest, Command: []string{"node", "worker.js"}},
		},
		Migrate: &bundle.Job{Service: "api", Command: []string{"node", "migrate.js"}},
		Vars:    map[string]string{"LOG_LEVEL": "info", "SECRET": "p$ss'w\"d\n#x"},
	}
}

func decode(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, data)
	}
	return m
}

func dig(m any, keys ...string) any {
	for _, k := range keys {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[k]
	}
	return m
}

func TestRenderComposeServices(t *testing.T) {
	b := testBundle()
	out, err := renderCompose(&b, map[string]int{"web": 20000, "api": 20001})
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	if m["name"] != "lwd-hello-staging-d42" {
		t.Errorf("name = %v", m["name"])
	}
	web := dig(m, "services", "web")
	if dig(web, "image") != "ghcr.io/obh/web"+testDigest {
		t.Errorf("image = %v", dig(web, "image"))
	}
	if dig(web, "restart") != "unless-stopped" {
		t.Errorf("restart = %v", dig(web, "restart"))
	}
	if got := dig(web, "ports"); !reflect.DeepEqual(got, []any{"127.0.0.1:20000:8080"}) {
		t.Errorf("ports = %v", got)
	}
	if got := dig(web, "logging"); !reflect.DeepEqual(got, map[string]any{
		"driver": "json-file", "options": map[string]any{"max-size": "10m", "max-file": "3"},
	}) {
		t.Errorf("logging = %v", got)
	}
	wantLabels := map[string]any{
		"lwd.app": "hello", "lwd.env": "staging", "lwd.release": "7", "lwd.deployment": "42", "lwd.service": "web",
	}
	if got := dig(web, "labels"); !reflect.DeepEqual(got, wantLabels) {
		t.Errorf("labels = %v", got)
	}
	if dig(web, "command") != nil {
		t.Errorf("web has no command, got %v", dig(web, "command"))
	}
	if dig(web, "environment") != nil {
		t.Error("compose.yaml must not carry vars; they live in the 0600 environment file")
	}

	worker := dig(m, "services", "worker")
	if got := dig(worker, "command"); !reflect.DeepEqual(got, []any{"node", "worker.js"}) {
		t.Errorf("worker command = %v", got)
	}
	if dig(worker, "ports") != nil {
		t.Error("worker must not publish ports")
	}

	mig := dig(m, "services", migrateService)
	if dig(mig, "image") != "ghcr.io/obh/api"+testDigest {
		t.Errorf("migrate image = %v", dig(mig, "image"))
	}
	if got := dig(mig, "profiles"); !reflect.DeepEqual(got, []any{"migrate"}) {
		t.Errorf("migrate profiles = %v", got)
	}
	if dig(mig, "restart") != "no" {
		t.Errorf("migrate restart = %v", dig(mig, "restart"))
	}
	if got := dig(mig, "command"); !reflect.DeepEqual(got, []any{"node", "migrate.js"}) {
		t.Errorf("migrate command = %v", got)
	}
	if dig(mig, "ports") != nil {
		t.Error("migrate must not publish ports")
	}
}

func TestRenderComposeWithoutPlatformHasNoNetworks(t *testing.T) {
	b := testBundle()
	out, err := renderCompose(&b, map[string]int{"web": 20000, "api": 20001})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "networks") || strings.Contains(string(out), bundle.PlatformNetwork) {
		t.Errorf("non-platform bundle mentions networks:\n%s", out)
	}
}

func TestRenderComposePlatformJoinsEveryService(t *testing.T) {
	b := testBundle()
	b.Platform = true
	out, err := renderCompose(&b, map[string]int{"web": 20000, "api": 20001})
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	for _, svc := range []string{"web", "api", "worker", migrateService} {
		if got := dig(m, "services", svc, "networks"); !reflect.DeepEqual(got, []any{"default", bundle.PlatformNetwork}) {
			t.Errorf("%s networks = %v", svc, got)
		}
	}
	want := map[string]any{bundle.PlatformNetwork: map[string]any{"external": true, "name": bundle.PlatformNetwork}}
	if got := m["networks"]; !reflect.DeepEqual(got, want) {
		t.Errorf("top-level networks = %v", got)
	}
}

func TestRenderPlatformCompose(t *testing.T) {
	out := renderPlatformCompose("/srv/lwd", "pw-secret-value")
	m := decode(t, out)
	if m["name"] != platformProject {
		t.Errorf("name = %v", m["name"])
	}
	want := map[string]any{bundle.PlatformNetwork: map[string]any{"external": true, "name": bundle.PlatformNetwork}}
	if got := m["networks"]; !reflect.DeepEqual(got, want) {
		t.Errorf("networks = %v", got)
	}
	pg := dig(m, "services", "postgres")
	if dig(pg, "image") != postgresImage || dig(pg, "container_name") != "lwd-postgres" || dig(pg, "restart") != "unless-stopped" {
		t.Errorf("postgres = %v", pg)
	}
	if got := dig(pg, "volumes"); !reflect.DeepEqual(got, []any{"/srv/lwd/postgres:/var/lib/postgresql/data"}) {
		t.Errorf("postgres volumes = %v", got)
	}
	if dig(pg, "environment", "POSTGRES_PASSWORD") != "pw-secret-value" {
		t.Errorf("postgres environment = %v", dig(pg, "environment"))
	}
	if dig(pg, "healthcheck", "test") == nil {
		t.Error("postgres has no healthcheck")
	}
	s3 := dig(m, "services", "s3")
	if dig(s3, "image") != garageImage || dig(s3, "container_name") != "lwd-s3" || dig(s3, "restart") != "unless-stopped" {
		t.Errorf("s3 = %v", s3)
	}
	wantVols := []any{"/srv/lwd/platform/garage.toml:/etc/garage.toml:ro", "/srv/lwd/garage/meta:/var/lib/garage/meta", "/srv/lwd/garage/data:/var/lib/garage/data"}
	if got := dig(s3, "volumes"); !reflect.DeepEqual(got, wantVols) {
		t.Errorf("s3 volumes = %v", got)
	}
	for _, svc := range []string{"postgres", "s3"} {
		if dig(m, "services", svc, "ports") != nil {
			t.Errorf("%s publishes ports", svc)
		}
		if got := dig(m, "services", svc, "networks"); !reflect.DeepEqual(got, []any{bundle.PlatformNetwork}) {
			t.Errorf("%s networks = %v", svc, got)
		}
	}
	if !strings.Contains(garageImage, "@sha256:") || !strings.Contains(garageImage, ":v2.") {
		t.Errorf("garage image %q must be a pinned v2 release", garageImage)
	}
}

func TestRenderGarageConfig(t *testing.T) {
	out := string(renderGarageConfig("aa11", "tok22"))
	for _, want := range []string{
		`metadata_dir = "/var/lib/garage/meta"`, `data_dir = "/var/lib/garage/data"`,
		`replication_factor = 1`, `rpc_secret = "aa11"`, `admin_token = "tok22"`,
		`s3_region = "garage"`, `api_bind_addr = "[::]:3900"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("garage.toml missing %q:\n%s", want, out)
		}
	}
}

func TestRenderComposeEscapesInterpolation(t *testing.T) {
	b := testBundle()
	b.Services[2].Command = []string{"sh", "-c", "echo $HOME ${X}"}
	out, err := renderCompose(&b, map[string]int{"web": 20000, "api": 20001})
	if err != nil {
		t.Fatal(err)
	}
	got := dig(decode(t, out), "services", "worker", "command")
	if !reflect.DeepEqual(got, []any{"sh", "-c", "echo $$HOME $${X}"}) {
		t.Errorf("command = %v", got)
	}
}

func TestRenderComposeRequiresPortForHTTPService(t *testing.T) {
	b := testBundle()
	if _, err := renderCompose(&b, map[string]int{"web": 20000}); err == nil {
		t.Fatal("missing host port for api should be an error")
	}
}

func TestRenderEnvironment(t *testing.T) {
	b := testBundle()
	out, err := renderEnvironment(&b)
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	for _, svc := range []string{"web", "api", "worker", migrateService} {
		env := dig(m, "services", svc, "environment")
		want := map[string]any{
			"LOG_LEVEL": "info", "SECRET": "p$$ss'w\"d\n#x",
			"LWD_APP": "hello", "LWD_ENV": "staging", "LWD_RELEASE": "7", "LWD_DEPLOYMENT": "42",
		}
		if !reflect.DeepEqual(env, want) {
			t.Errorf("%s environment = %#v", svc, env)
		}
	}
}

func TestRenderEnvironmentRejectsBadKeys(t *testing.T) {
	for _, k := range []string{"", "A=B", "A\x00"} {
		b := testBundle()
		b.Vars = map[string]string{k: "v"}
		if _, err := renderEnvironment(&b); err == nil {
			t.Errorf("key %q accepted", k)
		}
	}
	b := testBundle()
	b.Vars = map[string]string{"K": "a\x00b"}
	if _, err := renderEnvironment(&b); err == nil {
		t.Error("NUL in value accepted")
	}
}

func TestRenderSystemCompose(t *testing.T) {
	out := renderSystemCompose("/srv/lwd", "caddy:2", "")
	m := decode(t, out)
	if m["name"] != "lwd-system" {
		t.Errorf("name = %v", m["name"])
	}
	c := dig(m, "services", "caddy")
	if dig(c, "image") != "caddy:2" || dig(c, "network_mode") != "host" || dig(c, "restart") != "unless-stopped" {
		t.Errorf("caddy = %v", c)
	}
	wantVols := []any{"/srv/lwd/caddy/etc:/etc/caddy", "/srv/lwd/caddy/data:/data", "/srv/lwd/caddy/config:/config"}
	if got := dig(c, "volumes"); !reflect.DeepEqual(got, wantVols) {
		t.Errorf("volumes = %v", got)
	}
	cmd, _ := json.Marshal(dig(c, "command"))
	if !strings.Contains(string(cmd), `"caddy","run","--config","/etc/caddy/Caddyfile","--adapter","caddyfile"`) {
		t.Errorf("command = %s", cmd)
	}
}

func TestRenderSystemComposeEnvFile(t *testing.T) {
	m := decode(t, renderSystemCompose("/srv/lwd", "caddy:2", "/etc/lwd/caddy.env"))
	if got := dig(m, "services", "caddy", "env_file"); !reflect.DeepEqual(got, []any{"/etc/lwd/caddy.env"}) {
		t.Fatalf("env_file = %#v", got)
	}
	if dig(decode(t, renderSystemCompose("/srv/lwd", "caddy:2", "")), "services", "caddy", "env_file") != nil {
		t.Fatal("env_file emitted without a file")
	}
}
