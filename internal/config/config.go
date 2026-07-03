// Package config resolves lwd's filesystem locations.
package config

import (
	"os"
	"path/filepath"
)

// DataDir returns the lwd data directory (LWD_DATA_DIR or /var/lib/lwd).
func DataDir() string {
	if d := os.Getenv("LWD_DATA_DIR"); d != "" {
		return d
	}
	return "/var/lib/lwd"
}

// SocketPath returns the daemon's unix socket path.
func SocketPath() string { return filepath.Join(DataDir(), "lwd.sock") }

// DBPath returns the SQLite database path.
func DBPath() string { return filepath.Join(DataDir(), "lwd.db") }
