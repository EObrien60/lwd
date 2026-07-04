package source

import (
	"context"
	"errors"
	"strings"
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

// TestGitEnvSetsAllowProtocol verifies that every git subprocess (via run,
// used by every git invocation in Clone) gets GIT_ALLOW_PROTOCOL restricting
// git to the transports lwd needs (http/https/git/ssh for real remotes, file
// for local clones — including this repo's own git e2e test) and excluding
// ext:: (and any other unlisted transport), which git would otherwise use to
// run an arbitrary host command supplied via the url. This is the
// defense-in-depth layer below spec.Validate's URL/ref checks.
func TestGitEnvSetsAllowProtocol(t *testing.T) {
	env := gitEnv()

	var allowProtocol string
	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_ALLOW_PROTOCOL=") {
			allowProtocol = strings.TrimPrefix(kv, "GIT_ALLOW_PROTOCOL=")
			found = true
			break
		}
	}
	if !found {
		t.Fatal("gitEnv() did not set GIT_ALLOW_PROTOCOL")
	}

	for _, want := range []string{"https", "git", "ssh", "file"} {
		if !containsProtocol(allowProtocol, want) {
			t.Errorf("GIT_ALLOW_PROTOCOL = %q, want it to include %q", allowProtocol, want)
		}
	}
	if containsProtocol(allowProtocol, "ext") {
		t.Errorf("GIT_ALLOW_PROTOCOL = %q, must NOT include ext (command-executing transport)", allowProtocol)
	}

	// gitEnv() must also carry through the rest of the process environment
	// (os.Environ()), not just GIT_ALLOW_PROTOCOL alone, or git would lose
	// PATH/HOME/etc and likely fail to run at all.
	if len(env) < 2 {
		t.Errorf("gitEnv() returned %d entries, want the full process env plus GIT_ALLOW_PROTOCOL", len(env))
	}
}

func containsProtocol(csv, want string) bool {
	for _, p := range strings.Split(csv, ":") {
		if p == want {
			return true
		}
	}
	return false
}

// TestCloneRejectsLeadingDashRef verifies Clone's defensive check: a ref
// starting with '-' is refused before any git command runs, closing the
// option-injection path on `git checkout <ref>` even if spec.Validate's
// ref-charset gate were somehow bypassed.
func TestCloneRejectsLeadingDashRef(t *testing.T) {
	c := NewCLI()
	sha, err := c.Clone(context.Background(), "https://example.com/repo.git", "-x", t.TempDir())
	if err == nil {
		t.Fatal("Clone() with a '-'-leading ref: want error, got nil")
	}
	if sha != "" {
		t.Errorf("Clone() with a '-'-leading ref: sha = %q, want empty", sha)
	}
	if !strings.Contains(err.Error(), "ref") {
		t.Errorf("Clone() error = %v, want it to mention the invalid ref", err)
	}
}
