// Package web implements the lwd-web dashboard: config loading, password
// authentication, session cookies, and (in later tasks) the HTTP handlers
// that proxy to the lwd daemon over its unix socket.
package web

import (
	"crypto/rand"
	"errors"
	"os"

	"lwd/internal/config"
)

// defaultAddr is the listen address used when LWD_WEB_ADDR is unset.
const defaultAddr = "127.0.0.1:8079"

// Config holds the runtime configuration for lwd-web.
type Config struct {
	Addr       string
	Password   string
	SocketPath string
	SigningKey []byte
}

// LoadConfig reads lwd-web configuration from the environment.
//
//   - LWD_WEB_ADDR: listen address (default "127.0.0.1:8079").
//   - LWD_WEB_PASSWORD: required admin password; error if unset/empty.
//   - LWD_WEB_SECRET: cookie signing key; if unset, 32 random bytes are
//     generated (sessions reset on restart).
//   - LWD_SOCKET: overrides the daemon socket path from internal/config.
func LoadConfig() (Config, error) {
	password := os.Getenv("LWD_WEB_PASSWORD")
	if password == "" {
		return Config{}, errors.New("web: LWD_WEB_PASSWORD is required")
	}

	addr := os.Getenv("LWD_WEB_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	socketPath := os.Getenv("LWD_SOCKET")
	if socketPath == "" {
		socketPath = config.SocketPath()
	}

	var key []byte
	if secret := os.Getenv("LWD_WEB_SECRET"); secret != "" {
		key = []byte(secret)
	} else {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return Config{}, errors.New("web: failed to generate signing key: " + err.Error())
		}
	}

	return Config{
		Addr:       addr,
		Password:   password,
		SocketPath: socketPath,
		SigningKey: key,
	}, nil
}
