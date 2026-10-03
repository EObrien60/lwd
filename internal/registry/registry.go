// Package registry pins image references to content digests. Releases are
// immutable, so every tag is resolved exactly once, at release creation, and
// the digest is what nodes pull from then on.
package registry

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Resolver turns "repo:tag" or "repo@sha256:..." into "repo@sha256:...".
type Resolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// CheckRef applies the release rules without touching the network: the
// reference must parse and carry an explicit tag other than "latest", or a
// digest. "latest" (explicit or implied) is refused because it names
// whatever was pushed last, which is the opposite of a release.
func CheckRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("invalid image reference: empty")
	}
	if _, err := name.ParseReference(ref); err != nil {
		return fmt.Errorf("invalid image reference %q: %v", ref, err)
	}
	if strings.Contains(ref, "@") {
		return nil
	}
	last := ref[strings.LastIndex(ref, "/")+1:]
	i := strings.LastIndex(last, ":")
	if i < 0 {
		return fmt.Errorf("image reference %q needs an explicit tag or digest", ref)
	}
	if last[i+1:] == "latest" {
		return fmt.Errorf("image reference %q: tag latest is refused; use an immutable tag", ref)
	}
	return nil
}

// Remote resolves against real registries using the Docker credential
// keychain (~/.docker/config.json, credential helpers), falling back to
// anonymous access when the keychain's credentials are rejected.
type Remote struct {
	insecure map[string]bool
}

// NewRemote returns a resolver; registries listed in insecure (host[:port])
// are spoken to over plain HTTP. Development only.
func NewRemote(insecure []string) *Remote {
	r := &Remote{insecure: map[string]bool{}}
	for _, h := range insecure {
		if h = strings.TrimSpace(h); h != "" {
			r.insecure[h] = true
		}
	}
	return r
}

// Resolve returns repo@digest for ref. Digest references are not trusted
// blindly: they are checked with a HEAD request so a typo fails at release
// time rather than at deploy time on the node.
func (r *Remote) Resolve(ctx context.Context, ref string) (string, error) {
	if err := CheckRef(ref); err != nil {
		return "", err
	}
	parsed, err := r.reference(ref)
	if err != nil {
		return "", err
	}
	desc, err := remote.Head(parsed, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		var anonErr error
		desc, anonErr = remote.Head(parsed, remote.WithContext(ctx), remote.WithAuth(authn.Anonymous))
		if anonErr != nil {
			return "", fmt.Errorf("resolve %s: %v", ref, err)
		}
	}
	return parsed.Context().Name() + "@" + desc.Digest.String(), nil
}

// reference parses ref, allowing plain HTTP only for opted-in registries.
func (r *Remote) reference(ref string) (name.Reference, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil || !r.insecure[parsed.Context().RegistryStr()] {
		return parsed, err
	}
	return name.ParseReference(ref, name.Insecure)
}

// Fake is an in-memory Resolver for tests: ref → pinned ref. It applies the
// same CheckRef rules as Remote.
type Fake map[string]string

// Resolve implements Resolver.
func (f Fake) Resolve(_ context.Context, ref string) (string, error) {
	if err := CheckRef(ref); err != nil {
		return "", err
	}
	if d, ok := f[ref]; ok {
		return d, nil
	}
	return "", fmt.Errorf("resolve %s: manifest unknown", ref)
}
