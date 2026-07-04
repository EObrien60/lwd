package reconciler

import (
	"strings"
	"testing"

	"lwd/internal/spec"
)

func TestRenderBackingComposeTwoServices(t *testing.T) {
	services := []spec.Service{
		{
			Name:   "db",
			Image:  "postgres:16",
			Env:    map[string]string{"POSTGRES_DB": "app"},
			Volume: "db-data:/var/lib/postgresql/data",
		},
		{
			Name:    "minio",
			Image:   "minio/minio:latest",
			Command: "server /data",
			Secrets: []string{"MINIO_ROOT_PASSWORD"},
			Volume:  "minio-data:/data",
		},
	}
	resolvedSecrets := map[string]string{"MINIO_ROOT_PASSWORD": "s3cr3t\"quote"}

	yaml, network := RenderBackingCompose("myapp", services, resolvedSecrets)

	if network != "lwd-myapp" {
		t.Fatalf("network = %q, want %q", network, "lwd-myapp")
	}
	if yaml == "" {
		t.Fatal("expected non-empty yaml")
	}

	dbIdx := strings.Index(yaml, `"db":`)
	minioIdx := strings.Index(yaml, `"minio":`)
	if dbIdx == -1 || minioIdx == -1 {
		t.Fatalf("expected both services present, db=%d minio=%d\n%s", dbIdx, minioIdx, yaml)
	}
	if dbIdx > minioIdx {
		t.Fatalf("expected db before minio (sorted), got:\n%s", yaml)
	}

	if !strings.Contains(yaml, "restart: unless-stopped") {
		t.Errorf("expected restart: unless-stopped, got:\n%s", yaml)
	}
	if strings.Count(yaml, "restart: unless-stopped") != 2 {
		t.Errorf("expected restart: unless-stopped on both services, got:\n%s", yaml)
	}

	if !strings.Contains(yaml, `"lwd-myapp":`) {
		t.Errorf("expected top-level network lwd-myapp, got:\n%s", yaml)
	}
	if strings.Count(yaml, "lwd-myapp") < 3 {
		t.Errorf("expected lwd-myapp referenced by top-level network + both services, got:\n%s", yaml)
	}

	if !strings.Contains(yaml, `"db-data":`) {
		t.Errorf("expected top-level named volume db-data, got:\n%s", yaml)
	}
	if !strings.Contains(yaml, `"minio-data":`) {
		t.Errorf("expected top-level named volume minio-data, got:\n%s", yaml)
	}

	if !strings.Contains(yaml, `s3cr3t\"quote`) {
		t.Errorf("expected injected secret value present (escaped), got:\n%s", yaml)
	}

	if strings.Contains(yaml, "ports:") {
		t.Errorf("expected NO ports published, got:\n%s", yaml)
	}
}

func TestRenderBackingComposeEmpty(t *testing.T) {
	yaml, network := RenderBackingCompose("myapp", nil, nil)
	if yaml != "" || network != "" {
		t.Fatalf("expected empty results, got yaml=%q network=%q", yaml, network)
	}
}

func TestRenderBackingDeterministic(t *testing.T) {
	services := []spec.Service{
		{Name: "minio", Image: "minio/minio:latest", Command: "server /data", Secrets: []string{"MINIO_ROOT_PASSWORD"}, Volume: "minio-data:/data"},
		{Name: "db", Image: "postgres:16", Env: map[string]string{"POSTGRES_DB": "app", "POSTGRES_USER": "app"}, Volume: "db-data:/var/lib/postgresql/data"},
	}
	resolvedSecrets := map[string]string{"MINIO_ROOT_PASSWORD": "s3cr3t"}

	yaml1, net1 := RenderBackingCompose("myapp", services, resolvedSecrets)
	yaml2, net2 := RenderBackingCompose("myapp", services, resolvedSecrets)

	if yaml1 != yaml2 {
		t.Fatalf("expected deterministic output, got:\n---1---\n%s\n---2---\n%s", yaml1, yaml2)
	}
	if net1 != net2 {
		t.Fatalf("expected deterministic network, got %q vs %q", net1, net2)
	}
}

// TestRenderBackingRejectsKeyInjection verifies a malicious env key cannot
// inject additional YAML structure (e.g. a real ports: mapping key) into
// the rendered backing compose document. Before the fix, env keys were
// emitted unquoted as YAML mapping keys, so a key containing a newline
// followed by valid YAML could add arbitrary sibling keys to the service.
//
// Note: the malicious key's literal text (the word "ports:") is still
// present somewhere in the output once neutralized — it is now embedded,
// escaped, inside one quoted scalar. That is safe and expected: a plain
// substring search for "ports:" is not the right invariant here (it would
// always find the attacker's own text). What matters is structural: the
// payload must not become a standalone "ports:" YAML key line or a
// standalone published-port list item, and the whole malicious key/value
// pair must collapse onto a single line (its embedded newlines escaped,
// not raw) so it cannot straddle YAML structure.
func TestRenderBackingRejectsKeyInjection(t *testing.T) {
	maliciousKey := "X: 1\n    ports:\n      - \"9999:9999\""
	services := []spec.Service{
		{
			Name:  "db",
			Image: "postgres",
			Env:   map[string]string{maliciousKey: "v"},
		},
	}

	yaml, _ := RenderBackingCompose("myapp", services, nil)

	for _, line := range strings.Split(yaml, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "ports:" {
			t.Fatalf("malicious env key injected a real ports: YAML key line, got:\n%s", yaml)
		}
		if trimmed == `- "9999:9999"` {
			t.Fatalf("malicious env key injected a real published-port list item, got:\n%s", yaml)
		}
	}

	// The malicious key/value must render as a single quoted-scalar
	// mapping entry with its embedded newlines escaped (\n, two chars),
	// not raw newlines that would fold into new YAML lines.
	if !strings.Contains(yaml, `"X: 1\n    ports:\n      - \"9999:9999\"": "v"`) {
		t.Fatalf("expected malicious key neutralized as a single escaped quoted scalar, got:\n%s", yaml)
	}
}

// TestRenderBackingEscapesDollar verifies a literal $ in an env value is
// doubled so docker compose's own variable-interpolation pass (which runs
// over the rendered file text) does not re-interpolate it.
func TestRenderBackingEscapesDollar(t *testing.T) {
	services := []spec.Service{
		{
			Name:  "db",
			Image: "postgres",
			Env:   map[string]string{"PASSWORD": "p@ss$word"},
		},
	}

	yaml, _ := RenderBackingCompose("myapp", services, nil)

	if !strings.Contains(yaml, "p@ss$$word") {
		t.Fatalf("expected doubled $ in rendered value, got:\n%s", yaml)
	}
}

// TestRenderBackingEscapesNewline verifies a real newline embedded in an
// env value is rendered as the two-character escape sequence \n inside the
// quoted scalar, rather than a raw newline (which would fold into
// additional YAML lines/structure).
func TestRenderBackingEscapesNewline(t *testing.T) {
	services := []spec.Service{
		{
			Name:  "db",
			Image: "postgres",
			Env:   map[string]string{"MULTILINE": "line1\nline2"},
		},
	}

	yaml, _ := RenderBackingCompose("myapp", services, nil)

	if !strings.Contains(yaml, `line1\nline2`) {
		t.Fatalf("expected escaped \\n in rendered value, got:\n%s", yaml)
	}
	if strings.Contains(yaml, "line1\nline2") {
		t.Fatalf("expected no raw embedded newline in rendered value, got:\n%s", yaml)
	}
}
