// Command lwd-web is the browser dashboard for lwd: a separate binary that
// authenticates with a single shared password, serves the embedded UI, and
// proxies browser requests to the lwd daemon over its unix socket.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"lwd/internal/client"
	"lwd/internal/version"
	"lwd/internal/web"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "version" {
		fmt.Println("lwd-web", version.String)
		return
	}

	cfg, err := web.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "lwd-web:", err)
		os.Exit(1)
	}

	c := client.New(cfg.SocketPath)
	auth := web.NewAuthenticator(cfg.SigningKey, cfg.Password)
	srv := web.NewServer(c, auth)

	log.Printf("lwd-web: listening on %s (daemon socket %s)", cfg.Addr, cfg.SocketPath)
	if err := http.ListenAndServe(cfg.Addr, srv.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, "lwd-web:", err)
		os.Exit(1)
	}
}
