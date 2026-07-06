package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIsLoopbackAddr covers the classification used by apiListenAllowed to
// decide whether an LWD_ADDR bind target is safe to expose without a token:
// only an explicit loopback host (127.0.0.1, ::1, localhost) qualifies. A
// bare ":8077" (empty host) and "0.0.0.0:8077" both mean "listen on every
// interface" — i.e. public — and any other routable host/IP is public too.
func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8077", true},
		{"localhost:1", true},
		{"[::1]:8077", true},
		{":8077", false},
		{"0.0.0.0:8077", false},
		{"10.0.0.5:8077", false},
	}
	for _, c := range cases {
		if got := isLoopbackAddr(c.addr); got != c.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

// TestApiListenAllowed covers the fail-closed guard: the only forbidden
// combination is a non-loopback bind address with no token configured, since
// that would expose an unauthenticated control plane on the network.
func TestApiListenAllowed(t *testing.T) {
	if err := apiListenAllowed("10.0.0.5:8077", ""); err == nil {
		t.Error("apiListenAllowed(non-loopback, no token) = nil, want error")
	}
	if err := apiListenAllowed("127.0.0.1:8077", ""); err != nil {
		t.Errorf("apiListenAllowed(loopback, no token) = %v, want nil", err)
	}
	if err := apiListenAllowed("10.0.0.5:8077", "tok"); err != nil {
		t.Errorf("apiListenAllowed(non-loopback, token) = %v, want nil", err)
	}
	if err := apiListenAllowed("", ""); err != nil {
		t.Errorf("apiListenAllowed(empty addr, no token) = %v, want nil", err)
	}
}

func stubHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// TestBearerMiddleware covers bearerMiddleware's three cases: no token
// configured is a passthrough (used for the unix socket / opt-out case);
// a configured token requires an exact "Authorization: Bearer <token>"
// match, else 401.
func TestBearerMiddleware(t *testing.T) {
	t.Run("no token configured passes through", func(t *testing.T) {
		h := bearerMiddleware("", stubHandler())
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/apps", nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("missing header rejected", func(t *testing.T) {
		h := bearerMiddleware("s3cr3t", stubHandler())
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/apps", nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("wrong token rejected", func(t *testing.T) {
		h := bearerMiddleware("s3cr3t", stubHandler())
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/apps", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("correct token accepted", func(t *testing.T) {
		h := bearerMiddleware("s3cr3t", stubHandler())
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/apps", nil)
		req.Header.Set("Authorization", "Bearer s3cr3t")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})
}
