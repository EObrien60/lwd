package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	key := []byte("test-signing-key")
	future := time.Now().Add(1 * time.Hour)

	cookie := signSession(key, future)
	if !verifySession(key, cookie) {
		t.Fatalf("expected valid session cookie to verify, got false for %q", cookie)
	}

	// Tamper a byte in the signature portion. Flip the FIRST signature
	// character (immediately after the "."), not the last: the last base64
	// char of a 32-byte HMAC carries zero-padding bits that decode ignores,
	// so flipping it can round-trip to the same bytes and spuriously verify.
	dot := strings.IndexByte(cookie, '.')
	if dot < 0 || dot+1 >= len(cookie) {
		t.Fatalf("unexpected cookie format: %q", cookie)
	}
	tampered := []byte(cookie)
	idx := dot + 1 // first character of the signature
	if tampered[idx] == 'a' {
		tampered[idx] = 'b'
	} else {
		tampered[idx] = 'a'
	}
	if verifySession(key, string(tampered)) {
		t.Fatalf("expected tampered cookie to fail verification")
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	key := []byte("test-signing-key")
	past := time.Now().Add(-1 * time.Hour)

	cookie := signSession(key, past)
	if verifySession(key, cookie) {
		t.Fatalf("expected expired cookie to fail verification")
	}
}

func TestCheckPasswordConstantTime(t *testing.T) {
	if !checkPassword("correct-horse", "correct-horse") {
		t.Fatalf("expected matching passwords to succeed")
	}
	if checkPassword("correct-horse", "wrong-password") {
		t.Fatalf("expected mismatched passwords to fail")
	}
	if checkPassword("correct-horse", "") {
		t.Fatalf("expected empty provided password to fail")
	}
	if checkPassword("", "") {
		t.Fatalf("expected empty configured password to fail")
	}
}

func TestLoadConfigRequiresPassword(t *testing.T) {
	t.Setenv("LWD_WEB_PASSWORD", "")
	t.Setenv("LWD_WEB_ADDR", "")
	t.Setenv("LWD_WEB_SECRET", "")
	t.Setenv("LWD_SOCKET", "")

	if _, err := LoadConfig(); err == nil {
		t.Fatalf("expected error when LWD_WEB_PASSWORD is unset")
	}

	t.Setenv("LWD_WEB_PASSWORD", "hunter2")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("expected no error with password set, got %v", err)
	}
	if cfg.Addr != "127.0.0.1:8079" {
		t.Fatalf("expected default addr, got %q", cfg.Addr)
	}
	if cfg.Password != "hunter2" {
		t.Fatalf("expected password to be loaded, got %q", cfg.Password)
	}
	if len(cfg.SigningKey) != 32 {
		t.Fatalf("expected generated signing key of 32 bytes, got %d", len(cfg.SigningKey))
	}
}

func TestMiddlewareBlocksUnauthed(t *testing.T) {
	key := []byte("test-signing-key")
	auth := NewAuthenticator(key, "hunter2")

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := auth.Middleware(next)

	// No cookie -> 401 for /api/ paths.
	req := httptest.NewRequest(http.MethodGet, "/api/apps", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without cookie, got %d", rec.Code)
	}
	if called {
		t.Fatalf("expected next handler not to be called without valid session")
	}

	// Valid cookie -> passes through.
	called = false
	cookieVal := signSession(key, time.Now().Add(1*time.Hour))
	req = httptest.NewRequest(http.MethodGet, "/api/apps", nil)
	req.AddCookie(&http.Cookie{Name: "lwd_session", Value: cookieVal})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid cookie, got %d", rec.Code)
	}
	if !called {
		t.Fatalf("expected next handler to be called with valid session")
	}
}

func TestLoginSetsCookie(t *testing.T) {
	key := []byte("test-signing-key")
	auth := NewAuthenticator(key, "hunter2")

	form := url.Values{}
	form.Set("password", "hunter2")
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	auth.Login(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", rec.Code)
	}
	res := rec.Result()
	var sessionCookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == "lwd_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatalf("expected lwd_session cookie to be set")
	}
	if !verifySession(key, sessionCookie.Value) {
		t.Fatalf("expected session cookie to verify")
	}
	if !sessionCookie.HttpOnly {
		t.Fatalf("expected cookie to be HttpOnly")
	}

	// Wrong password -> 401, no cookie.
	form = url.Values{}
	form.Set("password", "wrong")
	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	auth.Login(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong password, got %d", rec.Code)
	}
}

func TestIsSecureRequest(t *testing.T) {
	key := []byte("test-signing-key")
	auth := NewAuthenticator(key, "hunter2")

	loginCookie := func(t *testing.T, forwardedProto string) *http.Cookie {
		t.Helper()
		form := url.Values{}
		form.Set("password", "hunter2")
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if forwardedProto != "" {
			req.Header.Set("X-Forwarded-Proto", forwardedProto)
		}
		rec := httptest.NewRecorder()
		auth.Login(rec, req)

		for _, c := range rec.Result().Cookies() {
			if c.Name == sessionCookieName {
				return c
			}
		}
		t.Fatalf("expected lwd_session cookie to be set")
		return nil
	}

	// Behind a TLS-terminating proxy (Caddy) that sets X-Forwarded-Proto:
	// https, the cookie must be marked Secure even though r.TLS is nil.
	if c := loginCookie(t, "https"); !c.Secure {
		t.Fatalf("expected Secure=true with X-Forwarded-Proto: https")
	}

	// Case-insensitive match.
	if c := loginCookie(t, "HTTPS"); !c.Secure {
		t.Fatalf("expected Secure=true with X-Forwarded-Proto: HTTPS")
	}

	// No TLS, no forwarded-proto header (plain-HTTP / SSH-tunnel mode):
	// Secure must stay off or browsers would refuse to send the cookie.
	if c := loginCookie(t, ""); c.Secure {
		t.Fatalf("expected Secure=false without TLS or X-Forwarded-Proto")
	}

	// Logout must apply the same logic when clearing the cookie.
	logoutCookie := func(t *testing.T, forwardedProto string) *http.Cookie {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/logout", nil)
		if forwardedProto != "" {
			req.Header.Set("X-Forwarded-Proto", forwardedProto)
		}
		rec := httptest.NewRecorder()
		auth.Logout(rec, req)

		for _, c := range rec.Result().Cookies() {
			if c.Name == sessionCookieName {
				return c
			}
		}
		t.Fatalf("expected lwd_session cookie to be cleared")
		return nil
	}

	if c := logoutCookie(t, "https"); !c.Secure {
		t.Fatalf("expected Secure=true on logout with X-Forwarded-Proto: https")
	}
	if c := logoutCookie(t, ""); c.Secure {
		t.Fatalf("expected Secure=false on logout without TLS or X-Forwarded-Proto")
	}
}

func TestLoadConfigRejectsShortSecret(t *testing.T) {
	t.Setenv("LWD_WEB_PASSWORD", "hunter2")
	t.Setenv("LWD_WEB_ADDR", "")
	t.Setenv("LWD_SOCKET", "")

	t.Setenv("LWD_WEB_SECRET", "short")
	if _, err := LoadConfig(); err == nil {
		t.Fatalf("expected error for LWD_WEB_SECRET shorter than 16 bytes")
	}

	longSecret := "0123456789abcdef" // exactly 16 bytes
	t.Setenv("LWD_WEB_SECRET", longSecret)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("expected no error for 16-byte LWD_WEB_SECRET, got %v", err)
	}
	if string(cfg.SigningKey) != longSecret {
		t.Fatalf("expected signing key to be loaded from env, got %q", cfg.SigningKey)
	}
}
