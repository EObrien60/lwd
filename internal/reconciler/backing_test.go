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

	dbIdx := strings.Index(yaml, "db:")
	minioIdx := strings.Index(yaml, "minio:")
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

	if !strings.Contains(yaml, "lwd-myapp:") {
		t.Errorf("expected top-level network lwd-myapp, got:\n%s", yaml)
	}
	if strings.Count(yaml, "lwd-myapp") < 3 {
		t.Errorf("expected lwd-myapp referenced by top-level network + both services, got:\n%s", yaml)
	}

	if !strings.Contains(yaml, "db-data:") {
		t.Errorf("expected top-level named volume db-data, got:\n%s", yaml)
	}
	if !strings.Contains(yaml, "minio-data:") {
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
