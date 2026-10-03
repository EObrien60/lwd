package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireTokenReadOnlyIsGetOnly(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := requireToken("admin", []string{"reader"}, ok)
	cases := []struct {
		method, token string
		want          int
	}{
		{http.MethodGet, "admin", http.StatusNoContent},
		{http.MethodPost, "admin", http.StatusNoContent},
		{http.MethodGet, "reader", http.StatusNoContent},
		{http.MethodHead, "reader", http.StatusNoContent},
		{http.MethodPost, "reader", http.StatusForbidden},
		{http.MethodPut, "reader", http.StatusForbidden},
		{http.MethodDelete, "reader", http.StatusForbidden},
		{http.MethodGet, "", http.StatusUnauthorized},
		{http.MethodGet, "readerx", http.StatusUnauthorized},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "/v1/apps", nil)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s with %q = %d, want %d", c.method, c.token, rec.Code, c.want)
		}
	}
}

func TestRequireTokenIgnoresEmptyReadToken(t *testing.T) {
	h := requireToken("admin", []string{"", "  "}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/v1/apps", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty bearer accepted: %d", rec.Code)
	}
}
