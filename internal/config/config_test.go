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
