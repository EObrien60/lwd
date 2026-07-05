// Package config resolves lwd's filesystem locations.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"time"
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

// CaddyfilePath returns the path to the generated Caddyfile.
func CaddyfilePath() string { return filepath.Join(DataDir(), "Caddyfile") }

// KeyPath returns the path to the secret-encryption key file.
func KeyPath() string { return filepath.Join(DataDir(), "secret.key") }

// defaultReconcileInterval is the delay between passes of the Phase 10
// continuous reconciler loop when LWD_RECONCILE_INTERVAL is unset or
// unparseable.
const defaultReconcileInterval = 15 * time.Second

// ReconcileInterval returns the delay between passes of the continuous
// reconciler loop (LWD_RECONCILE_INTERVAL, parsed with time.ParseDuration; an
// empty or unparseable value falls back to the 15s default).
func ReconcileInterval() time.Duration {
	v := os.Getenv("LWD_RECONCILE_INTERVAL")
	if v == "" {
		return defaultReconcileInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return defaultReconcileInterval
	}
	return d
}

// defaultHealMaxAttempts is the number of consecutive self-heal attempts the
// reconciler makes on a dead surface before giving up, used when
// LWD_HEAL_MAX_ATTEMPTS is unset, unparseable, or not positive.
const defaultHealMaxAttempts = 5

// HealMaxAttempts returns the max number of consecutive self-heal attempts
// the reconciler makes on a dead surface (LWD_HEAL_MAX_ATTEMPTS, parsed with
// strconv.Atoi; an empty, unparseable, or non-positive value falls back to
// the default of 5).
func HealMaxAttempts() int {
	v := os.Getenv("LWD_HEAL_MAX_ATTEMPTS")
	if v == "" {
		return defaultHealMaxAttempts
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return defaultHealMaxAttempts
	}
	return n
}
