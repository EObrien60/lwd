// Command lwd is the lightweight deploy engine: daemon + CLI in one binary.
package main

import (
	"fmt"
	"os"

	"lwd/internal/cli"
	"lwd/internal/version"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "version" {
		fmt.Println("lwd", version.String)
		return
	}
	os.Exit(cli.Run(os.Args[1:]))
}
