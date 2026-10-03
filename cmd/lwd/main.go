// Command lwd is the OBH application platform: CLI, controller and node agent
// in one binary. See docs/lwd2/DESIGN.md.
package main

import (
	"fmt"
	"os"

	"lwd/internal/cli"
	"lwd/internal/node"
	"lwd/internal/version"
)

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "version":
			fmt.Println("lwd", version.String)
			return
		case "node":
			if err := node.Main(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "lwd node:", err)
				os.Exit(1)
			}
			return
		}
	}
	os.Exit(cli.Run(os.Args[1:]))
}
