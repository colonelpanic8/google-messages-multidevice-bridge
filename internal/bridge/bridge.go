package bridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
)

// ErrStorage wraps failures that leave the database untrustworthy. Callers
// should stop serving; every other Run error leaves stored history readable.
var ErrStorage = errors.New("storage failure")

type Status struct {
	State                 string     `json:"state"`
	Reason                string     `json:"reason,omitempty"`
	Detail                string     `json:"detail,omitempty"`
	Updated               time.Time  `json:"updated"`
	Transport             bool       `json:"transport_connected"`
	Phone                 bool       `json:"phone_responsive"`
	SyncState             string     `json:"sync_state"`
	LastSync              *time.Time `json:"last_sync,omitempty"`
	SessionEpoch          uint64     `json:"session_epoch"`
	PreviousConversations int        `json:"previous_session_conversations"`
	PreviousMessages      int        `json:"previous_session_messages"`
}

type Bridge struct {
	mutationMu       sync.Mutex
	offlineOnly      bool
	pairingState     PairingState
	pairCookies      map[string]string
	pairWake         chan struct{}
	pairCancel       context.CancelFunc
	pairDone         chan struct{}
	pairAttempt      func(context.Context, map[string]string, func(string)) error
	connectionCancel context.CancelFunc
	reconnectWake    chan struct{}
	reconnectDelay   time.Duration
	historyWake      chan struct{}
	typingSent       map[string]time.Time
	contactsMu       sync.Mutex
	mediaRequested   map[string]time.Time
	Store            *store.Store
	Hub              *Hub
	connector        connection
	fatal            chan error
	mu               sync.RWMutex
	status           Status
	provider         provider.Provider
	providerCtx      context.Context
	providerOps      sync.WaitGroup
	prepareOnce      sync.Once
	prepareErr       error
	storageErr       error
	syncWake         chan struct{}
	sendWake         chan struct{}
	eventMu          sync.Mutex
	stopped          bool
	pairing          bool

	syncEvery   time.Duration
	sendRetry   time.Duration
	sendTimeout time.Duration
}

func New(s *store.Store) *Bridge {
	b := &Bridge{connector: &googleConnection{}, pairWake: make(chan struct{}, 1), reconnectWake: make(chan struct{}, 1), reconnectDelay: 5 * time.Second, historyWake: make(chan struct{}, 1), Store: s, Hub: NewHub(), fatal: make(chan error, 1), syncWake: make(chan struct{}, 1), sendWake: make(chan struct{}, 1), syncEvery: 5 * time.Minute, sendRetry: 5 * time.Second, sendTimeout: 90 * time.Second}
	b.setStatus("offline", "")
	return b
}
func (b *Bridge) Status() Status {
	b.mu.RLock()
	status := b.status
	b.mu.RUnlock()
	if summary, err := b.Store.SessionSummary(); err == nil {
		status.SessionEpoch = summary.Epoch
		status.PreviousConversations = summary.PreviousConversations
		status.PreviousMessages = summary.PreviousMessages
	}
	return status
}
func (b *Bridge) setStatus(state, detail string) {
	b.setStatusReason(state, "", detail)
}
func (b *Bridge) setStatusReason(state, reason, detail string) {
	b.mu.Lock()
	b.status.State, b.status.Reason, b.status.Detail, b.status.Updated = state, reason, detail, time.Now().UTC()
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
	wake(b.reconnectWake)
	wake(b.pairWake)
}

// persist commits before the error-aware provider callback admits its ACK.
func (b *Bridge) persist(snap provider.Snapshot) error {
	changed, err := b.Store.Apply(snap.Event, snap.Private, nil)
	if err != nil {
		b.storageFailure(err)
		return fmt.Errorf("%w: %v", ErrStorage, err)
	}
	if changed {
		b.Hub.Notify()
	}
	return nil
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

// connection owns network-specific pairing, events and connection lifetime.
type connection interface {
	Run(*Bridge, context.Context, bool, map[string]string, func(string)) error
	Handle(*Bridge, any) error
}

func (b *Bridge) Run(ctx context.Context, offline bool, credentials map[string]string, confirmation func(string)) error {
	return b.connector.Run(b, ctx, offline, credentials, confirmation)
}

func (b *Bridge) Handle(event any) error { return b.connector.Handle(b, event) }
