package api

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed ui/*
var operatorUI embed.FS

func (h *Handler) redirectUI(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/", http.StatusTemporaryRedirect)
}

func (h *Handler) uiHandler() http.Handler {
	content, err := fs.Sub(operatorUI, "ui")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(content))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path == "/ui/" {
			w.Header().Set("Cache-Control", "no-store")
		}
		http.StripPrefix("/ui/", files).ServeHTTP(w, r)
	})
}
