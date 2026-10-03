package node

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestComposeConfigAcceptsRenderedFiles runs `docker compose config` over the
// rendered files so a compose-schema mistake is caught without a VM. Gated on
// LWD_DOCKER_TEST=1 because it needs the docker CLI with the compose plugin.
func TestComposeConfigAcceptsRenderedFiles(t *testing.T) {
	if os.Getenv("LWD_DOCKER_TEST") != "1" {
		t.Skip("set LWD_DOCKER_TEST=1 to run against the docker compose CLI")
	}
	dir := t.TempDir()
	b := testBundle()
	c, err := renderCompose(&b, map[string]int{"web": 20000, "api": 20001})
	if err != nil {
		t.Fatal(err)
	}
	e, err := renderEnvironment(&b)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, composeFile), c, 0o644)
	os.WriteFile(filepath.Join(dir, environmentFile), e, 0o600)

	out, err := exec.Command("docker", "compose", "-p", b.ProjectName(),
		"-f", filepath.Join(dir, composeFile), "-f", filepath.Join(dir, environmentFile),
		"--profile", migrateProfile, "config", "--format", "json").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// The secret must survive interpolation byte for byte. `config` re-escapes
	// `$` in its output so it can be fed back to compose; an unescaped input
	// would have come out as "p'w..." with $ss interpolated away.
	if !strings.Contains(string(out), `"SECRET": "p$$ss'w\"d\n#x"`) {
		t.Fatalf("secret mangled:\n%s", out)
	}
	if !strings.Contains(string(out), `"host_ip": "127.0.0.1"`) {
		t.Fatalf("loopback publish missing:\n%s", out)
	}

	sys := filepath.Join(dir, "system.yaml")
	os.WriteFile(sys, renderSystemCompose("/srv/lwd", "caddy:2"), 0o644)
	if out, err := exec.Command("docker", "compose", "-f", sys, "config").CombinedOutput(); err != nil {
		t.Fatalf("system compose: %v\n%s", err, out)
	}
}
