package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"sync"
	"time"

	waProvider "github.com/colonelpanic8/google-messages-multidevice-bridge/internal/whatsapp"
	"go.mau.fi/whatsmeow"
	waStore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

type whatsappConnection struct {
	client   *whatsmeow.Client
	provider *waProvider.Provider
	ctx      context.Context
	paired   chan error
}
type whatsappSession struct {
	Namespace string `json:"whatsapp_namespace"`
}

func (b *Bridge) SetNetwork(network string) error {
	if network != "google-messages" && network != "whatsapp" {
		return ErrInvalid
	}
	if err := b.Store.BindNetwork(network); err != nil {
		return err
	}
	b.network = network
	if network == "whatsapp" {
		b.connector = &whatsappConnection{}
	}
	return nil
}
func (b *Bridge) Network() string {
	if b.network == "" {
		return "google-messages"
	}
	return b.network
}
func (b *Bridge) BeginWhatsAppPairing(phone string, newPhone bool) (PairingState, error) {
	if b.Network() != "whatsapp" {
		return PairingState{}, ErrInvalid
	}
	if phone != "" && !phoneNumber.MatchString(phone) {
		return PairingState{}, ErrInvalid
	}
	b.mutationMu.Lock()
	defer b.mutationMu.Unlock()
	b.mu.Lock()
	if b.offlineOnly {
		b.mu.Unlock()
		return PairingState{}, ErrInvalid
	}
	if activePair(b.pairingState.State) {
		state := b.pairingState
		b.mu.Unlock()
		return state, nil
	}
	if b.pairCancel != nil {
		b.mu.Unlock()
		return PairingState{}, ErrPairing
	}
	now := time.Now().UTC()
	state := PairingState{generation: b.pairingState.generation + 1, newPhone: newPhone, State: "connecting", Expires: now.Add(3 * time.Minute), Detail: "Connecting to WhatsApp"}
	if err := b.Store.BeginPairingAttempt(now, state.Expires); err != nil {
		b.mu.Unlock()
		b.storageFailure(err)
		return PairingState{}, err
	}
	b.pairingState = state
	b.pairCookies = map[string]string{"phone": phone}
	b.mu.Unlock()
	b.RequestReconnect()
	wake(b.pairWake)
	return state, nil
}
func (b *Bridge) whatsappCode(generation uint64, kind, code string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pairingState.generation != generation || !activePair(b.pairingState.State) {
		return
	}
	b.pairingState.QR, b.pairingState.PairingCode = "", ""
	if kind == "qr" {
		b.pairingState.State = "scan_qr"
		b.pairingState.QR = code
		b.pairingState.Detail = "Scan with WhatsApp → Linked devices"
	}
	if kind == "code" {
		b.pairingState.State = "enter_pairing_code"
		b.pairingState.PairingCode = code
		b.pairingState.Detail = "Enter this code in WhatsApp → Linked devices → Link with phone number"
	}
}
func (w *whatsappConnection) Run(b *Bridge, ctx context.Context, offline bool, credentials map[string]string, _ func(string)) (result error) {
	if err := b.Prepare(); err != nil {
		return err
	}
	if offline {
		b.setStatus("offline", "Serving stored history without an upstream connection")
		<-ctx.Done()
		return nil
	}
	namespace := ""
	if credentials != nil {
		namespace = waProvider.NewNamespace()
	} else {
		raw, err := b.Store.Session()
		if err != nil {
			return err
		}
		var session whatsappSession
		if len(raw) == 0 {
			b.setStatusReason("authentication_required", "no_session", "Link WhatsApp from Pair / Re-pair")
			return errors.New("no paired session")
		}
		if json.Unmarshal(raw, &session) != nil || session.Namespace == "" {
			b.setStatusReason("authentication_required", "invalid_session", "Stored WhatsApp session is invalid")
			return errors.New("invalid session")
		}
		namespace = session.Namespace
	}
	keys := waProvider.NewCredentials(b.Store, namespace)
	keys.Failure = b.storageFailure
	device, err := keys.Device(ctx)
	if err != nil {
		return err
	}
	if credentials == nil && device.ID == nil {
		b.setStatusReason("authentication_required", "session_expired", "WhatsApp logged out; link this device again")
		return errors.New("session expired")
	}
	providerCtx, cancel := context.WithCancel(ctx)
	log := zerolog.Nop()
	providerCtx = log.WithContext(providerCtx)
	w.ctx = providerCtx
	w.paired = make(chan error, 1)
	// A fresh client is constructed for every connection. Disconnected clients
	// are never reconnected, preventing whatsmeow's reconnect-and-replay path.
	client := whatsmeow.NewClient(device, waLog.Noop)
	client.EnableAutoReconnect = false
	client.InitialAutoReconnect = false
	client.DisableLoginAutoReconnect = true
	client.PreRetryCallback = func(*events.Receipt, types.MessageID, int, *waE2E.Message) bool { return false }
	w.client = client
	w.provider = &waProvider.Provider{Client: client, Keys: keys, Device: device, DB: b.Store}
	b.mu.Lock()
	b.providerCtx = providerCtx
	b.mu.Unlock()
	handler := client.AddEventHandlerWithSuccessStatus(func(e any) bool {
		if err := w.Handle(b, e); err != nil {
			if !errors.Is(err, context.Canceled) {
				b.storageFailure(err)
			}
			return false
		}
		return true
	})
	var workers sync.WaitGroup
	defer func() {
		cancel()
		b.setProvider(nil)
		client.Disconnect()
		b.providerOps.Wait()
		workers.Wait()
		client.RemoveEventHandler(handler)
		b.closeHandler()
		if ctx.Err() != nil && !b.failed() {
			b.setStatus("stopped", "")
		}
	}()
	var qr <-chan whatsmeow.QRChannelItem
	generation := b.PairingStatus().generation
	if credentials != nil {
		qr, err = client.GetQRChannel(providerCtx)
		if err != nil {
			return err
		}
		// Configuration is fixed before this instance's first connection.
		waStore.DeviceProps.RequireFullSync = proto.Bool(true)
		waStore.DeviceProps.HistorySyncConfig.FullSyncDaysLimit = proto.Uint32(3650)
		waStore.DeviceProps.HistorySyncConfig.FullSyncSizeMbLimit = proto.Uint32(10240)
		waStore.DeviceProps.HistorySyncConfig.OnDemandReady = proto.Bool(true)
	}
	b.setStatus("connecting", "")
	if err = client.ConnectContext(providerCtx); err != nil {
		return errors.New("WhatsApp connection failed")
	}
	if credentials != nil {
		phoneRequested := false
		for {
			select {
			case item, ok := <-qr:
				if !ok {
					qr = nil
					continue
				}
				if item.Event == "code" {
					if credentials["phone"] != "" {
						if !phoneRequested {
							phoneRequested = true
							code, e := client.PairPhone(providerCtx, credentials["phone"], true, whatsmeow.PairClientChrome, "Chrome (Linux)")
							if e != nil {
								return errors.New("WhatsApp phone linking failed")
							}
							b.whatsappCode(generation, "code", code)
						}
					} else {
						b.whatsappCode(generation, "qr", item.Code)
					}
				} else if item.Event == "timeout" || item.Event == "error" {
					return errors.New("WhatsApp pairing expired or failed")
				}
			case err := <-w.paired:
				if err != nil {
					return err
				}
				oldRaw, loadErr := b.Store.Session()
				if loadErr != nil {
					return loadErr
				}
				var oldSession whatsappSession
				if len(oldRaw) > 0 && json.Unmarshal(oldRaw, &oldSession) == nil && oldSession.Namespace != "" {
					oldDevice, loadErr := waProvider.NewCredentials(b.Store, oldSession.Namespace).Device(providerCtx)
					if loadErr != nil {
						return loadErr
					}
					if oldDevice.GetJID().ToNonAD() != device.GetJID().ToNonAD() {
						b.mu.Lock()
						b.pairingState.newPhone = true
						b.mu.Unlock()
					}
				}
				raw, _ := json.Marshal(whatsappSession{namespace})
				return b.commitPairedSession(providerCtx, generation, raw)
			case err := <-b.fatal:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	b.setProvider(w.provider)
	workers.Add(4)
	go func() { defer workers.Done(); b.syncLoop(providerCtx) }()
	go func() { defer workers.Done(); b.sendLoop(providerCtx) }()
	go func() { defer workers.Done(); b.historyLoop(providerCtx) }()
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-providerCtx.Done():
				return
			case <-ticker.C:
				if err := w.provider.Drain(providerCtx, b.persist); err != nil && !errors.Is(err, context.Canceled) {
					b.storageFailure(err)
					return
				}
			}
		}
	}()
	select {
	case err := <-b.fatal:
		return err
	case <-ctx.Done():
		return nil
	}
}
func WhatsAppFailure(event any) (state, reason string) {
	switch e := event.(type) {
	case *events.LoggedOut:
		if e.Reason == events.ConnectFailureUnknownLogout {
			return "authentication_required", "account_banned"
		}
		return "authentication_required", "session_expired"
	case *events.StreamReplaced:
		return "connection_failed", "stream_replaced"
	case *events.TemporaryBan:
		return "connection_failed", "temporary_ban"
	case *events.ClientOutdated:
		return "connection_failed", "client_outdated"
	case *events.ConnectFailure:
		if e.Reason.IsLoggedOut() {
			return "authentication_required", "session_expired"
		}
		if e.Reason == events.ConnectFailureTempBanned {
			return "connection_failed", "temporary_ban"
		}
		return "connection_failed", "provider_failure"
	}
	return "", ""
}
func (w *whatsappConnection) Handle(b *Bridge, event any) error {
	b.eventMu.Lock()
	defer b.eventMu.Unlock()
	if b.stopped {
		return context.Canceled
	}
	if state, reason := WhatsAppFailure(event); state != "" {
		b.setStatusReason(state, reason, "WhatsApp connection requires attention")
		b.fail(errors.New("WhatsApp connection stopped"))
		return nil
	}
	switch e := event.(type) {
	case *events.Connected:
		b.connection(true, true)
		b.RequestSync()
	case *events.ManualLoginReconnect, *events.StreamError, *events.KeepAliveTimeout:
		b.fail(errors.New("WhatsApp connection interrupted"))
	case *events.Disconnected:
		b.fail(errors.New("WhatsApp disconnected"))
	case *events.PairSuccess:
		select {
		case w.paired <- nil:
		default:
		}
	case *events.PairError:
		select {
		case w.paired <- errors.New("WhatsApp pairing failed"):
		default:
		}
	case *events.Message:
		return w.provider.Ingest(w.ctx, "message", e)
	case *events.Receipt:
		return w.provider.Ingest(w.ctx, "receipt", e)
	case *events.HistorySync:
		return w.provider.Ingest(w.ctx, "history", e)
	case *events.ChatPresence:
		chat, err := w.provider.Canonical(w.ctx, e.Chat)
		if err != nil {
			return nil
		}
		sender, err := w.provider.Canonical(w.ctx, e.Sender)
		if err != nil {
			return nil
		}
		raw, _ := json.Marshal(model.Typing{Schema: 1, ConversationID: chat.String(), ParticipantID: sender.String(), Active: e.State == types.ChatPresenceComposing})
		b.Hub.Publish(store.Event{Type: "typing", EntityID: chat.String(), Data: raw})
	}
	return nil
}

func (*whatsappConnection) TransactionID() string {
	return (*whatsmeow.Client)(nil).GenerateMessageID()
}
