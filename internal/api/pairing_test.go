package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/bridge"
)

func pairingRequest(t *testing.T, server *httptest.Server, method, requestPath, token, contentType string, body io.Reader) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+requestPath, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func requiredPairingCookies() map[string]string {
	return map[string]string{
		"SID":     "synthetic-sid",
		"HSID":    "synthetic-hsid",
		"OSID":    "synthetic-osid",
		"SSID":    "synthetic-ssid",
		"APISID":  "synthetic-apisid",
		"SAPISID": "synthetic-sapisid",
	}
}

func pairingCredentialsBody(t *testing.T, ticket string, cookies map[string]string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"ticket": ticket, "cookies": cookies})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPairingControlRoutesRequireBearerAuthentication(t *testing.T) {
	_, server := fixture(t)
	for _, test := range []struct {
		name, method, path, token string
		want                      int
	}{
		{name: "status missing bearer", method: "GET", path: "/v1/pairing", want: 401},
		{name: "status query token", method: "GET", path: "/v1/pairing?token=test-token", want: 401},
		{name: "status wrong bearer", method: "GET", path: "/v1/pairing", token: "wrong", want: 401},
		{name: "status authorized", method: "GET", path: "/v1/pairing", token: "test-token", want: 200},
		{name: "start missing bearer", method: "POST", path: "/v1/pairing/start", want: 401},
		{name: "cancel missing bearer", method: "POST", path: "/v1/pairing/cancel", want: 401},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := pairingRequest(t, server, test.method, test.path, test.token, "", nil)
			requireStatus(t, status, body, test.want)
		})
	}

	status, body := pairingRequest(t, server, "POST", "/v1/pairing/start", "test-token", "application/json", bytes.NewBufferString(`{}`))
	requireStatus(t, status, body, http.StatusAccepted)
	var started bridge.PairingState
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatal(err)
	}
	if started.State != "waiting_for_login" || len(started.Ticket) != 43 || started.Expires.IsZero() {
		t.Fatalf("pairing start: state=%q ticket-length=%d expires=%v", started.State, len(started.Ticket), started.Expires)
	}
	status, body = pairingRequest(t, server, "GET", "/v1/pairing", "test-token", "", nil)
	requireStatus(t, status, body, http.StatusOK)
	var current bridge.PairingState
	if err := json.Unmarshal(body, &current); err != nil || current.State != started.State || current.Ticket != started.Ticket {
		t.Fatalf("pairing status did not retain waiting ticket: state=%q ticket-match=%v err=%v", current.State, current.Ticket == started.Ticket, err)
	}
}

func TestPairingCredentialsAreStrictTicketOnlyAndOneUse(t *testing.T) {
	_, server := fixture(t)
	status, body := pairingRequest(t, server, "POST", "/v1/pairing/start", "test-token", "application/json", bytes.NewBufferString(`{}`))
	requireStatus(t, status, body, http.StatusAccepted)
	var started bridge.PairingState
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatal(err)
	}
	cookies := requiredPairingCookies()

	status, body = pairingRequest(t, server, "POST", "/v1/pairing/credentials", "test-token", "application/json", bytes.NewReader(pairingCredentialsBody(t, "", cookies)))
	requireStatus(t, status, body, http.StatusForbidden)
	status, body = pairingRequest(t, server, "POST", "/v1/pairing/credentials", "", "application/json", bytes.NewReader(pairingCredentialsBody(t, "invalid-ticket", cookies)))
	requireStatus(t, status, body, http.StatusForbidden)

	missing := requiredPairingCookies()
	delete(missing, "SID")
	status, body = pairingRequest(t, server, "POST", "/v1/pairing/credentials", "", "application/json", bytes.NewReader(pairingCredentialsBody(t, started.Ticket, missing)))
	requireStatus(t, status, body, http.StatusBadRequest)

	for _, test := range []struct {
		name, contentType, body string
	}{
		{name: "wrong content type", contentType: "text/plain", body: string(pairingCredentialsBody(t, started.Ticket, cookies))},
		{name: "content type parameters", contentType: "application/json; charset=utf-8", body: string(pairingCredentialsBody(t, started.Ticket, cookies))},
		{name: "unknown field", contentType: "application/json", body: `{"ticket":"` + started.Ticket + `","cookies":{},"unknown":true}`},
		{name: "multiple values", contentType: "application/json", body: string(pairingCredentialsBody(t, started.Ticket, cookies)) + `{}`},
		{name: "invalid cookie value", contentType: "application/json", body: `{"ticket":"` + started.Ticket + `","cookies":{"SID":1}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := pairingRequest(t, server, "POST", "/v1/pairing/credentials", "", test.contentType, strings.NewReader(test.body))
			requireStatus(t, status, body, http.StatusBadRequest)
		})
	}

	status, body = pairingRequest(t, server, "POST", "/v1/pairing/credentials", "", "application/json", bytes.NewReader(pairingCredentialsBody(t, started.Ticket, cookies)))
	requireStatus(t, status, body, http.StatusAccepted)
	status, body = pairingRequest(t, server, "POST", "/v1/pairing/credentials", "", "application/json", bytes.NewReader(pairingCredentialsBody(t, started.Ticket, cookies)))
	requireStatus(t, status, body, http.StatusForbidden)

	status, body = pairingRequest(t, server, "GET", "/v1/pairing", "test-token", "", nil)
	requireStatus(t, status, body, http.StatusOK)
	var current bridge.PairingState
	if err := json.Unmarshal(body, &current); err != nil {
		t.Fatal(err)
	}
	if current.State != "connecting" || current.Ticket != "" {
		t.Fatalf("consumed ticket remained visible: state=%q ticket-length=%d", current.State, len(current.Ticket))
	}
}

func TestPairingHelperZIPIsInstallableAndExcludesTests(t *testing.T) {
	_, server := fixture(t)
	status, body := pairingRequest(t, server, "GET", "/pairing-helper.zip", "", "", nil)
	requireStatus(t, status, body, http.StatusOK)
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string][]byte)
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		if file.Name == "" || path.IsAbs(file.Name) || strings.HasPrefix(file.Name, "../") || strings.Contains(file.Name, "/../") {
			t.Fatalf("unsafe ZIP path %q", file.Name)
		}
		if strings.Contains(file.Name, ".test.") {
			t.Fatalf("test source included in helper ZIP: %q", file.Name)
		}
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(r)
		closeErr := r.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read %q: %v %v", file.Name, readErr, closeErr)
		}
		files[file.Name] = data
	}
	for _, required := range []string{"manifest.json", "service-worker.js", "setup.html", "setup.mjs", "setup.css", "core.mjs"} {
		if len(files[required]) == 0 {
			t.Errorf("helper ZIP missing %s", required)
		}
	}
	if _, ok := files["core.test.mjs"]; ok {
		t.Fatal("helper ZIP includes core.test.mjs")
	}
	var manifest struct {
		ManifestVersion int      `json:"manifest_version"`
		Name            string   `json:"name"`
		Version         string   `json:"version"`
		Permissions     []string `json:"permissions"`
		Background      struct {
			ServiceWorker string `json:"service_worker"`
		} `json:"background"`
	}
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ManifestVersion != 3 || manifest.Name == "" || manifest.Version == "" || manifest.Background.ServiceWorker == "" {
		t.Fatalf("invalid extension manifest: %+v", manifest)
	}
	if len(files[manifest.Background.ServiceWorker]) == 0 {
		t.Fatalf("manifest service worker %q missing", manifest.Background.ServiceWorker)
	}
	foundCookies := false
	for _, permission := range manifest.Permissions {
		foundCookies = foundCookies || permission == "cookies"
	}
	if !foundCookies {
		t.Fatal("manifest lacks cookies permission")
	}
}
