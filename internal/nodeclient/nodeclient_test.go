package nodeclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lwd/internal/bundle"
)

func TestDeploySendsBundleWithBearer(t *testing.T) {
	var got bundle.Bundle
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/deploy" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(bundle.Result{Status: bundle.StatusSucceeded, Phase: "done"})
	}))
	defer srv.Close()

	c := New(strings.TrimPrefix(srv.URL, "http://"), "tok") // bare host:port gets http://
	res, err := c.Deploy(context.Background(), &bundle.Bundle{App: "a", Env: "e", Deployment: 7})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != bundle.StatusSucceeded || got.Deployment != 7 {
		t.Errorf("res = %+v, got = %+v", res, got)
	}
}

func TestErrorsAreTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/deploy":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":{"code":"conflict","message":"a/e is busy"}}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "plain failure\n")
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")

	_, err := c.Deploy(context.Background(), &bundle.Bundle{})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 409 || he.Message != "a/e is busy" {
		t.Fatalf("err = %#v", err)
	}
	err = c.Restart(context.Background(), "a", "e", "")
	if !errors.As(err, &he) || he.Status != 500 || he.Message != "plain failure" {
		t.Fatalf("err = %#v", err)
	}
	if errors.Is(err, ErrUnreachable) {
		t.Error("HTTP errors are not unreachable")
	}
}

func TestUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close()
	_, err := New(addr, "t").Health(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

func TestRestartLogsStatusRemove(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.String())
		switch {
		case r.URL.Path == "/v1/status":
			_, _ = io.WriteString(w, `{"host":"h1"}`)
		case r.URL.Path == "/v1/health":
			_, _ = io.WriteString(w, `{"ok":true,"version":"2.0"}`)
		case strings.HasSuffix(r.URL.Path, "/logs"):
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "line1\nline2\n")
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "t")
	ctx := context.Background()

	if err := c.Restart(ctx, "a", "e", "web"); err != nil {
		t.Fatal(err)
	}
	logs, err := c.Logs(ctx, "a", "e", "api", 50)
	if err != nil || logs != "line1\nline2\n" {
		t.Fatalf("logs = %q, %v", logs, err)
	}
	st, err := c.Status(ctx)
	if err != nil || string(st) != `{"host":"h1"}` {
		t.Fatalf("status = %s, %v", st, err)
	}
	h, err := c.Health(ctx)
	if err != nil || !h.OK || h.Version != "2.0" {
		t.Fatalf("health = %+v, %v", h, err)
	}
	if err := c.Remove(ctx, "a", "e"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /v1/apps/a/e/restart?service=web",
		"GET /v1/apps/a/e/logs?service=api&tail=50",
		"GET /v1/status",
		"GET /v1/health",
		"DELETE /v1/apps/a/e",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls =\n%s", strings.Join(calls, "\n"))
	}
}
