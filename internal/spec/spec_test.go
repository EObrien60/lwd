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
