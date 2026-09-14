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
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/bridge"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
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
	availableMediaID := strings.Repeat("a", 64)
	unavailableMediaID := strings.Repeat("b", 64)
	if err := s.PutMedia(availableMediaID, []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="320" height="180"><rect width="320" height="180" fill="#e4f1ed"/><text x="24" y="96" fill="#186a65">Synthetic attachment</text></svg>`)); err != nil {
		t.Fatal(err)
	}
	appendRecord("message", "m1", model.Message{Schema: 1, ID: "m1", ConversationID: "synthetic-conversation", SenderID: "friend", Time: now.Add(-3 * time.Minute), Text: "Want to meet at the park this afternoon?", Direction: "incoming", Status: "incoming_displayed", Reactions: []model.Reaction{}, Attachments: []model.Attachment{}})
	appendRecord("message", "m2", model.Message{Schema: 1, ID: "m2", ConversationID: "synthetic-conversation", SenderID: "me", Time: now.Add(-2 * time.Minute), Text: "Sounds good! Around 3 works for me.", Direction: "outgoing", Status: "outgoing_displayed", Reactions: []model.Reaction{{Emoji: "👍", Participants: []string{"friend", "me"}}, {Emoji: "🎉", Participants: []string{"friend"}}}, Attachments: []model.Attachment{}})
	appendRecord("message", "m3", model.Message{Schema: 1, ID: "m3", ConversationID: "synthetic-conversation", SenderID: "friend", Time: now.Add(-time.Minute), Text: "See you at the park! The video is still on the phone.", Direction: "incoming", Status: "incoming_complete", Reactions: []model.Reaction{}, Attachments: []model.Attachment{{ID: unavailableMediaID, Name: "park-preview.mp4", MIME: "video/mp4", Size: 7340032, Available: false}}})
	appendRecord("message", "m4", model.Message{Schema: 1, ID: "m4", ConversationID: "synthetic-conversation", SenderID: "me", Time: now, Direction: "outgoing", Status: "outgoing_complete", Reactions: []model.Reaction{}, Attachments: []model.Attachment{{ID: availableMediaID, Name: "park-map.svg", MIME: "image/svg+xml", Size: 171, Available: true}}})
	// The conversation snapshots arrive after the messages they describe, as
	// they do from Google.
	appendRecord("conversation", "synthetic-conversation", model.Conversation{Schema: 1, ID: "synthetic-conversation", Name: "Weekend plans", Preview: "See you at the park!", Protocol: "rcs", State: "active", Updated: now, Unread: true, Participants: []model.Participant{{ID: "friend", Name: "Alex (synthetic)", Address: "+1 555 0100"}, {ID: "me", Name: "You", IsMe: true}}})
	appendRecord("conversation", "synthetic-sms", model.Conversation{Schema: 1, ID: "synthetic-sms", Name: "Project notes", Preview: "The bridge keeps our history together.", Protocol: "sms", State: "active", Updated: now.Add(-time.Hour), Participants: []model.Participant{{ID: "teammate", Name: "Sam (synthetic)"}}})
	// Long enough that reaching the start needs several bulk pages.
	for i := 1; i <= 1500; i++ {
		sender, direction, status := "archivist", "incoming", "incoming_complete"
		if i%2 == 0 {
			sender, direction, status = "me", "outgoing", "outgoing_complete"
		}
		id := fmt.Sprintf("long-%04d", i)
		appendRecord("message", id, model.Message{Schema: 1, ID: id, ConversationID: "synthetic-long", SenderID: sender, Time: now.Add(time.Duration(i-1501) * time.Hour), Text: fmt.Sprintf("Message %d", i), Direction: direction, Status: status, Reactions: []model.Reaction{}, Attachments: []model.Attachment{}})
	}
	appendRecord("conversation", "synthetic-long", model.Conversation{Schema: 1, ID: "synthetic-long", Name: "Long history", Preview: "Message 1500", Protocol: "rcs", State: "active", Updated: now.Add(-2 * time.Hour), Participants: []model.Participant{{ID: "archivist", Name: "Robin (synthetic)", Address: "+1 555 0142"}, {ID: "me", Name: "You", IsMe: true}}})
	appendRecord("conversation", "synthetic-group", model.Conversation{Schema: 1, ID: "synthetic-group", Name: "Book club", Preview: "Chapter four tonight?", Protocol: "rcs", State: "active", Updated: now.Add(-30 * time.Minute), Participants: []model.Participant{
		{ID: "me", Name: "You", Address: "+15550000000", IsMe: true},
		{ID: "friend", Name: "Alex (synthetic)", Address: "+14155550100"},
		{ID: "teammate", Name: "Sam (synthetic)", Address: "+442071838750"},
		{ID: "archivist", Address: "+15125550111"},
	}})
	contacts, err := json.Marshal(model.ContactBook{Schema: 1, Updated: now, Contacts: []model.Contact{
		{ID: "friend", ContactID: "contact-1", Name: "Alex Rivera (synthetic)", Address: "+14155550100", Formatted: "(415) 555-0100", Frequent: true},
		{ID: "teammate", ContactID: "contact-2", Name: "Sam Okafor (synthetic)", Address: "+442071838750", Formatted: "+44 20 7183 8750", Frequent: true},
		{ID: "archivist", ContactID: "contact-3", Name: "Robin Chase (synthetic)", Address: "+15125550111", Formatted: "(512) 555-0111"},
		{ID: "neighbor", ContactID: "contact-4", Name: "Dana Whitfield (synthetic)", Address: "+13035550142", Formatted: "(303) 555-0142"},
		{ID: "unlisted", ContactID: "contact-5", Formatted: "(206) 555-0163"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveContacts(contacts); err != nil {
		t.Fatal(err)
	}
	appendRecord("history", "conversations:inbox", model.HistoryJob{Schema: 1, ID: "conversations:inbox", Kind: "conversations", Folder: "inbox", State: "queued", Pages: 3, Records: 147, Updated: now})
	// One import per state the thread reports: fetching now, still in line, and
	// stopped. The worker takes queued jobs oldest-updated first.
	appendRecord("history", "messages:synthetic-conversation", model.HistoryJob{Schema: 1, ID: "messages:synthetic-conversation", Kind: "messages", ConversationID: "synthetic-conversation", State: "queued", Pages: 1, Records: 4, Updated: now.Add(-2 * time.Second)})
	appendRecord("history", "messages:synthetic-long", model.HistoryJob{Schema: 1, ID: "messages:synthetic-long", Kind: "messages", ConversationID: "synthetic-long", State: "queued", Pages: 12, Records: 1500, Updated: now.Add(-time.Second)})
	appendRecord("history", "messages:synthetic-sms", model.HistoryJob{Schema: 1, ID: "messages:synthetic-sms", Kind: "messages", ConversationID: "synthetic-sms", State: "failed", Pages: 2, Records: 30, Detail: "Provider repeated a history cursor; import stopped", Updated: now})
	if os.Getenv("BRIDGE_BROWSER_RECOVERY_TEST") == "1" {
		if err := s.SavePairedSession([]byte("synthetic replacement session")); err != nil {
			t.Fatal(err)
		}
		if err := b.Handle(&events.GaiaLoggedOut{}); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	server := &http.Server{Handler: New(b, "synthetic-browser-test-token-only", nil), ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
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
