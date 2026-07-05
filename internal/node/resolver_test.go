package node

import (
	"fmt"
	"testing"
)

func TestRegistryResolverLocalEmptyAndNamed(t *testing.T) {
	local := NewFake()
	rr := NewRegistryResolver(local, func(name string) (string, bool, error) {
		t.Fatalf("lookup should not be called for a local node name, got %q", name)
		return "", false, nil
	})

	for _, name := range []string{"", "local"} {
		n, err := rr.Resolve(name)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", name, err)
		}
		if n != local {
			t.Errorf("Resolve(%q) = %v, want the local node", name, n)
		}
	}
}

func TestRegistryResolverRemoteIsCached(t *testing.T) {
	local := NewFake()
	var calls int
	rr := NewRegistryResolver(local, func(name string) (string, bool, error) {
		calls++
		if name != "web1" {
			t.Fatalf("lookup called with unexpected name %q", name)
		}
		return "deploy@web1", true, nil
	})

	n1, err := rr.Resolve("web1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if n1 == nil {
		t.Fatal("want a non-nil node for a registered remote name")
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
}

func TestRegistryResolverUnknownNode(t *testing.T) {
	local := NewFake()
	rr := NewRegistryResolver(local, func(name string) (string, bool, error) {
		return "", false, nil
	})
	if _, err := rr.Resolve("ghost"); err == nil {
		t.Fatal("want an error for an unregistered node name")
	}
}

func TestRegistryResolverLookupError(t *testing.T) {
	local := NewFake()
	wantErr := fmt.Errorf("store unavailable")
	rr := NewRegistryResolver(local, func(name string) (string, bool, error) {
		return "", false, wantErr
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
