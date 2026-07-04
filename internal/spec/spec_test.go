package spec

import (
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
