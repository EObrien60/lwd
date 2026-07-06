package spec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const singleService = `
name = "blog"
image = "ghcr.io/me/blog:latest"
domain = "blog.example.com"
port = 8080
env = { LOG_LEVEL = "info" }

[health]
path = "/healthz"
timeout = "30s"
`

func TestParseSingleService(t *testing.T) {
	a, err := Parse([]byte(singleService))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Name != "blog" {
		t.Errorf("Name = %q, want blog", a.Name)
	}
	if a.Image != "ghcr.io/me/blog:latest" {
		t.Errorf("Image = %q", a.Image)
	}
	if a.Port != 8080 {
		t.Errorf("Port = %d, want 8080", a.Port)
	}
	if a.Node != "" {
		t.Errorf("Node = %q, want empty (unset means schedule)", a.Node)
	}
	if a.Env["LOG_LEVEL"] != "info" {
		t.Errorf("Env[LOG_LEVEL] = %q", a.Env["LOG_LEVEL"])
	}
	if a.Health.Path != "/healthz" {
		t.Errorf("Health.Path = %q", a.Health.Path)
	}
	if a.Health.Timeout != 30*time.Second {
		t.Errorf("Health.Timeout = %v, want 30s", a.Health.Timeout)
	}
}

func TestValidateRejectsMissingName(t *testing.T) {
	a := &App{Image: "x", Port: 80}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for missing name")
	}
}

func TestValidateRejectsMissingImage(t *testing.T) {
	a := &App{Name: "x", Port: 80}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for missing image")
	}
}

func TestValidateRejectsMissingPort(t *testing.T) {
	a := &App{Name: "x", Image: "y"}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for missing port")
	}
}

func TestValidateRejectsUnsupportedFeatures(t *testing.T) {
	cases := map[string]*App{
		"compose":  {Name: "x", Image: "y", Port: 80, Compose: "docker-compose.yml"},
		"build":    {Name: "x", Port: 80, Build: &Build{Context: "."}},
		"surfaces": {Name: "x", Image: "y", Port: 80, Surfaces: []string{"web"}},
	}
	for label, a := range cases {
		if err := a.Validate(); err == nil {
			t.Errorf("%s: want 'not supported yet' error", label)
		}
	}
}

func TestValidateAcceptsGoodSpec(t *testing.T) {
	a := &App{Name: "x", Image: "y", Port: 80, Node: "local", Replicas: 1}
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejectsBadName(t *testing.T) {
	for _, bad := range []string{"has space", "bad/slash", "-leading", "café"} {
		a := &App{Name: bad, Image: "y", Port: 80}
		if err := a.Validate(); err == nil {
			t.Errorf("name %q: want invalid-name error", bad)
		}
	}
}

func TestValidateComposeApp(t *testing.T) {
	a := &App{
		Name:     "webapp",
		Compose:  "docker-compose.yml",
		Service:  "web",
		Domain:   "x.example.com",
		Port:     8080,
		Replicas: 1,
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestComposeRequiresService(t *testing.T) {
	a := &App{
		Name:    "webapp",
		Compose: "docker-compose.yml",
		Domain:  "x.example.com",
		Port:    8080,
	}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for compose without service")
	}
}

func TestComposeRequiresDomain(t *testing.T) {
	a := &App{
		Name:    "webapp",
		Compose: "docker-compose.yml",
		Service: "web",
		Port:    8080,
	}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for compose without domain")
	}
}

func TestComposeRequiresPort(t *testing.T) {
	a := &App{
		Name:    "webapp",
		Compose: "docker-compose.yml",
		Service: "web",
		Domain:  "x.example.com",
	}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for compose without port")
	}
}

func TestComposeRejectsImageMix(t *testing.T) {
	a := &App{
		Name:    "webapp",
		Compose: "docker-compose.yml",
		Service: "web",
		Domain:  "x.example.com",
		Port:    8080,
		Image:   "some-image:latest",
	}
	if err := a.Validate(); err == nil {
		t.Fatal("want error when compose and image both set")
	}
}

func TestComposeRejectsRemoteNode(t *testing.T) {
	a := &App{
		Name:    "webapp",
		Compose: "docker-compose.yml",
		Service: "web",
		Domain:  "x.example.com",
		Port:    8080,
		Node:    "web1",
	}
	err := a.Validate()
	if err == nil {
		t.Fatal("want error for compose app placed on a remote node")
	}
	if !strings.Contains(err.Error(), "compose apps on remote nodes are not supported") {
		t.Fatalf("Validate error = %q, want mention of unsupported remote compose", err.Error())
	}
}

func TestComposeAcceptsLocalNode(t *testing.T) {
	for _, node := range []string{"", "local"} {
		a := &App{
			Name:     "webapp",
			Compose:  "docker-compose.yml",
			Service:  "web",
			Domain:   "x.example.com",
			Port:     8080,
			Node:     node,
			Replicas: 1,
		}
		if err := a.Validate(); err != nil {
			t.Fatalf("Validate (node=%q): %v", node, err)
		}
	}
}

func TestComposeStillRejectsSurfaces(t *testing.T) {
	a := &App{
		Name:     "webapp",
		Compose:  "docker-compose.yml",
		Service:  "web",
		Domain:   "x.example.com",
		Port:     8080,
		Surfaces: []string{"web"},
	}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for surfaces in compose app")
	}
}

func TestLoadResolvesComposePath(t *testing.T) {
	dir := t.TempDir()
	toml := `
name = "webapp"
compose = "docker-compose.yml"
service = "web"
domain = "x.example.com"
port = 8080
`
	if err := os.WriteFile(filepath.Join(dir, "lwd.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("write lwd.toml: %v", err)
	}

	a, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(dir, "docker-compose.yml")
	if !filepath.IsAbs(a.Compose) {
		t.Errorf("Compose = %q, want absolute path", a.Compose)
	}
	if a.Compose != want {
		t.Errorf("Compose = %q, want %q", a.Compose, want)
	}
}

func TestLoadSingleServiceUnaffected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lwd.toml"), []byte(singleService), 0o644); err != nil {
		t.Fatalf("write lwd.toml: %v", err)
	}

	a, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if a.Compose != "" {
		t.Errorf("Compose = %q, want empty for single-service app", a.Compose)
	}
	if a.Name != "blog" {
		t.Errorf("Name = %q, want blog", a.Name)
	}
}

func TestParseServiceField(t *testing.T) {
	toml := `
name = "webapp"
compose = "docker-compose.yml"
service = "web"
domain = "x.example.com"
port = 8080
`
	a, err := Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Service != "web" {
		t.Errorf("Service = %q, want web", a.Service)
	}
	if a.Compose != "docker-compose.yml" {
		t.Errorf("Compose = %q", a.Compose)
	}
}

func TestGitAppValid(t *testing.T) {
	a := &App{
		Name:     "myapp",
		Git:      &Git{URL: "https://github.com/me/myapp"},
		Build:    &Build{Dockerfile: "Dockerfile"},
		Domain:   "myapp.example.com",
		Port:     8080,
		Replicas: 1,
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestGitAppRequiresBuild(t *testing.T) {
	a := &App{
		Name:   "myapp",
		Git:    &Git{URL: "https://github.com/me/myapp"},
		Domain: "myapp.example.com",
		Port:   8080,
	}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for git app without build")
	}
}

func TestGitAppRejectsImage(t *testing.T) {
	a := &App{
		Name:   "myapp",
		Git:    &Git{URL: "https://github.com/me/myapp"},
		Build:  &Build{Dockerfile: "Dockerfile"},
		Image:  "some-image:latest",
		Domain: "myapp.example.com",
		Port:   8080,
	}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for git app with image")
	}
}

func TestGitAppRejectsCompose(t *testing.T) {
	a := &App{
		Name:    "myapp",
		Git:     &Git{URL: "https://github.com/me/myapp"},
		Build:   &Build{Dockerfile: "Dockerfile"},
		Compose: "docker-compose.yml",
		Domain:  "myapp.example.com",
		Port:    8080,
	}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for git app with compose")
	}
}

func TestGitAppRequiresDomain(t *testing.T) {
	a := &App{
		Name:  "myapp",
		Git:   &Git{URL: "https://github.com/me/myapp"},
		Build: &Build{Dockerfile: "Dockerfile"},
		Port:  8080,
	}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for git app without domain")
	}
}

func TestGitAppRequiresPort(t *testing.T) {
	a := &App{
		Name:   "myapp",
		Git:    &Git{URL: "https://github.com/me/myapp"},
		Build:  &Build{Dockerfile: "Dockerfile"},
		Domain: "myapp.example.com",
	}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for git app without port")
	}
}

func TestServiceWithoutName(t *testing.T) {
	a := &App{
		Name:     "myapp",
		Image:    "myimage:latest",
		Port:     8080,
		Services: []Service{{Image: "postgres:16"}},
	}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for service without name")
	}
}

func TestServiceWithoutImage(t *testing.T) {
	a := &App{
		Name:     "myapp",
		Image:    "myimage:latest",
		Port:     8080,
		Services: []Service{{Name: "db"}},
	}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for service without image")
	}
}

func TestServiceDuplicateNames(t *testing.T) {
	a := &App{
		Name:  "myapp",
		Image: "myimage:latest",
		Port:  8080,
		Services: []Service{
			{Name: "db", Image: "postgres:16"},
			{Name: "db", Image: "postgres:16"},
		},
	}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for duplicate service names")
	}
}

func TestServiceBadName(t *testing.T) {
	a := &App{
		Name:  "myapp",
		Image: "myimage:latest",
		Port:  8080,
		Services: []Service{
			{Name: "Bad_Name", Image: "postgres:16"},
		},
	}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for service with invalid name")
	}
}

func TestComposeRejectsServices(t *testing.T) {
	a := &App{
		Name:    "webapp",
		Compose: "docker-compose.yml",
		Service: "web",
		Domain:  "x.example.com",
		Port:    8080,
		Services: []Service{
			{Name: "db", Image: "postgres:16"},
		},
	}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for compose app with services")
	}
}

func TestImageAppWithService(t *testing.T) {
	a := &App{
		Name:     "myapp",
		Image:    "myimage:latest",
		Port:     8080,
		Replicas: 1,
		Services: []Service{
			{Name: "db", Image: "postgres:16"},
		},
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestParseGitApp(t *testing.T) {
	toml := `
name = "myapp"
domain = "myapp.example.com"
port = 8080

[git]
url = "https://github.com/me/myapp"
ref = "develop"
path = "."

[build]
dockerfile = "Dockerfile"
`
	a, err := Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Git == nil {
		t.Fatal("Git is nil")
	}
	if a.Git.URL != "https://github.com/me/myapp" {
		t.Errorf("Git.URL = %q", a.Git.URL)
	}
	if a.Git.Ref != "develop" {
		t.Errorf("Git.Ref = %q, want develop", a.Git.Ref)
	}
	if a.Git.Path != "." {
		t.Errorf("Git.Path = %q", a.Git.Path)
	}
	if a.Build == nil {
		t.Fatal("Build is nil")
	}
	if a.Build.Dockerfile != "Dockerfile" {
		t.Errorf("Build.Dockerfile = %q", a.Build.Dockerfile)
	}
}

func TestParseGitRefDefault(t *testing.T) {
	toml := `
name = "myapp"
domain = "myapp.example.com"
port = 8080

[git]
url = "https://github.com/me/myapp"

[build]
dockerfile = "Dockerfile"
`
	a, err := Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Git == nil {
		t.Fatal("Git is nil")
	}
	if a.Git.Ref != "main" {
		t.Errorf("Git.Ref = %q, want main (default)", a.Git.Ref)
	}
}

func TestValidateRejectsBadSecretName(t *testing.T) {
	t.Run("app secret with space", func(t *testing.T) {
		a := &App{Name: "myapp", Image: "y", Port: 80, Secrets: []string{"BAD NAME"}}
		if err := a.Validate(); err == nil {
			t.Fatal("want error for app secret name with space")
		}
	})

	t.Run("app secret with YAML injection payload", func(t *testing.T) {
		a := &App{Name: "myapp", Image: "y", Port: 80, Secrets: []string{"X\"\n ports: 1"}}
		if err := a.Validate(); err == nil {
			t.Fatal("want error for app secret name with embedded quote/newline")
		}
	})

	t.Run("service secret with bad name", func(t *testing.T) {
		a := &App{
			Name:  "myapp",
			Image: "myimage:latest",
			Port:  8080,
			Services: []Service{
				{Name: "db", Image: "postgres:16", Secrets: []string{"BAD NAME"}},
			},
		}
		if err := a.Validate(); err == nil {
			t.Fatal("want error for service secret name with space")
		}
	})

	t.Run("normal secret name is accepted", func(t *testing.T) {
		a := &App{Name: "myapp", Image: "y", Port: 80, Replicas: 1, Secrets: []string{"DATABASE_URL"}}
		if err := a.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
}

func TestParseServices(t *testing.T) {
	toml := `
name = "myapp"
domain = "myapp.example.com"
port = 8080
image = "myimage:latest"

[[services]]
name = "db"
image = "postgres:16"
env = { POSTGRES_USER = "app", POSTGRES_DB = "app" }

[[services]]
name = "cache"
image = "redis:7"
command = "redis-server --appendonly yes"
`
	a, err := Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(a.Services) != 2 {
		t.Fatalf("Services count = %d, want 2", len(a.Services))
	}
	if a.Services[0].Name != "db" {
		t.Errorf("Services[0].Name = %q", a.Services[0].Name)
	}
	if a.Services[0].Image != "postgres:16" {
		t.Errorf("Services[0].Image = %q", a.Services[0].Image)
	}
	if a.Services[0].Env["POSTGRES_USER"] != "app" {
		t.Errorf("Services[0].Env[POSTGRES_USER] = %q", a.Services[0].Env["POSTGRES_USER"])
	}
	if a.Services[1].Name != "cache" {
		t.Errorf("Services[1].Name = %q", a.Services[1].Name)
	}
	if a.Services[1].Command != "redis-server --appendonly yes" {
		t.Errorf("Services[1].Command = %q", a.Services[1].Command)
	}
}

// --- Git url/ref/path hardening (host-RCE + option-injection + path
// traversal defenses; see validateGitURL, validateGitRef,
// validateRelativeNoTraversal) ---

func gitApp(overrides func(*App)) *App {
	a := &App{
		Name:     "myapp",
		Git:      &Git{URL: "https://github.com/me/myapp", Ref: "main"},
		Build:    &Build{Dockerfile: "Dockerfile"},
		Domain:   "myapp.example.com",
		Port:     8080,
		Replicas: 1,
	}
	if overrides != nil {
		overrides(a)
	}
	return a
}

func TestGitURLRejectsExtTransport(t *testing.T) {
	a := gitApp(func(a *App) { a.Git.URL = "ext::sh -c whoami" })
	if err := a.Validate(); err == nil {
		t.Fatal("want error for ext:: git url (host command execution)")
	}
}

func TestGitURLRejectsFdTransport(t *testing.T) {
	a := gitApp(func(a *App) { a.Git.URL = "fd::5" })
	if err := a.Validate(); err == nil {
		t.Fatal("want error for fd:: git url")
	}
}

func TestGitURLRejectsLeadingDash(t *testing.T) {
	a := gitApp(func(a *App) { a.Git.URL = "-oProxyCommand=whoami" })
	if err := a.Validate(); err == nil {
		t.Fatal("want error for git url starting with -")
	}
}

func TestGitURLAcceptsHTTPSAndScpLike(t *testing.T) {
	for _, url := range []string{
		"https://github.com/me/app",
		"http://internal.example.com/me/app.git",
		"git://example.com/me/app.git",
		"ssh://git@example.com/me/app.git",
		"file:///tmp/some/repo",
		"git@github.com:me/app.git",
	} {
		a := gitApp(func(a *App) { a.Git.URL = url })
		if err := a.Validate(); err != nil {
			t.Errorf("Validate(%q): unexpected error: %v", url, err)
		}
	}
}

func TestGitRefRejectsLeadingDash(t *testing.T) {
	a := gitApp(func(a *App) { a.Git.Ref = "-x" })
	if err := a.Validate(); err == nil {
		t.Fatal("want error for git ref starting with -")
	}
}

func TestGitRefRejectsWhitespace(t *testing.T) {
	a := gitApp(func(a *App) { a.Git.Ref = "a b" })
	if err := a.Validate(); err == nil {
		t.Fatal("want error for git ref containing whitespace")
	}
}

func TestGitRefRejectsShellMetacharacters(t *testing.T) {
	a := gitApp(func(a *App) { a.Git.Ref = "$(x)" })
	if err := a.Validate(); err == nil {
		t.Fatal("want error for git ref containing shell metacharacters")
	}
}

func TestGitRefAcceptsCommonForms(t *testing.T) {
	for _, ref := range []string{"main", "feature/x", "v1.2.3", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"} {
		a := gitApp(func(a *App) { a.Git.Ref = ref })
		if err := a.Validate(); err != nil {
			t.Errorf("Validate(ref=%q): unexpected error: %v", ref, err)
		}
	}
}

func TestGitPathRejectsTraversal(t *testing.T) {
	a := gitApp(func(a *App) { a.Git.Path = "../etc" })
	if err := a.Validate(); err == nil {
		t.Fatal("want error for git path escaping the clone root")
	}
}

func TestBuildDockerfileRejectsTraversal(t *testing.T) {
	a := gitApp(func(a *App) { a.Build.Dockerfile = "../../x" })
	if err := a.Validate(); err == nil {
		t.Fatal("want error for build dockerfile escaping the clone root")
	}
}

func TestBuildContextRejectsAbsolutePath(t *testing.T) {
	a := gitApp(func(a *App) { a.Build.Context = "/etc" })
	if err := a.Validate(); err == nil {
		t.Fatal("want error for absolute build context")
	}
}

func TestGitPathAndBuildDockerfileAcceptNormalValues(t *testing.T) {
	a := gitApp(func(a *App) {
		a.Git.Path = "."
		a.Build.Dockerfile = "Dockerfile"
		a.Build.Context = "."
	})
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate: unexpected error: %v", err)
	}
}

// --- Phase 11a Task 5: pool + requirements + ParseSize + schedule-preserving Node ---

func TestParseSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"1024", 1024, false},
		{"512M", 512 * 1024 * 1024, false},
		{"2G", 2 * 1024 * 1024 * 1024, false},
		{"1Ki", 1024, false},
		{"1ki", 1024, false},
		{"1k", 1024, false},
		{"1T", 1024 * 1024 * 1024 * 1024, false},
		{" 512M ", 512 * 1024 * 1024, false},
		{"bad", 0, true},
		{"-5", 0, true},
		{"-5M", 0, true},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseSize(%q) = %d, nil; want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSize(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParsePreservesEmptyNode(t *testing.T) {
	a, err := Parse([]byte(singleService))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Node != "" {
		t.Errorf("Node = %q, want empty (unset node means schedule, not local)", a.Node)
	}
}

func TestParseExplicitLocalNode(t *testing.T) {
	toml := `
name = "blog"
image = "ghcr.io/me/blog:latest"
domain = "blog.example.com"
port = 8080
node = "local"
`
	a, err := Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Node != "local" {
		t.Errorf("Node = %q, want local", a.Node)
	}
}

func TestParsePoolAndRequirements(t *testing.T) {
	toml := `
name = "blog"
image = "ghcr.io/me/blog:latest"
domain = "blog.example.com"
port = 8080
pool = "web"

[requirements]
cpu = 0.5
memory = "512M"
`
	a, err := Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Pool != "web" {
		t.Errorf("Pool = %q, want web", a.Pool)
	}
	if a.Requirements == nil {
		t.Fatal("Requirements is nil")
	}
	if a.Requirements.CPU != 0.5 {
		t.Errorf("Requirements.CPU = %v, want 0.5", a.Requirements.CPU)
	}
	if a.Requirements.Memory != "512M" {
		t.Errorf("Requirements.Memory = %q, want 512M", a.Requirements.Memory)
	}
}

func TestValidateBadRequirements(t *testing.T) {
	t.Run("bad memory", func(t *testing.T) {
		a := &App{Name: "x", Image: "y", Port: 80, Requirements: &Requirements{Memory: "bad"}}
		if err := a.Validate(); err == nil {
			t.Fatal("want error for bad requirements.memory")
		}
	})
	t.Run("negative cpu", func(t *testing.T) {
		a := &App{Name: "x", Image: "y", Port: 80, Requirements: &Requirements{CPU: -1}}
		if err := a.Validate(); err == nil {
			t.Fatal("want error for negative requirements.cpu")
		}
	})
	t.Run("valid requirements accepted", func(t *testing.T) {
		a := &App{Name: "x", Image: "y", Port: 80, Replicas: 1, Requirements: &Requirements{CPU: 1.5, Memory: "1G"}}
		if err := a.Validate(); err != nil {
			t.Fatalf("Validate: unexpected error: %v", err)
		}
	})
}

func TestValidateBadPool(t *testing.T) {
	a := &App{Name: "x", Image: "y", Port: 80, Pool: "bad pool!", Replicas: 1}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for invalid pool name")
	}
	a2 := &App{Name: "x", Image: "y", Port: 80, Pool: "web-1", Replicas: 1}
	if err := a2.Validate(); err != nil {
		t.Fatalf("Validate: unexpected error for valid pool: %v", err)
	}
}

// TestParseDefaultsReplicasToOne covers Phase 12 Task 2: an lwd.toml that
// doesn't declare replicas defaults to 1 (today's single-container
// behavior).
func TestParseDefaultsReplicasToOne(t *testing.T) {
	a, err := Parse([]byte(singleService))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Replicas != 1 {
		t.Errorf("Replicas = %d, want 1 (default)", a.Replicas)
	}
}

// TestParseReplicas covers Phase 12 Task 2: an explicit replicas value is
// parsed through unchanged.
func TestParseReplicas(t *testing.T) {
	toml := `
name = "blog"
image = "ghcr.io/me/blog:latest"
domain = "blog.example.com"
port = 8080
replicas = 3
`
	a, err := Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Replicas != 3 {
		t.Errorf("Replicas = %d, want 3", a.Replicas)
	}
}

// TestValidateReplicasMin covers Phase 12 Task 2: a negative Replicas is
// rejected. Zero is NOT rejected here (see TestValidateReplicasZeroIsUnset)
// — 0 means "unset", which a spec.App reconstructed from a pre-Phase-12
// snapshot legitimately has.
func TestValidateReplicasMin(t *testing.T) {
	a := &App{Name: "x", Image: "y", Port: 80, Replicas: -1}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for negative replicas")
	}
}

// TestValidateReplicasZeroIsUnset covers Phase 12 Task 2's backward-compat
// rule: Replicas == 0 ("unset") is VALID. Parse normalizes a fresh spec's 0
// to 1, but heal/rollback re-validate a spec.App reconstructed from a
// pre-Phase-12 snapshot that has no replicas field (so Replicas == 0) —
// rejecting 0 would make healing/rolling back any existing deployment fail
// after upgrade.
func TestValidateReplicasZeroIsUnset(t *testing.T) {
	a := &App{Name: "x", Image: "y", Port: 80, Replicas: 0}
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate: replicas=0 (unset) must be valid, got: %v", err)
	}
}

// TestValidatePreV12SnapshotReplicasZero proves the upgrade path: a spec.App
// unmarshaled from a JSON deployment snapshot that predates Phase 12 (no
// "replicas" key → Replicas defaults to 0) validates cleanly, so heal
// (healSurfaceLocked) and rollback (rollbackGit/rollbackImage), which
// reconstruct and re-Validate such snapshots, are not broken by this
// upgrade.
func TestValidatePreV12SnapshotReplicasZero(t *testing.T) {
	snapshot := []byte(`{"Name":"blog","Image":"img:1","Domain":"blog.example.com","Port":8080,"Node":"local"}`)
	var a App
	if err := json.Unmarshal(snapshot, &a); err != nil {
		t.Fatalf("unmarshal pre-12 snapshot: %v", err)
	}
	if a.Replicas != 0 {
		t.Fatalf("pre-12 snapshot Replicas = %d, want 0 (no field in JSON)", a.Replicas)
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate of reconstructed pre-12 snapshot must succeed, got: %v", err)
	}
}

// TestValidateReplicasMax covers Phase 12 Task 2: Replicas above the 50 cap
// is rejected.
func TestValidateReplicasMax(t *testing.T) {
	a := &App{Name: "x", Image: "y", Port: 80, Replicas: 51}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for replicas > 50")
	}
}

// TestValidateReplicasCompose covers Phase 12 Task 2: replicas > 1 is not
// supported for compose apps (compose's Up/routing path doesn't have a
// replica-set notion).
func TestValidateReplicasCompose(t *testing.T) {
	a := &App{
		Name:     "webapp",
		Compose:  "docker-compose.yml",
		Service:  "web",
		Domain:   "x.example.com",
		Port:     8080,
		Replicas: 2,
	}
	if err := a.Validate(); err == nil {
		t.Fatal("want error for replicas > 1 with compose")
	}
}

// TestValidateReplicasWithBackingRejected covers Phase 12 Task 5's guard: a
// multi-replica app declaring backing [[services]] is rejected, since backing
// services run PINNED on a single node's per-app network and a replica spread
// across other nodes has no way to reach it. replicas=1 with services is
// still valid — backing is only a problem once there's more than one node in
// play.
func TestValidateReplicasWithBackingRejected(t *testing.T) {
	base := App{
		Name:     "webapp",
		Image:    "img:1",
		Domain:   "x.example.com",
		Port:     8080,
		Services: []Service{{Name: "db", Image: "postgres:16"}},
	}

	t.Run("replicas > 1 with services is rejected", func(t *testing.T) {
		a := base
		a.Replicas = 2
		err := a.Validate()
		if err == nil {
			t.Fatal("want error for replicas > 1 with backing services")
		}
		if !strings.Contains(err.Error(), "backing") {
			t.Errorf("error = %q, want it to mention backing services", err.Error())
		}
	})

	t.Run("replicas = 1 with services is OK", func(t *testing.T) {
		a := base
		a.Replicas = 1
		if err := a.Validate(); err != nil {
			t.Errorf("want no error for replicas=1 with backing services, got %v", err)
		}
	})
}
