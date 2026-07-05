// Command lwd-agent is the dumb per-node worker: it binds to the node's
// WireGuard mesh interface (or wherever LWD_AGENT_ADDR points) and exposes
// node.Node's Docker primitives over an authed HTTP API. It performs no
// orchestration of its own — every request is delegated straight through to
// a local node.Node (node.NewLocal()).
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"lwd/internal/agent"
	"lwd/internal/node"
	"lwd/internal/version"
)

// defaultAddr is used when LWD_AGENT_ADDR is unset.
const defaultAddr = ":8078"

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "version" {
		fmt.Println("lwd-agent", version.String)
		return
	}

	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)

	token := os.Getenv("LWD_AGENT_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "lwd-agent: LWD_AGENT_TOKEN is required")
		os.Exit(1)
	}

	addr := os.Getenv("LWD_AGENT_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	n, err := node.NewLocal()
	if err != nil {
		fmt.Fprintln(os.Stderr, "lwd-agent:", err)
		os.Exit(1)
	}

	srv := agent.NewServer(n, token)

	log.Printf("lwd-agent: listening on %s", addr)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, "lwd-agent:", err)
		os.Exit(1)
	}
}
