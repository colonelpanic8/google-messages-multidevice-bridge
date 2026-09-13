package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
)

func TestCreateConversationIsDurableAndNeverRepeatedAfterAmbiguousResult(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		b := testBridge(t)
		req := model.SendRequest{Kind: "conversation", Recipients: []string{"+15550123456"}}
		o, _, err := b.Queue("create-conversation-1", req)
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		b.setProvider(&fakeProvider{create: func(context.Context, []string) (provider.Snapshot, error) {
			calls++
			stored, err := b.Store.Outbox(o.ID)
			if err != nil || stored.State != "sending" {
				t.Fatalf("not durable: %+v %v", stored, err)
			}
			if uncertain {
				return provider.Snapshot{}, provider.ErrAmbiguous
			}
			return snapshot(t, "conversation", "new", model.Conversation{Schema: 1, ID: "new"}), nil
		}})
		if sent, err := b.sendOne(context.Background()); err != nil || !sent {
			t.Fatalf("%v %v", sent, err)
		}
		got, _ := b.Store.Outbox(o.ID)
		if uncertain && got.State != "ambiguous" {
			t.Fatal(got)
		}
		if !uncertain && (got.State != "accepted" || got.ConversationID != "new") {
			t.Fatal(got)
		}
		if _, created, err := b.Queue(o.ID, req); err != nil || created {
			t.Fatalf("%v %v", created, err)
		}
		if _, err := b.sendOne(context.Background()); err != nil || calls != 1 {
			t.Fatalf("create repeated %d %v", calls, err)
		}
	}
}
func TestMediaPreparationCanBeCanceledWithoutSending(t *testing.T) {
	b := testBridge(t)
	b.persist(snapshot(t, "conversation", "c", model.Conversation{Schema: 1, ID: "c"}))
	u, err := b.SaveUpload("sample.txt", "text/plain", []byte("synthetic"))
	if err != nil {
		t.Fatal(err)
	}
	o, _, err := b.Queue("media-cancel-key-1", model.SendRequest{ConversationID: "c", AttachmentIDs: []string{u.ID}})
	if err != nil {
		t.Fatal(err)
	}
	b.setProvider(&fakeProvider{upload: func(ctx context.Context, data []byte, name, mime string) ([]byte, error) {
		got, _ := b.Store.Outbox(o.ID)
		if got.State != "queued" {
			t.Fatal(got)
		}
		if string(data) != "synthetic" || name != "sample.txt" || mime != "text/plain" {
			t.Fatal("upload mapping")
		}
		if _, err := b.Store.CancelQueued(o.ID); err != nil {
			t.Fatal(err)
		}
		return []byte("private-upload-receipt"), nil
	}, send: func(context.Context, model.Outbox) error { t.Fatal("canceled media sent"); return nil }})
	if _, err := b.sendOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := b.Store.Outbox(o.ID)
	if got.State != "canceled" {
		t.Fatal(got)
	}
	raw, err := b.Store.Private("upload:" + u.ID)
	if err != nil || string(raw) != "private-upload-receipt" {
		t.Fatalf("receipt not saved: %q %v", raw, err)
	}
}
func TestReactionUsesAttemptBoundaryAndDoesNotRetryTransportFailure(t *testing.T) {
	b := testBridge(t)
	b.persist(snapshot(t, "conversation", "c", model.Conversation{Schema: 1, ID: "c"}))
	b.persist(snapshot(t, "message", "m", model.Message{Schema: 1, ID: "m", ConversationID: "c"}))
	req := model.SendRequest{Kind: "reaction", ConversationID: "c", MessageID: "m", Emoji: "👍"}
	o, _, err := b.Queue("reaction-key-0001", req)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	b.setProvider(&fakeProvider{react: func(ctx context.Context, target provider.SendTarget, id, emoji string, remove bool) error {
		calls++
		got, _ := b.Store.Outbox(o.ID)
		if got.State != "sending" || id != "m" || emoji != "👍" || remove {
			t.Fatalf("bad dispatch %+v", got)
		}
		return context.DeadlineExceeded
	}})
	if _, err := b.sendOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.sendOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := b.Store.Outbox(o.ID)
	if got.State != "ambiguous" || calls != 1 {
		t.Fatalf("%+v calls=%d", got, calls)
	}
}
func TestHistoryImportResumesAndPreservesNewerLiveReceipts(t *testing.T) {
	b := testBridge(t)
	b.persist(snapshot(t, "conversation", "c", model.Conversation{Schema: 1, ID: "c"}))
	job, err := b.QueueHistory("c", "", false)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	b.setProvider(&fakeProvider{messagePage: func(ctx context.Context, id string, cursor []byte) ([]provider.Snapshot, []byte, error) {
		calls++
		msg := model.Message{Schema: 1, ID: "m", ConversationID: "c", Status: "outgoing_complete"}
		if calls == 1 {
			return []provider.Snapshot{snapshot(t, "message", "m", msg)}, []byte("older"), nil
		}
		if string(cursor) != "older" {
			t.Fatalf("lost cursor %q", cursor)
		}
		live := msg
		live.Status = "outgoing_displayed"
		b.persist(snapshot(t, "message", "m", live))
		return []provider.Snapshot{snapshot(t, "message", "m", msg)}, nil, nil
	}})
	if err := b.historyOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A new bridge instance uses only the checkpoint persisted in the same store.
	resumed := New(b.Store)
	resumed.setProvider(b.getProvider())
	if err := resumed.historyOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, _ := b.Store.Record("history", job.ID)
	var got model.HistoryJob
	_ = json.Unmarshal(raw, &got)
	if got.State != "complete" || got.Pages != 2 {
		t.Fatal(got)
	}
	raw, _ = b.Store.Record("message", "m")
	var msg model.Message
	_ = json.Unmarshal(raw, &msg)
	if msg.Status != "outgoing_displayed" {
		t.Fatal(msg)
	}
}
func TestSupervisorRetriesTransientFailuresAndHonorsManualReconnect(t *testing.T) {
	b := testBridge(t)
	b.reconnectDelay = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	started := make(chan struct{}, 2)
	done := make(chan error, 1)
	go func() {
		done <- b.supervise(ctx, false, func(ctx context.Context) error {
			if calls.Add(1) == 1 {
				return errors.New("synthetic connection failure")
			}
			started <- struct{}{}
			<-ctx.Done()
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("did not reconnect")
	}
	b.RequestReconnect()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("manual reconnect did not join/restart")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor did not stop")
	}
	if calls.Load() != 3 {
		t.Fatal(calls.Load())
	}
}

func TestUnsupportedHistoryCursorStopsImport(t *testing.T) {
	b := testBridge(t)
	job, err := b.QueueHistory("", "inbox", false)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	b.setProvider(&fakeProvider{conversationPage: func(context.Context, string, []byte) ([]provider.Snapshot, []byte, error) {
		calls++
		return nil, nil, provider.ErrUnsupportedCursor
	}})
	if err = b.historyOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = b.historyOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := b.Store.Record("history", job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got model.HistoryJob
	if json.Unmarshal(raw, &got) != nil || got.State != "failed" || calls != 1 {
		t.Fatalf("job=%+v calls=%d", got, calls)
	}
}
