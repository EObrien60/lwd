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
// empty, unparseable, or non-positive value falls back to the 15s default).
// The non-positive guard matters: this value is fed straight into
// time.NewTicker, which panics on a zero or negative duration, so a
// misconfigured "0"/"0s"/"-5s" must never reach it.
func ReconcileInterval() time.Duration {
	v := os.Getenv("LWD_RECONCILE_INTERVAL")
	if v == "" {
		return defaultReconcileInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
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

// defaultFailoverGrace is how long a registered node must be observed
// continuously unreachable before the continuous reconciler loop
// automatically evacuates its scheduled surfaces, used when
// LWD_FAILOVER_GRACE is unset, unparseable, or not positive.
const defaultFailoverGrace = 60 * time.Second

// FailoverGrace returns the grace period a registered node must stay
// unreachable before Phase 11b Task 5's automatic node-loss failover kicks
// in (LWD_FAILOVER_GRACE, parsed with time.ParseDuration; an empty,
// unparseable, or non-positive value falls back to the 60s default — mirrors
// ReconcileInterval's guard, since a degenerate zero grace would evacuate a
// node on its very first missed probe).
func FailoverGrace() time.Duration {
	v := os.Getenv("LWD_FAILOVER_GRACE")
	if v == "" {
		return defaultFailoverGrace
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return defaultFailoverGrace
	}
	return d
}

// APIAddr returns the address the daemon should additionally listen on for
// remote (TCP) API access (LWD_ADDR, e.g. ":8077" or "127.0.0.1:8077"). The
// default is "" — disabled — since exposing the control plane over the
// network at all must be an explicit operator opt-in, not a default.
func APIAddr() string { return os.Getenv("LWD_ADDR") }

// APIToken returns the bearer token required to authenticate against the
// optional TCP listener (LWD_API_TOKEN). The default is "" — no token — but
// see the fail-closed guard in internal/cli: a non-loopback APIAddr with an
// empty APIToken refuses to start the daemon rather than serving an
// unauthenticated control plane on the network.
func APIToken() string { return os.Getenv("LWD_API_TOKEN") }
