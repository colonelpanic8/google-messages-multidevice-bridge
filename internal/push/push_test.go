package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
)

func testSender(t *testing.T) (*Sender, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sender, err := New(s, "https://example.invalid/bridge")
	if err != nil {
		t.Fatal(err)
	}
	return sender, s
}

func TestVAPIDIdentityIsStableAcrossSenders(t *testing.T) {
	first, s := testSender(t)
	second, err := New(s, "https://example.invalid/bridge")
	if err != nil {
		t.Fatal(err)
	}
	if first.PublicKey() == "" || first.PublicKey() != second.PublicKey() {
		t.Fatalf("key changed: %q %q", first.PublicKey(), second.PublicKey())
	}
}

func TestSubscribeRejectsIncompleteSubscriptionsAndDeduplicatesByEndpoint(t *testing.T) {
	sender, _ := testSender(t)
	for name, body := range map[string]string{
		"not json":     "{",
		"no endpoint":  `{"keys":{"auth":"a","p256dh":"b"}}`,
		"no auth":      `{"endpoint":"https://push.example/1","keys":{"p256dh":"b"}}`,
		"no p256dh":    `{"endpoint":"https://push.example/1","keys":{"auth":"a"}}`,
		"empty object": `{}`,
	} {
		if _, err := sender.Subscribe([]byte(body)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	valid := `{"endpoint":"https://push.example/1","keys":{"auth":"a","p256dh":"b"}}`
	for i := 0; i < 3; i++ {
		if _, err := sender.Subscribe([]byte(valid)); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := sender.Count(); err != nil || count != 1 {
		t.Fatalf("count %d %v", count, err)
	}
	if err := sender.Unsubscribe("https://push.example/1"); err != nil {
		t.Fatal(err)
	}
	if count, _ := sender.Count(); count != 0 {
		t.Fatalf("count after unsubscribe: %d", count)
	}
}

func TestSendDropsSubscriptionsThePushServiceRetired(t *testing.T) {
	var requests atomic.Int32
	status := http.StatusGone
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") == "" {
			t.Error("missing VAPID authorization header")
		}
		w.WriteHeader(status)
	}))
	defer server.Close()
	sender, _ := testSender(t)
	subscription := map[string]any{"endpoint": server.URL + "/sub", "keys": browserKeys(t)}
	raw, _ := json.Marshal(subscription)
	if _, err := sender.Subscribe(raw); err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(context.Background(), Notification{Title: "hi"}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests: %d", requests.Load())
	}
	if count, _ := sender.Count(); count != 0 {
		t.Fatalf("retired subscription kept: %d", count)
	}
	// A transient failure must keep the subscription for the next attempt.
	status = http.StatusInternalServerError
	if _, err := sender.Subscribe(raw); err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(context.Background(), Notification{Title: "hi"}); err == nil {
		t.Fatal("expected the 500 to surface")
	}
	if count, _ := sender.Count(); count != 1 {
		t.Fatalf("subscription dropped on a transient failure: %d", count)
	}
}

// browserKeys produces the pair a real browser would hand back, so payload
// encryption exercises the genuine code path.
func browserKeys(t *testing.T) map[string]string {
	t.Helper()
	private, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err = rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"auth":   base64.RawURLEncoding.EncodeToString(auth),
		"p256dh": base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes()),
	}
}
