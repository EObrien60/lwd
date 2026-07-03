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
