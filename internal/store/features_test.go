package store

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
)

func TestUploadAndAttachmentIdempotencySurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	u := model.Upload{Schema: 1, ID: "u", Name: "sample.txt", MIME: "text/plain", Size: 9}
	if err = s.SaveUpload(u, []byte("synthetic")); err != nil {
		t.Fatal(err)
	}
	req := model.SendRequest{ConversationID: "c", AttachmentIDs: []string{"u"}}
	if _, _, err = s.Enqueue("attachment-send-id", "tx", req); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	data, err := s.Media("upload:u")
	if err != nil || string(data) != "synthetic" {
		t.Fatalf("%q %v", data, err)
	}
	if _, created, err := s.Enqueue("attachment-send-id", "other-tx", req); err != nil || created {
		t.Fatalf("idempotency failed: %v %v", created, err)
	}
	req.AttachmentIDs = []string{"other"}
	if _, _, err = s.Enqueue("attachment-send-id", "tx", req); err != ErrConflict {
		t.Fatal(err)
	}
}
func TestHistoryCheckpointPauseRestartAndCursorCycle(t *testing.T) {
	s := openTestStore(t)
	job, err := s.QueueHistory(model.HistoryJob{ID: "messages:c", Kind: "messages", ConversationID: "c"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CheckpointHistory(job.ID, job.Generation, nil, []byte("page-2"), 50, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PauseHistory(job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckpointHistory(job.ID, job.Generation, []byte("page-2"), nil, 50, ""); err != nil {
		t.Fatal(err)
	}
	cursor, err := s.HistoryCursor(job.ID)
	if err != nil || string(cursor) != "page-2" {
		t.Fatalf("pause advanced cursor: %q %v", cursor, err)
	}
	resumed, err := s.QueueHistory(job, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CheckpointHistory(job.ID, job.Generation, []byte("page-2"), nil, 50, ""); err != nil {
		t.Fatal(err)
	}
	cursor, _ = s.HistoryCursor(job.ID)
	if string(cursor) != "page-2" {
		t.Fatal("stale worker advanced resumed job")
	}
	if err = s.CheckpointHistory(job.ID, resumed.Generation, []byte("page-2"), []byte("page-2"), 50, ""); err != nil {
		t.Fatal(err)
	}
	var got model.HistoryJob
	raw, _ := s.Record("history", job.ID)
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "failed" {
		t.Fatalf("cursor loop not detected: %+v", got)
	}
	restarted, err := s.QueueHistory(job, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CheckpointHistory(job.ID, restarted.Generation, nil, []byte("page-2"), 50, ""); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckpointHistory(job.ID, restarted.Generation, []byte("page-2"), nil, 3, ""); err != nil {
		t.Fatal(err)
	}
	raw, _ = s.Record("history", job.ID)
	_ = json.Unmarshal(raw, &got)
	if got.State != "complete" || got.Pages != 2 || got.Records != 53 {
		t.Fatalf("%+v", got)
	}
}
func TestConversationCreationResultDoesNotOverwriteNewerLiveSnapshot(t *testing.T) {
	s := openTestStore(t)
	request := model.SendRequest{Kind: "conversation", Recipients: []string{"+15550123456"}}
	if _, _, err := s.Enqueue("create-key", "tx", request); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimQueued("create-key"); err != nil {
		t.Fatal(err)
	}
	mark, _ := s.HistoryWatermark()
	live := Event{Type: "conversation", EntityID: "c", Data: json.RawMessage(`{"schema":1,"id":"c","name":"live"}`)}
	if _, err := s.Append(live); err != nil {
		t.Fatal(err)
	}
	old := live
	old.Data = json.RawMessage(`{"schema":1,"id":"c","name":"old"}`)
	if err := s.FinishConversation("create-key", old, nil, mark); err != nil {
		t.Fatal(err)
	}
	raw, _ := s.Record("conversation", "c")
	if string(raw) != string(live.Data) {
		t.Fatalf("regressed %s", raw)
	}
	o, err := s.Outbox("create-key")
	if err != nil || o.ConversationID != "c" || o.State != "accepted" {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestPairingChangesRoutingEpochAndCancelsOldWork(t *testing.T) {
	s := openTestStore(t)
	event := Event{Type: "conversation", EntityID: "c", Data: json.RawMessage(`{"schema":1,"id":"c"}`)}
	if _, err := s.Append(event); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Enqueue("queued-key", "tx", model.SendRequest{ConversationID: "c", Text: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPrivate("upload:u", []byte("old-google-upload")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPrivate("attachment", []byte("old-google-download")); err != nil {
		t.Fatal(err)
	}
	message := Event{Type: "message", EntityID: "m", Data: json.RawMessage(`{"schema":1,"id":"m","conversation_id":"c"}`)}
	if _, err := s.Append(message); err != nil {
		t.Fatal(err)
	}
	job, err := s.QueueHistory(model.HistoryJob{ID: "messages:c", Kind: "messages", ConversationID: "c"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CheckpointHistory(job.ID, job.Generation, nil, []byte("older"), 50, ""); err != nil {
		t.Fatal(err)
	}
	if err = s.SavePairedSession([]byte("synthetic-auth"), true); err != nil {
		t.Fatal(err)
	}
	if current, err := s.EntityCurrent("conversation", "c"); err != nil || current {
		t.Fatalf("old routing still eligible: %v %v", current, err)
	}
	o, _ := s.Outbox("queued-key")
	if o.State != "canceled" {
		t.Fatal(o)
	}
	if _, err = s.Private("upload:u"); err != ErrNotFound {
		t.Fatal("old upload credential retained", err)
	}
	if _, err = s.Private("attachment"); err != ErrNotFound {
		t.Fatal("old attachment credential retained", err)
	}
	cursor, err := s.HistoryCursor(job.ID)
	if err != nil || len(cursor) != 0 {
		t.Fatalf("old history cursor retained: %q %v", cursor, err)
	}
	// Even an identical conversation must be observed by the new session.
	if _, err = s.Append(event); err != nil {
		t.Fatal(err)
	}
	if current, err := s.EntityCurrent("conversation", "c"); err != nil || !current {
		t.Fatalf("new routing unavailable: %v %v", current, err)
	}
	summary, err := s.SessionSummary()
	if err != nil || summary.Epoch != 1 || summary.PreviousConversations != 0 || summary.PreviousMessages != 1 {
		t.Fatalf("session boundary summary: %+v %v", summary, err)
	}
	job, err = s.QueueHistory(job, false)
	if err != nil || job.SessionEpoch != summary.Epoch || job.State != "queued" {
		t.Fatalf("history was not rebound to current session: %+v %v", job, err)
	}
}

func TestRepairingSamePhoneKeepsRecordsWritable(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.Append(Event{Type: "conversation", EntityID: "c", Data: json.RawMessage(`{"schema":1,"id":"c"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Enqueue("queued-key", "tx", model.SendRequest{ConversationID: "c", Text: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPrivate("upload:u", []byte("google-upload")); err != nil {
		t.Fatal(err)
	}
	job, err := s.QueueHistory(model.HistoryJob{ID: "messages:c", Kind: "messages", ConversationID: "c"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CheckpointHistory(job.ID, job.Generation, nil, []byte("older"), 50, ""); err != nil {
		t.Fatal(err)
	}
	if err = s.SavePairedSession([]byte("same-phone"), false); err != nil {
		t.Fatal(err)
	}
	if current, err := s.EntityCurrent("conversation", "c"); err != nil || !current {
		t.Fatalf("same-phone re-pair demoted routing: %v %v", current, err)
	}
	if o, _ := s.Outbox("queued-key"); o.State != "canceled" {
		t.Fatal(o)
	}
	if _, err = s.Private("upload:u"); err != nil {
		t.Fatal("upload credential dropped", err)
	}
	if cursor, err := s.HistoryCursor(job.ID); err != nil || string(cursor) != "older" {
		t.Fatalf("history cursor reset: %q %v", cursor, err)
	}
	if summary, err := s.SessionSummary(); err != nil || summary.Epoch != 0 || summary.PreviousConversations != 0 {
		t.Fatalf("session boundary reported for the same phone: %+v %v", summary, err)
	}
}

func TestOldSessionOutboxCannotBeConfirmedByNewSession(t *testing.T) {
	s := openTestStore(t)
	request := model.SendRequest{ConversationID: "c", Text: "synthetic"}
	o, _, err := s.Enqueue("old-session-send", "transaction", request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimQueued(o.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.SavePairedSession([]byte("new-session"), true); err != nil {
		t.Fatal(err)
	}
	message := model.Message{Schema: 1, ID: "m", ConversationID: "c", TransactionID: "transaction"}
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Append(Event{Type: "message", EntityID: message.ID, Data: data}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Outbox(o.ID)
	if err != nil || got.State != "sending" || got.SessionEpoch != 0 {
		t.Fatalf("old-session attempt crossed pairing boundary: %+v %v", got, err)
	}
}
