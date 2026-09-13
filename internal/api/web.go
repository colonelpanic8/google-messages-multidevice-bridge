package api

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"sort"
)

//go:embed web/index.html web/style.css web/app.js web/stream.mjs web/view.mjs web/sw.js web/manifest.webmanifest web/icons
var webFiles embed.FS

// assetVersion fingerprints the embedded client so a new build always
// invalidates the service worker's cached shell.
var assetVersion = fingerprint()

func fingerprint() string {
	var names []string
	_ = fs.WalkDir(webFiles, "web", func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			names = append(names, path)
		}
		return err
	})
	sort.Strings(names)
	sum := sha256.New()
	for _, name := range names {
		data, err := webFiles.ReadFile(name)
		if err != nil {
			continue
		}
		sum.Write([]byte(name))
		sum.Write(data)
	}
	return hex.EncodeToString(sum.Sum(nil))[:12]
}

type asset struct {
	file, contentType string
	revalidate        bool
}

var assets = map[string]asset{
	"/":                            {"index.html", "text/html; charset=utf-8", false},
	"/style.css":                   {"style.css", "text/css; charset=utf-8", false},
	"/app.js":                      {"app.js", "text/javascript; charset=utf-8", false},
	"/stream.mjs":                  {"stream.mjs", "text/javascript; charset=utf-8", false},
	"/view.mjs":                    {"view.mjs", "text/javascript; charset=utf-8", false},
	"/manifest.webmanifest":        {"manifest.webmanifest", "application/manifest+json; charset=utf-8", false},
	"/sw.js":                       {"sw.js", "text/javascript; charset=utf-8", true},
	"/icons/icon-192.png":          {"icons/icon-192.png", "image/png", false},
	"/icons/icon-512.png":          {"icons/icon-512.png", "image/png", false},
	"/icons/favicon-32.png":        {"icons/favicon-32.png", "image/png", false},
	"/icons/icon-maskable-512.png": {"icons/icon-maskable-512.png", "image/png", false},
}

func serveAsset(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != "GET" && r.Method != "HEAD" {
		return false
	}
	if r.URL.Path == "/favicon.ico" {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	entry, ok := assets[r.URL.Path]
	if !ok {
		return false
	}
	data, err := webFiles.ReadFile("web/" + entry.file)
	if err != nil {
		http.Error(w, "client unavailable", http.StatusInternalServerError)
		return true
	}
	if entry.file == "sw.js" {
		data = bytes.ReplaceAll(data, []byte("__ASSET_VERSION__"), []byte(assetVersion))
	}
	w.Header().Set("Content-Type", entry.contentType)
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if entry.revalidate {
		// The worker decides when the rest of the shell is replaced, so it must
		// never be served from a stale browser cache.
		w.Header().Set("Cache-Control", "no-cache")
	}
	if r.Method != "HEAD" {
		_, _ = w.Write(data)
	}
	return true
}

// AssetVersion identifies the embedded client build.
func AssetVersion() string { return assetVersion }
