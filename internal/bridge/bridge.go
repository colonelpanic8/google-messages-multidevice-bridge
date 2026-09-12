package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/util/exhttp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type Status struct {
	State   string    `json:"state"`
	Updated time.Time `json:"updated"`
}
type pending struct {
	event   store.Event
	session []byte
}
type Bridge struct {
	Store  *store.Store
	Hub    *Hub
	client *libgm.Client
	queue  chan pending
	fatal  chan error
	mu     sync.RWMutex
	status Status
}

func New(s *store.Store) *Bridge {
	b := &Bridge{Store: s, Hub: NewHub(), queue: make(chan pending, 1024), fatal: make(chan error, 1)}
	b.setStatus("offline")
	return b
}
func (b *Bridge) Status() Status { b.mu.RLock(); defer b.mu.RUnlock(); return b.status }
func (b *Bridge) setStatus(state string) {
	b.mu.Lock()
	b.status = Status{state, time.Now().UTC()}
	b.mu.Unlock()
}
func (b *Bridge) fail(err error) {
	select {
	case b.fatal <- err:
	default:
	}
}
func (b *Bridge) enqueue(item pending) {
	select {
	case b.queue <- item:
	default:
		b.fail(errors.New("ingestion queue full; reconnect and backfill required"))
	}
}

func encode(kind, id string, msg proto.Message) (store.Event, error) {
	data, err := protojson.Marshal(msg)
	return store.Event{Type: kind, EntityID: id, Time: time.Now().UTC(), Data: data}, err
}
func (b *Bridge) ingest(kind, id string, msg proto.Message) {
	event, err := encode(kind, id, msg)
	if err != nil {
		b.fail(err)
		return
	}
	b.enqueue(pending{event: event})
}
func (b *Bridge) snapshotSession() {
	b.client.AuthData.CookiesLock.RLock()
	data, err := json.Marshal(b.client.AuthData)
	b.client.AuthData.CookiesLock.RUnlock()
	if err != nil {
		b.fail(errors.New("serialize session"))
		return
	}
	b.enqueue(pending{session: data})
}

func (b *Bridge) Handle(event any) {
	switch e := event.(type) {
	case *libgm.WrappedMessage:
		b.ingest("message", e.GetMessageID(), e.Message)
	case *gmproto.Message:
		b.ingest("message", e.GetMessageID(), e)
	case *gmproto.Conversation:
		b.ingest("conversation", e.GetConversationID(), e)
	case *gmproto.TypingData:
		out, err := encode("typing", e.GetConversationID(), e)
		if err != nil {
			b.fail(err)
			return
		}
		b.Hub.Publish(out)
	case *events.ClientReady:
		b.setStatus("connected")
		for _, conv := range e.Conversations {
			b.Handle(conv)
		}
	case *events.AuthTokenRefreshed, *events.PairSuccessful:
		b.snapshotSession()
	case *events.ListenTemporaryError, *events.PingFailed, *events.PhoneNotResponding, *events.NoDataReceived:
		b.setStatus("degraded")
	case *events.ListenRecovered, *events.PhoneRespondingAgain:
		b.setStatus("connected")
	case *events.GaiaLoggedOut:
		b.setStatus("authentication_required")
		b.fail(errors.New("google session expired; pair again"))
	case *events.ListenFatalError:
		b.setStatus("connection_failed")
		b.fail(errors.New("google connection failed"))
	}
}

func (b *Bridge) persist(item pending) error {
	if item.session != nil {
		return b.Store.SaveSession(item.session)
	}
	added, err := b.Store.Append(item.event)
	if added {
		b.Hub.Notify()
	}
	return err
}

func (b *Bridge) consume(ctx context.Context) error {
	for {
		select {
		case err := <-b.fatal:
			return err
		case item := <-b.queue:
			if err := b.persist(item); err != nil {
				return fmt.Errorf("persist event: %w", err)
			}
		case <-ctx.Done():
			return b.drain()
		}
	}
}

func (b *Bridge) drain() error {
	for {
		select {
		case item := <-b.queue:
			if err := b.persist(item); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// Run owns the provider connection. Offline mode only serves existing history.
func (b *Bridge) Run(ctx context.Context, offline bool, cookies map[string]string, emoji func(string)) error {
	if offline {
		return b.consume(ctx)
	}
	auth := libgm.NewAuthData()
	if cookies != nil {
		auth.SetCookies(cookies)
	} else {
		data, err := b.Store.Session()
		if err != nil {
			return err
		}
		if len(data) == 0 {
			return errors.New("no session; run pair first")
		}
		if err := json.Unmarshal(data, auth); err != nil {
			return errors.New("invalid stored session")
		}
	}
	providerCtx, cancel := context.WithCancel(ctx)
	// Upstream diagnostics can contain message bodies and authentication data.
	providerLog := zerolog.Nop()
	providerCtx = providerLog.WithContext(providerCtx)
	b.client = libgm.NewClient(auth, nil, providerLog, exhttp.SensibleClientSettings)
	b.client.SetEventHandler(b.Handle)
	defer func() { cancel(); b.client.Disconnect() }()
	b.setStatus("connecting")
	started := make(chan error, 1)
	go func() {
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
	for {
		select {
		case err := <-started:
			if err != nil {
				return err
			}
			if cookies != nil {
				for {
					select {
					case item := <-b.queue:
						if err := b.persist(item); err != nil {
							return err
						}
					default:
						return nil
					}
				}
			}
			return b.consume(providerCtx)
		case item := <-b.queue:
			if err := b.persist(item); err != nil {
				return err
			}
		case err := <-b.fatal:
			return err
		case <-ctx.Done():
			return nil
		}
	}
}
