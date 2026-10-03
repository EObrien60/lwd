// Package version holds build identity for lwd.
package version

// String is the human-readable version of lwd, overridable at build time with
// -ldflags "-X lwd/internal/version.String=...".
var String = "2.0.0-dev"
