package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/bridge"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
)

func featureRequest(t *testing.T, server *httptest.Server, method, path, contentType, key string, body io.Reader) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
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

func requireStatus(t *testing.T, got int, body []byte, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("status %d want %d: %s", got, want, body)
	}
}

func appendAPIRecord(t *testing.T, b *bridge.Bridge, kind, id string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Store.Append(store.Event{Type: kind, EntityID: id, Data: data}); err != nil {
		t.Fatal(err)
	}
}

func TestUploadEndpointValidationAndDurability(t *testing.T) {
	b, server := fixture(t)
	payload := []byte("synthetic attachment")
	status, body := featureRequest(t, server, "POST", "/v1/uploads?name=..%2Fsample.txt", "text/plain; charset=utf-8", "", bytes.NewReader(payload))
	requireStatus(t, status, body, http.StatusCreated)
	var upload model.Upload
	if err := json.Unmarshal(body, &upload); err != nil {
		t.Fatal(err)
	}
	if upload.Schema != model.Schema || len(upload.ID) != 64 || upload.Name != "sample.txt" || upload.MIME != "text/plain" || upload.Size != int64(len(payload)) {
		t.Fatalf("unexpected upload: %+v", upload)
	}
	raw, err := b.Store.Record("upload", upload.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stored model.Upload
	if err = json.Unmarshal(raw, &stored); err != nil || stored.ID != upload.ID {
		t.Fatalf("stored upload: %+v %v", stored, err)
	}
	media, err := b.Store.Media("upload:" + upload.ID)
	if err != nil || !bytes.Equal(media, payload) {
		t.Fatalf("stored media %q: %v", media, err)
	}

	status, body = featureRequest(t, server, "POST", "/v1/uploads?name=..%2Fsample.txt", "text/plain; charset=utf-8", "", bytes.NewReader(payload))
	requireStatus(t, status, body, http.StatusCreated)
	var duplicate model.Upload
	if err = json.Unmarshal(body, &duplicate); err != nil || duplicate.ID != upload.ID || !duplicate.Created.Equal(upload.Created) {
		t.Fatalf("content-addressed retry changed record: %+v %v", duplicate, err)
	}

	for _, test := range []struct {
		name, path, contentType string
		body                    io.Reader
		want                    int
	}{
		{name: "missing name", path: "/v1/uploads", contentType: "text/plain", body: bytes.NewReader(payload), want: 400},
		{name: "missing content type", path: "/v1/uploads?name=sample.txt", body: bytes.NewReader(payload), want: 400},
		{name: "empty", path: "/v1/uploads?name=sample.txt", contentType: "text/plain", body: bytes.NewReader(nil), want: 400},
		{name: "invalid name", path: "/v1/uploads?name=bad%0Aname", contentType: "text/plain", body: bytes.NewReader(payload), want: 400},
		{name: "too large", path: "/v1/uploads?name=large.bin", contentType: "application/octet-stream", body: bytes.NewReader(make([]byte, provider.MaxAttachmentBytes+1)), want: 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := featureRequest(t, server, "POST", test.path, test.contentType, "", test.body)
			requireStatus(t, status, body, test.want)
		})
	}
}

func TestConversationAndReactionEndpointsAreDurableAndIdempotent(t *testing.T) {
	b, server := fixture(t)
	now := time.Now().UTC()
	appendAPIRecord(t, b, "conversation", "c1", model.Conversation{Schema: model.Schema, ID: "c1", Updated: now})
	appendAPIRecord(t, b, "conversation", "c2", model.Conversation{Schema: model.Schema, ID: "c2", Updated: now})
	appendAPIRecord(t, b, "message", "m1", model.Message{Schema: model.Schema, ID: "m1", ConversationID: "c1", Time: now})

	conversationKey := "conversation-key-01"
	conversationBody := `{"recipients":["+442071838750","+14155550100"]}`
	status, first := featureRequest(t, server, "POST", "/v1/conversations", "application/json", conversationKey, bytes.NewBufferString(conversationBody))
	requireStatus(t, status, first, http.StatusAccepted)
	var created model.Outbox
	if err := json.Unmarshal(first, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID != conversationKey || created.State != "queued" || created.Request.Kind != "conversation" || len(created.Request.Recipients) != 2 || created.Request.Recipients[0] != "+14155550100" {
		t.Fatalf("conversation outbox: %+v", created)
	}
	status, second := featureRequest(t, server, "POST", "/v1/conversations", "application/json", conversationKey, bytes.NewBufferString(`{"recipients":["+14155550100","+442071838750"]}`))
	requireStatus(t, status, second, http.StatusOK)
	if !bytes.Equal(first, second) {
		t.Fatalf("idempotent conversation response changed:\n%s\n%s", first, second)
	}
	stored, err := b.Store.Outbox(conversationKey)
	if err != nil || !stored.Request.Equal(created.Request) {
		t.Fatalf("durable conversation: %+v %v", stored, err)
	}

	for _, test := range []struct {
		name, key, body string
		want            int
	}{
		{name: "same key changed request", key: conversationKey, body: `{"recipients":["+14155550101"]}`, want: 409},
		{name: "missing key", body: `{"recipients":["+14155550101"]}`, want: 400},
		{name: "invalid number", key: "conversation-key-02", body: `{"recipients":["4155550100"]}`, want: 400},
		{name: "duplicate recipient", key: "conversation-key-03", body: `{"recipients":["+14155550100","+14155550100"]}`, want: 400},
		{name: "unknown field", key: "conversation-key-04", body: `{"recipients":["+14155550100"],"unknown":true}`, want: 400},
	} {
		t.Run("conversation "+test.name, func(t *testing.T) {
			status, body := featureRequest(t, server, "POST", "/v1/conversations", "application/json", test.key, bytes.NewBufferString(test.body))
			requireStatus(t, status, body, test.want)
		})
	}

	reactionKey := "reaction-key-0001"
	reactionBody := `{"message_id":"m1","emoji":"👍","remove":false}`
	status, first = featureRequest(t, server, "POST", "/v1/conversations/c1/reactions", "application/json", reactionKey, bytes.NewBufferString(reactionBody))
	requireStatus(t, status, first, http.StatusAccepted)
	var reaction model.Outbox
	if err = json.Unmarshal(first, &reaction); err != nil {
		t.Fatal(err)
	}
	if reaction.Request.Kind != "reaction" || reaction.Request.ConversationID != "c1" || reaction.Request.MessageID != "m1" || reaction.Request.Emoji != "👍" || reaction.Request.Remove {
		t.Fatalf("reaction outbox: %+v", reaction)
	}
	status, second = featureRequest(t, server, "POST", "/v1/conversations/c1/reactions", "application/json", reactionKey, bytes.NewBufferString(reactionBody))
	requireStatus(t, status, second, http.StatusOK)
	if !bytes.Equal(first, second) {
		t.Fatalf("idempotent reaction response changed:\n%s\n%s", first, second)
	}
	stored, err = b.Store.Outbox(reactionKey)
	if err != nil || !stored.Request.Equal(reaction.Request) {
		t.Fatalf("durable reaction: %+v %v", stored, err)
	}

	for _, test := range []struct {
		name, path, key, body string
		want                  int
	}{
		{name: "same key changed request", path: "/v1/conversations/c1/reactions", key: reactionKey, body: `{"message_id":"m1","emoji":"❤️","remove":false}`, want: 409},
		{name: "wrong conversation", path: "/v1/conversations/c2/reactions", key: "reaction-key-0002", body: reactionBody, want: 400},
		{name: "missing message", path: "/v1/conversations/c1/reactions", key: "reaction-key-0003", body: `{"message_id":"missing","emoji":"👍","remove":false}`, want: 404},
		{name: "empty emoji", path: "/v1/conversations/c1/reactions", key: "reaction-key-0004", body: `{"message_id":"m1","emoji":"","remove":false}`, want: 400},
		{name: "unknown field", path: "/v1/conversations/c1/reactions", key: "reaction-key-0005", body: `{"message_id":"m1","emoji":"👍","remove":false,"unknown":true}`, want: 400},
	} {
		t.Run("reaction "+test.name, func(t *testing.T) {
			status, body := featureRequest(t, server, "POST", test.path, "application/json", test.key, bytes.NewBufferString(test.body))
			requireStatus(t, status, body, test.want)
		})
	}
}

func TestHistoryEndpointsValidateAndExposeDurableJobs(t *testing.T) {
	b, server := fixture(t)
	appendAPIRecord(t, b, "conversation", "c1", model.Conversation{Schema: model.Schema, ID: "c1", Updated: time.Now().UTC()})

	status, first := featureRequest(t, server, "POST", "/v1/history", "application/json", "", bytes.NewBufferString(`{"folder":"inbox"}`))
	requireStatus(t, status, first, http.StatusAccepted)
	var job model.HistoryJob
	if err := json.Unmarshal(first, &job); err != nil {
		t.Fatal(err)
	}
	if job.ID != "conversations:inbox" || job.Kind != "conversations" || job.State != "queued" || job.Generation == 0 {
		t.Fatalf("history job: %+v", job)
	}
	status, second := featureRequest(t, server, "POST", "/v1/history", "application/json", "", bytes.NewBufferString(`{"folder":"inbox"}`))
	requireStatus(t, status, second, http.StatusAccepted)
	var repeated model.HistoryJob
	if err := json.Unmarshal(second, &repeated); err != nil {
		t.Fatal(err)
	}
	if repeated.ID != job.ID || repeated.Generation != job.Generation || repeated.State != job.State || repeated.Pages != job.Pages || repeated.Records != job.Records || !repeated.Updated.Equal(job.Updated) {
		t.Fatalf("queued history resume changed work: first=%+v repeated=%+v", job, repeated)
	}
	if err := b.Store.CheckpointHistory(job.ID, job.Generation, nil, []byte("older"), 7, ""); err != nil {
		t.Fatal(err)
	}

	status, body := featureRequest(t, server, "GET", "/v1/history", "", "", nil)
	requireStatus(t, status, body, http.StatusOK)
	var listing struct {
		Jobs   []model.HistoryJob `json:"jobs"`
		Cursor uint64             `json:"cursor"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Jobs) != 1 || listing.Jobs[0].Pages != 1 || listing.Jobs[0].Records != 7 || listing.Cursor == 0 {
		t.Fatalf("history listing: %+v", listing)
	}

	status, body = featureRequest(t, server, "POST", "/v1/history", "application/json", "", bytes.NewBufferString(`{"folder":"inbox","restart":true}`))
	requireStatus(t, status, body, http.StatusAccepted)
	var restarted model.HistoryJob
	if err := json.Unmarshal(body, &restarted); err != nil {
		t.Fatal(err)
	}
	if restarted.Generation <= job.Generation || restarted.Pages != 0 || restarted.Records != 0 || restarted.State != "queued" {
		t.Fatalf("restarted history: %+v", restarted)
	}
	status, body = featureRequest(t, server, "POST", "/v1/history/conversations:inbox/pause", "", "", nil)
	requireStatus(t, status, body, http.StatusOK)
	var paused model.HistoryJob
	if err := json.Unmarshal(body, &paused); err != nil || paused.State != "paused" {
		t.Fatalf("paused history: %+v %v", paused, err)
	}
	status, body = featureRequest(t, server, "POST", "/v1/history", "application/json", "", bytes.NewBufferString(`{"folder":"inbox"}`))
	requireStatus(t, status, body, http.StatusAccepted)
	var resumed model.HistoryJob
	if err := json.Unmarshal(body, &resumed); err != nil || resumed.State != "queued" || resumed.Generation <= paused.Generation {
		t.Fatalf("resumed history: %+v %v", resumed, err)
	}

	status, body = featureRequest(t, server, "POST", "/v1/history", "application/json", "", bytes.NewBufferString(`{"conversation_id":"c1"}`))
	requireStatus(t, status, body, http.StatusAccepted)
	var conversationJob model.HistoryJob
	if err := json.Unmarshal(body, &conversationJob); err != nil || conversationJob.ID != "messages:c1" || conversationJob.Kind != "messages" {
		t.Fatalf("conversation history: %+v %v", conversationJob, err)
	}

	for _, test := range []struct {
		name, body string
		want       int
	}{
		{name: "invalid folder", body: `{"folder":"trash"}`, want: 400},
		{name: "both targets", body: `{"folder":"inbox","conversation_id":"c1"}`, want: 400},
		{name: "missing conversation", body: `{"conversation_id":"missing"}`, want: 404},
		{name: "unknown field", body: `{"folder":"inbox","unknown":true}`, want: 400},
		{name: "multiple values", body: `{"folder":"inbox"}{"folder":"spam"}`, want: 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := featureRequest(t, server, "POST", "/v1/history", "application/json", "", bytes.NewBufferString(test.body))
			requireStatus(t, status, body, test.want)
		})
	}
}

func TestAddParticipantsAddressesEveryoneAlreadyInTheConversation(t *testing.T) {
	b, server := fixture(t)
	appendAPIRecord(t, b, "conversation", "c1", model.Conversation{
		Schema: model.Schema,
		ID:     "c1",
		Name:   "Book club",
		Participants: []model.Participant{
			{ID: "me", Address: "+15550000000", IsMe: true},
			{ID: "p1", Address: "+14155550100"},
			{ID: "p2", Address: "+442071838750"},
		},
		Updated: time.Now().UTC(),
	})
	status, body := featureRequest(t, server, "POST", "/v1/conversations/c1/participants", "application/json", "add-key-00000001", bytes.NewBufferString(`{"recipients":["+15125550111","+14155550100"],"name":"Book club"}`))
	requireStatus(t, status, body, http.StatusAccepted)
	var out model.Outbox
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	// Everyone already in the conversation rides along, the owner does not, and
	// naming someone twice adds them once.
	want := []string{"+14155550100", "+15125550111", "+442071838750"}
	if out.Request.Kind != "conversation" || out.Request.GroupName != "Book club" || !slices.Equal(out.Request.Recipients, want) {
		t.Fatalf("participants outbox: %+v", out.Request)
	}

	status, body = featureRequest(t, server, "POST", "/v1/conversations/unknown/participants", "application/json", "add-key-00000002", bytes.NewBufferString(`{"recipients":["+15125550111"]}`))
	requireStatus(t, status, body, http.StatusNotFound)

	// A participant the phone reported without a number cannot be addressed, so
	// the request is refused rather than silently dropping them from the group.
	appendAPIRecord(t, b, "conversation", "c2", model.Conversation{
		Schema:       model.Schema,
		ID:           "c2",
		Participants: []model.Participant{{ID: "me", IsMe: true}, {ID: "p3", Name: "No number"}},
		Updated:      time.Now().UTC(),
	})
	status, body = featureRequest(t, server, "POST", "/v1/conversations/c2/participants", "application/json", "add-key-00000003", bytes.NewBufferString(`{"recipients":["+15125550111"]}`))
	requireStatus(t, status, body, http.StatusBadRequest)
}

func TestGroupNameOnlyRidesAlongWithAGroup(t *testing.T) {
	_, server := fixture(t)
	for _, test := range []struct {
		name, key, body string
		want            int
	}{
		{name: "group", key: "group-name-0000001", body: `{"recipients":["+14155550100","+442071838750"],"name":"Book club"}`, want: http.StatusAccepted},
		{name: "two party", key: "group-name-0000002", body: `{"recipients":["+14155550100"],"name":"Book club"}`, want: http.StatusBadRequest},
		{name: "unnamed two party", key: "group-name-0000003", body: `{"recipients":["+14155550100"],"name":"  "}`, want: http.StatusAccepted},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := featureRequest(t, server, "POST", "/v1/conversations", "application/json", test.key, bytes.NewBufferString(test.body))
			requireStatus(t, status, body, test.want)
		})
	}
}
