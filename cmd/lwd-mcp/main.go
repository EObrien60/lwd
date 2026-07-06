// Command lwd-mcp is a Model Context Protocol (MCP) server: an agent
// launches it over stdio, and it exposes lwd's daemon operations (list,
// deploy, roll back, logs, secrets, ...) as MCP tools by calling the daemon,
// exactly as the CLI does. By default it dials `lwd daemon`'s unix socket on
// the same box; set LWD_DAEMON (+ optionally LWD_API_TOKEN) to point it at a
// remote daemon over TCP instead (see client.FromEnv). It makes no daemon
// changes and opens no network listener of its own.
//
// stdout is the MCP JSON-RPC channel: nothing but protocol frames may be
// written there. All diagnostic logging goes to stderr.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"lwd/internal/client"
	"lwd/internal/mcp"
	"lwd/internal/version"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "version" {
		fmt.Println("lwd-mcp", version.String)
		return
	}

	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)

	c := client.FromEnv()
	srv := mcp.NewServer(c)

	log.Printf("lwd-mcp: serving MCP over stdio")
	if err := srv.Serve(context.Background()); err != nil {
		log.Printf("lwd-mcp: %v", err)
		os.Exit(1)
	}
}
