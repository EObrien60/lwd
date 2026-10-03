package registry

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// startRegistry runs an in-memory OCI registry over plain HTTP and pushes one
// random image as <host>/app/web:v1, returning the host and the image digest.
func startRegistry(t *testing.T) (host, digest string) {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	host = u.Host

	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(host+"/app/web:v1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return host, d.String()
}

func TestRemoteResolveTag(t *testing.T) {
	host, digest := startRegistry(t)
	r := NewRemote([]string{host})
	got, err := r.Resolve(context.Background(), host+"/app/web:v1")
	if err != nil {
		t.Fatal(err)
	}
	if want := host + "/app/web@" + digest; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRemoteResolveDigestVerifiesExistence(t *testing.T) {
	host, digest := startRegistry(t)
	r := NewRemote([]string{host})
	got, err := r.Resolve(context.Background(), host+"/app/web@"+digest)
	if err != nil {
		t.Fatal(err)
	}
	if got != host+"/app/web@"+digest {
		t.Errorf("got %q", got)
	}
	missing := host + "/app/web@sha256:" + strings.Repeat("0", 64)
	if _, err := r.Resolve(context.Background(), missing); err == nil {
		t.Error("expected error for a digest the registry does not have")
	}
}

func TestRemoteResolveUnknownTag(t *testing.T) {
	host, _ := startRegistry(t)
	r := NewRemote([]string{host})
	_, err := r.Resolve(context.Background(), host+"/app/web:v2")
	if err == nil || !strings.Contains(err.Error(), host+"/app/web:v2") {
		t.Errorf("expected error naming the ref, got %v", err)
	}
}

func TestRemoteInsecureOptIn(t *testing.T) {
	// go-containerregistry already allows HTTP for loopback, so check scheme
	// selection directly for a non-loopback registry.
	const ref = "registry.dev.internal:5000/app/web:v1"
	secure, err := NewRemote(nil).reference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if s := secure.Context().Scheme(); s != "https" {
		t.Errorf("default scheme = %s, want https", s)
	}
	insecure, err := NewRemote([]string{" registry.dev.internal:5000 "}).reference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if s := insecure.Context().Scheme(); s != "http" {
		t.Errorf("opted-in scheme = %s, want http", s)
	}
}

func TestCheckRef(t *testing.T) {
	cases := []struct {
		ref, want string // want "" = ok
	}{
		{"ghcr.io/o/web:v1", ""},
		{"localhost:5000/o/web:1.2.3", ""},
		{"ghcr.io/o/web@sha256:" + strings.Repeat("a", 64), ""},
		{"ghcr.io/o/web", "explicit tag"},
		{"localhost:5000/o/web", "explicit tag"},
		{"ghcr.io/o/web:latest", "latest"},
		{"ghcr.io/o/web:", "invalid"},
		{"", "invalid"},
		{"UPPER/x:1", "invalid"},
	}
	for _, tc := range cases {
		err := CheckRef(tc.ref)
		if tc.want == "" {
			if err != nil {
				t.Errorf("CheckRef(%q) = %v", tc.ref, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("CheckRef(%q) = %v, want error containing %q", tc.ref, err, tc.want)
		}
	}
}

func TestRemoteRejectsLatestWithoutNetwork(t *testing.T) {
	if _, err := NewRemote(nil).Resolve(context.Background(), "ghcr.io/o/web:latest"); err == nil {
		t.Error("expected latest to be refused")
	}
}

func TestFake(t *testing.T) {
	f := Fake{"r/web:v1": "r/web@sha256:" + strings.Repeat("1", 64)}
	got, err := f.Resolve(context.Background(), "r/web:v1")
	if err != nil || got != "r/web@sha256:"+strings.Repeat("1", 64) {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := f.Resolve(context.Background(), "r/web:v2"); err == nil {
		t.Error("expected unknown ref error")
	}
	if _, err := f.Resolve(context.Background(), "r/web:latest"); err == nil || !strings.Contains(err.Error(), "latest") {
		t.Errorf("fake must apply the same tag rules, got %v", err)
	}
}
