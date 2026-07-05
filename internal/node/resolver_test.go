package node

import (
	"fmt"
	"testing"
)

func TestRegistryResolverLocalEmptyAndNamed(t *testing.T) {
	local := NewFake()
	rr := NewRegistryResolver(local, func(name string) (string, string, bool, error) {
		t.Fatalf("lookup should not be called for a local node name, got %q", name)
		return "", "", false, nil
	})

	for _, name := range []string{"", "local"} {
		n, err := rr.Resolve(name)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", name, err)
		}
		if n != local {
			t.Errorf("Resolve(%q) = %v, want the local node", name, n)
		}

		n, meshAddr, isLocal, err := rr.ResolveMeta(name)
		if err != nil {
			t.Fatalf("ResolveMeta(%q): %v", name, err)
		}
		if n != local || meshAddr != "" || !isLocal {
			t.Errorf("ResolveMeta(%q) = (%v, %q, %v), want (local, \"\", true)", name, n, meshAddr, isLocal)
		}
	}
}

func TestRegistryResolverRemoteIsCached(t *testing.T) {
	local := NewFake()
	var calls int
	rr := NewRegistryResolver(local, func(name string) (string, string, bool, error) {
		calls++
		if name != "web1" {
			t.Fatalf("lookup called with unexpected name %q", name)
		}
		return "deploy@web1", "100.64.0.2", true, nil
	})

	n1, meshAddr, isLocal, err := rr.ResolveMeta("web1")
	if err != nil {
		t.Fatalf("ResolveMeta: %v", err)
	}
	if n1 == nil {
		t.Fatal("want a non-nil node for a registered remote name")
	}
	if isLocal {
		t.Error("want isLocal=false for a registered remote name")
	}
	if meshAddr != "100.64.0.2" {
		t.Errorf("meshAddr = %q, want 100.64.0.2", meshAddr)
	}
	if calls != 1 {
		t.Fatalf("lookup calls = %d, want 1", calls)
	}

	n2, err := rr.Resolve("web1")
	if err != nil {
		t.Fatalf("Resolve (second time): %v", err)
	}
	if n2 != n1 {
		t.Error("want the cached node instance returned on a repeat Resolve")
	}
	if calls != 1 {
		t.Errorf("lookup calls = %d, want still 1 (cached, lookup not called again)", calls)
	}

	// ResolveMeta should also hit the cache (and still report the cached
	// mesh address) rather than calling lookup again.
	n3, meshAddr2, isLocal2, err := rr.ResolveMeta("web1")
	if err != nil {
		t.Fatalf("ResolveMeta (second time): %v", err)
	}
	if n3 != n1 || meshAddr2 != "100.64.0.2" || isLocal2 {
		t.Errorf("ResolveMeta (cached) = (%v, %q, %v), want (%v, 100.64.0.2, false)", n3, meshAddr2, isLocal2, n1)
	}
	if calls != 1 {
		t.Errorf("lookup calls = %d, want still 1 (cached, lookup not called again)", calls)
	}
}

func TestRegistryResolverInvalidate(t *testing.T) {
	local := NewFake()
	sshHost := "deploy@web1"
	var calls int
	rr := NewRegistryResolver(local, func(name string) (string, string, bool, error) {
		calls++
		return sshHost, "100.64.0.2", true, nil
	})

	n1, err := rr.Resolve("web1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if calls != 1 {
		t.Fatalf("lookup calls = %d, want 1", calls)
	}
	n1Again, err := rr.Resolve("web1")
	if err != nil {
		t.Fatalf("Resolve (cached): %v", err)
	}
	if n1Again != n1 || calls != 1 {
		t.Fatalf("expected cached node with no extra lookup, calls = %d", calls)
	}

	rr.Invalidate("web1")

	// A changed ssh_host is picked up: lookup runs again and a fresh node is
	// built rather than the stale cached one being returned.
	sshHost = "deploy@web1-new"
	n2, err := rr.Resolve("web1")
	if err != nil {
		t.Fatalf("Resolve after invalidate: %v", err)
	}
	if calls != 2 {
		t.Fatalf("lookup calls after invalidate = %d, want 2 (lookup re-run)", calls)
	}
	if n2 == n1 {
		t.Error("want a freshly built node after Invalidate, not the stale cached instance")
	}

	// Invalidating a name with no cached entry (never resolved, or local) is
	// a harmless no-op.
	rr.Invalidate("never-resolved")
	rr.Invalidate("local")
}

func TestRegistryResolverUnknownNode(t *testing.T) {
	local := NewFake()
	rr := NewRegistryResolver(local, func(name string) (string, string, bool, error) {
		return "", "", false, nil
	})
	if _, err := rr.Resolve("ghost"); err == nil {
		t.Fatal("want an error for an unregistered node name")
	}
}

func TestRegistryResolverLookupError(t *testing.T) {
	local := NewFake()
	wantErr := fmt.Errorf("store unavailable")
	rr := NewRegistryResolver(local, func(name string) (string, string, bool, error) {
		return "", "", false, wantErr
	})
	_, err := rr.Resolve("web1")
	if err == nil {
		t.Fatal("want the lookup error propagated")
	}
}

func TestFakeResolver(t *testing.T) {
	local := NewFake()
	web1 := NewFake()
	fr := FakeResolver{"local": local, "web1": web1}

	got, err := fr.Resolve("local")
	if err != nil || got != local {
		t.Fatalf("Resolve(local) = (%v, %v), want (%v, nil)", got, err, local)
	}

	got, err = fr.Resolve("web1")
	if err != nil || got != web1 {
		t.Fatalf("Resolve(web1) = (%v, %v), want (%v, nil)", got, err, web1)
	}

	if _, err := fr.Resolve("missing"); err == nil {
		t.Fatal("want an error for an unmapped node name")
	}
}

// TestFakeResolverResolveMeta covers the local/remote metadata FakeResolver
// derives for the reconciler's mesh-address routing tests: "local" is always
// isLocal=true with no mesh address, and a non-local name reports the mapped
// Fake's MeshAddr field.
func TestFakeResolverResolveMeta(t *testing.T) {
	local := NewFake()
	web1 := NewFake()
	web1.MeshAddr = "100.64.0.2"
	fr := FakeResolver{"local": local, "web1": web1}

	n, meshAddr, isLocal, err := fr.ResolveMeta("local")
	if err != nil || n != local || meshAddr != "" || !isLocal {
		t.Fatalf("ResolveMeta(local) = (%v, %q, %v, %v), want (local, \"\", true, nil)", n, meshAddr, isLocal, err)
	}

	n, meshAddr, isLocal, err = fr.ResolveMeta("web1")
	if err != nil || n != web1 || meshAddr != "100.64.0.2" || isLocal {
		t.Fatalf("ResolveMeta(web1) = (%v, %q, %v, %v), want (web1, 100.64.0.2, false, nil)", n, meshAddr, isLocal, err)
	}

	if _, _, _, err := fr.ResolveMeta("missing"); err == nil {
		t.Fatal("want an error for an unmapped node name")
	}
}
