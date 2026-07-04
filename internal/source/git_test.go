package source

import (
	"context"
	"errors"
	"testing"
)

// TestFakeCloneRecords verifies that Fake records Clone calls and returns its configured SHA.
func TestFakeCloneRecords(t *testing.T) {
	fake := NewFake()
	fake.SHA = "abc123def456"

	ctx := context.Background()
	url := "https://github.com/example/repo"
	ref := "main"
	dir := "/tmp/clone"

	sha, err := fake.Clone(ctx, url, ref, dir)
	if err != nil {
		t.Fatalf("Clone() error = %v, want nil", err)
	}

	if sha != "abc123def456" {
		t.Errorf("Clone() sha = %q, want %q", sha, "abc123def456")
	}

	if len(fake.Calls) != 1 {
		t.Errorf("Clone() recorded %d calls, want 1", len(fake.Calls))
	}

	if fake.LastURL != url {
		t.Errorf("LastURL = %q, want %q", fake.LastURL, url)
	}
	if fake.LastRef != ref {
		t.Errorf("LastRef = %q, want %q", fake.LastRef, ref)
	}
	if fake.LastDir != dir {
		t.Errorf("LastDir = %q, want %q", fake.LastDir, dir)
	}
}

// TestFakeCloneErr verifies that Fake returns its configured error when Err is set.
func TestFakeCloneErr(t *testing.T) {
	fake := NewFake()
	expectedErr := errors.New("clone failed")
	fake.Err = expectedErr

	ctx := context.Background()
	sha, err := fake.Clone(ctx, "https://github.com/example/repo", "main", "/tmp/clone")

	if err != expectedErr {
		t.Errorf("Clone() error = %v, want %v", err, expectedErr)
	}

	if sha != "" {
		t.Errorf("Clone() returned sha = %q, want empty", sha)
	}

	if len(fake.Calls) != 1 {
		t.Errorf("Clone() recorded %d calls, want 1 (even on error)", len(fake.Calls))
	}
}

// Verify interface implementations.
var (
	_ Git = (*CLI)(nil)
	_ Git = (*Fake)(nil)
)
