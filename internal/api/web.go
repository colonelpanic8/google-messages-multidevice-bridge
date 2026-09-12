package api

import (
	"embed"
	"net/http"
)

//go:embed web/index.html web/style.css web/app.js web/stream.mjs
var webFiles embed.FS

func serveAsset(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if r.Method != "GET" && r.Method != "HEAD" {
		return false
	}
	var file, contentType string
	switch path {
	case "/favicon.ico":
		w.WriteHeader(http.StatusNoContent)
		return true
	case "/":
		file, contentType = "index.html", "text/html; charset=utf-8"
	case "/style.css":
		file, contentType = "style.css", "text/css; charset=utf-8"
	case "/app.js":
		file, contentType = "app.js", "text/javascript; charset=utf-8"
	case "/stream.mjs":
		file, contentType = "stream.mjs", "text/javascript; charset=utf-8"
	default:
		return false
	}
	data, err := webFiles.ReadFile("web/" + file)
	if err != nil {
		http.Error(w, "client unavailable", http.StatusInternalServerError)
		return true
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != "HEAD" {
		_, _ = w.Write(data)
	}
	return true
}
