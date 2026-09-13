package api

import (
	"archive/zip"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/bridge"
)

//go:embed pairinghelper
var pairingHelper embed.FS

func registerPairing(mux *http.ServeMux, b *bridge.Bridge) {
	mux.HandleFunc("GET /v1/pairing", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, b.PairingStatus()) })
	mux.HandleFunc("POST /v1/pairing/start", func(w http.ResponseWriter, r *http.Request) {
		state, err := b.BeginPairing()
		if err != nil {
			apiError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, state)
	})
	mux.HandleFunc("POST /v1/pairing/cancel", func(w http.ResponseWriter, r *http.Request) {
		state, err := b.CancelPairingAndWait(r.Context())
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, state)
	})
}
func pairingCredentials(w http.ResponseWriter, r *http.Request, b *bridge.Bridge) {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// This endpoint accepts only a short-lived, one-use pairing ticket. A normal
	// browser page cannot authenticate here with cookies or the bridge bearer.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req struct {
		Ticket  string            `json:"ticket"`
		Cookies map[string]string `json:"cookies"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if r.Header.Get("Content-Type") != "application/json" || decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid pairing handoff", 400)
		return
	}
	err := b.SubmitPairingCookies(req.Ticket, req.Cookies)
	if errors.Is(err, bridge.ErrPairingTicket) {
		http.Error(w, "invalid or expired pairing ticket", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "Google sign-in cookies are incomplete", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
func servePairingHelper(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/pairing-helper.zip" || (r.Method != "GET" && r.Method != "HEAD") {
		return false
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="google-messages-pairing-helper.zip"`)
	if r.Method == "HEAD" {
		return true
	}
	archive := zip.NewWriter(w)
	_ = fs.WalkDir(pairingHelper, "pairinghelper", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.Contains(path, ".test.") {
			return nil
		}
		data, err := pairingHelper.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := archive.Create(strings.TrimPrefix(path, "pairinghelper/"))
		if err != nil {
			return err
		}
		_, err = file.Write(data)
		return err
	})
	_ = archive.Close()
	return true
}
