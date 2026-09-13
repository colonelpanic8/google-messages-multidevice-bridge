package bridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
)

type recordingNotifier struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingNotifier) Send(_ context.Context, title, body, tag, conversation string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, title+"|"+body+"|"+tag+"|"+conversation)
	return nil
}

func (r *recordingNotifier) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// settle gives the watcher a chance to drain, re-waking it so the test never
// depends on winning a race with the subscription.
func (b *Bridge) settle(notifier *recordingNotifier, want int) []string {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b.Hub.Notify()
		time.Sleep(10 * time.Millisecond)
		if len(notifier.seen()) >= want {
			break
		}
	}
	// Let a wrongly-allowed extra delivery show up rather than pass silently.
	time.Sleep(80 * time.Millisecond)
	return notifier.seen()
}

func TestPushAnnouncesOnlyNewIncomingMessages(t *testing.T) {
	b := testBridge(t)
	b.persist(snapshot(t, "conversation", "c1", model.Conversation{
		Schema: 1, ID: "c1",
		Participants: []model.Participant{{ID: "p2", Name: "Dana", Address: "+15550000002"}},
	}))
	notifier := &recordingNotifier{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.WatchForPush(ctx, notifier) }()

	now := time.Now().UTC()
	message := func(id, direction, text string, age time.Duration, deleted bool) model.Message {
		return model.Message{
			Schema: 1, ID: id, ConversationID: "c1", SenderID: "p2",
			Time: now.Add(-age), Direction: direction, Text: text, Deleted: deleted,
			Status: "incoming_complete",
		}
	}
	// Backfilled history is far older than the window and must stay silent.
	b.persist(snapshot(t, "message", "old", message("old", "incoming", "ancient", 3*time.Hour, false)))
	b.persist(snapshot(t, "message", "mine", message("mine", "outgoing", "from me", time.Second, false)))
	b.persist(snapshot(t, "message", "gone", message("gone", "incoming", "removed", time.Second, true)))
	b.persist(snapshot(t, "message", "new", message("new", "incoming", "hello there", time.Second, false)))

	got := b.settle(notifier, 1)
	if len(got) != 1 || got[0] != "Dana|hello there|new|c1" {
		t.Fatalf("notifications: %q", got)
	}

	// A status update republishes the same entity; it must not notify twice.
	updated := message("new", "incoming", "hello there", time.Second, false)
	updated.Status = "incoming_displayed"
	b.persist(snapshot(t, "message", "new", updated))
	if got = b.settle(notifier, 2); len(got) != 1 {
		t.Fatalf("status update notified again: %q", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop")
	}
}

func TestPushDescribesAttachmentsAndUnknownSenders(t *testing.T) {
	b := testBridge(t)
	b.persist(snapshot(t, "conversation", "c2", model.Conversation{Schema: 1, ID: "c2", Name: "Book club"}))
	title, body := b.describe(model.Message{
		ID: "m", ConversationID: "c2", SenderID: "ghost", Time: time.Now(),
		Attachments: []model.Attachment{{ID: "a", MIME: "image/png"}},
	})
	if title != "Book club" || body != "Sent an attachment" {
		t.Fatalf("%q %q", title, body)
	}
	if title, body = b.describe(model.Message{ID: "m", ConversationID: "missing"}); title != "New message" || body != "" {
		t.Fatalf("unknown conversation: %q %q", title, body)
	}
}
