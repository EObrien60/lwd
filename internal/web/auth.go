package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// sessionCookieName is the name of the signed session cookie set on login.
const sessionCookieName = "lwd_session"

// defaultTTL is how long a session cookie remains valid after login.
const defaultTTL = 24 * time.Hour

// publicPaths lists paths that are reachable without a valid session: the
// login/logout endpoints, the embedded static assets, and the app-shell
// root ("/" is handled separately in isPublicPath since it must match only
// the exact root, not every path as a prefix match would).
var publicPaths = []string{
	"/login",
	"/logout",
	"/static/",
	"/login.html",
}

// signSession produces a signed session token encoding the given expiry.
// Format: base64url(expiryUnixDecimal) + "." + base64url(HMAC-SHA256(key, expiryUnixDecimal)).
func signSession(key []byte, expiry time.Time) string {
	expiryBytes := []byte(strconv.FormatInt(expiry.Unix(), 10))

	mac := hmac.New(sha256.New, key)
	mac.Write(expiryBytes)
	sig := mac.Sum(nil)

	return base64.RawURLEncoding.EncodeToString(expiryBytes) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// verifySession reports whether cookie is a validly signed, unexpired
// session token produced by signSession with the given key.
func verifySession(key []byte, cookie string) bool {
	parts := strings.SplitN(cookie, ".", 2)
	if len(parts) != 2 {
		return false
	}

	expiryBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, key)
	mac.Write(expiryBytes)
	expectedSig := mac.Sum(nil)
	if !hmac.Equal(sig, expectedSig) {
		return false
	}

	expiryUnix, err := strconv.ParseInt(string(expiryBytes), 10, 64)
	if err != nil {
		return false
	}

	return time.Unix(expiryUnix, 0).After(time.Now())
}

// checkPassword reports whether provided matches configured, using a
// constant-time comparison. Empty inputs never match.
func checkPassword(configured, provided string) bool {
	if len(configured) == 0 || len(provided) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(configured), []byte(provided)) == 1
}

// Authenticator implements password login and session-cookie validation for
// the lwd-web dashboard.
type Authenticator struct {
	key      []byte
	password string
	ttl      time.Duration
}

// NewAuthenticator creates an Authenticator with a default 24h session TTL.
func NewAuthenticator(key []byte, password string) *Authenticator {
	return &Authenticator{key: key, password: password, ttl: defaultTTL}
}

// Login handles POST /login: on a correct password it sets a signed session
// cookie and redirects to "/"; on failure it responds 401.
func (a *Authenticator) Login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	provided := r.FormValue("password")
	if !checkPassword(a.password, provided) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	expiry := time.Now().Add(a.ttl)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    signSession(a.key, expiry),
		Path:     "/",
		Expires:  expiry,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Logout clears the session cookie and redirects to the login page.
func (a *Authenticator) Logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})

	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// Middleware requires a valid session cookie for all requests except the
// login/logout endpoints and static assets. Unauthenticated API requests
// (path prefix "/api/") get a 401 JSON body; everything else is redirected
// to the login page.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie(sessionCookieName)
		if err == nil && verifySession(a.key, cookie.Value) {
			next.ServeHTTP(w, r)
			return
		}

		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}

		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func isPublicPath(path string) bool {
	// The app shell is served publicly at exactly "/"; its JS calls the
	// authed /api endpoints and redirects to /login itself on a 401. This
	// must be an exact match, not a prefix match (a prefix match on "/"
	// would make every path public).
	if path == "/" {
		return true
	}
	for _, p := range publicPaths {
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(path, p) {
				return true
			}
			continue
		}
		if path == p {
			return true
		}
	}
	return false
}
