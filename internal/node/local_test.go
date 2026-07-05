package node

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

// This test requires a real Docker daemon and network access to pull an image.
// Run with: LWD_DOCKER_TEST=1 go test ./internal/node/ -run TestLocal -v
func TestLocalRunRemove(t *testing.T) {
	if os.Getenv("LWD_DOCKER_TEST") == "" {
		t.Skip("set LWD_DOCKER_TEST=1 to run Docker integration tests")
	}
	l, err := NewLocal()
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := l.EnsureImage(ctx, "traefik/whoami:latest"); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	c, err := l.RunContainer(ctx, RunSpec{
		Name:   "lwd-itest-whoami",
		Image:  "traefik/whoami:latest",
		Labels: map[string]string{"lwd.app": "itest"},
		Port:   80,
	})
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}
	defer l.RemoveContainer(ctx, c.ID)

	if err := l.Health(ctx, c, HealthSpec{Path: "/", Timeout: 20 * time.Second}); err != nil {
		t.Fatalf("Health: %v", err)
	}
	got, err := l.ListContainers(ctx, map[string]string{"lwd.app": "itest"})
	if err != nil || len(got) == 0 {
		t.Fatalf("ListContainers = %+v, err %v", got, err)
	}
}

// TestApplyResourceLimits verifies the pure helper that translates RunSpec's
// CPUs/MemoryBytes into Docker's HostConfig.Resources fields, without needing
// a Docker daemon.
func TestApplyResourceLimits(t *testing.T) {
	hostCfg := &container.HostConfig{}
	applyResourceLimits(hostCfg, 0.5, 536870912)
	if hostCfg.Resources.NanoCPUs != 500000000 {
		t.Errorf("NanoCPUs = %d, want 500000000", hostCfg.Resources.NanoCPUs)
	}
	if hostCfg.Resources.Memory != 536870912 {
		t.Errorf("Memory = %d, want 536870912", hostCfg.Resources.Memory)
	}

	hostCfg2 := &container.HostConfig{}
	applyResourceLimits(hostCfg2, 0, 0)
	if hostCfg2.Resources.NanoCPUs != 0 {
		t.Errorf("NanoCPUs = %d, want 0 (no limit)", hostCfg2.Resources.NanoCPUs)
	}
	if hostCfg2.Resources.Memory != 0 {
		t.Errorf("Memory = %d, want 0 (no limit)", hostCfg2.Resources.Memory)
	}
}

// TestRunSpecCarriesLimits verifies RunSpec's new CPUs/MemoryBytes fields
// survive a RunContainer call, using the Fake so no Docker daemon is needed.
func TestRunSpecCarriesLimits(t *testing.T) {
	fake := NewFake()
	ctx := context.Background()
	_, err := fake.RunContainer(ctx, RunSpec{
		Name:        "limited",
		Image:       "test:latest",
		CPUs:        0.5,
		MemoryBytes: 536870912,
	})
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}
	if fake.LastRunSpec.CPUs != 0.5 {
		t.Errorf("LastRunSpec.CPUs = %v, want 0.5", fake.LastRunSpec.CPUs)
	}
	if fake.LastRunSpec.MemoryBytes != 536870912 {
		t.Errorf("LastRunSpec.MemoryBytes = %d, want 536870912", fake.LastRunSpec.MemoryBytes)
	}
}
