package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

func syntheticCookies() map[string]string {
	return map[string]string{"SID": "synthetic", "HSID": "synthetic", "OSID": "synthetic", "SSID": "synthetic", "APISID": "synthetic", "SAPISID": "synthetic"}
}
func TestPairingTicketsAreSingleUseAndNeverPersisted(t *testing.T) {
	b := testBridge(t)
	_, _, err := b.Store.Enqueue("old-queued-key-001", "tx", model.SendRequest{ConversationID: "old", Text: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	state, err := b.BeginPairing()
	if err != nil || len(state.Ticket) != 43 {
		t.Fatalf("%+v %v", state, err)
	}
	if _, _, err = b.Queue("new-queued-key-001", model.SendRequest{ConversationID: "old", Text: "synthetic"}); !errors.Is(err, ErrPairing) {
		t.Fatal(err)
	}
	if err = b.SubmitPairingCookies("wrong", syntheticCookies()); !errors.Is(err, ErrPairingTicket) {
		t.Fatal(err)
	}
	if err = b.SubmitPairingCookies(state.Ticket, syntheticCookies()); err != nil {
		t.Fatal(err)
	}
	if err = b.SubmitPairingCookies(state.Ticket, syntheticCookies()); !errors.Is(err, ErrPairingTicket) {
		t.Fatal(err)
	}
	if b.PairingStatus().Ticket != "" {
		t.Fatal("ticket retained")
	}
	b.pairAttempt = func(ctx context.Context, cookies map[string]string, emoji func(string)) error {
		emoji("🍕")
		if b.PairingStatus().State != "confirm_on_phone" {
			t.Fatal(b.PairingStatus())
		}
		return b.Store.SavePairedSession([]byte("synthetic-session"))
	}
	if err = b.processPairing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.PairingStatus().State != "paired" {
		t.Fatal(b.PairingStatus())
	}
	o, _ := b.Store.Outbox("old-queued-key-001")
	if o.State != "canceled" {
		t.Fatal(o)
	}
	events, err := b.Store.Events(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "pairing" {
			t.Fatal("pairing credentials entered event log")
		}
	}
}
func TestPairingCancellationJoinsAttempt(t *testing.T) {
	b := testBridge(t)
	state, err := b.BeginPairing()
	if err != nil {
		t.Fatal(err)
	}
	if err = b.SubmitPairingCookies(state.Ticket, syntheticCookies()); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	b.pairAttempt = func(ctx context.Context, cookies map[string]string, emoji func(string)) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	go func() { done <- b.processPairing(context.Background()) }()
	<-started
	b.CancelPairing()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pairing did not join")
	}
	if b.PairingStatus().State != "canceled" || b.PairingStatus().Ticket != "" {
		t.Fatal(b.PairingStatus())
	}
}
func TestOldProviderReplayCannotRegressKnownMessage(t *testing.T) {
	b := testBridge(t)
	current := &gmproto.Message{MessageID: "m", MessageStatus: &gmproto.MessageStatus{Status: gmproto.MessageStatusType_OUTGOING_DISPLAYED}}
	if err := b.Handle(current); err != nil {
		t.Fatal(err)
	}
	old := &gmproto.Message{MessageID: "m", MessageStatus: &gmproto.MessageStatus{Status: gmproto.MessageStatusType_OUTGOING_COMPLETE}}
	if err := b.Handle(&libgm.WrappedMessage{Message: old, IsOld: true}); err != nil {
		t.Fatal(err)
	}
	raw, _ := b.Store.Record("message", "m")
	if !strings.Contains(string(raw), "outgoing_displayed") {
		t.Fatalf("replayed snapshot regressed %s", raw)
	}
}

func TestPairingCompletionCannotReplaceNewTicket(t *testing.T) {
	b := testBridge(t)
	first, err := b.BeginPairing()
	if err != nil {
		t.Fatal(err)
	}
	b.CancelPairing()
	next, err := b.BeginPairing()
	if err != nil {
		t.Fatal(err)
	}
	b.finishPairing("paired", "old attempt completed", first.generation)
	if got := b.PairingStatus(); got.Ticket != next.Ticket || got.State != "waiting_for_login" {
		t.Fatal("old attempt replaced new setup")
	}
	b.CancelPairing()
	b.SetOfflineOnly(true)
	if _, err = b.BeginPairing(); err == nil {
		t.Fatal("offline mode allowed pairing")
	}
}

func TestSupervisorPairsOnlyAfterPreviousConnectionJoins(t *testing.T) {
	b := testBridge(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstStarted, firstJoined, secondStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	b.pairAttempt = func(ctx context.Context, cookies map[string]string, emoji func(string)) error {
		select {
		case <-firstJoined:
		default:
			return errors.New("pairing overlapped old connection")
		}
		emoji("🍕")
		return nil
	}
	done := make(chan error, 1)
	calls := 0
	go func() {
		done <- b.supervise(ctx, false, func(ctx context.Context) error {
			calls++
			if calls == 1 {
				close(firstStarted)
				<-ctx.Done()
				close(firstJoined)
			} else {
				close(secondStarted)
				<-ctx.Done()
			}
			return nil
		})
	}()
	<-firstStarted
	state, err := b.BeginPairing()
	if err != nil {
		t.Fatal(err)
	}
	if err = b.SubmitPairingCookies(state.Ticket, syntheticCookies()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("paired connection did not restart")
	}
	if got := b.PairingStatus(); got.State != "paired" {
		t.Fatal(got)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor did not join")
	}
}

func TestPairingCancelAndCommitHaveOneOutcome(t *testing.T) {
	b := testBridge(t)
	previous := []byte(nil)
	for i := 0; i < 32; i++ {
		state, err := b.BeginPairing()
		if err != nil {
			t.Fatal(err)
		}
		data := []byte(state.Ticket)
		commit := make(chan error, 1)
		canceled := make(chan struct{})
		start := make(chan struct{})
		go func() { <-start; commit <- b.commitPairedSession(context.Background(), state.generation, data) }()
		go func() { <-start; b.CancelPairing(); close(canceled) }()
		close(start)
		err = <-commit
		<-canceled
		session, readErr := b.Store.Session()
		if readErr != nil {
			t.Fatal(readErr)
		}
		got := b.PairingStatus()
		if err == nil {
			if string(session) != string(data) || got.State != "paired" {
				t.Fatal("committed session reported canceled")
			}
			previous = data
		} else if !errors.Is(err, context.Canceled) || string(session) != string(previous) || got.State != "canceled" {
			t.Fatalf("canceled pairing changed session: state=%s err=%v", got.State, err)
		}
	}
}

func TestFailedEventConversionWithholdsAcknowledgement(t *testing.T) {
	b := testBridge(t)
	message := &gmproto.Message{MessageID: "bad-media", MessageInfo: []*gmproto.MessageInfo{{Data: &gmproto.MessageInfo_MediaContent{MediaContent: &gmproto.MediaContent{MediaName: string([]byte{0xff})}}}}}
	if err := b.Handle(message); err == nil {
		t.Fatal("failed conversion was acknowledged")
	}
	if _, err := b.Store.Record("message", "bad-media"); err == nil {
		t.Fatal("failed conversion persisted")
	}
}

func TestLatchedStorageFailureWithholdsLaterAcknowledgements(t *testing.T) {
	b := testBridge(t)
	b.storageFailure(errors.New("synthetic disk failure"))
	if err := b.Handle(&gmproto.Message{MessageID: "later"}); !errors.Is(err, ErrStorage) {
		t.Fatalf("latched storage failure lost: %v", err)
	}
}
