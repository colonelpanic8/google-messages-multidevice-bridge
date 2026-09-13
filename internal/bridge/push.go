package bridge

import (
	"context"
	"encoding/json"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
)

// PushNotifier delivers one notification to every subscribed browser.
type PushNotifier interface {
	Send(ctx context.Context, title, body, tag, conversation string) error
}

// pushWindow bounds how old a message may be and still raise a notification.
// History imports and reconciliation replay old messages through the same event
// log, and none of those should light up a phone.
const pushWindow = 5 * time.Minute

// pushMemory caps how many delivered message IDs are remembered so repeated
// status updates for one message notify at most once.
const pushMemory = 512

// WatchForPush notifies on genuinely new incoming messages until ctx ends. It
// starts from the current watermark, so nothing already stored is announced.
func (b *Bridge) WatchForPush(ctx context.Context, notifier PushNotifier) error {
	cursor, err := b.Store.Watermark()
	if err != nil {
		return err
	}
	sub, unsubscribe := b.Hub.Subscribe()
	defer unsubscribe()
	seen := map[string]bool{}
	var order []string
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sub.Done:
			return nil
		case <-sub.Wake:
		}
		for {
			events, err := b.Store.Events(cursor, 100)
			if err != nil || len(events) == 0 {
				break
			}
			for _, event := range events {
				cursor = event.ID
				if event.Type != "message" {
					continue
				}
				var message model.Message
				if json.Unmarshal(event.Data, &message) != nil {
					continue
				}
				if !b.shouldPush(message, seen) {
					continue
				}
				seen[message.ID] = true
				order = append(order, message.ID)
				if len(order) > pushMemory {
					delete(seen, order[0])
					order = order[1:]
				}
				title, body := b.describe(message)
				send, cancel := context.WithTimeout(ctx, 30*time.Second)
				_ = notifier.Send(send, title, body, message.ID, message.ConversationID)
				cancel()
			}
		}
	}
}

func (b *Bridge) shouldPush(message model.Message, seen map[string]bool) bool {
	if message.Direction != "incoming" || message.Deleted || seen[message.ID] {
		return false
	}
	return time.Since(message.Time) <= pushWindow
}

// describe names the sender without leaking more of the thread than the phone's
// own notification would.
func (b *Bridge) describe(message model.Message) (string, string) {
	title := "New message"
	var conversation model.Conversation
	if raw, err := b.Store.Record("conversation", message.ConversationID); err == nil &&
		json.Unmarshal(raw, &conversation) == nil {
		if conversation.Name != "" {
			title = conversation.Name
		}
		for _, participant := range conversation.Participants {
			if participant.ID != message.SenderID || participant.IsMe {
				continue
			}
			if name := participant.Name; name != "" && conversation.Name == "" {
				title = name
			} else if participant.Address != "" && conversation.Name == "" {
				title = participant.Address
			}
		}
	}
	body := message.Text
	if body == "" && len(message.Attachments) > 0 {
		body = "Sent an attachment"
	}
	return title, body
}
