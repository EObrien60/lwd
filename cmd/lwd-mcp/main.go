// Command lwd-mcp is a local Model Context Protocol (MCP) server: an agent
// launches it over stdio, and it exposes lwd's daemon operations (list,
// deploy, roll back, logs, secrets, ...) as MCP tools by calling the daemon
// over its unix socket, exactly as the CLI does. It requires `lwd daemon` to
// already be running on the same box; it makes no daemon changes and opens
// no network listener.
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
	"lwd/internal/config"
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

	socketPath := config.SocketPath()
	c := client.New(socketPath)
	srv := mcp.NewServer(c)

	log.Printf("lwd-mcp: serving MCP over stdio (daemon socket %s)", socketPath)
	if err := srv.Serve(context.Background()); err != nil {
		log.Printf("lwd-mcp: %v", err)
		os.Exit(1)
	}
}
