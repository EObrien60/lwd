// Command lwd is the lightweight deploy engine: daemon + CLI in one binary.
package main

import (
	"fmt"
	"os"

	"lwd/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println("lwd", version.String)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: lwd <command> [args]

commands:
  daemon            run the lwd daemon
  apply <dir>       deploy the app defined in <dir>/lwd.toml
  ls                list apps and status
  logs <app> [-f]   stream an app's logs
  rm <app>          stop and deregister an app
  version           print version`)
}
