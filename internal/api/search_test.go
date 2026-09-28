package api

import (
	"encoding/json"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestSearchNamesAndPhoneNumbers(t *testing.T) {
	for _, tt := range []struct {
		q    string
		want bool
	}{
		{"", true}, {"alice", true}, {"BOOK", true}, {"415555", true}, {"+1 (415)", true}, {"555-0100", true}, {"xyz415", false}, {"---", false},
	} {
		t.Run(tt.q, func(t *testing.T) {
			q, _, ok := searchOptions(httptest.NewRecorder(), httptest.NewRequest("GET", "/?q="+url.QueryEscape(tt.q), nil))
			if !ok || searchMatch(q, []string{"Book club", "Alice"}, []string{"+14155550100"}) != tt.want {
				t.Fatal(tt)
			}
		})
	}
}

func TestSearchLimitValidation(t *testing.T) {
	for _, query := range []string{"limit=0", "limit=-1", "limit=501", "limit=x", "limit=", "limit=1&limit=2"} {
		w := httptest.NewRecorder()
		if _, _, ok := searchOptions(w, httptest.NewRequest("GET", "/?"+query, nil)); ok || w.Code != 400 {
			t.Fatal(query)
		}
	}
	for _, query := range []string{"", "limit=1", "limit=500"} {
		if _, _, ok := searchOptions(httptest.NewRecorder(), httptest.NewRequest("GET", "/?"+query, nil)); !ok {
			t.Fatal(query)
		}
	}
}

func TestConversationSearchFiltersBeforeLimitAndPreservesCursor(t *testing.T) {
	b, server := fixture(t)
	for i, c := range []model.Conversation{
		{Schema: 1, ID: "newest", Name: "Other"},
		{Schema: 1, ID: "match", Name: "Book club", Participants: []model.Participant{{Name: "Alice", Address: "+14155550100"}}},
		{Schema: 1, ID: "older", Name: "Alice"},
	} {
		c.Updated = time.Now().Add(-time.Duration(i) * time.Hour)
		raw, _ := json.Marshal(c)
		if _, err := b.Store.Append(store.Event{Type: "conversation", EntityID: c.ID, Data: raw}); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{"ALICE", "415-555", "book"} {
		req, _ := http.NewRequest("GET", server.URL+"/v1/conversations?q="+url.QueryEscape(q)+"&limit=1", nil)
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Conversations []model.Conversation
			Cursor        uint64
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		cursor, _ := b.Store.Watermark()
		if err != nil || resp.StatusCode != 200 || len(body.Conversations) != 1 || body.Conversations[0].ID != "match" || body.Cursor != cursor {
			t.Fatal(body, err)
		}
	}
}
func TestFinishedConversationOutboxHasID(t *testing.T) {
	b, server := fixture(t)
	id := "create-conversation-01"
	_, _, err := b.Queue(id, model.SendRequest{Kind: "conversation", Recipients: []string{"+14155550100"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Store.ClaimQueued(id); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(model.Conversation{Schema: 1, ID: "canonical-chat"})
	if err = b.Store.FinishConversation(id, store.Event{Type: "conversation", EntityID: "canonical-chat", Data: raw}, nil, 0); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", server.URL+"/v1/outbox/"+id, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out model.Outbox
	if err = json.NewDecoder(resp.Body).Decode(&out); err != nil || out.ConversationID != "canonical-chat" || out.State != "accepted" {
		t.Fatal(out, err)
	}
}

func TestContactSearchAndDefaultCompleteSnapshot(t *testing.T) {
	b, server := fixture(t)
	book := model.ContactBook{Schema: 1, Updated: time.Now(), Contacts: []model.Contact{
		{ID: "a", Name: "Alice", Address: "+14155550100"},
		{ID: "b", Name: "Alice Work", Address: "+14155550200"},
		{ID: "c", Name: "Bob", Address: "+442071838750"},
	}}
	raw, _ := json.Marshal(book)
	if err := b.Store.SaveContacts(raw); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		query string
		count int
		first string
	}{
		{"", 3, "a"}, {"?q=ALICE&limit=1", 1, "a"}, {"?q=2071", 1, "c"}, {"?q=unknown", 0, ""},
	} {
		req, _ := http.NewRequest("GET", server.URL+"/v1/contacts"+tt.query, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var got model.ContactBook
		err = json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if err != nil || len(got.Contacts) != tt.count || (tt.count > 0 && got.Contacts[0].ID != tt.first) {
			t.Fatal(got, err)
		}
	}
}

func TestWhatsAppSnapshotsHideLegacyProtocolArtifacts(t *testing.T) {
	b, server := fixture(t)
	if err := b.SetNetwork("whatsapp"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"empty@lid", "real@lid", "status@broadcast"} {
		c := model.Conversation{Schema: 1, ID: id, Protocol: "whatsapp"}
		raw, _ := json.Marshal(c)
		if _, err := b.Store.Apply(store.Event{Type: "conversation", EntityID: id, Data: raw}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []model.Message{
		{Schema: 1, ID: "real@lid/visible", ConversationID: "real@lid", Text: "hello", Time: time.Unix(100, 0)},
		{Schema: 1, ID: "real@lid/protocol", ConversationID: "real@lid", Time: time.Unix(101, 0)},
	} {
		raw, _ := json.Marshal(m)
		if _, err := b.Store.Apply(store.Event{Type: "message", EntityID: m.ID, Data: raw}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Store.SavePairedSession([]byte("new"), true); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", server.URL+"/v1/conversations", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var conversations struct{ Conversations []model.Conversation }
	err = json.NewDecoder(resp.Body).Decode(&conversations)
	resp.Body.Close()
	if err != nil || len(conversations.Conversations) != 1 || conversations.Conversations[0].ID != "real@lid" || !conversations.Conversations[0].ReadOnly {
		t.Fatal(conversations, err)
	}
	req, _ = http.NewRequest("GET", server.URL+"/v1/conversations/real@lid/messages", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var messages struct{ Messages []model.Message }
	err = json.NewDecoder(resp.Body).Decode(&messages)
	resp.Body.Close()
	if err != nil || len(messages.Messages) != 1 || !messages.Messages[0].ReadOnly || messages.Messages[0].Text != "hello" {
		t.Fatal(messages, err)
	}
}
