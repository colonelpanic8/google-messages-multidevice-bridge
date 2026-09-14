package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
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
	go func() { done <- serve(ctx, b, false, "synthetic-test-token", listener, nil) }()
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

func TestSecretValueReadsPassWithoutShellOrLeakingErrors(t *testing.T) {
	dir := t.TempDir()
	// The test executable stands in for pass without consulting a password store.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(exe, filepath.Join(dir, "pass")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("BRIDGE_TEST_PASS", "1")
	for _, entry := range []string{"-option", "bad\nentry"} {
		if _, err := secretValue("IGNORED", entry, ""); err == nil {
			t.Fatal("accepted unsafe entry")
		}
	}
	value, err := secretValue("IGNORED", "entry with $literal and `ticks`", "")
	if err != nil || value != "synthetic-secret" {
		t.Fatalf("value=%q err=%v", value, err)
	}
	t.Setenv("BRIDGE_TEST_PASS", "fail")
	value, err = secretValue("IGNORED", "valid-entry", "")
	if value != "" || err == nil || strings.Contains(err.Error(), "private-output") {
		t.Fatal("failed secret lookup exposed output")
	}
	t.Setenv("BRIDGE_TEST_PASS", "oversize")
	if value, err = secretValue("IGNORED", "valid-entry", ""); err == nil || value != "" {
		t.Fatal("accepted oversized output")
	}
}
func TestSecretValueReadsFilesWithoutPass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("  file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGE_SECRET", "from-env")
	// The file wins over both the pass entry and the environment.
	value, err := secretValue("BRIDGE_SECRET", "some-entry", path)
	if err != nil || value != "file-secret" {
		t.Fatalf("value=%q err=%v", value, err)
	}
	if _, err = secretValue("BRIDGE_SECRET", "", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("accepted a missing secret file")
	}
	oversize := filepath.Join(t.TempDir(), "oversize")
	if err = os.WriteFile(oversize, make([]byte, 4097), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = secretValue("BRIDGE_SECRET", "", oversize); err == nil {
		t.Fatal("accepted an oversized secret file")
	}
}

func TestMain(m *testing.M) {
	if mode := os.Getenv("BRIDGE_TEST_PASS"); mode != "" {
		if len(os.Args) != 3 || os.Args[1] != "show" {
			os.Exit(2)
		}
		switch mode {
		case "fail":
			_, _ = os.Stdout.WriteString("private-output")
			os.Exit(1)
		case "oversize":
			_, _ = os.Stdout.WriteString(strings.Repeat("x", 8192))
		default:
			_, _ = os.Stdout.WriteString("synthetic-secret\n")
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
