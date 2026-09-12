package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/bridge"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
)

// TestBrowserFixture is an opt-in, synthetic-only browser test server.
func TestBrowserFixture(t *testing.T) {
	if os.Getenv("BRIDGE_BROWSER_TEST") != "1" {
		t.Skip("set BRIDGE_BROWSER_TEST=1 for the synthetic browser fixture")
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "browser.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := bridge.New(s)
	appendRecord := func(kind, id string, value any) {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Append(store.Event{Type: kind, EntityID: id, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	appendRecord("conversation", "synthetic-conversation", model.Conversation{Schema: 1, ID: "synthetic-conversation", Name: "Weekend plans", Preview: "See you at the park!", Protocol: "rcs", State: "active", Updated: now, Unread: true, Participants: []model.Participant{{ID: "friend", Name: "Alex (synthetic)", Address: "+1 555 0100"}, {ID: "me", Name: "You", IsMe: true}}})
	appendRecord("conversation", "synthetic-sms", model.Conversation{Schema: 1, ID: "synthetic-sms", Name: "Project notes", Preview: "The bridge keeps our history together.", Protocol: "sms", State: "active", Updated: now.Add(-time.Hour), Participants: []model.Participant{{ID: "teammate", Name: "Sam (synthetic)"}}})
	appendRecord("message", "m1", model.Message{Schema: 1, ID: "m1", ConversationID: "synthetic-conversation", SenderID: "friend", Time: now.Add(-3 * time.Minute), Text: "Want to meet at the park this afternoon?", Direction: "incoming", Status: "incoming_displayed", Reactions: []model.Reaction{}, Attachments: []model.Attachment{}})
	appendRecord("message", "m2", model.Message{Schema: 1, ID: "m2", ConversationID: "synthetic-conversation", SenderID: "me", Time: now.Add(-2 * time.Minute), Text: "Sounds good! Around 3 works for me.", Direction: "outgoing", Status: "outgoing_displayed", Reactions: []model.Reaction{{Emoji: "👍", Participants: []string{"friend"}}}, Attachments: []model.Attachment{}})
	appendRecord("message", "m3", model.Message{Schema: 1, ID: "m3", ConversationID: "synthetic-conversation", SenderID: "friend", Time: now.Add(-time.Minute), Text: "See you at the park!", Direction: "incoming", Status: "incoming_complete", Reactions: []model.Reaction{}, Attachments: []model.Attachment{}})
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	server := &http.Server{Handler: New(b, "synthetic-browser-test-token-only"), ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Printf("SYNTHETIC_BROWSER_PORT=%d\n", listener.Addr().(*net.TCPAddr).Port)
	select {
	case <-ctx.Done():
	case err = <-done:
		t.Fatal(err)
	}
	shutdownCtx, finish := context.WithTimeout(context.Background(), time.Second)
	defer finish()
	_ = server.Shutdown(shutdownCtx)
}
