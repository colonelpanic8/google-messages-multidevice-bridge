package store

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	bolt "go.etcd.io/bbolt"
)

func conversationEvent(t *testing.T, c model.Conversation) Event {
	t.Helper()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return Event{Type: "conversation", EntityID: c.ID, Data: data}
}

func storedConversation(t *testing.T, s *Store, id string) model.Conversation {
	t.Helper()
	raw, err := s.Record("conversation", id)
	if err != nil {
		t.Fatal(err)
	}
	var c model.Conversation
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConversationPreviewFollowsNewestMessage(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "preview.db"), bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	snapshot := model.Conversation{Schema: model.Schema, ID: "c1", Name: "Group", Preview: "stale provider preview", Updated: old}
	if _, err := s.Append(conversationEvent(t, snapshot)); err != nil {
		t.Fatal(err)
	}
	incoming := model.Message{Schema: model.Schema, ID: "m1", ConversationID: "c1", SenderID: "p1", Time: old.Add(time.Minute), Text: "see you  soon", Direction: "incoming"}
	if changed, err := s.Append(messageEvent(t, incoming)); err != nil || !changed {
		t.Fatalf("apply message: %v %v", changed, err)
	}
	c := storedConversation(t, s, "c1")
	if c.Preview != "see you soon" || !c.Updated.Equal(incoming.Time) || c.PreviewDirection != "incoming" || c.PreviewSenderID != "p1" {
		t.Fatalf("preview not taken from the new message: %+v", c)
	}

	// The provider resends its own older snapshot minutes later; the newer
	// message still describes the conversation.
	if _, err := s.Append(conversationEvent(t, snapshot)); err != nil {
		t.Fatal(err)
	}
	if c = storedConversation(t, s, "c1"); c.Preview != "see you soon" || c.PreviewSenderID != "p1" {
		t.Fatalf("stale snapshot overwrote the preview: %+v", c)
	}

	// A provider snapshot that knows of no later message must not pair its own
	// wording with the stored message's sender.
	agreed := snapshot
	agreed.Preview, agreed.Updated = "Google wording", incoming.Time
	if _, err := s.Append(conversationEvent(t, agreed)); err != nil {
		t.Fatal(err)
	}
	if c = storedConversation(t, s, "c1"); c.Preview != "see you soon" || c.PreviewDirection != "incoming" {
		t.Fatalf("preview and sender disagree: %+v", c)
	}

	outgoing := model.Message{Schema: model.Schema, ID: "m2", ConversationID: "c1", Time: incoming.Time.Add(time.Minute), Direction: "outgoing", Attachments: []model.Attachment{{ID: "a1", MIME: "image/jpeg"}}}
	if _, err := s.Append(messageEvent(t, outgoing)); err != nil {
		t.Fatal(err)
	}
	if c = storedConversation(t, s, "c1"); c.Preview != "Photo" || c.PreviewDirection != "outgoing" {
		t.Fatalf("attachment send not described: %+v", c)
	}

	// A message with nothing to show leaves the preview where it is.
	notice := model.Message{Schema: model.Schema, ID: "m3", ConversationID: "c1", Time: outgoing.Time.Add(time.Minute), Direction: "system", Status: "tombstone_protocol_switch_to_text"}
	if _, err := s.Append(messageEvent(t, notice)); err != nil {
		t.Fatal(err)
	}
	if c = storedConversation(t, s, "c1"); c.Preview != "Photo" || c.PreviewDirection != "outgoing" {
		t.Fatalf("empty notice cleared the preview: %+v", c)
	}

	// An older message from a history import must not move the preview back.
	older := model.Message{Schema: model.Schema, ID: "m0", ConversationID: "c1", SenderID: "p1", Time: old.Add(-time.Hour), Text: "ancient", Direction: "incoming"}
	if _, err := s.Append(messageEvent(t, older)); err != nil {
		t.Fatal(err)
	}
	if c = storedConversation(t, s, "c1"); c.Preview != "Photo" || c.PreviewDirection != "outgoing" {
		t.Fatalf("history import moved the preview: %+v", c)
	}
}

func TestConversationPreviewsRebuiltOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rebuild.db")
	key := bytes.Repeat([]byte{4}, 32)
	s, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	message := model.Message{Schema: model.Schema, ID: "m1", ConversationID: "c1", SenderID: "p1", Time: when, Text: "newest", Direction: "incoming"}
	if _, err := s.Append(messageEvent(t, message)); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(latestMessageBucket)) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(conversationEvent(t, model.Conversation{Schema: model.Schema, ID: "c1", Preview: "stale", Updated: when.Add(-time.Hour)})); err != nil {
		t.Fatal(err)
	}
	if c := storedConversation(t, s, "c1"); c.Preview != "stale" {
		t.Fatalf("preview derived without the index: %+v", c)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if c := storedConversation(t, s, "c1"); c.Preview != "newest" || c.PreviewSenderID != "p1" || !c.Updated.Equal(when) {
		t.Fatalf("preview not rebuilt on open: %+v", c)
	}
}

func TestUnreadFollowsLiveMessages(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "unread.db"), bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	if _, err := s.Append(conversationEvent(t, model.Conversation{Schema: model.Schema, ID: "c1", Preview: "older", Updated: now.Add(-time.Hour)})); err != nil {
		t.Fatal(err)
	}
	arrived := model.Message{Schema: model.Schema, ID: "m1", ConversationID: "c1", SenderID: "p1", Time: now, Text: "are you there?", Direction: "incoming", Status: "incoming_complete"}
	if _, err := s.Append(messageEvent(t, arrived)); err != nil {
		t.Fatal(err)
	}
	if c := storedConversation(t, s, "c1"); !c.Unread {
		t.Fatalf("live message did not raise the badge: %+v", c)
	}

	// An explicit read receipt clears the badge without waiting for Google.
	if changed, err := s.ReadConversation("c1"); err != nil || !changed {
		t.Fatalf("read receipt: %v %v", changed, err)
	}
	if c := storedConversation(t, s, "c1"); c.Unread {
		t.Fatalf("badge survived the read receipt: %+v", c)
	}

	// Reconciliation re-observing the same message must not light it again.
	if _, err := s.Append(messageEvent(t, arrived)); err != nil {
		t.Fatal(err)
	}
	if c := storedConversation(t, s, "c1"); c.Unread {
		t.Fatalf("re-observed message raised the badge: %+v", c)
	}

	// A message the phone already displayed, and a message old enough to come
	// from a history import, leave the badge alone; a send clears it.
	displayed := model.Message{Schema: model.Schema, ID: "m2", ConversationID: "c1", SenderID: "p1", Time: now.Add(time.Minute), Text: "still there?", Direction: "incoming", Status: "incoming_displayed"}
	if _, err := s.Append(messageEvent(t, displayed)); err != nil {
		t.Fatal(err)
	}
	if c := storedConversation(t, s, "c1"); c.Unread || c.Preview != "still there?" {
		t.Fatalf("displayed message: %+v", c)
	}
	if _, err := s.Append(conversationEvent(t, model.Conversation{Schema: model.Schema, ID: "c2", Preview: "provider", Updated: now.Add(-72 * time.Hour)})); err != nil {
		t.Fatal(err)
	}
	imported := model.Message{Schema: model.Schema, ID: "m3", ConversationID: "c2", SenderID: "p2", Time: now.Add(-48 * time.Hour), Text: "history", Direction: "incoming", Status: "incoming_complete"}
	if _, err := s.Append(messageEvent(t, imported)); err != nil {
		t.Fatal(err)
	}
	if c := storedConversation(t, s, "c2"); c.Unread || c.Preview != "history" {
		t.Fatalf("imported message raised the badge: %+v", c)
	}
	if _, err := s.Append(conversationEvent(t, model.Conversation{Schema: model.Schema, ID: "c1", Unread: true, Preview: "still there?", Updated: displayed.Time})); err != nil {
		t.Fatal(err)
	}
	sent := model.Message{Schema: model.Schema, ID: "m4", ConversationID: "c1", Time: now.Add(2 * time.Minute), Text: "on my way", Direction: "outgoing", Status: "outgoing_complete"}
	if _, err := s.Append(messageEvent(t, sent)); err != nil {
		t.Fatal(err)
	}
	if c := storedConversation(t, s, "c1"); c.Unread || c.Preview != "on my way" {
		t.Fatalf("send did not clear the badge: %+v", c)
	}
}
