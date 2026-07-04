package build

import "context"

// Fake is a test fake for the Builder interface. It records all calls and allows
// configurable return values for testing.
type Fake struct {
	// BuildErr is returned by Build if non-nil.
	BuildErr error

	// Exists maps image tags to whether they exist. If a tag is not in the map,
	// ImageExists returns false.
	Exists map[string]bool

	// ExistsErr is returned by ImageExists if non-nil.
	ExistsErr error

	// Calls records the method calls made (for debugging).
	Calls []string

	// LastContext, LastDockerfile, LastTag record the last Build call's arguments.
	LastContext    string
	LastDockerfile string
	LastTag        string
}

// NewFake returns a new Fake builder.
func NewFake() *Fake {
	return &Fake{
		Exists: make(map[string]bool),
		Calls:  make([]string, 0),
	}
}

var _ Builder = (*Fake)(nil)

// Build records the arguments and returns BuildErr if set.
func (f *Fake) Build(ctx context.Context, contextDir, dockerfile, tag string) error {
	f.Calls = append(f.Calls, "Build")
	f.LastContext = contextDir
	f.LastDockerfile = dockerfile
	f.LastTag = tag
	return f.BuildErr
}

// ImageExists returns whether the tag is in the Exists map, or ExistsErr if set.
func (f *Fake) ImageExists(ctx context.Context, tag string) (bool, error) {
	f.Calls = append(f.Calls, "ImageExists")
	if f.ExistsErr != nil {
		return false, f.ExistsErr
	}
	return f.Exists[tag], nil
}
