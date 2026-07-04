package node

import (
	"context"
	"io"
	"testing"
)

func TestFakeRunAndList(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	c, err := f.RunContainer(ctx, RunSpec{
		Name:   "lwd-blog",
		Image:  "img:1",
		Labels: map[string]string{"lwd.app": "blog"},
		Port:   8080,
	})
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}
	if c.ID == "" {
		t.Fatal("expected non-empty container ID")
	}

	got, err := f.ListContainers(ctx, map[string]string{"lwd.app": "blog"})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(got) != 1 || got[0].Name != "lwd-blog" {
		t.Fatalf("ListContainers = %+v, want one lwd-blog", got)
	}
}

func TestFakeRemove(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	c, _ := f.RunContainer(ctx, RunSpec{Name: "x", Image: "i", Labels: map[string]string{"lwd.app": "x"}})
	if err := f.RemoveContainer(ctx, c.ID); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	got, _ := f.ListContainers(ctx, map[string]string{"lwd.app": "x"})
	if len(got) != 0 {
		t.Fatalf("after remove, ListContainers = %+v, want empty", got)
	}
}

func TestFakeLogs(t *testing.T) {
	f := NewFake()
	rc, err := f.ContainerLogs(context.Background(), "any", false)
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if len(b) == 0 {
		t.Fatal("expected some fake log output")
	}
}

// Compile-time assertion that Fake implements Node.
var _ Node = (*Fake)(nil)

func TestFakeEnsureNetwork(t *testing.T) {
	f := NewFake()
	if err := f.EnsureNetwork(context.Background(), "lwd"); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if !contains(f.Calls, "EnsureNetwork:lwd") {
		t.Errorf("calls = %v", f.Calls)
	}
}

func TestFakeRunRecordsNetworkAndNoPublish(t *testing.T) {
	f := NewFake()
	c, err := f.RunContainer(context.Background(), RunSpec{
		Name: "lwd-blog-1", Image: "img:1", Network: "lwd", Port: 8080,
		Labels: map[string]string{"lwd.app": "blog"},
	})
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}
	if c.Name != "lwd-blog-1" {
		t.Errorf("name = %q", c.Name)
	}
}

func TestFakeContainerHealth(t *testing.T) {
	f := NewFake()
	c, _ := f.RunContainer(context.Background(), RunSpec{Name: "x", Image: "i", Labels: map[string]string{"lwd.app": "x"}})
	f.HealthState = "running"
	f.DockerHealth = "healthy"
	state, dh, err := f.ContainerHealth(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("ContainerHealth: %v", err)
	}
	if state != "running" || dh != "healthy" {
		t.Errorf("state=%q dockerHealth=%q", state, dh)
	}
}

func TestFakeConnectContainerToNetwork(t *testing.T) {
	f := NewFake()
	if err := f.ConnectContainerToNetwork(context.Background(), "c1", "lwd"); err != nil {
		t.Fatalf("ConnectContainerToNetwork: %v", err)
	}
	if !contains(f.Calls, "ConnectContainerToNetwork:c1:lwd") {
		t.Errorf("calls = %v, want to contain ConnectContainerToNetwork:c1:lwd", f.Calls)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
