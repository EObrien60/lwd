// Package node is the lwd node agent: it executes deployment bundles on the
// local host with Docker Compose and Caddy. See docs/lwd2/DESIGN.md.
package node

import "errors"

// Main runs `lwd node` with the remaining arguments.
func Main(args []string) error { return errors.New("lwd node: not implemented") }
