// Package provider adapts an upstream messaging connection to the bridge's
// records. Every send is at most one attempt: the bridge commits its intent
// before Send and never repeats an attempt whose outcome is unknown.
package provider

import (
	"context"
	"errors"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
)

// ErrRejected reports a definite refusal before or by the phone; nothing was sent.
var ErrRejected = errors.New("provider rejected send")

// ErrUnavailable reports a read-only operation that could not complete.
var ErrUnavailable = errors.New("provider unavailable")

var ErrUnsupportedCursor = errors.New("provider history cursor is unsupported")

// ErrAmbiguous reports a mutating request whose outcome is unknown.
var ErrAmbiguous = errors.New("send outcome unknown")

// Snapshot keeps private download credentials outside the public record.
type Snapshot struct {
	Event   store.Event
	Private map[string][]byte
}

// SendTarget is resolved by a read-only preflight and consumed by one Send.
type SendTarget struct {
	ConversationID string
	Media          [][]byte
	participantID  string
	sim            simPayload
}

type Provider interface {
	Conversations(context.Context) ([]Snapshot, error)
	Messages(context.Context, string) ([]Snapshot, error)
	CreateConversation(context.Context, []string) (Snapshot, error)
	React(context.Context, SendTarget, string, string, bool) error
	Typing(context.Context, SendTarget) error
	Upload(context.Context, []byte, string, string) ([]byte, error)
	MessagePage(context.Context, string, []byte) ([]Snapshot, []byte, error)
	ConversationPage(context.Context, string, []byte) ([]Snapshot, []byte, error)
	// Prepare validates the destination without side effects. ErrRejected means
	// the send can be refused durably; any other error leaves it queued.
	Prepare(context.Context, string) (SendTarget, error)
	// Send performs exactly one attempt. nil: accepted by the phone. ErrRejected:
	// the phone refused. Anything else: unknown, and must not be retried.
	Send(context.Context, SendTarget, model.Outbox) error
	MarkRead(context.Context, string, string) error
	Attachment(context.Context, []byte) ([]byte, error)
}
