package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/bridge"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
)

func TestOutboxHTTPIdempotencyAndCancellation(t *testing.T) {
	b, server := fixture(t)
	data, _ := json.Marshal(model.Conversation{Schema: 1, ID: "c1", Name: "Synthetic", Updated: time.Now()})
	if _, err := b.Store.Append(store.Event{Type: "conversation", EntityID: "c1", Data: data}); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, key, body string, want int) []byte {
		t.Helper()
		req, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s: status %d want %d: %s", path, resp.StatusCode, want, data)
		}
		return data
	}
	body := `{"conversation_id":"c1","text":"synthetic message"}`
	first := request("POST", "/v1/messages", "idempotency-key-01", body, 202)
	second := request("POST", "/v1/messages", "idempotency-key-01", body, 200)
	if !bytes.Equal(first, second) {
		t.Fatal("same key did not return same outbox record")
	}
	request("POST", "/v1/messages", "idempotency-key-01", `{"conversation_id":"c1","text":"changed"}`, 409)
	request("POST", "/v1/messages", "", body, 400)
	request("POST", "/v1/messages", "idempotency-key-02", body+body, 400)
	request("POST", "/v1/outbox/idempotency-key-01/cancel", "", "", 200)
	if claimed, err := b.Store.Claim(); err != nil || claimed != nil {
		t.Fatalf("canceled item claimed: %+v %v", claimed, err)
	}
	request("GET", "/v1/outbox/idempotency-key-01", "", "", 200)
	request("GET", "/v1/events?after=999999", "", "", 400)
}

func TestSessionBoundaryIsVisibleInStatusPairingAndStoredRecords(t *testing.T) {
	b, server := fixture(t)
	conversation, _ := json.Marshal(model.Conversation{Schema: 1, ID: "old", Name: "Previous account", Updated: time.Now()})
	message, _ := json.Marshal(model.Message{Schema: 1, ID: "old-message", ConversationID: "old", Time: time.Now(), Text: "synthetic"})
	if _, err := b.Store.Append(store.Event{Type: "conversation", EntityID: "old", Data: conversation}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.Append(store.Event{Type: "message", EntityID: "old-message", Data: message}); err != nil {
		t.Fatal(err)
	}
	if err := b.Store.SavePairedSession([]byte("new-session"), true); err != nil {
		t.Fatal(err)
	}

	get := func(path string, out any) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %s", path, resp.Status)
		}
		if err = json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	var status struct {
		SessionEpoch          uint64 `json:"session_epoch"`
		PreviousConversations int    `json:"previous_session_conversations"`
		PreviousMessages      int    `json:"previous_session_messages"`
	}
	get("/v1/status", &status)
	if status.SessionEpoch != 1 || status.PreviousConversations != 1 || status.PreviousMessages != 1 {
		t.Fatalf("status boundary: %+v", status)
	}
	var pairing bridge.PairingState
	get("/v1/pairing", &pairing)
	if pairing.SessionEpoch != 1 || pairing.PreviousConversations != 1 || pairing.PreviousMessages != 1 {
		t.Fatalf("pairing boundary: %+v", pairing)
	}
	var conversations struct {
		Conversations []model.Conversation `json:"conversations"`
	}
	get("/v1/conversations", &conversations)
	if len(conversations.Conversations) != 1 || !conversations.Conversations[0].ReadOnly {
		t.Fatalf("conversation boundary: %+v", conversations)
	}
	var messages struct {
		Messages []model.Message `json:"messages"`
	}
	get("/v1/conversations/old/messages", &messages)
	if len(messages.Messages) != 1 || !messages.Messages[0].ReadOnly {
		t.Fatalf("message boundary: %+v", messages)
	}
}
func TestLegacyReplayDoesNotExposeMediaKeys(t *testing.T) {
	b, server := fixture(t)
	raw := `{"messageID":"old-message","conversationID":"c1","messageInfo":[{"mediaContent":{"mediaID":"google-media-secret","decryptionKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}}]}`
	if _, err := b.Store.Append(store.Event{Type: "message", EntityID: "old-message", Data: json.RawMessage(raw)}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", server.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	for _, secret := range []string{"decryptionKey", "google-media-secret", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="} {
		if bytes.Contains(body, []byte(secret)) {
			t.Fatalf("private metadata leaked: %s", body)
		}
	}
	if !bytes.Contains(body, []byte(`"schema":1`)) {
		t.Fatalf("missing public schema: %s", body)
	}
}
func TestMessagePaginationAndConsistentSnapshotCursor(t *testing.T) {
	b, server := fixture(t)
	data, _ := json.Marshal(model.Conversation{Schema: 1, ID: "c1"})
	_, _ = b.Store.Append(store.Event{Type: "conversation", EntityID: "c1", Data: data})
	for _, id := range []string{"a", "b", "c"} {
		data, _ := json.Marshal(model.Message{Schema: 1, ID: id, ConversationID: "c1", Time: time.Unix(1, 0)})
		_, _ = b.Store.Append(store.Event{Type: "message", EntityID: id, Data: data})
	}
	watermark, err := b.Store.Watermark()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		query string
		want  string
	}{{"?limit=2", "c,b"}, {"?limit=2&before=b", "a"}} {
		req, _ := http.NewRequest("GET", server.URL+"/v1/conversations/c1/messages"+test.query, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Messages []model.Message `json:"messages"`
			Cursor   uint64          `json:"cursor"`
		}
		err = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, m := range result.Messages {
			ids = append(ids, m.ID)
		}
		if strings.Join(ids, ",") != test.want || result.Cursor != watermark {
			t.Fatalf("%+v", result)
		}
	}
}

func TestPublicClientAssetsAndCrossOriginMutation(t *testing.T) {
	_, server := fixture(t)
	for _, path := range []string{"/", "/app.js", "/style.css", "/stream.mjs"} {
		resp, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Security-Policy") == "" {
			t.Fatalf("asset %s status=%d", path, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest("POST", server.URL+"/v1/sync", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Origin", "https://unrelated.invalid")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal(resp.StatusCode)
	}
}
