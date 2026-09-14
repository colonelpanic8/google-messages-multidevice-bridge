package store

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	bolt "go.etcd.io/bbolt"
)

// Google resends a conversation snapshot minutes after the message that changed
// it, so the newest stored message is what keeps a conversation list current.
// latestMessageBucket remembers that message per conversation, which also
// supplies the sender a list row needs to attribute its preview.
const latestMessageBucket = "conversation-latest"

type latestMessage struct {
	MessageID string    `json:"message_id"`
	Time      time.Time `json:"time"`
	Preview   string    `json:"preview"`
	SenderID  string    `json:"sender_id,omitempty"`
	Direction string    `json:"direction,omitempty"`
}

// unreadWindow bounds how old a message may be and still raise the unread
// badge, for the same reason push notifications are bounded: history imports
// and reconciliation replay old messages through the same path.
const unreadWindow = 5 * time.Minute

// previewSource summarizes a message snapshot when it can describe its
// conversation in a list row. A message with nothing to show, such as a
// protocol notice, leaves the existing preview alone.
func previewSource(data []byte) (model.Message, latestMessage, bool) {
	var m model.Message
	if json.Unmarshal(data, &m) != nil || m.Schema != model.Schema || m.ID == "" || m.ConversationID == "" || m.Deleted || m.Time.IsZero() {
		return m, latestMessage{}, false
	}
	l := latestMessage{MessageID: m.ID, Time: m.Time, Preview: m.PreviewText(), SenderID: m.SenderID, Direction: m.Direction}
	return m, l, l.Preview != ""
}

// unreadAfter reports how the newest message leaves the unread badge before
// Google resends the conversation itself. Only a message that is new to this
// store can move it: reconciliation re-observing what it already holds must not
// light a badge the phone has since cleared. A message the phone already
// displayed, or one sent from any of the owner's devices, clears it instead.
func unreadAfter(m model.Message, fresh bool) *bool {
	if !fresh {
		return nil
	}
	read, unread := false, true
	if m.Direction == "outgoing" {
		return &read
	}
	if m.Direction == "incoming" && !strings.Contains(m.Status, "displayed") && time.Since(m.Time) <= unreadWindow {
		return &unread
	}
	return nil
}

func (s *Store) latestMessageTx(tx *bolt.Tx, conversationID string) (latestMessage, bool, error) {
	var l latestMessage
	b := tx.Bucket([]byte(latestMessageBucket))
	if b == nil {
		return l, false, nil
	}
	v := b.Get([]byte(conversationID))
	if v == nil {
		return l, false, nil
	}
	plain, err := s.decrypt(v, latestMessageBucket+":"+conversationID)
	if err != nil {
		return l, false, err
	}
	if err = json.Unmarshal(plain, &l); err != nil {
		return l, false, err
	}
	return l, true, nil
}

func (s *Store) putLatestMessageTx(tx *bolt.Tx, conversationID string, l latestMessage) error {
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return tx.Bucket([]byte(latestMessageBucket)).Put([]byte(conversationID), s.encrypt(data, latestMessageBucket+":"+conversationID))
}

// mergeLatestMessage folds the newest stored message into a conversation. It
// describes the conversation unless the provider knows of a later message the
// bridge has not stored, in which case the provider's own preview stands
// unattributed rather than being labelled with the wrong sender.
func mergeLatestMessage(c *model.Conversation, l latestMessage, unread *bool) bool {
	if l.Time.Before(c.Updated) {
		return false
	}
	changed := c.Preview != l.Preview || c.PreviewSenderID != l.SenderID || c.PreviewDirection != l.Direction || l.Time.After(c.Updated)
	c.Preview, c.PreviewSenderID, c.PreviewDirection = l.Preview, l.SenderID, l.Direction
	if l.Time.After(c.Updated) {
		c.Updated = l.Time
	}
	if unread != nil && c.Unread != *unread {
		c.Unread = *unread
		changed = true
	}
	return changed
}

// previewConversation rewrites an incoming conversation snapshot so a preview
// already derived from a newer message survives it. Legacy prototype records
// pass through untouched; they are migrated elsewhere.
func (s *Store) previewConversation(tx *bolt.Tx, data json.RawMessage) (json.RawMessage, error) {
	var c model.Conversation
	if json.Unmarshal(data, &c) != nil || c.Schema != model.Schema || c.ID == "" {
		return data, nil
	}
	l, ok, err := s.latestMessageTx(tx, c.ID)
	if err != nil || !ok || !mergeLatestMessage(&c, l, nil) {
		return data, err
	}
	return json.Marshal(c)
}

// updateConversationTx rewrites a stored conversation snapshot in place,
// republishing it as an ordinary conversation event when mutate changes it.
func (s *Store) updateConversationTx(tx *bolt.Tx, conversationID string, mutate func(*model.Conversation) bool) (bool, error) {
	key := "conversation:" + conversationID
	stored := tx.Bucket([]byte("latest")).Get([]byte(key))
	if stored == nil {
		return false, nil
	}
	plain, err := s.decrypt(stored, key)
	if err != nil {
		return false, err
	}
	var c model.Conversation
	if json.Unmarshal(plain, &c) != nil || c.Schema != model.Schema {
		return false, nil
	}
	if !mutate(&c) {
		return false, nil
	}
	data, err := json.Marshal(c)
	if err != nil {
		return false, err
	}
	return s.appendTx(tx, Event{Type: "conversation", EntityID: conversationID, Data: data})
}

// ReadConversation clears the unread badge as soon as the phone accepts a read
// receipt, rather than leaving it lit until Google resends the conversation.
func (s *Store) ReadConversation(conversationID string) (bool, error) {
	changed := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		changed, err = s.updateConversationTx(tx, conversationID, func(c *model.Conversation) bool {
			if !c.Unread {
				return false
			}
			c.Unread = false
			return true
		})
		return err
	})
	return changed, err
}

// refreshConversationTx records a just-applied message as its conversation's
// newest and updates the conversation snapshot when that changes what a list
// row shows. fresh reports whether the message snapshot was new to the store.
func (s *Store) refreshConversationTx(tx *bolt.Tx, data []byte, fresh bool) (bool, error) {
	m, l, ok := previewSource(data)
	if !ok {
		return false, nil
	}
	previous, found, err := s.latestMessageTx(tx, m.ConversationID)
	if err != nil {
		return false, err
	}
	if found && previous.Time.After(l.Time) {
		return false, nil
	}
	if err = s.putLatestMessageTx(tx, m.ConversationID, l); err != nil {
		return false, err
	}
	unread := unreadAfter(m, fresh)
	return s.updateConversationTx(tx, m.ConversationID, func(c *model.Conversation) bool {
		return mergeLatestMessage(c, l, unread)
	})
}

// ensureConversationPreviews derives the newest message of every conversation
// once, for stores written before previews were derived locally. Without it an
// existing conversation would keep an unattributed preview until its next
// message arrives.
func (s *Store) ensureConversationPreviews(tx *bolt.Tx) error {
	if tx.Bucket([]byte(latestMessageBucket)) != nil {
		return nil
	}
	if _, err := tx.CreateBucket([]byte(latestMessageBucket)); err != nil {
		return err
	}
	newest := map[string]latestMessage{}
	c := tx.Bucket([]byte("latest")).Cursor()
	prefix := []byte("message:")
	for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
		plain, err := s.decrypt(v, string(k))
		if err != nil {
			return err
		}
		m, l, ok := previewSource(plain)
		if !ok {
			continue
		}
		if best, seen := newest[m.ConversationID]; seen && best.Time.After(l.Time) {
			continue
		}
		newest[m.ConversationID] = l
	}
	for id, l := range newest {
		if err := s.putLatestMessageTx(tx, id, l); err != nil {
			return err
		}
		mutate := func(c *model.Conversation) bool { return mergeLatestMessage(c, l, nil) }
		if _, err := s.updateConversationTx(tx, id, mutate); err != nil {
			return err
		}
	}
	return nil
}
