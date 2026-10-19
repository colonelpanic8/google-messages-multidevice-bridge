package api

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestAssetCompressionAndRevalidation(t *testing.T) {
	for _, network := range []string{"google-messages", "whatsapp"} {
		for _, path := range []string{"/", "/app.js", "/style.css", "/emoji-data.mjs", "/sw.js", "/icons/icon-192.png"} {
			t.Run(network+path, func(t *testing.T) {
				get := func(method, encoding, etag string) *httptest.ResponseRecorder {
					r := httptest.NewRequest(method, path, nil)
					r.Header.Set("Accept-Encoding", encoding)
					r.Header.Set("If-None-Match", etag)
					w := httptest.NewRecorder()
					if !serveAsset(w, r, network) {
						t.Fatal("asset not handled")
					}
					return w
				}
				plain := get("GET", "", "")
				compressed := get("GET", "gzip", "")
				if plain.Code != 200 || compressed.Code != 200 {
					t.Fatal(plain.Code, compressed.Code)
				}
				data := compressed.Body.Bytes()
				if compressed.Header().Get("Content-Encoding") == "gzip" {
					reader, err := gzip.NewReader(bytes.NewReader(data))
					if err != nil {
						t.Fatal(err)
					}
					data, err = io.ReadAll(reader)
					if err != nil {
						t.Fatal(err)
					}
					reader.Close()
					if compressed.Body.Len() >= plain.Body.Len() {
						t.Fatal("compression did not reduce transfer size")
					}
					if compressed.Header().Get("Vary") != "Accept-Encoding" {
						t.Fatal("missing encoding variation")
					}
					if compressed.Header().Get("ETag") == plain.Header().Get("ETag") {
						t.Fatal("representations share a strong ETag")
					}
				} else if path == "/app.js" {
					t.Fatal("JavaScript not compressed")
				}
				if !bytes.Equal(data, plain.Body.Bytes()) {
					t.Fatal("compression changed asset")
				}
				etag := compressed.Header().Get("ETag")
				if etag == "" || compressed.Header().Get("Cache-Control") != "no-cache" {
					t.Fatal("missing revalidation headers")
				}
				cached := get("GET", "gzip", etag)
				if cached.Code != 304 || cached.Body.Len() != 0 {
					t.Fatal("conditional request did not revalidate")
				}
				head := get("HEAD", "gzip", "")
				if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(compressed.Body.Len()) {
					t.Fatal("incorrect HEAD response")
				}
				if get("GET", "gzip;q=0", "").Header().Get("Content-Encoding") != "" {
					t.Fatal("ignored gzip exclusion")
				}
				if path == "/" && network == "whatsapp" && !strings.Contains(string(data), "<title>WhatsApp</title>") {
					t.Fatal("lost network branding")
				}
			})
		}
	}
}

func TestAssetVariantsDoNotShareValidators(t *testing.T) {
	validators := map[string]bool{}
	for _, network := range []string{"google-messages", "whatsapp"} {
		w := httptest.NewRecorder()
		serveAsset(w, httptest.NewRequest("GET", "/sw.js", nil), network)
		etag := w.Header().Get("ETag")
		if validators[etag] {
			t.Fatal("network variants share validator")
		}
		validators[etag] = true
	}
}

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"": false, "br": false, "gzip": true, "br, GZip;q=0.5": true,
		"gzip;q=0": false, "*;q=1, gzip;q=0": false, "*;q=0.5": true,
		"gzip;q=invalid": false, "gzip;q=NaN": false, "gzip;q=2": false,
	} {
		if got := acceptsGzip(header); got != want {
			t.Errorf("%q: got %v, want %v", header, got, want)
		}
	}
}
