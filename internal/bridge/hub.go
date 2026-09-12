package bridge

import (
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
	"sync"
)

// Durable notifications are hints: consumers always replay from the store.
type Subscription struct {
	Wake chan struct{}
	Live chan store.Event
	Done chan struct{}
}
type Hub struct {
	mu          sync.Mutex
	subscribers map[*Subscription]bool
}

func NewHub() *Hub { return &Hub{subscribers: make(map[*Subscription]bool)} }
func (h *Hub) Subscribe() (*Subscription, func()) {
	s := &Subscription{make(chan struct{}, 1), make(chan store.Event, 32), make(chan struct{})}
	h.mu.Lock()
	h.subscribers[s] = true
	h.mu.Unlock()
	return s, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.subscribers[s] {
			delete(h.subscribers, s)
			close(s.Done)
		}
	}
}
func (h *Hub) Notify() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subscribers {
		select {
		case s.Wake <- struct{}{}:
		default:
		}
	}
}
func (h *Hub) Publish(event store.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subscribers {
		select {
		case s.Live <- event:
		default:
			delete(h.subscribers, s)
			close(s.Done)
		}
	}
}
