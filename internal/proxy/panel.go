package proxy

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed web/*
var webFS embed.FS

func mountPanel(mux *http.ServeMux, auth *PanelAuth) {
	h := panelHandler()
	// The dashboard shell and its script require a dashboard login.
	for _, p := range []string{
		"GET /{$}",
		"GET /index.html",
		"GET /panel",
		"GET /panel/",
		"GET /app.js",
	} {
		mux.Handle(p, auth.requirePage(h))
	}
	// The login screen and its static assets stay public.
	for _, p := range []string{
		"GET /app.css",
		"GET /quick.png",
		"GET /favicon.ico",
	} {
		mux.Handle(p, h)
	}
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if !auth.Enabled() || auth.Authenticated(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		h.ServeHTTP(w, r)
	})
	mux.HandleFunc("/auth/login", auth.handleLogin)
	mux.HandleFunc("/auth/logout", auth.handleLogout)
	mux.HandleFunc("GET /auth/status", auth.handleStatus)
}

func panelHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.Trim(r.URL.Path, "/")
		if name == "" || name == "panel" || name == "index.html" {
			name = "index.html"
		}
		if name == "login" {
			name = "login.html"
		}
		if name == "favicon.ico" {
			name = "quick.png"
		}
		data, err := webFS.ReadFile("web/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		ctype := "text/html; charset=utf-8"
		switch {
		case strings.HasSuffix(name, ".css"):
			ctype = "text/css; charset=utf-8"
		case strings.HasSuffix(name, ".js"):
			ctype = "text/javascript; charset=utf-8"
		case name == "quick.png":
			ctype = "image/png"
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	})
}
