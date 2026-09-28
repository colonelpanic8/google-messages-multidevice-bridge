package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
)

func recoveryBridge(t *testing.T) *Bridge {
	t.Helper()
	b := testBridge(t)
	if err := b.SetNetwork("whatsapp"); err != nil {
		t.Fatal(err)
	}
	if err := b.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := b.ConfigurePairingRecovery(true, "+15550000001", false); err != nil {
		t.Fatal(err)
	}
	b.setStatusReason("authentication_required", "session_expired", "")
	return b
}

func TestRecoveryOnlyStartsForMissingOrExpiredAuthentication(t *testing.T) {
	for _, status := range []Status{
		{State: "connected"}, {State: "connection_failed", Reason: "transient_connection_failure"},
		{State: "connection_failed", Reason: "stream_replaced"}, {State: "connection_failed", Reason: "temporary_ban"},
		{State: "connection_failed", Reason: "client_outdated"}, {State: "authentication_required", Reason: "account_banned"},
		{State: "authentication_required", Reason: "invalid_session"},
		{State: "authentication_required", Reason: "no_session"}, {State: "authentication_required", Reason: "session_expired"},
	} {
		t.Run(status.State+status.Reason, func(t *testing.T) {
			b := recoveryBridge(t)
			b.whatsappQuarantined = 2
			b.setStatusReason(status.State, status.Reason, "")
			started, _, err := b.recoverPairing(time.Now())
			if err != nil || started != recoveryEligible(status) {
				t.Fatalf("started=%v err=%v", started, err)
			}
		})
	}
	for _, mode := range []string{"disabled", "offline", "google"} {
		t.Run(mode, func(t *testing.T) {
			b := recoveryBridge(t)
			switch mode {
			case "disabled":
				_ = b.ConfigurePairingRecovery(false, "", false)
			case "offline":
				b.SetOfflineOnly(true)
			case "google":
				b.network = "google-messages"
			}
			if started, _, err := b.recoverPairing(time.Now()); err != nil || started {
				t.Fatalf("started=%v err=%v", started, err)
			}
		})
	}
}

func TestRecoveryBudgetSurvivesRestartAndSuccessResetsIt(t *testing.T) {
	b := recoveryBridge(t)
	now := time.Now().UTC()
	for attempt := 1; attempt <= 3; attempt++ {
		if started, _, err := b.recoverPairing(now); err != nil || !started {
			t.Fatalf("attempt %d: %v %v", attempt, started, err)
		}
		state := b.PairingStatus()
		if !state.Automatic || state.Recovery.Attempts != attempt {
			t.Fatalf("%+v", state)
		}
		b.finishPairingReason("failed", "ticket_expired", "", state.generation)
		// A fresh Bridge models process restart; all budget state must come from DB.
		b = New(b.Store)
		if err := b.SetNetwork("whatsapp"); err != nil {
			t.Fatal(err)
		}
		b.setStatusReason("authentication_required", "session_expired", "")
		if err := b.ConfigurePairingRecovery(true, "+15550000001", false); err != nil {
			t.Fatal(err)
		}
		started, delay, err := b.recoverPairing(now.Add(time.Second))
		if err != nil || started || (attempt < 3 && delay <= 0) {
			t.Fatalf("cooldown: %v %v %v", started, delay, err)
		}
		now = now.Add(whatsappPairingLifetime + recoveryCooldown)
	}
	if started, delay, err := b.recoverPairing(now.Add(time.Hour)); started || delay != 0 || err != nil {
		t.Fatalf("exhausted: %v %v %v", started, delay, err)
	}
	// Manual linking is still possible after the automatic budget is exhausted.
	state, err := b.BeginWhatsAppPairing("+15550000001", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.commitPairedSession(context.Background(), state.generation, []byte(`{"whatsapp_namespace":"synthetic"}`)); err != nil {
		t.Fatal(err)
	}
	state = b.PairingStatus()
	if state.State != "paired" || !state.Recovery.Enabled || state.Recovery.Attempts != 0 || !state.Recovery.NextAttempt.IsZero() || state.SessionEpoch != 1 {
		t.Fatalf("%+v", state)
	}
}

func TestRecoveryCancellationPausesAndDisableCancels(t *testing.T) {
	b := recoveryBridge(t)
	_, _, err := b.Store.Enqueue("recovery-queued-0001", "tx", model.SendRequest{ConversationID: "c", Text: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if started, _, err := b.recoverPairing(time.Now()); err != nil || !started {
		t.Fatal(started, err)
	}
	if out, err := b.Store.Outbox("recovery-queued-0001"); err != nil || out.State != "canceled" {
		t.Fatal(out, err)
	}
	b.CancelPairing()
	state := b.PairingStatus()
	if !state.Recovery.Paused || state.PairingCode != "" {
		t.Fatal(state)
	}
	if started, _, err := b.recoverPairing(time.Now().Add(time.Hour)); err != nil || started {
		t.Fatal(started, err)
	}
	if err := b.ConfigurePairingRecovery(true, "+15550000001", true); err != nil {
		t.Fatal(err)
	}
	if started, _, err := b.recoverPairing(time.Now()); err != nil || !started {
		t.Fatal(started, err)
	}
	b.whatsappCode(b.PairingStatus().generation, "code", "ABCD-EFGH")
	if err := b.ConfigurePairingRecovery(false, "", false); err != nil {
		t.Fatal(err)
	}
	state = b.PairingStatus()
	if state.State != "canceled" || state.PairingCode != "" || state.Recovery.Enabled || state.Recovery.PhoneNumber != "" {
		t.Fatal(state)
	}
}

func TestSupervisorAutomaticallyPairsUsingSavedPhone(t *testing.T) {
	b := recoveryBridge(t)
	b.pairAttempt = func(ctx context.Context, credentials map[string]string, _ func(string)) error {
		if credentials["phone"] != "+15550000001" {
			t.Error("wrong phone")
		}
		state := b.PairingStatus()
		b.whatsappCode(state.generation, "code", "ABCD-EFGH")
		return b.commitPairedSession(ctx, state.generation, []byte(`{"whatsapp_namespace":"synthetic"}`))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	calls := 0
	err := b.supervise(ctx, false, func(context.Context) error {
		calls++
		if calls == 1 {
			b.setStatusReason("authentication_required", "session_expired", "")
			return errors.New("expired")
		}
		b.setStatus("connected", "")
		cancel()
		return nil
	})
	if err != nil || calls != 2 || b.PairingStatus().State != "paired" {
		t.Fatalf("calls=%d err=%v state=%s", calls, err, b.PairingStatus().State)
	}
}

func TestRecoveryPushContainsNoPhoneOrPairingCode(t *testing.T) {
	b := recoveryBridge(t)
	if started, _, err := b.recoverPairing(time.Now()); err != nil || !started {
		t.Fatal(started, err)
	}
	state := b.PairingStatus()
	b.whatsappCode(state.generation, "code", "ABCD-EFGH")
	notifier := &recordingNotifier{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.WatchForPush(ctx, notifier) }()
	got := b.settle(notifier, 1)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || strings.Contains(got[0], "ABCD") || strings.Contains(got[0], "+1555") || !strings.Contains(got[0], "whatsapp-pairing") {
		t.Fatal(got)
	}
}

func TestRecoveryCooldownWakesWithoutManualRestart(t *testing.T) {
	b := recoveryBridge(t)
	// Drain the configuration wake so this exercises the timer.
	<-b.reconnectWake
	if err := b.Store.UpdatePairingRecovery(func(state *store.PairingRecovery) { state.NextAttempt = time.Now().Add(10 * time.Millisecond) }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.waitForAuthentication(ctx); err != nil {
		t.Fatal(err)
	}
	if !b.PairingStatus().Automatic {
		t.Fatal("cooldown did not initiate pairing")
	}
}

func TestTerminalPairingFailurePausesRecovery(t *testing.T) {
	b := recoveryBridge(t)
	if started, _, err := b.recoverPairing(time.Now()); err != nil || !started {
		t.Fatal(started, err)
	}
	b.pairAttempt = func(context.Context, map[string]string, func(string)) error {
		b.setStatusReason("connection_failed", "temporary_ban", "")
		return errors.New("synthetic ban")
	}
	if err := b.processPairing(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := b.PairingStatus()
	if !state.Recovery.Paused || state.Recovery.Attempts != 1 {
		t.Fatal(state)
	}
	b.setStatusReason("authentication_required", "session_expired", "")
	if started, _, err := b.recoverPairing(time.Now().Add(time.Hour)); err != nil || started {
		t.Fatal(started, err)
	}
}
