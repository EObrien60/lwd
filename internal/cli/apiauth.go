package cli

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// isLoopbackAddr reports whether addr (a "host:port" listen address, as
// passed to net.Listen) binds only the loopback interface. It's the
// dividing line apiListenAllowed uses to decide whether serving the daemon's
// API without a bearer token is safe: 127.0.0.1, ::1, and localhost are
// loopback-only and never reachable from another machine, so an
// unauthenticated listener there is no more exposed than the existing unix
// socket. Anything else — a bare ":8077" (empty host, meaning "every
// interface"), "0.0.0.0:8077" (explicit every-interface), a routable IP, or
// any other hostname — is treated as potentially reachable from the network
// and therefore NOT loopback, so it's rejected here (an addr with no ":"
// separator at all is likewise treated as non-loopback/invalid).
func isLoopbackAddr(addr string) bool {
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

// apiListenAllowed is the fail-closed guard for the optional TCP API
// listener: it refuses to start the daemon on a non-loopback address unless
// a bearer token is configured, so lwd can never accidentally expose its
// control plane to the network with no authentication. A pure function
// (no I/O) so it's directly unit-testable; runDaemon calls it before ever
// calling net.Listen.
func apiListenAllowed(addr, token string) error {
	if addr == "" {
		return nil
	}
	if !isLoopbackAddr(addr) && token == "" {
		return fmt.Errorf("LWD_ADDR %q binds a non-loopback interface but LWD_API_TOKEN is unset — refusing to expose an unauthenticated control plane", addr)
	}
	return nil
}

// bearerMiddleware wraps h with a bearer-token check modeled on
// internal/agent/auth.go's authMiddleware. If token is "" it returns h
// unchanged (used for the unix socket, and for a loopback-only TCP listener
// with no token configured) — otherwise every request must carry an
// "Authorization: Bearer <token>" header matching token, compared in
// constant time to avoid a timing side-channel; a missing or mismatched
// token gets a 401 with a JSON {"error":"unauthorized"} body.
func bearerMiddleware(token string, h http.Handler) http.Handler {
	if token == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		authz := r.Header.Get("Authorization")
		provided := ""
		if strings.HasPrefix(authz, prefix) {
			provided = strings.TrimPrefix(authz, prefix)
		}
		if len(provided) == 0 || subtle.ConstantTimeCompare([]byte(token), []byte(provided)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		h.ServeHTTP(w, r)
	})
}
