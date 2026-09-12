package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func messageEvent(t *testing.T, m model.Message) Event {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return Event{Type: "message", EntityID: m.ID, Data: data}
}
func TestConcurrentIdempotencyAndClaim(t *testing.T) {
	s := openTestStore(t)
	req := model.SendRequest{ConversationID: "c1", Text: "synthetic text"}
	var created, claimed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Go(func() {
			_, new, err := s.Enqueue("idempotency-key-01", "tx1", req)
			if err != nil {
				t.Error(err)
			}
			if new {
				created.Add(1)
			}
		})
	}
	wg.Wait()
	for i := 0; i < 24; i++ {
		wg.Go(func() {
			o, err := s.Claim()
			if err != nil {
				t.Error(err)
			}
			if o != nil {
				claimed.Add(1)
			}
		})
	}
	wg.Wait()
	if created.Load() != 1 || claimed.Load() != 1 {
		t.Fatalf("created=%d claimed=%d", created.Load(), claimed.Load())
	}
	if _, _, err := s.Enqueue("idempotency-key-01", "tx2", model.SendRequest{ConversationID: "c2", Text: req.Text}); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}
func TestCrashRecoveryNeverRequeuesAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	key := bytes.Repeat([]byte{4}, 32)
	s, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Enqueue("idempotency-key-01", "tx1", model.SendRequest{ConversationID: "c1", Text: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.RecoverSending(); err != nil {
		t.Fatal(err)
	}
	o, err := s.Outbox("idempotency-key-01")
	if err != nil || o.State != "ambiguous" {
		t.Fatalf("%+v %v", o, err)
	}
	if next, err := s.Claim(); err != nil || next != nil {
		t.Fatalf("attempt reclaimed: %+v %v", next, err)
	}
	m := model.Message{Schema: 1, ID: "google-message", ConversationID: "c1", TransactionID: "tx1"}
	before, _ := s.Watermark()
	if changed, err := s.Apply(messageEvent(t, m), map[string][]byte{"media": []byte("private-key-material")}, nil); err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	o, err = s.Outbox(o.ID)
	if err != nil || o.State != "confirmed" || o.MessageID != m.ID {
		t.Fatalf("%+v %v", o, err)
	}
	events, err := s.Events(before, 10)
	if err != nil || len(events) != 2 || events[0].Type != "message" || events[1].Type != "outbox" {
		t.Fatalf("atomic observation events: %+v %v", events, err)
	}
	if err = s.Finish(o.ID, "ambiguous", "late error"); err != nil {
		t.Fatal(err)
	}
	o, _ = s.Outbox(o.ID)
	if o.State != "confirmed" {
		t.Fatal("late response regressed observation")
	}
}
func TestStaleHistoryCannotOverwriteLiveReceipt(t *testing.T) {
	s := openTestStore(t)
	m := model.Message{Schema: 1, ID: "m1", ConversationID: "c1", Status: "outgoing_complete", Text: "synthetic"}
	if _, err := s.Append(messageEvent(t, m)); err != nil {
		t.Fatal(err)
	}
	mark, _ := s.HistoryWatermark()
	m.Status = "outgoing_displayed"
	if _, err := s.Append(messageEvent(t, m)); err != nil {
		t.Fatal(err)
	}
	m.Status = "outgoing_complete"
	if added, err := s.AppendIfUnchanged(messageEvent(t, m), mark); added || err != nil {
		t.Fatalf("stale overwrite: %v %v", added, err)
	}
	raw, err := s.Record("message", "m1")
	if err != nil {
		t.Fatal(err)
	}
	var got model.Message
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "outgoing_displayed" {
		t.Fatal(got.Status)
	}
}
func TestCanceledQueueIsNeverClaimed(t *testing.T) {
	s := openTestStore(t)
	_, _, err := s.Enqueue("idempotency-key-01", "tx1", model.SendRequest{ConversationID: "c1", Text: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if o, err := s.CancelQueued("idempotency-key-01"); err != nil || o.State != "canceled" {
		t.Fatalf("%+v %v", o, err)
	}
	if o, err := s.Claim(); err != nil || o != nil {
		t.Fatalf("canceled send claimed: %+v %v", o, err)
	}
}

func TestSnapshotAndObservationRollbackTogether(t *testing.T) {
	s := openTestStore(t)
	before, _ := s.Watermark()
	_, err := s.Apply(Event{Type: "message", EntityID: "bad", Data: json.RawMessage(`42`)}, map[string][]byte{"media": []byte("private")}, nil)
	if err == nil {
		t.Fatal("invalid message unexpectedly committed")
	}
	after, _ := s.Watermark()
	if before != after {
		t.Fatal("event committed before observation failed")
	}
	if _, err = s.Private("media"); !errors.Is(err, ErrNotFound) {
		t.Fatal("private metadata survived rollback")
	}
}

func TestSnapshotCursorBridgesConcurrentUpdates(t *testing.T) {
	s := openTestStore(t)
	m := model.Message{Schema: 1, ID: "m1", ConversationID: "c1", Text: "first"}
	if _, err := s.Append(messageEvent(t, m)); err != nil {
		t.Fatal(err)
	}
	records, mark, err := s.Snapshot("message")
	if err != nil || len(records) != 1 {
		t.Fatalf("%v %v", records, err)
	}
	m.Text = "second"
	if _, err = s.Append(messageEvent(t, m)); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(mark, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("missing update after snapshot: %v %v", events, err)
	}
	var got model.Message
	if err = json.Unmarshal(events[0].Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Text != "second" {
		t.Fatal("wrong replay")
	}
}

func TestAttachmentCiphertextCannotBeMovedBetweenBuckets(t *testing.T) {
	s := openTestStore(t)
	encrypted := s.encrypt([]byte("private attachment credentials"), recordContext("private", "same-id"))
	if _, err := s.decrypt(encrypted, recordContext("media", "same-id")); err == nil {
		t.Fatal("private credentials accepted as media bytes")
	}
}

func TestCancellationWinsDuringSendPreflight(t *testing.T) {
	s := openTestStore(t)
	const id = "idempotency-key-01"
	if _, _, err := s.Enqueue(id, "tx1", model.SendRequest{ConversationID: "c1", Text: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	// A provider preflight is read-only; the user can cancel while it runs.
	if _, err := s.CancelQueued(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimQueued(id); !errors.Is(err, ErrConflict) {
		t.Fatalf("attempt started after cancellation: %v", err)
	}
	o, err := s.Outbox(id)
	if err != nil || o.State != "canceled" {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestIdenticalLiveObservationStillFencesStaleHistory(t *testing.T) {
	s := openTestStore(t)
	m := model.Message{Schema: 1, ID: "m1", ConversationID: "c1", Status: "outgoing_displayed"}
	if _, err := s.Append(messageEvent(t, m)); err != nil {
		t.Fatal(err)
	}
	mark, _ := s.HistoryWatermark()
	cursor, _ := s.Watermark()
	if added, err := s.Append(messageEvent(t, m)); added || err != nil {
		t.Fatalf("dedup failed %v %v", added, err)
	}
	if after, _ := s.Watermark(); after != cursor {
		t.Fatal("duplicate advanced public event cursor")
	}
	m.Status = "outgoing_complete"
	if added, err := s.AppendIfUnchanged(messageEvent(t, m), mark); added || err != nil {
		t.Fatalf("stale response crossed live observation: %v %v", added, err)
	}
}
