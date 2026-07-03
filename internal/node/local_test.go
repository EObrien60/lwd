package node

import (
	"context"
	"os"
	"testing"
	"time"
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
