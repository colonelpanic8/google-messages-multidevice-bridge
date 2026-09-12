package bridge

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/colonelpanic8/multiconnect-bridge/internal/store"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

func TestProviderMessagesPersistButTypingDoesNot(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := New(s)
	sub, unsubscribe := b.Hub.Subscribe()
	defer unsubscribe()
	b.Handle(&libgm.WrappedMessage{Message: &gmproto.Message{MessageID: "m1"}})
	b.Handle(&gmproto.TypingData{ConversationID: "c1"})
	select {
	case e := <-sub.Live:
		if e.Type != "typing" || e.EntityID != "c1" {
			t.Fatalf("typing: %+v", e)
		}
	default:
		t.Fatal("typing not forwarded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Run(ctx, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	items, err := s.Events(0, 100)
	if err != nil || len(items) != 1 || items[0].Type != "message" {
		t.Fatalf("stored events: %+v %v", items, err)
	}
}

func TestSlowSubscriberCannotBlockIngestion(t *testing.T) {
	h := NewHub()
	slow, stop := h.Subscribe()
	defer stop()
	for i := 0; i < 33; i++ {
		h.Publish(store.Event{Type: "typing"})
	}
	select {
	case <-slow.Done:
	case <-time.After(time.Second):
		t.Fatal("slow subscriber not disconnected")
	}
	fast, stopFast := h.Subscribe()
	defer stopFast()
	h.Notify()
	h.Notify()
	select {
	case <-fast.Wake:
	default:
		t.Fatal("wake missing")
	}
	h.Publish(store.Event{Type: "typing"})
	select {
	case <-fast.Live:
	default:
		t.Fatal("healthy subscriber blocked")
	}
}

func TestOverflowFailsInsteadOfSilentlyLosingHistory(t *testing.T) {
	b := New(nil)
	for i := 0; i < cap(b.queue)+1; i++ {
		b.enqueue(pending{})
	}
	select {
	case <-b.fatal:
	default:
		t.Fatal("overflow not reported")
	}
}
