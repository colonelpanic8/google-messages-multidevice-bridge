package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
)

type fakeProvider struct {
	create           func(context.Context, []string) (provider.Snapshot, error)
	react            func(context.Context, provider.SendTarget, string, string, bool) error
	upload           func(context.Context, []byte, string, string) ([]byte, error)
	messagePage      func(context.Context, string, []byte) ([]provider.Snapshot, []byte, error)
	conversationPage func(context.Context, string, []byte) ([]provider.Snapshot, []byte, error)
	prepare          func(context.Context, string) (provider.SendTarget, error)
	send             func(context.Context, model.Outbox) error
	conversations    func(context.Context) ([]provider.Snapshot, error)
	messages         func(context.Context, string) ([]provider.Snapshot, error)
}

func (f *fakeProvider) Prepare(ctx context.Context, id string) (provider.SendTarget, error) {
	if f.prepare != nil {
		return f.prepare(ctx, id)
	}
	return provider.SendTarget{ConversationID: id}, nil
}
func (f *fakeProvider) Send(ctx context.Context, target provider.SendTarget, o model.Outbox) error {
	return f.send(ctx, o)
}
func (f *fakeProvider) Conversations(ctx context.Context) ([]provider.Snapshot, error) {
	return f.conversations(ctx)
}
func (f *fakeProvider) Messages(ctx context.Context, id string) ([]provider.Snapshot, error) {
	return f.messages(ctx, id)
}
func (f *fakeProvider) MarkRead(context.Context, string, string) error { return nil }
func (f *fakeProvider) Attachment(context.Context, []byte) ([]byte, error) {
	return []byte("synthetic media"), nil
}
func testBridge(t *testing.T) *Bridge {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"), bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s)
}
func snapshot(t *testing.T, kind, id string, value any) provider.Snapshot {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return provider.Snapshot{Event: store.Event{Type: kind, EntityID: id, Data: data}}
}
func TestSendPersistsAttemptBeforeProviderAndNeverAutomaticallyRetries(t *testing.T) {
	for _, test := range []struct {
		name, state string
		err         error
	}{{"accepted", "accepted", nil}, {"timeout", "ambiguous", context.DeadlineExceeded}, {"rejected", "rejected", provider.ErrRejected}} {
		t.Run(test.name, func(t *testing.T) {
			b := testBridge(t)
			conv := snapshot(t, "conversation", "c1", model.Conversation{Schema: 1, ID: "c1"})
			if _, err := b.Store.Append(conv.Event); err != nil {
				t.Fatal(err)
			}
			req := model.SendRequest{ConversationID: "c1", Text: "synthetic"}
			o, created, err := b.Queue("idempotency-key-01", req)
			if err != nil || !created {
				t.Fatalf("%+v %v %v", o, created, err)
			}
			calls := 0
			b.provider = &fakeProvider{send: func(ctx context.Context, o model.Outbox) error {
				calls++
				stored, err := b.Store.Outbox(o.ID)
				if err != nil || stored.State != "sending" {
					t.Fatalf("attempt not durable before send: %+v %v", stored, err)
				}
				return test.err
			}}
			if sent, err := b.sendOne(context.Background()); err != nil || !sent {
				t.Fatalf("%v %v", sent, err)
			}
			stored, err := b.Store.Outbox(o.ID)
			if err != nil || stored.State != test.state {
				t.Fatalf("%+v %v", stored, err)
			}
			if _, created, err = b.Queue(o.ID, req); err != nil || created {
				t.Fatalf("duplicate queue: %v %v", created, err)
			}
			if sent, err := b.sendOne(context.Background()); err != nil || sent || calls != 1 {
				t.Fatalf("unexpected resend %v %v calls=%d", sent, err, calls)
			}
		})
	}
}
func TestReconciliationRepairsMissingMessageWithoutRegressingLiveReceipt(t *testing.T) {
	b := testBridge(t)
	conv := snapshot(t, "conversation", "c1", model.Conversation{Schema: 1, ID: "c1"})
	old := model.Message{Schema: 1, ID: "m1", ConversationID: "c1", Status: "outgoing_complete", Text: "synthetic"}
	b.provider = &fakeProvider{
		conversations: func(context.Context) ([]provider.Snapshot, error) { return []provider.Snapshot{conv}, nil },
		messages: func(context.Context, string) ([]provider.Snapshot, error) {
			live := old
			live.Status = "outgoing_displayed"
			if _, err := b.Store.Append(snapshot(t, "message", "m1", live).Event); err != nil {
				t.Fatal(err)
			}
			missing := model.Message{Schema: 1, ID: "m2", ConversationID: "c1", Text: "missed while disconnected"}
			return []provider.Snapshot{snapshot(t, "message", "m1", old), snapshot(t, "message", "m2", missing)}, nil
		},
	}
	if err := b.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := b.Store.Record("message", "m1")
	if err != nil {
		t.Fatal(err)
	}
	var got model.Message
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "outgoing_displayed" {
		t.Fatal("receipt regressed")
	}
	if _, err = b.Store.Record("message", "m2"); err != nil {
		t.Fatal("gap was not filled", err)
	}
}
func TestTransportRecoveryDoesNotInventPhoneRecovery(t *testing.T) {
	b := testBridge(t)
	b.Handle(&events.ClientReady{})
	b.Handle(&events.PhoneNotResponding{})
	b.Handle(&events.ListenTemporaryError{})
	b.Handle(&events.ListenRecovered{})
	status := b.Status()
	if status.State != "degraded" || !status.Transport || status.Phone {
		t.Fatalf("%+v", status)
	}
	b.Handle(&events.PhoneRespondingAgain{})
	if b.Status().State != "connected" {
		t.Fatal(b.Status())
	}
}
func TestDuplicateRequestSurvivesConversationBecomingReadOnly(t *testing.T) {
	b := testBridge(t)
	conv := model.Conversation{Schema: 1, ID: "c1"}
	if _, err := b.Store.Append(snapshot(t, "conversation", "c1", conv).Event); err != nil {
		t.Fatal(err)
	}
	req := model.SendRequest{ConversationID: "c1", Text: "synthetic"}
	if _, _, err := b.Queue("idempotency-key-01", req); err != nil {
		t.Fatal(err)
	}
	conv.ReadOnly = true
	if _, err := b.Store.Append(snapshot(t, "conversation", "c1", conv).Event); err != nil {
		t.Fatal(err)
	}
	if _, created, err := b.Queue("idempotency-key-01", req); err != nil || created {
		t.Fatalf("lost idempotency: %v %v", created, err)
	}
	if _, _, err := b.Queue("idempotency-key-02", req); !errors.Is(err, ErrInvalid) {
		t.Fatalf("new send to read-only conversation: %v", err)
	}
}

func TestPreparationDoesNotConsumeSendAndCancellationWins(t *testing.T) {
	for _, outcome := range []string{"unavailable", "canceled", "rejected"} {
		t.Run(outcome, func(t *testing.T) {
			b := testBridge(t)
			if _, err := b.Store.Append(snapshot(t, "conversation", "c1", model.Conversation{Schema: 1, ID: "c1"}).Event); err != nil {
				t.Fatal(err)
			}
			o, _, err := b.Queue("preparation-key-01", model.SendRequest{ConversationID: "c1", Text: "synthetic"})
			if err != nil {
				t.Fatal(err)
			}
			b.setProvider(&fakeProvider{prepare: func(ctx context.Context, id string) (provider.SendTarget, error) {
				item, err := b.Store.Outbox(o.ID)
				if err != nil || item.State != "queued" {
					t.Fatalf("preparation consumed send: %+v %v", item, err)
				}
				if outcome == "canceled" {
					if _, err := b.Store.CancelQueued(o.ID); err != nil {
						t.Fatal(err)
					}
					return provider.SendTarget{ConversationID: id}, nil
				}
				if outcome == "rejected" {
					return provider.SendTarget{}, provider.ErrRejected
				}
				return provider.SendTarget{}, provider.ErrUnavailable
			}, send: func(context.Context, model.Outbox) error { t.Fatal("unexpected send"); return nil }})
			if _, err := b.sendOne(context.Background()); err != nil {
				t.Fatal(err)
			}
			item, err := b.Store.Outbox(o.ID)
			expected := outcome
			if outcome == "unavailable" {
				expected = "queued"
			}
			if err != nil || item.State != expected {
				t.Fatalf("%+v %v", item, err)
			}
		})
	}
}

func TestReconciliationChecksUnchangedConversationsAndFencesDuplicateLiveReceipt(t *testing.T) {
	b := testBridge(t)
	conv := snapshot(t, "conversation", "c1", model.Conversation{Schema: 1, ID: "c1"})
	current := model.Message{Schema: 1, ID: "m1", ConversationID: "c1", Status: "outgoing_displayed"}
	b.persist(conv)
	b.persist(snapshot(t, "message", "m1", current))
	calls := 0
	b.setProvider(&fakeProvider{
		conversations: func(context.Context) ([]provider.Snapshot, error) { return []provider.Snapshot{conv}, nil },
		messages: func(context.Context, string) ([]provider.Snapshot, error) {
			calls++
			// Identical live snapshots still establish a newer observation boundary.
			b.persist(snapshot(t, "message", "m1", current))
			old := current
			old.Status = "outgoing_complete"
			return []provider.Snapshot{snapshot(t, "message", "m1", old)}, nil
		},
	})
	for range 2 {
		if err := b.reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := b.Store.Record("message", "m1")
	if err != nil {
		t.Fatal(err)
	}
	var got model.Message
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || got.Status != current.Status {
		t.Fatalf("calls=%d message=%+v", calls, got)
	}
}

func TestProviderAPICallsCancelAndDrainBeforeShutdown(t *testing.T) {
	b := testBridge(t)
	lifetime, cancel := context.WithCancel(context.Background())
	b.providerCtx = lifetime
	b.setProvider(&fakeProvider{})
	_, ctx, release := b.borrowProvider(context.Background())
	cancel()
	b.setProvider(nil)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("provider call not canceled")
	}
	done := make(chan struct{})
	go func() { b.providerOps.Wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("shutdown did not wait for active operation")
	default:
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not drain")
	}
	p, _, release := b.borrowProvider(context.Background())
	defer release()
	if p != nil {
		t.Fatal("provider still admits operations")
	}
}

func TestPrepareOnlyRecoversInterruptedSendsOnce(t *testing.T) {
	b := testBridge(t)
	if err := b.Prepare(); err != nil {
		t.Fatal(err)
	}
	o, _, err := b.Store.Enqueue("recovery-key-0001", "txn", model.SendRequest{ConversationID: "c1", Text: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.ClaimQueued(o.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.Prepare(); err != nil {
		t.Fatal(err)
	}
	got, err := b.Store.Outbox(o.ID)
	if err != nil || got.State != "sending" {
		t.Fatalf("live attempt changed by repeated preparation: %+v %v", got, err)
	}
}

func TestUnavailablePreparationPreservesConversationOrderWithoutBlockingOthers(t *testing.T) {
	b := testBridge(t)
	for i, c := range []string{"c1", "c1", "c2"} {
		_, _, err := b.Store.Enqueue(fmt.Sprintf("outbox-key-%06d", i), fmt.Sprint("txn", i), model.SendRequest{ConversationID: c, Text: "synthetic"})
		if err != nil {
			t.Fatal(err)
		}
	}
	prepared := []string{}
	b.setProvider(&fakeProvider{prepare: func(ctx context.Context, id string) (provider.SendTarget, error) {
		prepared = append(prepared, id)
		if id == "c1" {
			return provider.SendTarget{}, provider.ErrUnavailable
		}
		return provider.SendTarget{ConversationID: id}, nil
	}, send: func(ctx context.Context, o model.Outbox) error {
		if o.Request.ConversationID != "c2" {
			t.Fatal("sent past unavailable predecessor")
		}
		return nil
	}})
	if sent, err := b.sendOne(context.Background()); err != nil || !sent {
		t.Fatalf("%v %v", sent, err)
	}
	if len(prepared) != 2 || prepared[0] != "c1" || prepared[1] != "c2" {
		t.Fatal(prepared)
	}
}

func TestMissingSessionReportsAuthenticationRequired(t *testing.T) {
	b := testBridge(t)
	if err := b.Run(context.Background(), false, nil, nil); err == nil {
		t.Fatal("expected missing session")
	}
	b.Handle(&events.ListenRecovered{})
	s := b.Status()
	if s.State != "authentication_required" || s.Transport || s.Phone {
		t.Fatalf("%+v", s)
	}
}

func TestShutdownDuringAttemptRecordsAmbiguityAndStopsWorker(t *testing.T) {
	b := testBridge(t)
	o, _, err := b.Store.Enqueue("shutdown-send-key1", "transaction", model.SendRequest{ConversationID: "c1", Text: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	b.setProvider(&fakeProvider{send: func(ctx context.Context, o model.Outbox) error { close(started); <-ctx.Done(); return ctx.Err() }})
	b.connection(true, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { b.sendLoop(ctx); close(done) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("send did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("send worker did not join")
	}
	got, err := b.Store.Outbox(o.ID)
	if err != nil || got.State != "ambiguous" {
		t.Fatalf("%+v %v", got, err)
	}
	if err := b.Store.RecoverSending(); err != nil {
		t.Fatal(err)
	}
	if attempted, err := b.sendOne(context.Background()); err != nil || attempted {
		t.Fatalf("ambiguous attempt reclaimed: %v %v", attempted, err)
	}
}

func TestStorageFailureTakesPriorityOverQueuedProviderFailure(t *testing.T) {
	b := testBridge(t)
	b.fail(errors.New("synthetic connection failure"))
	b.storageFailure(errors.New("synthetic disk failure"))
	if err := b.Run(context.Background(), true, nil, nil); !errors.Is(err, ErrStorage) {
		t.Fatalf("storage failure hidden: %v", err)
	}
	if b.Status().State != "storage_failed" {
		t.Fatal(b.Status())
	}
}

func (f *fakeProvider) CreateConversation(ctx context.Context, recipients []string) (provider.Snapshot, error) {
	if f.create != nil {
		return f.create(ctx, recipients)
	}
	return provider.Snapshot{}, provider.ErrUnavailable
}
func (f *fakeProvider) React(ctx context.Context, target provider.SendTarget, id, emoji string, remove bool) error {
	if f.react != nil {
		return f.react(ctx, target, id, emoji, remove)
	}
	return provider.ErrUnavailable
}
func (f *fakeProvider) Typing(context.Context, provider.SendTarget) error { return nil }
func (f *fakeProvider) Upload(ctx context.Context, data []byte, name, mime string) ([]byte, error) {
	if f.upload != nil {
		return f.upload(ctx, data, name, mime)
	}
	return nil, provider.ErrUnavailable
}
func (f *fakeProvider) MessagePage(ctx context.Context, id string, cursor []byte) ([]provider.Snapshot, []byte, error) {
	if f.messagePage != nil {
		return f.messagePage(ctx, id, cursor)
	}
	return nil, nil, provider.ErrUnavailable
}
func (f *fakeProvider) ConversationPage(ctx context.Context, folder string, cursor []byte) ([]provider.Snapshot, []byte, error) {
	if f.conversationPage != nil {
		return f.conversationPage(ctx, folder, cursor)
	}
	return nil, nil, provider.ErrUnavailable
}
