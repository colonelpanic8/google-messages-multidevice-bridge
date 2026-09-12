package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/bridge"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
)

func TestServeKeepsHistoryAvailableWithoutPairingAndJoinsShutdown(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := bridge.New(s)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, b, false, "synthetic-test-token", listener) }()
	// There is no session and no cookies: this must not contact Google.
	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest("GET", "http://"+listener.Addr().String()+"/v1/conversations", nil)
	req.Header.Set("Authorization", "Bearer synthetic-test-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"conversations":[]`) {
		t.Fatalf("history unavailable: %d %s", resp.StatusCode, body)
	}
	select {
	case err := <-done:
		t.Fatalf("provider failure closed service: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}
