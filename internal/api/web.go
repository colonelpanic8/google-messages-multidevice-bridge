package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/client"
)

// assetVersion fingerprints the embedded client so a new build always
// invalidates the service worker's cached shell.
var assetVersion = fingerprint()

func fingerprint() string {
	var names []string
	_ = fs.WalkDir(client.Files, ".", func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			names = append(names, path)
		}
		return err
	})
	sort.Strings(names)
	sum := sha256.New()
	for _, name := range names {
		data, err := client.Files.ReadFile(name)
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
}

var assets = map[string]asset{
	"/":                            {"index.html", "text/html; charset=utf-8"},
	"/style.css":                   {"style.css", "text/css; charset=utf-8"},
	"/app.js":                      {"app.js", "text/javascript; charset=utf-8"},
	"/stream.mjs":                  {"stream.mjs", "text/javascript; charset=utf-8"},
	"/view.mjs":                    {"view.mjs", "text/javascript; charset=utf-8"},
	"/emoji.mjs":                   {"emoji.mjs", "text/javascript; charset=utf-8"},
	"/emoji-data.mjs":              {"emoji-data.mjs", "text/javascript; charset=utf-8"},
	"/manifest.webmanifest":        {"manifest.webmanifest", "application/manifest+json; charset=utf-8"},
	"/favicon.svg":                 {"favicon.svg", "image/svg+xml"},
	"/sw.js":                       {"sw.js", "text/javascript; charset=utf-8"},
	"/icons/icon-192.png":          {"icons/icon-192.png", "image/png"},
	"/icons/icon-512.png":          {"icons/icon-512.png", "image/png"},
	"/icons/favicon-32.png":        {"icons/favicon-32.png", "image/png"},
	"/icons/icon-maskable-512.png": {"icons/icon-maskable-512.png", "image/png"},
}

func serveAsset(w http.ResponseWriter, r *http.Request, networks ...string) bool {
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
	network := "google-messages"
	if len(networks) > 0 && networks[0] == "whatsapp" {
		network = "whatsapp"
	}
	key := network + ":" + entry.file
	cached, _ := assetBodies.LoadOrStore(key, sync.OnceValues(func() (*assetBody, error) {
		return prepareAsset(entry, network)
	}))
	body, err := cached.(func() (*assetBody, error))()
	if err != nil {
		http.Error(w, "client unavailable", http.StatusInternalServerError)
		return true
	}
	data := body.plain
	etag := body.etag
	if len(body.gzip) > 0 {
		w.Header().Set("Vary", "Accept-Encoding")
		if acceptsGzip(r.Header.Get("Accept-Encoding")) {
			data = body.gzip
			etag += "-gzip"
			w.Header().Set("Content-Encoding", "gzip")
		}
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Content-Type", entry.contentType)
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	http.ServeContent(w, r, entry.file, time.Time{}, bytes.NewReader(data))
	return true
}

// AssetVersion identifies the embedded client build.
func AssetVersion() string { return assetVersion }

var assetBodies sync.Map

type assetBody struct {
	plain, gzip []byte
	etag        string
}

func prepareAsset(entry asset, network string) (*assetBody, error) {
	data, err := client.Files.ReadFile(entry.file)
	if err != nil {
		return nil, err
	}
	if network == "whatsapp" && (entry.file == "index.html" || entry.file == "manifest.webmanifest") {
		data = bytes.ReplaceAll(data, []byte("Google Messages Multi-Device Bridge"), []byte("WhatsApp Multi-Device Bridge"))
		data = bytes.ReplaceAll(data, []byte("Google Messages"), []byte("WhatsApp"))
		data = bytes.ReplaceAll(data, []byte("Google account"), []byte("WhatsApp account"))
		data = bytes.ReplaceAll(data, []byte("Google may return an existing matching conversation; WhatsApp creates"), []byte("WhatsApp creates"))
		data = bytes.ReplaceAll(data, []byte("<title>Messages</title>"), []byte("<title>WhatsApp</title>"))
		data = bytes.ReplaceAll(data, []byte(`"short_name": "Messages"`), []byte(`"short_name": "WhatsApp"`))
		data = bytes.ReplaceAll(data, []byte("Read shared SMS, MMS, and RCS history and send through your Android phone."), []byte("Your linked WhatsApp device and encrypted message archive."))
	}
	if entry.file == "sw.js" {
		data = bytes.ReplaceAll(data, []byte("__ASSET_VERSION__"), []byte(assetVersion+"-"+network))
	}
	sum := sha256.Sum256(data)
	body := &assetBody{plain: data, etag: hex.EncodeToString(sum[:])}
	if len(data) >= 1024 && (strings.HasPrefix(entry.contentType, "text/") || strings.Contains(entry.contentType, "json") || strings.Contains(entry.contentType, "svg")) {
		var compressed bytes.Buffer
		writer, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
		if err != nil {
			return nil, err
		}
		if _, err = writer.Write(data); err != nil {
			return nil, err
		}
		if err = writer.Close(); err != nil {
			return nil, err
		}
		body.gzip = compressed.Bytes()
	}
	return body, nil
}

func acceptsGzip(header string) bool {
	wildcard := false
	for _, coding := range strings.Split(header, ",") {
		parts := strings.Split(coding, ";")
		name := strings.ToLower(strings.TrimSpace(parts[0]))
		quality := 1.0
		for _, parameter := range parts[1:] {
			key, value, ok := strings.Cut(parameter, "=")
			if ok && strings.EqualFold(strings.TrimSpace(key), "q") {
				var err error
				quality, err = strconv.ParseFloat(strings.TrimSpace(value), 64)
				if err != nil {
					quality = 0
				}
			}
		}
		accepted := quality > 0 && quality <= 1
		if name == "gzip" {
			return accepted
		}
		if name == "*" {
			wildcard = accepted
		}
	}
	return wildcard
}
