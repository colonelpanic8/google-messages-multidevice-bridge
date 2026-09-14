package api

import (
	"net/http"
	"testing"
)

// The desktop app bundles the client, so it reaches the API from a foreign
// origin and every authenticated call is preflighted first.
func TestDesktopOriginIsPreflightedAndAllowed(t *testing.T) {
	_, server := fixture(t)
	const origin = "tauri://localhost"

	req, _ := http.NewRequest("OPTIONS", server.URL+"/v1/sync", nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != origin {
		t.Fatalf("allow-origin %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); got != "Authorization, Content-Type" {
		t.Fatalf("allow-headers %q", got)
	}

	req, _ = http.NewRequest("POST", server.URL+"/v1/sync", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Origin", origin)
	resp, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("desktop origin rejected")
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != origin {
		t.Fatalf("allow-origin %q", got)
	}
}

// A preflight carries no Authorization header, so it is answered before the
// bearer check. An unknown origin must not get that answer.
func TestPreflightFromUnknownOriginIsRejected(t *testing.T) {
	_, server := fixture(t)
	req, _ := http.NewRequest("OPTIONS", server.URL+"/v1/sync", nil)
	req.Header.Set("Origin", "https://unrelated.invalid")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("unknown origin was allowed")
	}
}

func TestBridgeOwnOriginStillAllowed(t *testing.T) {
	_, server := fixture(t)
	req, _ := http.NewRequest("POST", server.URL+"/v1/sync", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Origin", server.URL)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("same origin rejected")
	}
}
