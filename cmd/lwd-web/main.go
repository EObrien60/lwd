// Command lwd-web is the browser dashboard for lwd: a separate binary that
// authenticates with a single shared password, serves the embedded UI, and
// proxies browser requests to the lwd daemon. By default it dials the
// daemon's local unix socket; set LWD_DAEMON (+ optionally LWD_API_TOKEN) to
// point it at a remote daemon over TCP instead (see client.FromEnv).
package main

import (
	"fmt"
	"log"
	"net"
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

	if !hostIsLoopback(cfg.Addr) {
		log.Printf("lwd-web: WARNING: bound to a non-loopback address (%s) over plain HTTP — put it behind TLS (e.g. Caddy) or an SSH tunnel; the session cookie is marked Secure only behind an X-Forwarded-Proto: https proxy", cfg.Addr)
	}

	c := client.FromEnv()
	auth := web.NewAuthenticator(cfg.SigningKey, cfg.Password)
	srv := web.NewServer(c, auth)

	log.Printf("lwd-web: listening on %s", cfg.Addr)
	if err := http.ListenAndServe(cfg.Addr, srv.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, "lwd-web:", err)
		os.Exit(1)
	}
}

// hostIsLoopback reports whether addr (a "host:port" listen address, as
// passed to http.ListenAndServe) binds only the loopback interface.
// 127.0.0.1, ::1, and localhost are loopback-only and never reachable from
// another machine; a bare ":8079" (empty host, meaning "every interface"),
// "0.0.0.0:8079", a routable IP, or any other hostname is treated as
// potentially reachable from the network. This mirrors isLoopbackAddr in
// internal/cli/apiauth.go, duplicated here in a few lines rather than
// exported from package cli, which lwd-web (a separate binary) has no other
// reason to import.
func hostIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	default:
		return false
	}
}
