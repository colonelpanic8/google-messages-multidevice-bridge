// Package model defines the bridge's versioned, provider-independent wire records.
package model

import (
	"slices"
	"strings"
	"time"
)

const Schema = 1

type Participant struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
	IsMe    bool   `json:"is_me"`
}

// Contact is one addressable entry from the phone's address book, used to
// complete recipients when starting a conversation.
type Contact struct {
	ID        string `json:"id"`
	ContactID string `json:"contact_id,omitempty"`
	Name      string `json:"name"`
	// Address is the E.164 number a new conversation can be addressed to, and
	// is empty for a contact the phone reported without a dialable number.
	Address string `json:"address,omitempty"`
	// Formatted is the phone's own rendering of Address, for display only.
	Formatted string `json:"formatted,omitempty"`
	// Frequent marks the handful of contacts the phone ranks as most used.
	Frequent bool `json:"frequent,omitempty"`
}

// ContactBook is the whole address book as one record, because the phone only
// ever hands it over in full.
type ContactBook struct {
	Schema   int       `json:"schema"`
	Contacts []Contact `json:"contacts"`
	Updated  time.Time `json:"updated"`
	// Stale means the phone could not be reached and these are the contacts
	// from the last successful read.
	Stale bool `json:"stale"`
}

type Conversation struct {
	Schema       int           `json:"schema"`
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Preview      string        `json:"preview"`
	Updated      time.Time     `json:"updated"`
	Unread       bool          `json:"unread"`
	ReadOnly     bool          `json:"read_only"`
	Protocol     string        `json:"protocol"`
	State        string        `json:"state"`
	Participants []Participant `json:"participants"`
	// PreviewSenderID and PreviewDirection describe the message Preview was
	// taken from so a list row can name its author.
	PreviewSenderID  string `json:"preview_sender_id,omitempty"`
	PreviewDirection string `json:"preview_direction,omitempty"`
}
type Attachment struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MIME      string `json:"mime"`
	Size      int64  `json:"size"`
	Available bool   `json:"available"`
	// Preview means only a thumbnail or inline preview can be downloaded.
	Preview bool `json:"preview"`
	// Requestable means the phone can be asked to upload the full media.
	Requestable bool `json:"requestable"`
}
type Reaction struct {
	Emoji        string   `json:"emoji"`
	Participants []string `json:"participants"`
}
type Message struct {
	Schema         int          `json:"schema"`
	ID             string       `json:"id"`
	ConversationID string       `json:"conversation_id"`
	SenderID       string       `json:"sender_id"`
	Time           time.Time    `json:"time"`
	Text           string       `json:"text"`
	Subject        string       `json:"subject,omitempty"`
	Direction      string       `json:"direction"`
	Status         string       `json:"status"`
	Deleted        bool         `json:"deleted"`
	ReadOnly       bool         `json:"read_only,omitempty"`
	TransactionID  string       `json:"transaction_id,omitempty"`
	Attachments    []Attachment `json:"attachments"`
	Reactions      []Reaction   `json:"reactions"`
}
type Typing struct {
	Schema         int    `json:"schema"`
	ConversationID string `json:"conversation_id"`
	ParticipantID  string `json:"participant_id"`
	Active         bool   `json:"active"`
}
type Upload struct {
	Schema  int       `json:"schema"`
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	MIME    string    `json:"mime"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
}
type SendRequest struct {
	Kind           string   `json:"kind,omitempty"`
	Recipients     []string `json:"recipients,omitempty"`
	GroupName      string   `json:"group_name,omitempty"`
	AttachmentIDs  []string `json:"attachment_ids,omitempty"`
	MessageID      string   `json:"message_id,omitempty"`
	Emoji          string   `json:"emoji,omitempty"`
	Remove         bool     `json:"remove,omitempty"`
	ConversationID string   `json:"conversation_id"`
	Text           string   `json:"text"`
}

// PreviewLimit bounds a derived conversation preview so a long message does not
// bloat every conversation snapshot.
const PreviewLimit = 160

// PreviewText summarizes a message the way a conversation list row shows it.
func (m Message) PreviewText() string {
	text := strings.Join(strings.Fields(m.Text), " ")
	if text == "" && len(m.Attachments) > 0 {
		switch kind, _, _ := strings.Cut(m.Attachments[0].MIME, "/"); kind {
		case "image":
			text = "Photo"
		case "video":
			text = "Video"
		case "audio":
			text = "Audio message"
		default:
			text = "Attachment"
		}
	}
	if runes := []rune(text); len(runes) > PreviewLimit {
		text = strings.TrimRight(string(runes[:PreviewLimit]), " ") + "…"
	}
	return text
}

func (r SendRequest) Equal(other SendRequest) bool {
	return r.Kind == other.Kind && r.GroupName == other.GroupName && r.ConversationID == other.ConversationID && r.Text == other.Text && r.MessageID == other.MessageID && r.Emoji == other.Emoji && r.Remove == other.Remove && slices.Equal(r.Recipients, other.Recipients) && slices.Equal(r.AttachmentIDs, other.AttachmentIDs)
}

type Outbox struct {
	ConversationID string      `json:"conversation_id,omitempty"`
	SessionEpoch   uint64      `json:"session_epoch"`
	Schema         int         `json:"schema"`
	ID             string      `json:"id"`
	Request        SendRequest `json:"request"`
	TransactionID  string      `json:"transaction_id"`
	State          string      `json:"state"`
	MessageID      string      `json:"message_id,omitempty"`
	Detail         string      `json:"detail,omitempty"`
	Created        time.Time   `json:"created"`
	Updated        time.Time   `json:"updated"`
}

type HistoryJob struct {
	Generation     uint64    `json:"generation"`
	SessionEpoch   uint64    `json:"session_epoch"`
	Schema         int       `json:"schema"`
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	ConversationID string    `json:"conversation_id,omitempty"`
	Folder         string    `json:"folder,omitempty"`
	State          string    `json:"state"`
	Pages          int64     `json:"pages"`
	Records        int64     `json:"records"`
	Detail         string    `json:"detail,omitempty"`
	Updated        time.Time `json:"updated"`
	RetryAt        time.Time `json:"retry_at,omitempty"`
}
