// Package web implements the lwd-web dashboard: config loading, password
// authentication, session cookies, and (in later tasks) the HTTP handlers
// that proxy to the lwd daemon over its unix socket.
package web

import (
	"crypto/rand"
	"errors"
	"os"
)

// defaultAddr is the listen address used when LWD_WEB_ADDR is unset.
const defaultAddr = "127.0.0.1:8079"

// Config holds the runtime configuration for lwd-web.
type Config struct {
	Addr       string
	Password   string
	SigningKey []byte
}

// LoadConfig reads lwd-web configuration from the environment.
//
//   - LWD_WEB_ADDR: listen address (default "127.0.0.1:8079").
//   - LWD_WEB_PASSWORD: required admin password; error if unset/empty.
//   - LWD_WEB_SECRET: cookie signing key; if unset, 32 random bytes are
//     generated (sessions reset on restart). If set, it must be at least
//     16 bytes.
//
// The daemon target itself (local unix socket vs. remote TCP) is NOT part of
// this config: it's resolved by client.FromEnv (LWD_DAEMON/LWD_API_TOKEN,
// falling back to LWD_SOCKET/the default socket path) when lwd-web builds
// its daemon client.
func LoadConfig() (Config, error) {
	password := os.Getenv("LWD_WEB_PASSWORD")
	if password == "" {
		return Config{}, errors.New("web: LWD_WEB_PASSWORD is required")
	}

	addr := os.Getenv("LWD_WEB_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	var key []byte
	if secret := os.Getenv("LWD_WEB_SECRET"); secret != "" {
		if len(secret) < 16 {
			return Config{}, errors.New("web: LWD_WEB_SECRET must be at least 16 bytes")
		}
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
		SigningKey: key,
	}, nil
}
