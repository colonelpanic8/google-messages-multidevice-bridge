package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/bridge"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
)

func fixture(t *testing.T) (*bridge.Bridge, *httptest.Server) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	b := bridge.New(s)
	server := httptest.NewServer(New(b, "test-token", nil))
	t.Cleanup(server.Close)
	return b, server
}

func TestAuthenticationAndCursorValidation(t *testing.T) {
	_, server := fixture(t)
	for _, test := range []struct {
		path, token string
		want        int
	}{
		{"/v1/status", "", 401}, {"/v1/status?token=test-token", "", 401},
		{"/v1/events", "incorrect", 401}, {"/v1/status", "test-token", 200},
		{"/v1/events?after=-1", "test-token", 400}, {"/v1/events?limit=1001", "test-token", 400},
		{"/v1/stream?after=garbage", "test-token", 400},
	} {
		req, _ := http.NewRequest("GET", server.URL+test.path, nil)
		if test.token != "" {
			req.Header.Set("Authorization", "Bearer "+test.token)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != test.want {
			t.Fatalf("%s: got %d want %d", test.path, resp.StatusCode, test.want)
		}
	}
}

func TestSSEReplayLiveAndResume(t *testing.T) {
	b, server := fixture(t)
	appendEvent := func(id string) {
		t.Helper()
		if _, err := b.Store.Append(store.Event{Type: "message", EntityID: id, Data: json.RawMessage(`{"schema":1,"text":"hi"}`)}); err != nil {
			t.Fatal(err)
		}
		b.Hub.Notify()
	}
	appendEvent("m1")
	appendEvent("m2")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/v1/stream?after=0", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Last-Event-ID", "1")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	next := func() (string, string) {
		t.Helper()
		id, kind := "", ""
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "id: ") {
				id = strings.TrimPrefix(line, "id: ")
			}
			if strings.HasPrefix(line, "event: ") {
				kind = strings.TrimPrefix(line, "event: ")
			}
			if line == "" && kind != "" {
				return id, kind
			}
		}
		t.Fatalf("stream ended: %v", scanner.Err())
		return "", ""
	}
	if id, kind := next(); id != "2" || kind != "message" {
		t.Fatalf("replay: %s %s", id, kind)
	}
	if id, kind := next(); id != "" || kind != "live" {
		t.Fatalf("replay boundary: %s %s", id, kind)
	}
	b.Hub.Publish(store.Event{Type: "typing", Time: time.Now(), Data: json.RawMessage(`{"conversationID":"c1"}`)})
	if id, kind := next(); id != "" || kind != "typing" {
		t.Fatalf("ephemeral: %s %s", id, kind)
	}
	appendEvent("m3")
	if id, kind := next(); id != "3" || kind != "message" {
		t.Fatalf("live: %s %s", id, kind)
	}
	items, _ := b.Store.Events(2, 10)
	if len(items) != 1 || items[0].ID != 3 {
		t.Fatal("typing corrupted durable cursor")
	}
}
