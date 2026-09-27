package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"go.mau.fi/whatsmeow/types/events"
)

func TestWhatsAppPairingRotationCancellationAndOffline(t *testing.T) {
	b := testBridge(t)
	if err := b.SetNetwork("whatsapp"); err != nil {
		t.Fatal(err)
	}
	state, err := b.BeginWhatsAppPairing("", false)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != "connecting" || state.Ticket != "" {
		t.Fatal(state)
	}
	b.whatsappCode(state.generation, "qr", "synthetic-first")
	b.whatsappCode(state.generation, "qr", "synthetic-second")
	got := b.PairingStatus()
	if got.State != "scan_qr" || got.QR != "synthetic-second" || !b.PairingActive() {
		t.Fatal(got)
	}
	if _, _, err = b.Queue("synthetic-operation", model.SendRequest{Kind: "conversation", Recipients: []string{"+14155550100"}}); err != ErrPairing {
		t.Fatal(err)
	}
	b.CancelPairing()
	b.whatsappCode(state.generation, "qr", "obsolete")
	if got = b.PairingStatus(); got.QR != "" || got.State != "canceled" {
		t.Fatal(got)
	}
	next, err := b.BeginWhatsAppPairing("+14155550100", false)
	if err != nil {
		t.Fatal(err)
	}
	b.whatsappCode(state.generation, "code", "obsolete")
	b.whatsappCode(next.generation, "code", "1234-5678")
	if got = b.PairingStatus(); got.State != "enter_pairing_code" || got.PairingCode != "1234-5678" || got.QR != "" {
		t.Fatal(got)
	}
	if err = b.commitPairedSession(context.Background(), next.generation, []byte(`{"whatsapp_namespace":"fake"}`)); err != nil {
		t.Fatal(err)
	}
	if got = b.PairingStatus(); got.State != "paired" || got.PairingCode != "" {
		t.Fatal(got)
	}
	b.SetOfflineOnly(true)
	if _, err = b.BeginWhatsAppPairing("", false); err != ErrInvalid {
		t.Fatal(err)
	}
}
func TestWhatsAppPairingExpiresAndInvalidPhone(t *testing.T) {
	b := testBridge(t)
	if err := b.SetNetwork("whatsapp"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.BeginWhatsAppPairing("4155550100", false); err != ErrInvalid {
		t.Fatal(err)
	}
	state, err := b.BeginWhatsAppPairing("", false)
	if err != nil {
		t.Fatal(err)
	}
	b.whatsappCode(state.generation, "qr", "synthetic")
	b.mu.Lock()
	b.pairingState.Expires = time.Now().Add(-time.Second)
	b.mu.Unlock()
	if got := b.PairingStatus(); got.State != "failed" || got.QR != "" {
		t.Fatal(got)
	}
}
func TestWhatsAppConnectionFailureReasons(t *testing.T) {
	for _, tt := range []struct {
		event         any
		state, reason string
	}{
		{&events.LoggedOut{}, "authentication_required", "session_expired"},
		{&events.LoggedOut{Reason: events.ConnectFailureUnknownLogout}, "authentication_required", "account_banned"},
		{&events.StreamReplaced{}, "connection_failed", "stream_replaced"},
		{&events.TemporaryBan{}, "connection_failed", "temporary_ban"},
		{&events.ConnectFailure{Reason: events.ConnectFailureServiceUnavailable}, "connection_failed", "provider_failure"},
	} {
		state, reason := WhatsAppFailure(tt.event)
		if state != tt.state || reason != tt.reason {
			t.Fatal(state, reason)
		}
	}
}
