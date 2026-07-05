package agent

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// checkToken reports whether provided matches configured, using a
// constant-time comparison. Empty inputs never match.
func checkToken(configured, provided string) bool {
	if len(configured) == 0 || len(provided) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(configured), []byte(provided)) == 1
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header, or "" if the header is absent or malformed.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}

// authMiddleware requires a valid "Authorization: Bearer <token>" header
// matching token on every request except PathHealthz. A missing or
// mismatched token gets a 401 with an ErrorResponse body.
func authMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathHealthz {
			next.ServeHTTP(w, r)
			return
		}

		if !checkToken(token, bearerToken(r)) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(ErrorResponse{Error: "unauthorized"})
			return
		}

		next.ServeHTTP(w, r)
	})
}
