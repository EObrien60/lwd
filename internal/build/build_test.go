package build

import (
	"context"
	"testing"
)

// TestFakeBuildRecords ensures the Fake records calls with the expected context/dockerfile/tag.
func TestFakeBuildRecords(t *testing.T) {
	fake := NewFake()
	ctx := context.Background()

	err := fake.Build(ctx, "/path/to/context", "Dockerfile", "myimage:latest")
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	if len(fake.Calls) != 1 {
		t.Errorf("expected 1 call, got %d", len(fake.Calls))
	}
	if fake.LastContext != "/path/to/context" {
		t.Errorf("expected LastContext '/path/to/context', got %q", fake.LastContext)
	}
	if fake.LastDockerfile != "Dockerfile" {
		t.Errorf("expected LastDockerfile 'Dockerfile', got %q", fake.LastDockerfile)
	}
	if fake.LastTag != "myimage:latest" {
		t.Errorf("expected LastTag 'myimage:latest', got %q", fake.LastTag)
	}
}

// TestFakeBuildError ensures the Fake returns BuildErr when set.
func TestFakeBuildError(t *testing.T) {
	fake := NewFake()
	fake.BuildErr = &fakeError{msg: "build failed"}
	ctx := context.Background()

	err := fake.Build(ctx, "/path/to/context", "Dockerfile", "myimage:latest")
	if err != fake.BuildErr {
		t.Errorf("expected BuildErr, got %v", err)
	}
}

// TestFakeImageExistsTrue ensures ImageExists returns true when tag is in Exists map.
func TestFakeImageExistsTrue(t *testing.T) {
	fake := NewFake()
	fake.Exists = map[string]bool{
		"myimage:latest": true,
		"other:v1":       false,
	}
	ctx := context.Background()

	exists, err := fake.ImageExists(ctx, "myimage:latest")
	if err != nil {
		t.Fatalf("ImageExists failed: %v", err)
	}
	if !exists {
		t.Errorf("expected ImageExists to return true for 'myimage:latest'")
	}
}

// TestFakeImageExistsFalse ensures ImageExists returns false when tag is in Exists map as false.
func TestFakeImageExistsFalse(t *testing.T) {
	fake := NewFake()
	fake.Exists = map[string]bool{
		"myimage:latest": true,
		"other:v1":       false,
	}
	ctx := context.Background()

	exists, err := fake.ImageExists(ctx, "other:v1")
	if err != nil {
		t.Fatalf("ImageExists failed: %v", err)
	}
	if exists {
		t.Errorf("expected ImageExists to return false for 'other:v1'")
	}
}

// TestFakeImageExistsNotFound ensures ImageExists returns false for unknown tags.
func TestFakeImageExistsNotFound(t *testing.T) {
	fake := NewFake()
	fake.Exists = map[string]bool{}
	ctx := context.Background()

	exists, err := fake.ImageExists(ctx, "unknown:tag")
	if err != nil {
		t.Fatalf("ImageExists failed: %v", err)
	}
	if exists {
		t.Errorf("expected ImageExists to return false for unknown tag")
	}
}

// TestFakeImageExistsError ensures ImageExists returns ExistsErr when set.
func TestFakeImageExistsError(t *testing.T) {
	fake := NewFake()
	fake.ExistsErr = &fakeError{msg: "docker error"}
	ctx := context.Background()

	exists, err := fake.ImageExists(ctx, "myimage:latest")
	if err != fake.ExistsErr {
		t.Errorf("expected ExistsErr, got %v", err)
	}
	if exists {
		t.Errorf("expected exists to be false when error returned")
	}
}

// TestBuilderInterface ensures both CLI and Fake implement the Builder interface.
func TestBuilderInterface(t *testing.T) {
	var _ Builder = (*CLI)(nil)
	var _ Builder = (*Fake)(nil)
}

// fakeError is a simple error for testing.
type fakeError struct {
	msg string
}

func (e *fakeError) Error() string {
	return e.msg
}
