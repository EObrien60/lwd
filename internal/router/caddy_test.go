package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidEmail(t *testing.T) {
	if !ValidEmail("ops@example.com") {
		t.Error("plain address rejected")
	}
	for _, e := range []string{"", "ops", "a b@c.d", "a@b}\n", "x@y {"} {
		if ValidEmail(e) {
			t.Errorf("%q should be invalid", e)
		}
	}
}

func TestApplyWritesFileThenLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Caddyfile")
	var gotBody, gotType string
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/load" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		gotType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		// The file must already be in place when Caddy is told to load.
		onDisk, _ := os.ReadFile(path)
		if string(onDisk) != gotBody {
			t.Errorf("file not written before /load")
		}
	}))
	defer admin.Close()

	c := &Caddy{Path: path, AdminURL: admin.URL}
	if err := c.Apply(context.Background(), "site {\n}\n"); err != nil {
		t.Fatal(err)
	}
	if gotType != "text/caddyfile" || gotBody != "site {\n}\n" {
		t.Fatalf("type=%q body=%q", gotType, gotBody)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

func TestApplyReportsLoadError(t *testing.T) {
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "adapting config: bad directive", http.StatusBadRequest)
	}))
	defer admin.Close()
	c := &Caddy{Path: filepath.Join(t.TempDir(), "Caddyfile"), AdminURL: admin.URL}
	err := c.Apply(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "bad directive") {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitAdminPollsUntilUp(t *testing.T) {
	var calls atomic.Int32
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("{}"))
	}))
	defer admin.Close()
	c := &Caddy{AdminURL: admin.URL, PollInterval: time.Millisecond}
	if err := c.WaitAdmin(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if calls.Load() < 3 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestWaitAdminTimesOut(t *testing.T) {
	c := &Caddy{AdminURL: "http://127.0.0.1:1", PollInterval: time.Millisecond}
	if err := c.WaitAdmin(context.Background(), 50*time.Millisecond); err == nil {
		t.Fatal("expected timeout")
	}
}
