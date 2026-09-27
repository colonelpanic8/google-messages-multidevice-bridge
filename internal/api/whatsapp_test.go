package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestWhatsAppAssetsAndPhonePairing(t *testing.T) {
	b, server := fixture(t)
	if err := b.SetNetwork("whatsapp"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/manifest.webmanifest"} {
		resp, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "Google") || !strings.Contains(string(raw), "WhatsApp") {
			t.Fatal(path, "wrong network branding")
		}
	}
	req, _ := http.NewRequest("POST", server.URL+"/v1/pairing/phone", strings.NewReader(`{"phone_number":"+14155550100"}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var state map[string]any
	if err = json.NewDecoder(resp.Body).Decode(&state); err != nil || resp.StatusCode != 202 || state["state"] != "connecting" {
		t.Fatal(resp.StatusCode, state, err)
	}
	req, _ = http.NewRequest("GET", server.URL+"/v1/pairing/qr", nil)
	resp, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("QR route was public", resp.StatusCode)
	}
}
