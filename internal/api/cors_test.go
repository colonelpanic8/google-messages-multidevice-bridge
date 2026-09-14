package api

import (
	"net/http"
	"strings"
	"testing"
)

// The desktop app bundles the client, so it reaches the API from a foreign
// origin and every authenticated call is preflighted first.
func TestDesktopOriginIsPreflightedAndAllowed(t *testing.T) {
	_, server := fixture(t)
	const origin = "tauri://localhost"

	// The headers a send carries.
	req, _ := http.NewRequest("OPTIONS", server.URL+"/v1/messages", nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type,idempotency-key")
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
	allowed := map[string]bool{}
	for _, name := range strings.Split(resp.Header.Get("Access-Control-Allow-Headers"), ",") {
		allowed[strings.ToLower(strings.TrimSpace(name))] = true
	}
	for _, name := range strings.Split(req.Header.Get("Access-Control-Request-Headers"), ",") {
		if !allowed[name] {
			t.Fatalf("allow-headers %q omits %s", resp.Header.Get("Access-Control-Allow-Headers"), name)
		}
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
