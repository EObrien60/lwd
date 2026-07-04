package web

import (
	"embed"
	"io/fs"
	"net/http"
)

// assetsFS embeds the placeholder dashboard UI (app shell, login page, CSS,
// JS). Task 5 replaces the contents of internal/web/assets/ with the real
// crafted UI; this embed and the routes below don't need to change.
//
//go:embed assets/*
var assetsFS embed.FS

// staticFS is assetsFS rooted at "assets", so paths under /static/ map
// directly to file names (e.g. /static/app.css -> assets/app.css).
var staticFS = mustSubFS(assetsFS, "assets")

func mustSubFS(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// indexHandler serves the app shell for GET /. It is public: the shell's JS
// calls the authed /api endpoints and redirects to /login itself on a 401,
// so there's no need to gate the shell markup behind a session.
func (s *Server) indexHandler(w http.ResponseWriter, r *http.Request) {
	s.serveAsset(w, r, "index.html")
}

// loginPageHandler serves the login form for GET /login.
func (s *Server) loginPageHandler(w http.ResponseWriter, r *http.Request) {
	s.serveAsset(w, r, "login.html")
}

func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	data, err := fs.ReadFile(staticFS, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// staticHandler serves /static/* from the embedded assets (app.css, app.js,
// and later alpine.min.js) with content types inferred from file extension
// via http.FileServerFS.
func (s *Server) staticHandler() http.Handler {
	return http.StripPrefix("/static/", http.FileServerFS(staticFS))
}
