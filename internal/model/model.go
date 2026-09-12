// Package model defines the bridge's versioned, provider-independent wire records.
package model

import "time"

const Schema = 1

type Participant struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
	IsMe    bool   `json:"is_me"`
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
}
type Attachment struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MIME      string `json:"mime"`
	Size      int64  `json:"size"`
	Available bool   `json:"available"`
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
type SendRequest struct {
	ConversationID string `json:"conversation_id"`
	Text           string `json:"text"`
}
type Outbox struct {
	Schema        int         `json:"schema"`
	ID            string      `json:"id"`
	Request       SendRequest `json:"request"`
	TransactionID string      `json:"transaction_id"`
	State         string      `json:"state"`
	MessageID     string      `json:"message_id,omitempty"`
	Detail        string      `json:"detail,omitempty"`
	Created       time.Time   `json:"created"`
	Updated       time.Time   `json:"updated"`
}
