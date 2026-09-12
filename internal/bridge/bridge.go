package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/util/exhttp"
	"google.golang.org/protobuf/proto"
)

// ErrStorage wraps failures that leave the database untrustworthy. Callers
// should stop serving; every other Run error leaves stored history readable.
var ErrStorage = errors.New("storage failure")

type Status struct {
	State     string     `json:"state"`
	Detail    string     `json:"detail,omitempty"`
	Updated   time.Time  `json:"updated"`
	Transport bool       `json:"transport_connected"`
	Phone     bool       `json:"phone_responsive"`
	SyncState string     `json:"sync_state"`
	LastSync  *time.Time `json:"last_sync,omitempty"`
}

type Bridge struct {
	Store       *store.Store
	Hub         *Hub
	client      *libgm.Client
	fatal       chan error
	mu          sync.RWMutex
	status      Status
	provider    provider.Provider
	providerCtx context.Context
	providerOps sync.WaitGroup
	prepareOnce sync.Once
	prepareErr  error
	storageErr  error
	syncWake    chan struct{}
	sendWake    chan struct{}
	eventMu     sync.Mutex
	stopped     bool
	pairing     bool

	syncEvery   time.Duration
	sendRetry   time.Duration
	sendTimeout time.Duration
}

func New(s *store.Store) *Bridge {
	b := &Bridge{Store: s, Hub: NewHub(), fatal: make(chan error, 1), syncWake: make(chan struct{}, 1), sendWake: make(chan struct{}, 1), syncEvery: 5 * time.Minute, sendRetry: 5 * time.Second, sendTimeout: 90 * time.Second}
	b.setStatus("offline", "")
	return b
}
func (b *Bridge) Status() Status { b.mu.RLock(); defer b.mu.RUnlock(); return b.status }
func (b *Bridge) setStatus(state, detail string) {
	b.mu.Lock()
	b.status.State, b.status.Detail, b.status.Updated = state, detail, time.Now().UTC()
	if state != "connected" && state != "degraded" {
		b.status.Transport, b.status.Phone = false, false
	}
	b.mu.Unlock()
}
func (b *Bridge) fail(err error) {
	if !b.failed() {
		b.setStatus("connection_failed", "Provider stopped; restart the service or pair again")
	}
	select {
	case b.fatal <- err:
	default:
	}
}
func (b *Bridge) storageFailure(err error) {
	b.mu.Lock()
	b.storageErr = fmt.Errorf("%w: %v", ErrStorage, err)
	b.mu.Unlock()
	b.setStatus("storage_failed", "Database write failed; stop the service and inspect storage")
	b.fail(fmt.Errorf("%w: %v", ErrStorage, err))
}

// persist commits synchronously inside the provider callback so the upstream
// ack boundary is as close to local persistence as the library allows.
// Upstream queues the ack first; reconciliation can repair recent gaps.
func (b *Bridge) persist(snap provider.Snapshot) {
	changed, err := b.Store.Apply(snap.Event, snap.Private, nil)
	if err != nil {
		b.storageFailure(err)
		return
	}
	if changed {
		b.Hub.Notify()
	}
}
func (b *Bridge) ingest(msg proto.Message) {
	snapshot, err := provider.SnapshotOf(msg)
	if err != nil {
		b.fail(err)
		return
	}
	b.persist(snapshot)
}
func (b *Bridge) snapshotSession() {
	b.client.AuthData.CookiesLock.RLock()
	data, err := json.Marshal(b.client.AuthData)
	b.client.AuthData.CookiesLock.RUnlock()
	if err != nil {
		b.fail(errors.New("serialize session"))
		return
	}
	if err := b.Store.SaveSession(data); err != nil {
		b.storageFailure(err)
	}
}

// Handle runs on the provider's polling goroutine. It is gated so nothing
// reaches the store after Run returns, even if upstream goroutines linger.
func (b *Bridge) Handle(event any) {
	b.eventMu.Lock()
	defer b.eventMu.Unlock()
	if b.stopped {
		return
	}
	switch e := event.(type) {
	case *libgm.WrappedMessage:
		b.ingest(e.Message)
	case *gmproto.Message:
		b.ingest(e)
	case *gmproto.Conversation:
		b.ingest(e)
	case *gmproto.TypingData:
		out, err := provider.SnapshotOf(e)
		if err != nil {
			b.fail(err)
			return
		}
		b.Hub.Publish(out.Event)
	case *events.ClientReady:
		b.connection(true, true)
		for _, conv := range e.Conversations {
			b.ingest(conv)
		}
		b.RequestSync()
	case *events.AuthTokenRefreshed:
		if !b.pairing {
			b.snapshotSession()
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
		b.setStatus("authentication_required", "Google signed this device out; pair again")
		b.fail(errors.New("google session expired; pair again"))
	case *events.ListenFatalError:
		b.setStatus("connection_failed", "Google connection failed; restart the service or pair again")
		b.fail(errors.New("google connection failed"))
	}
}

// Run owns the provider connection until ctx ends or the provider fails. It
// returns only after every goroutine it started has exited and the event
// handler is closed. Stored history stays readable afterwards unless the
// error wraps ErrStorage. Upstream polling helpers are not joined; see
// third_party/mautrix-gmessages/PATCHES.md.
func (b *Bridge) Run(ctx context.Context, offline bool, cookies map[string]string, emoji func(string)) (result error) {
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
			b.setStatus("authentication_required", "No paired session; run pair first")
			return errors.New("no session; run pair first")
		}
		if err := json.Unmarshal(data, auth); err != nil {
			b.setStatus("authentication_required", "Invalid stored session; pair again")
			return errors.New("invalid stored session")
		}
	}
	providerCtx, cancel := context.WithCancel(ctx)
	// Upstream diagnostics can contain message bodies and authentication data.
	providerLog := zerolog.Nop()
	providerCtx = providerLog.WithContext(providerCtx)
	b.pairing = cookies != nil
	b.client = libgm.NewClient(auth, nil, providerLog, exhttp.SensibleClientSettings)
	b.client.SetEventHandler(b.Handle)
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
		b.client.Disconnect()
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
			code, session, err := b.client.StartGaiaPairing(pairCtx, providerCtx)
			if err != nil {
				started <- errors.New("could not start Google pairing; check cookies and account pairing mode")
				return
			}
			emoji(code)
			if _, err := b.client.FinishGaiaPairing(pairCtx, session); err != nil {
				started <- errors.New("google pairing did not complete")
				return
			}
			b.snapshotSession()
			started <- nil
			return
		}
		if err := b.client.Connect(providerCtx); err != nil {
			started <- errors.New("could not connect Google session; re-pair may be needed")
			return
		}
		started <- nil
	}()
	select {
	case err := <-started:
		if err != nil {
			b.setStatus("connection_failed", err.Error())
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
	b.setProvider(provider.NewGoogle(b.client))
	owned.Add(2)
	go func() { defer owned.Done(); b.syncLoop(providerCtx) }()
	go func() { defer owned.Done(); b.sendLoop(providerCtx) }()
	select {
	case err := <-b.fatal:
		return err
	case <-ctx.Done():
		return nil
	}
}
func (b *Bridge) closeHandler() {
	b.eventMu.Lock()
	b.stopped = true
	b.eventMu.Unlock()
}
func (b *Bridge) failed() bool {
	switch b.Status().State {
	case "authentication_required", "connection_failed", "storage_failed":
		return true
	}
	return false
}
