package source

import (
	"context"
)

// Fake is a testable Git implementation that records calls and returns
// configurable results.
type Fake struct {
	// SHA is the commit SHA to return from Clone. Defaults to "abc123def456".
	SHA string
	// Err is the error to return from Clone. If set, Clone returns ("", Err).
	Err error

	// Calls records all Clone calls as "url:ref:dir" strings.
	Calls []string
	// LastURL, LastRef, LastDir record the most recent Clone arguments.
	LastURL string
	LastRef string
	LastDir string
}

// NewFake returns a ready-to-use Fake Git with default SHA.
func NewFake() *Fake {
	return &Fake{
		SHA: "abc123def456",
	}
}

var _ Git = (*Fake)(nil)

// Clone records the call and returns the configured SHA and Err.
func (f *Fake) Clone(ctx context.Context, url, ref, dir string) (sha string, err error) {
	f.Calls = append(f.Calls, url+":"+ref+":"+dir)
	f.LastURL = url
	f.LastRef = ref
	f.LastDir = dir

	if f.Err != nil {
		return "", f.Err
	}

	return f.SHA, nil
}
