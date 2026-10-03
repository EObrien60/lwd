package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigFromEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "lwd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lwd", "token"), []byte("filetok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"XDG_CONFIG_HOME": dir}
	c, err := FromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "http://127.0.0.1:7470" || c.Token != "filetok" {
		t.Errorf("client = %+v", c)
	}

	env["LWD_URL"] = "https://lwd.example.com/"
	env["LWD_TOKEN"] = "envtok"
	c, err = FromEnv(func(k string) string { return env[k] })
	if err != nil || c.BaseURL != "https://lwd.example.com" || c.Token != "envtok" {
		t.Errorf("client = %+v, %v", c, err)
	}

	_, err = FromEnv(func(k string) string { return map[string]string{"HOME": t.TempDir()}[k] })
	if err == nil {
		t.Error("expected error when no token is configured")
	}
}

func TestErrorDecoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("X-LWD-Actor") != "ethan@box" {
			t.Errorf("headers = %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"conflict","message":"busy"}}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	c.Actor = "ethan@box"
	_, err := c.Deploy(context.Background(), "a", "e", DeployRequest{})
	var e *Error
	if !errors.As(err, &e) || e.Code != "conflict" || e.Status != 409 || e.Error() != "conflict: busy" {
		t.Fatalf("err = %#v", err)
	}
}

func TestSetSecretSendsRawBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPut || r.URL.Path != "/v1/apps/a/envs/e/secrets/KEY" || string(b) != "s3cret" {
			t.Errorf("%s %s %q", r.Method, r.URL.Path, b)
		}
		_, _ = io.WriteString(w, `{"key":"KEY","version":2}`)
	}))
	defer srv.Close()
	m, err := New(srv.URL, "t").SetSecret(context.Background(), "a", "e", "KEY", []byte("s3cret"))
	if err != nil || m.Version != 2 {
		t.Fatalf("m = %+v, %v", m, err)
	}
}
