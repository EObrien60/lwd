package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDaemonUnreachableFriendlyError verifies that when the daemon can't be
// reached at the transport level (dial failure), both New (unix socket) and
// NewHTTP (TCP) wrap the raw dial error in an actionable, friendly message
// that names the target and suggests a fix.
func TestDaemonUnreachableFriendlyError(t *testing.T) {
	t.Run("unix socket missing", func(t *testing.T) {
		c := New("/nonexistent/lwd.sock")
		_, err := c.Apps(context.Background())
		if err == nil {
			t.Fatal("Apps: want error, got nil")
		}
		if !strings.Contains(err.Error(), "cannot reach the lwd daemon") {
			t.Errorf("Apps error = %q, want it to contain %q", err.Error(), "cannot reach the lwd daemon")
		}
		if !strings.Contains(err.Error(), "/nonexistent/lwd.sock") {
			t.Errorf("Apps error = %q, want it to name the target socket", err.Error())
		}
	})

	t.Run("tcp closed port", func(t *testing.T) {
		// Port 1 on loopback should refuse the connection immediately rather
		// than hang, without requiring root or any real listener.
		c := NewHTTP("127.0.0.1:1", "")
		_, err := c.Apps(context.Background())
		if err == nil {
			t.Fatal("Apps: want error, got nil")
		}
		if !strings.Contains(err.Error(), "cannot reach the lwd daemon") {
			t.Errorf("Apps error = %q, want it to contain %q", err.Error(), "cannot reach the lwd daemon")
		}
		if !strings.Contains(err.Error(), "127.0.0.1:1") {
			t.Errorf("Apps error = %q, want it to name the target", err.Error())
		}
	})
}

// TestNormalHTTPErrorNotWrapped verifies that a normal (non-transport) HTTP
// error response — the daemon reachable but answering with an error status —
// surfaces its own decodeErr message unmodified, NOT the friendly
// "cannot reach the lwd daemon" wrap. Only connection-level failures
// (RoundTrip returning a non-nil error) should get the friendly treatment.
func TestNormalHTTPErrorNotWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	c := NewHTTP(srv.URL, "")
	_, err := c.Apps(context.Background())
	if err == nil {
		t.Fatal("Apps: want error, got nil")
	}
	if err.Error() != "boom" {
		t.Errorf("Apps error = %q, want %q (unwrapped decodeErr)", err.Error(), "boom")
	}
	if strings.Contains(err.Error(), "cannot reach the lwd daemon") {
		t.Errorf("Apps error = %q, should NOT contain the friendly daemon-unreachable wrap for a normal HTTP response", err.Error())
	}
}
