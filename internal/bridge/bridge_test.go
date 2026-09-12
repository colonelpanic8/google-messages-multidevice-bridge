package bridge

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
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

func TestStoppedHandlerCannotWriteAfterDatabaseCloses(t *testing.T) {
	b := testBridge(t)
	b.closeHandler()
	if err := b.Store.Close(); err != nil {
		t.Fatal(err)
	}
	b.Handle(&gmproto.Message{MessageID: "late"})
	select {
	case err := <-b.fatal:
		t.Fatalf("late callback touched storage: %v", err)
	default:
	}
}
