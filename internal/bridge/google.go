package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/util/exhttp"
	"google.golang.org/protobuf/proto"
)

type googleConnection struct{ client *libgm.Client }

func (g *googleConnection) ingest(b *Bridge, msg proto.Message) error {
	snapshot, err := provider.SnapshotOf(msg)
	if err != nil {
		b.fail(err)
		return err
	}
	return b.persist(snapshot)
}
func (g *googleConnection) authSnapshot() ([]byte, error) {
	g.client.AuthData.CookiesLock.RLock()
	defer g.client.AuthData.CookiesLock.RUnlock()
	data, err := json.Marshal(g.client.AuthData)
	if err != nil {
		return nil, errors.New("serialize session")
	}
	return data, nil
}
func (g *googleConnection) snapshotSession(b *Bridge) error {
	data, err := g.authSnapshot()
	if err != nil {
		b.fail(err)
		return err
	}
	if err = b.Store.SaveSession(data); err != nil {
		b.storageFailure(err)
		return fmt.Errorf("%w: %v", ErrStorage, err)
	}
	return nil
}

// Handle runs on the provider's polling goroutine. It is gated so nothing
// reaches the store after Run returns.
func (g *googleConnection) Handle(b *Bridge, event any) error {
	b.eventMu.Lock()
	defer b.eventMu.Unlock()
	if b.stopped {
		return context.Canceled
	}
	switch e := event.(type) {
	case *libgm.WrappedMessage:
		if e.IsOld {
			snap, err := provider.SnapshotOf(e.Message)
			if err != nil {
				return err
			}
			if err = b.apply(snap, 0); err != nil {
				return err
			}
		} else {
			if err := g.ingest(b, e.Message); err != nil {
				return err
			}
		}
	case *gmproto.Message:
		if err := g.ingest(b, e); err != nil {
			return err
		}
	case *gmproto.Conversation:
		if err := g.ingest(b, e); err != nil {
			return err
		}
	case *gmproto.TypingData:
		out, err := provider.SnapshotOf(e)
		if err != nil {
			b.fail(err)
			return err
		}
		b.Hub.Publish(out.Event)
	case *events.ClientReady:
		b.connection(true, true)
		for _, conv := range e.Conversations {
			if err := g.ingest(b, conv); err != nil {
				return err
			}
		}
		b.RequestSync()
	case *events.AuthTokenRefreshed:
		if !b.pairing {
			if err := g.snapshotSession(b); err != nil {
				return err
			}
		}
	case *events.ListenTemporaryError:
		b.connection(false, b.Status().Phone)
	case *events.PingFailed, *events.PhoneNotResponding, *events.NoDataReceived:
		b.connection(b.Status().Transport, false)
	case *events.ListenRecovered:
		b.connection(true, b.Status().Phone)
		b.RequestSync()
	case *events.PhoneRespondingAgain:
		b.connection(b.Status().Transport, true)
		b.RequestSync()
	case *events.GaiaLoggedOut:
		b.setStatusReason("authentication_required", "session_expired", "Your Google Messages phone session expired or was revoked; re-pair to reconnect")
		b.fail(errors.New("google session expired; pair again"))
	case *events.ListenFatalError:
		if libgm.IsAuthFailure(e.Error) {
			b.setStatusReason("authentication_required", "session_expired", "Your Google Messages phone session expired or was revoked; re-pair to reconnect")
		} else {
			b.setStatusReason("connection_failed", "provider_failure", "Google connection failed; retry the connection")
		}
		b.fail(fmt.Errorf("google connection failed: %w", e.Error))
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.storageErr
}

// Run owns the provider connection until ctx ends or the provider fails. It
// returns only after every goroutine it started has exited and the event
// handler is closed. Stored history stays readable afterwards unless the
// error wraps ErrStorage. The patched client joins its connection workers.
func (g *googleConnection) Run(b *Bridge, ctx context.Context, offline bool, cookies map[string]string, emoji func(string)) (result error) {
	defer func() {
		b.closeHandler()
		b.mu.Lock()
		if b.storageErr != nil {
			result = b.storageErr
			b.status.State = "storage_failed"
			b.status.Transport, b.status.Phone = false, false
		}
		b.mu.Unlock()
	}()
	if err := b.Prepare(); err != nil {
		return fmt.Errorf("%w: %v", ErrStorage, err)
	}
	if offline {
		b.setStatus("offline", "Serving stored history without a Google connection")
		select {
		case <-ctx.Done():
			b.setStatus("stopped", "")
			return nil
		case err := <-b.fatal:
			return err
		}
	}
	auth := libgm.NewAuthData()
	if cookies != nil {
		auth.SetCookies(cookies)
	} else {
		data, err := b.Store.Session()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrStorage, err)
		}
		if len(data) == 0 {
			b.setStatusReason("authentication_required", "no_session", "No paired session; open Pair / Re-pair in the web client")
			return errors.New("no paired session")
		}
		if err := json.Unmarshal(data, auth); err != nil {
			b.setStatusReason("authentication_required", "invalid_session", "The stored Google Messages session is invalid; re-pair to replace it")
			return errors.New("invalid stored session")
		}
	}
	providerCtx, cancel := context.WithCancel(ctx)
	// Upstream diagnostics can contain message bodies and authentication data.
	providerLog := zerolog.Nop()
	providerCtx = providerLog.WithContext(providerCtx)
	b.pairing = cookies != nil
	pairGeneration := uint64(0)
	if state := b.PairingStatus(); cookies != nil && activePair(state.State) {
		pairGeneration = state.generation
	}
	g.client = libgm.NewClient(auth, nil, providerLog, exhttp.SensibleClientSettings)
	g.client.SetEventHandlerWithError(func(e any) error { return g.Handle(b, e) })
	b.mu.Lock()
	b.providerCtx = providerCtx
	b.mu.Unlock()
	var owned sync.WaitGroup
	defer func() {
		cancel()
		b.setProvider(nil)
		b.providerOps.Wait()
		owned.Wait()
		b.closeHandler()
		g.client.Disconnect()
		if ctx.Err() != nil && !b.failed() {
			b.setStatus("stopped", "")
		}
	}()
	b.setStatus("connecting", "")
	started := make(chan error, 1)
	owned.Add(1)
	go func() {
		defer owned.Done()
		if cookies != nil {
			pairCtx, done := context.WithTimeout(providerCtx, 3*time.Minute)
			defer done()
			code, session, err := g.client.StartGaiaPairing(pairCtx, providerCtx)
			if err != nil {
				started <- fmt.Errorf("could not start Google pairing: %w", err)
				return
			}
			emoji(code)
			if _, err := g.client.FinishGaiaPairing(pairCtx, session); err != nil {
				started <- fmt.Errorf("google pairing did not complete: %w", err)
				return
			}
			data, err := g.authSnapshot()
			if err == nil {
				err = b.commitPairedSession(pairCtx, pairGeneration, data)
			}
			if err != nil {
				started <- err
				return
			}
			started <- nil
			return
		}
		if err := g.client.Connect(providerCtx); err != nil {
			started <- fmt.Errorf("could not connect Google session: %w", err)
			return
		}
		started <- nil
	}()
	select {
	case err := <-started:
		if err != nil {
			if cookies == nil && libgm.IsAuthFailure(err) {
				b.setStatusReason("authentication_required", "session_expired", "Your Google Messages phone session expired or was revoked; re-pair to reconnect")
			} else {
				b.setStatusReason("connection_failed", "provider_failure", err.Error())
			}
			return err
		}
		if cookies != nil {
			select {
			case err := <-b.fatal:
				return err
			default:
				return nil
			}
		}
	case err := <-b.fatal:
		return err
	case <-ctx.Done():
		return nil
	}
	b.setProvider(provider.NewGoogle(g.client))
	b.RequestSync()
	owned.Add(3)
	go func() { defer owned.Done(); b.syncLoop(providerCtx) }()
	go func() { defer owned.Done(); b.sendLoop(providerCtx) }()
	go func() { defer owned.Done(); b.historyLoop(providerCtx) }()
	select {
	case err := <-b.fatal:
		return err
	case <-ctx.Done():
		return nil
	}
}
