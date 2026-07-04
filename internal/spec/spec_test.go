package spec

import (
	"os"
	"path/filepath"
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
	if a.Node != "local" {
		t.Errorf("Node = %q, want local (default)", a.Node)
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
	a := &App{Name: "x", Image: "y", Port: 80, Node: "local"}
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
		Name:    "webapp",
		Compose: "docker-compose.yml",
		Service: "web",
		Domain:  "x.example.com",
		Port:    8080,
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
		Name:   "myapp",
		Git:    &Git{URL: "https://github.com/me/myapp"},
		Build:  &Build{Dockerfile: "Dockerfile"},
		Domain: "myapp.example.com",
		Port:   8080,
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
		Name:  "myapp",
		Image: "myimage:latest",
		Port:  8080,
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
