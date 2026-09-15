package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
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

func TestSecondPairingStartReusesActiveAttempt(t *testing.T) {
	b := testBridge(t)
	first, err := b.BeginPairing()
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.BeginPairing()
	if err != nil {
		t.Fatal(err)
	}
	if first.Ticket != second.Ticket || first.generation != second.generation || second.State != "waiting_for_login" {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
}

func TestExpiredPairingRejectsTicketAndAllowsFreshAttempt(t *testing.T) {
	b := testBridge(t)
	first, err := b.BeginPairing()
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.pairingState.Expires = time.Now().Add(-time.Second)
	b.mu.Unlock()
	if err = b.SubmitPairingCookies(first.Ticket, syntheticCookies()); !errors.Is(err, ErrPairingTicket) {
		t.Fatalf("expired ticket: %v", err)
	}
	if state := b.PairingStatus(); state.State != "failed" || state.Reason != "ticket_expired" || state.Ticket != "" {
		t.Fatalf("expired state: %+v", state)
	}
	second, err := b.BeginPairing()
	if err != nil || second.Ticket == first.Ticket || second.State != "waiting_for_login" {
		t.Fatalf("fresh attempt: %+v %v", second, err)
	}
}

func TestCancelWaitsForPairingAttemptToJoin(t *testing.T) {
	b := testBridge(t)
	state, err := b.BeginPairing()
	if err != nil {
		t.Fatal(err)
	}
	if err = b.SubmitPairingCookies(state.Ticket, syntheticCookies()); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	b.pairAttempt = func(ctx context.Context, _ map[string]string, _ func(string)) error {
		close(started)
		<-ctx.Done()
		<-release
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- b.processPairing(context.Background()) }()
	<-started
	canceled := make(chan error, 1)
	go func() {
		_, err := b.CancelPairingAndWait(context.Background())
		canceled <- err
	}()
	select {
	case err := <-canceled:
		t.Fatalf("cancel returned before join: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err = <-canceled; err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if _, err = b.BeginPairing(); err != nil {
		t.Fatalf("new attempt remained blocked after cancel joined: %v", err)
	}
}

func TestRestartDuringPairingKeepsSavedSessionAndReportsInterruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	key := bytes.Repeat([]byte{7}, 32)
	s, err := store.Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveSession([]byte("previous-session")); err != nil {
		t.Fatal(err)
	}
	b := New(s)
	state, err := b.BeginPairing()
	if err != nil {
		t.Fatal(err)
	}
	if err = b.SubmitPairingCookies(state.Ticket, syntheticCookies()); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restarted := New(s)
	if err = restarted.Prepare(); err != nil {
		t.Fatal(err)
	}
	if saved, readErr := s.Session(); readErr != nil || string(saved) != "previous-session" {
		t.Fatalf("saved session changed: %q %v", saved, readErr)
	}
	got := restarted.PairingStatus()
	if got.State != "failed" || got.Reason != "bridge_restarted" || got.Ticket != "" {
		t.Fatalf("restart state: %+v", got)
	}
	if err = restarted.SubmitPairingCookies(state.Ticket, syntheticCookies()); !errors.Is(err, ErrPairingTicket) {
		t.Fatalf("pre-restart ticket accepted: %v", err)
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

func TestRepairUsesSavedSignInAndInvalidatesWaitingTicket(t *testing.T) {
	b := testBridge(t)
	cookies := syntheticCookies()
	cookies["unrelated"] = "must-not-forward"
	saved, _ := json.Marshal(map[string]any{"cookies": cookies})
	if err := b.Store.SaveSession(saved); err != nil {
		t.Fatal(err)
	}
	if !b.PairingStatus().CanRepair {
		t.Fatal("saved sign-in not offered")
	}
	waiting, err := b.BeginPairing()
	if err != nil {
		t.Fatal(err)
	}
	state, err := b.BeginRepairing()
	if err != nil || state.State != "connecting" || state.Ticket != "" {
		t.Fatalf("%+v %v", state, err)
	}
	if err := b.SubmitPairingCookies(waiting.Ticket, syntheticCookies()); !errors.Is(err, ErrPairingTicket) {
		t.Fatal(err)
	}
	again, err := b.BeginRepairing()
	if err != nil || again.generation != state.generation {
		t.Fatal("repeated repair replaced active attempt")
	}
	b.pairAttempt = func(ctx context.Context, got map[string]string, emoji func(string)) error {
		if len(got) != 6 || got["SID"] != "synthetic" {
			t.Fatal("saved cookies not filtered and forwarded")
		}
		emoji("🍕")
		if b.PairingStatus().State != "confirm_on_phone" {
			t.Fatal("missing phone confirmation")
		}
		return errors.New("synthetic rejected sign-in")
	}
	if err := b.processPairing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := b.PairingStatus(); got.State != "failed" || got.Reason != "provider_failure" || got.Detail != "Google pairing failed: synthetic rejected sign-in" || !got.CanRepair {
		t.Fatalf("%+v", got)
	}
	after, _ := b.Store.Session()
	if !bytes.Equal(saved, after) {
		t.Fatal("failed repair changed stored session")
	}
	browser, err := b.BeginPairing()
	if err != nil || browser.State != "waiting_for_login" || browser.Ticket == "" {
		t.Fatalf("browser fallback: %+v %v", browser, err)
	}
}

func TestRepairWithoutUsableSignInDoesNotCancelQueuedOperations(t *testing.T) {
	for _, data := range []string{"", "invalid", `{"cookies":{"SID":"incomplete"}}`} {
		t.Run(data, func(t *testing.T) {
			b := testBridge(t)
			if data != "" {
				if err := b.Store.SaveSession([]byte(data)); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := b.Store.Enqueue("repair-queued-key-001", "tx", model.SendRequest{ConversationID: "old", Text: "synthetic"}); err != nil {
				t.Fatal(err)
			}
			if b.PairingStatus().CanRepair {
				t.Fatal("offered unusable sign-in")
			}
			if _, err := b.BeginRepairing(); !errors.Is(err, ErrInvalid) {
				t.Fatal(err)
			}
			item, _ := b.Store.Outbox("repair-queued-key-001")
			if item.State != "queued" {
				t.Fatal(item.State)
			}
		})
	}
}

func TestRepairCancellationAndOfflineMode(t *testing.T) {
	b := testBridge(t)
	saved, _ := json.Marshal(map[string]any{"cookies": syntheticCookies()})
	if err := b.Store.SaveSession(saved); err != nil {
		t.Fatal(err)
	}
	b.SetOfflineOnly(true)
	if _, err := b.BeginRepairing(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	b.SetOfflineOnly(false)
	if _, err := b.BeginRepairing(); err != nil {
		t.Fatal(err)
	}
	b.CancelPairing()
	if b.pairCookies != nil || b.PairingActive() {
		t.Fatal("canceled repair retained cookies or remained active")
	}
}

func TestRepairCommitsOnlyAfterPhoneConfirmation(t *testing.T) {
	b := testBridge(t)
	saved, _ := json.Marshal(map[string]any{"cookies": syntheticCookies()})
	if err := b.Store.SaveSession(saved); err != nil {
		t.Fatal(err)
	}
	before, _ := b.Store.SessionSummary()
	if _, _, err := b.Store.Enqueue("repair-success-key-001", "tx", model.SendRequest{ConversationID: "old", Text: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	state, err := b.BeginRepairing()
	if err != nil {
		t.Fatal(err)
	}
	during, _ := b.Store.SessionSummary()
	if during.Epoch != before.Epoch {
		t.Fatal("repair advanced epoch before confirmation")
	}
	item, _ := b.Store.Outbox("repair-success-key-001")
	if item.State != "canceled" {
		t.Fatal("repair left queued operation active")
	}
	b.pairAttempt = func(ctx context.Context, cookies map[string]string, emoji func(string)) error {
		emoji("🍕")
		return b.commitPairedSession(ctx, state.generation, saved)
	}
	if err := b.processPairing(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := b.Store.SessionSummary()
	if after.Epoch != before.Epoch+1 || b.PairingStatus().State != "paired" {
		t.Fatal("confirmed repair did not commit")
	}
}
