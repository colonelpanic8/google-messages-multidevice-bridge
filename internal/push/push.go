// Package push delivers Web Push notifications to subscribed browsers. Payloads
// are encrypted for the subscription's own keys (RFC 8291), so the push service
// that relays them sees only metadata, never message content.
package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
)

// ErrInvalid reports a subscription the browser could not have produced.
var ErrInvalid = errors.New("invalid push subscription")

// Store is the durable half of the sender.
type Store interface {
	EnsurePushKeys(func() (store.PushKeys, error)) (store.PushKeys, error)
	PutSubscription(id string, data []byte) error
	DeleteSubscription(id string) error
	Subscriptions() (map[string][]byte, error)
}

// Notification is the payload the service worker receives.
type Notification struct {
	Title        string `json:"title"`
	Body         string `json:"body,omitempty"`
	Tag          string `json:"tag,omitempty"`
	Conversation string `json:"conversation,omitempty"`
}

type Sender struct {
	store   Store
	subject string
	client  *http.Client
	mu      sync.Mutex
	keys    store.PushKeys
}

// New loads or creates the server's VAPID identity.
func New(s Store, subject string) (*Sender, error) {
	keys, err := s.EnsurePushKeys(func() (store.PushKeys, error) {
		private, public, err := webpush.GenerateVAPIDKeys()
		return store.PushKeys{PrivateKey: private, PublicKey: public}, err
	})
	if err != nil {
		return nil, err
	}
	return &Sender{store: s, subject: subject, keys: keys, client: &http.Client{Timeout: 20 * time.Second}}, nil
}

// PublicKey is safe to hand to any authenticated client.
func (s *Sender) PublicKey() string { return s.keys.PublicKey }

// ID derives a stable identifier from the endpoint so a browser that
// re-subscribes replaces its record rather than adding another.
func ID(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:])
}

// Subscribe validates and stores one browser subscription.
func (s *Sender) Subscribe(raw []byte) (string, error) {
	var sub webpush.Subscription
	if err := json.Unmarshal(raw, &sub); err != nil {
		return "", ErrInvalid
	}
	if sub.Endpoint == "" || len(sub.Endpoint) > 2048 || sub.Keys.Auth == "" || sub.Keys.P256dh == "" {
		return "", ErrInvalid
	}
	normalized, err := json.Marshal(sub)
	if err != nil {
		return "", ErrInvalid
	}
	id := ID(sub.Endpoint)
	return id, s.store.PutSubscription(id, normalized)
}

func (s *Sender) Unsubscribe(endpoint string) error { return s.store.DeleteSubscription(ID(endpoint)) }

func (s *Sender) Count() (int, error) {
	subs, err := s.store.Subscriptions()
	return len(subs), err
}

// Send delivers to every subscription and drops the ones the push service
// reports as permanently gone.
func (s *Sender) Send(ctx context.Context, n Notification) error {
	payload, err := json.Marshal(n)
	if err != nil {
		return err
	}
	subs, err := s.store.Subscriptions()
	if err != nil {
		return err
	}
	// One at a time: a handful of subscriptions never justifies fan-out, and
	// serial delivery keeps ordering stable when several arrive together.
	var failures error
	for id, raw := range subs {
		var sub webpush.Subscription
		if json.Unmarshal(raw, &sub) != nil {
			_ = s.store.DeleteSubscription(id)
			continue
		}
		if err := s.deliver(ctx, payload, &sub, id); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

func (s *Sender) deliver(ctx context.Context, payload []byte, sub *webpush.Subscription, id string) error {
	s.mu.Lock()
	keys := s.keys
	s.mu.Unlock()
	res, err := webpush.SendNotificationWithContext(ctx, payload, sub, &webpush.Options{
		HTTPClient:      s.client,
		Subscriber:      s.subject,
		VAPIDPublicKey:  keys.PublicKey,
		VAPIDPrivateKey: keys.PrivateKey,
		TTL:             86400,
		Urgency:         webpush.UrgencyHigh,
	})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone:
		// The browser revoked or replaced this subscription.
		return s.store.DeleteSubscription(id)
	case res.StatusCode >= 300:
		return errors.New("push endpoint returned " + res.Status)
	}
	return nil
}
