package config

import (
	"testing"
	"time"
)

func TestReconcileIntervalDefault(t *testing.T) {
	if got := ReconcileInterval(); got != 15*time.Second {
		t.Errorf("ReconcileInterval() = %v, want 15s", got)
	}
}

func TestReconcileIntervalEnv(t *testing.T) {
	t.Setenv("LWD_RECONCILE_INTERVAL", "5s")
	if got := ReconcileInterval(); got != 5*time.Second {
		t.Errorf("ReconcileInterval() = %v, want 5s", got)
	}
}

func TestReconcileIntervalInvalid(t *testing.T) {
	t.Setenv("LWD_RECONCILE_INTERVAL", "garbage")
	if got := ReconcileInterval(); got != 15*time.Second {
		t.Errorf("ReconcileInterval() = %v, want default 15s", got)
	}
}

// TestReconcileIntervalNonPositive covers the guard against a zero or
// negative parsed duration: time.NewTicker (RunLoop's consumer of this
// value) panics on <= 0, so "0s"/"-1s" must fall back to the default rather
// than reaching the ticker and crashing the daemon at startup.
func TestReconcileIntervalNonPositive(t *testing.T) {
	t.Setenv("LWD_RECONCILE_INTERVAL", "0s")
	if got := ReconcileInterval(); got != 15*time.Second {
		t.Errorf("ReconcileInterval() = %v, want default 15s for \"0s\"", got)
	}

	t.Setenv("LWD_RECONCILE_INTERVAL", "-1s")
	if got := ReconcileInterval(); got != 15*time.Second {
		t.Errorf("ReconcileInterval() = %v, want default 15s for \"-1s\"", got)
	}
}

func TestHealMaxAttemptsDefault(t *testing.T) {
	if got := HealMaxAttempts(); got != 5 {
		t.Errorf("HealMaxAttempts() = %v, want 5", got)
	}
}

func TestHealMaxAttemptsEnv(t *testing.T) {
	t.Setenv("LWD_HEAL_MAX_ATTEMPTS", "3")
	if got := HealMaxAttempts(); got != 3 {
		t.Errorf("HealMaxAttempts() = %v, want 3", got)
	}
}

func TestHealMaxAttemptsInvalid(t *testing.T) {
	t.Setenv("LWD_HEAL_MAX_ATTEMPTS", "0")
	if got := HealMaxAttempts(); got != 5 {
		t.Errorf("HealMaxAttempts() = %v, want default 5 for \"0\"", got)
	}

	t.Setenv("LWD_HEAL_MAX_ATTEMPTS", "x")
	if got := HealMaxAttempts(); got != 5 {
		t.Errorf("HealMaxAttempts() = %v, want default 5 for \"x\"", got)
	}
}

// TestFailoverGraceDefault covers Phase 11b Task 5's default: an unset
// LWD_FAILOVER_GRACE falls back to 60s.
func TestFailoverGraceDefault(t *testing.T) {
	if got := FailoverGrace(); got != 60*time.Second {
		t.Errorf("FailoverGrace() = %v, want default 60s", got)
	}
}

// TestFailoverGraceEnv covers a valid LWD_FAILOVER_GRACE override.
func TestFailoverGraceEnv(t *testing.T) {
	t.Setenv("LWD_FAILOVER_GRACE", "10s")
	if got := FailoverGrace(); got != 10*time.Second {
		t.Errorf("FailoverGrace() = %v, want 10s", got)
	}
}

// TestFailoverGraceNonPositive covers the same non-positive/unparseable guard
// as ReconcileInterval: "0s"/"-1s"/garbage must all fall back to the 60s
// default rather than letting a degenerate grace period (e.g. instant
// failover on a zero grace) reach the reconcile loop.
func TestFailoverGraceNonPositive(t *testing.T) {
	t.Setenv("LWD_FAILOVER_GRACE", "0s")
	if got := FailoverGrace(); got != 60*time.Second {
		t.Errorf("FailoverGrace() = %v, want default 60s for \"0s\"", got)
	}

	t.Setenv("LWD_FAILOVER_GRACE", "-1s")
	if got := FailoverGrace(); got != 60*time.Second {
		t.Errorf("FailoverGrace() = %v, want default 60s for \"-1s\"", got)
	}

	t.Setenv("LWD_FAILOVER_GRACE", "garbage")
	if got := FailoverGrace(); got != 60*time.Second {
		t.Errorf("FailoverGrace() = %v, want default 60s for \"garbage\"", got)
	}
}

// TestAPIAddrEnv covers the optional remote-daemon-access TCP listener
// address: unset LWD_ADDR means "disabled" (empty string, not a default
// address like ":8077") since binding a TCP listener at all must be an
// explicit opt-in (see apiListenAllowed's fail-closed guard in package cli).
func TestAPIAddrEnv(t *testing.T) {
	if got := APIAddr(); got != "" {
		t.Errorf("APIAddr() = %q, want \"\" when LWD_ADDR is unset", got)
	}

	t.Setenv("LWD_ADDR", ":8077")
	if got := APIAddr(); got != ":8077" {
		t.Errorf("APIAddr() = %q, want \":8077\"", got)
	}
}

// TestAPITokenEnv covers the bearer token used to guard the optional TCP
// listener: unset LWD_API_TOKEN means "no token configured" (empty string).
func TestAPITokenEnv(t *testing.T) {
	if got := APIToken(); got != "" {
		t.Errorf("APIToken() = %q, want \"\" when LWD_API_TOKEN is unset", got)
	}

	t.Setenv("LWD_API_TOKEN", "s3cr3t")
	if got := APIToken(); got != "s3cr3t" {
		t.Errorf("APIToken() = %q, want \"s3cr3t\"", got)
	}
}
