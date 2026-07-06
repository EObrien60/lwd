package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// jsonAppsHandler returns an httptest.Server that answers every request with
// an empty JSON array (a valid, decodable response for Client.Apps) and
// records the Authorization header of the last request seen.
func jsonAppsHandler(gotAuth *string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotAuth != nil {
			*gotAuth = r.Header.Get("Authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	}))
}

func TestNewHTTPSendsBearer(t *testing.T) {
	var gotAuth string
	srv := jsonAppsHandler(&gotAuth)
	defer srv.Close()

	c := NewHTTP(srv.URL, "tok")
	if _, err := c.Apps(context.Background()); err != nil {
		t.Fatalf("Apps: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer tok")
	}
}

func TestNewHTTPNoToken(t *testing.T) {
	var gotAuth string
	srv := jsonAppsHandler(&gotAuth)
	defer srv.Close()

	c := NewHTTP(srv.URL, "")
	if _, err := c.Apps(context.Background()); err != nil {
		t.Fatalf("Apps: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization header = %q, want empty (no token set)", gotAuth)
	}
}

func TestNewHTTPNormalizesHostPort(t *testing.T) {
	srv := jsonAppsHandler(nil)
	defer srv.Close()

	// srv.URL looks like "http://127.0.0.1:PORT"; strip the scheme so we
	// exercise the bare-host:port normalization path.
	hostPort := strings.TrimPrefix(srv.URL, "http://")

	c := NewHTTP(hostPort, "")
	if _, err := c.Apps(context.Background()); err != nil {
		t.Fatalf("Apps against normalized host:port %q: %v", hostPort, err)
	}
}

func TestNewHTTPTrimsTrailingSlash(t *testing.T) {
	srv := jsonAppsHandler(nil)
	defer srv.Close()

	c := NewHTTP(srv.URL+"/", "")
	if _, err := c.Apps(context.Background()); err != nil {
		t.Fatalf("Apps with trailing slash in base: %v", err)
	}
}

func TestFromEnvPicksTCPvsUnix(t *testing.T) {
	t.Run("tcp", func(t *testing.T) {
		var gotAuth string
		srv := jsonAppsHandler(&gotAuth)
		defer srv.Close()

		hostPort := strings.TrimPrefix(srv.URL, "http://")
		t.Setenv("LWD_DAEMON", hostPort)
		t.Setenv("LWD_API_TOKEN", "envtok")

		c := FromEnv()
		if _, err := c.Apps(context.Background()); err != nil {
			t.Fatalf("Apps: %v", err)
		}
		if gotAuth != "Bearer envtok" {
			t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer envtok")
		}
	})

	t.Run("unix", func(t *testing.T) {
		// Explicitly clear LWD_DAEMON (t.Setenv restores per-subtest, but be
		// deterministic regardless of subtest ordering).
		t.Setenv("LWD_DAEMON", "")

		sock := startUnixServer(t)
		t.Setenv("LWD_SOCKET", sock)

		c := FromEnv()
		if c.base != "http://lwd" {
			t.Errorf("base = %q, want %q", c.base, "http://lwd")
		}
		if _, err := c.Apps(context.Background()); err != nil {
			t.Fatalf("Apps: %v", err)
		}
	})
}
