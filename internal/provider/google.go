package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const MaxAttachmentBytes = 20 << 20
const RecentConversations = 30
const RecentMessages = 50

var ErrTooLarge = errors.New("attachment exceeds 20 MiB")

type Google struct {
	Client    *libgm.Client
	listMu    sync.Mutex
	mediaSlot chan struct{}
	mediaOnce sync.Once
}

func NewGoogle(client *libgm.Client) *Google {
	return &Google{Client: client, mediaSlot: make(chan struct{}, 1)}
}

func SnapshotOf(msg proto.Message) (Snapshot, error) {
	var value any
	var kind, id string
	private := make(map[string][]byte)
	switch m := msg.(type) {
	case *gmproto.Message:
		kind, id = "message", m.GetMessageID()
		status := strings.ToLower(m.GetMessageStatus().GetStatus().String())
		direction := "system"
		if strings.HasPrefix(status, "incoming_") {
			direction = "incoming"
		} else if strings.HasPrefix(status, "outgoing_") {
			direction = "outgoing"
		}
		out := model.Message{Schema: model.Schema, ID: id, ConversationID: m.GetConversationID(), SenderID: m.GetParticipantID(), Time: time.UnixMicro(m.GetTimestamp()).UTC(), Subject: m.GetSubject(), Direction: direction, Status: status, TransactionID: m.GetTmpID(), Deleted: strings.Contains(status, "deleted"), Attachments: []model.Attachment{}, Reactions: []model.Reaction{}}
		for _, info := range m.GetMessageInfo() {
			out.Text += info.GetMessageContent().GetContent()
			if media := info.GetMediaContent(); media != nil {
				data, err := marshalPrivate(media)
				if err != nil {
					return Snapshot{}, err
				}
				hash := sha256.Sum256(data)
				aid := hex.EncodeToString(hash[:])
				private[aid] = data
				mime := media.GetMimeType()
				if mime == "" {
					mime = libgm.FormatToMediaType[media.GetFormat()].Format
				}
				if mime == "" {
					mime = "application/octet-stream"
				}
				available := len(media.GetMediaData()) > 0 || (media.GetMediaID() != "" && len(media.GetDecryptionKey()) == 32)
				out.Attachments = append(out.Attachments, model.Attachment{ID: aid, Name: media.GetMediaName(), MIME: mime, Size: media.GetSize(), Available: available})
			}
		}
		for _, r := range m.GetReactions() {
			emoji := r.GetData().GetUnicode()
			if emoji == "" {
				emoji = strings.ToLower(r.GetData().GetType().String())
			}
			out.Reactions = append(out.Reactions, model.Reaction{Emoji: emoji, Participants: r.GetParticipantIDs()})
		}
		value = out
	case *gmproto.Conversation:
		kind, id = "conversation", m.GetConversationID()
		out := model.Conversation{Schema: model.Schema, ID: id, Name: m.GetName(), Preview: m.GetLatestMessage().GetDisplayContent(), Updated: time.UnixMicro(m.GetLastMessageTimestamp()).UTC(), Unread: m.GetUnread(), ReadOnly: m.GetReadOnly(), Protocol: strings.ToLower(m.GetType().String()), State: strings.ToLower(m.GetStatus().String()), Participants: []model.Participant{}}
		for _, p := range m.GetParticipants() {
			name := p.GetFullName()
			if name == "" {
				name = p.GetFirstName()
			}
			out.Participants = append(out.Participants, model.Participant{ID: p.GetID().GetParticipantID(), Name: name, Address: p.GetID().GetNumber(), IsMe: p.GetIsMe()})
		}
		value = out
	case *gmproto.TypingData:
		kind, id = "typing", m.GetConversationID()
		value = model.Typing{Schema: model.Schema, ConversationID: id, ParticipantID: m.GetUser().GetNumber(), Active: m.GetType() == gmproto.TypingTypes_STARTED_TYPING}
	default:
		return Snapshot{}, errors.New("unsupported snapshot")
	}
	data, err := json.Marshal(value)
	return Snapshot{Event: store.Event{Type: kind, EntityID: id, Time: time.Now().UTC(), Data: data}, Private: private}, err
}
func (g *Google) Conversations(ctx context.Context) ([]Snapshot, error) {
	// libgm's first-list flag is not synchronized.
	g.listMu.Lock()
	defer g.listMu.Unlock()
	res, err := g.Client.ListConversations(ctx, &gmproto.ListConversationsRequest{Count: RecentConversations, Folder: gmproto.ListConversationsRequest_INBOX})
	if err != nil {
		return nil, ErrUnavailable
	}
	out := make([]Snapshot, 0, len(res.GetConversations()))
	for _, c := range res.GetConversations() {
		s, err := SnapshotOf(c)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}
func (g *Google) Messages(ctx context.Context, id string) ([]Snapshot, error) {
	res, err := g.Client.FetchMessages(ctx, id, RecentMessages, nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	out := make([]Snapshot, 0, len(res.GetMessages()))
	for _, m := range res.GetMessages() {
		s, err := SnapshotOf(m)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

type simPayload = *gmproto.SIMPayload

// Prepare fetches the conversation and resolves the outgoing identity. It sends
// nothing, so a transient failure here leaves the outbox record queued.
func (g *Google) Prepare(ctx context.Context, conversationID string) (SendTarget, error) {
	conv, err := g.Client.GetConversation(ctx, conversationID)
	if err != nil {
		return SendTarget{}, ErrUnavailable
	}
	if conv == nil || conv.GetConversationID() != conversationID || conv.GetReadOnly() || conv.GetDefaultOutgoingID() == "" {
		return SendTarget{}, ErrRejected
	}
	sim := conv.GetSimCard().GetSIMData().GetSIMPayload()
	if sim == nil {
		for _, p := range conv.GetParticipants() {
			if p.GetIsMe() && p.GetID().GetParticipantID() == conv.GetDefaultOutgoingID() {
				sim = p.GetSimPayload()
			}
		}
	}
	// Never guess which subscription to charge on a multi-SIM phone.
	if sim == nil {
		return SendTarget{}, ErrRejected
	}
	return SendTarget{ConversationID: conversationID, participantID: conv.GetDefaultOutgoingID(), sim: sim}, nil
}

// Send issues one SendMessage RPC. The patched libgm never re-POSTs it.
func (g *Google) Send(ctx context.Context, target SendTarget, o model.Outbox) error {
	if target.ConversationID != o.Request.ConversationID || target.participantID == "" || target.sim == nil {
		return ErrRejected
	}
	req := &gmproto.SendMessageRequest{ConversationID: o.Request.ConversationID, TmpID: o.TransactionID, SIMPayload: target.sim, MessagePayload: &gmproto.MessagePayload{TmpID: o.TransactionID, TmpID2: o.TransactionID, ConversationID: o.Request.ConversationID, ParticipantID: target.participantID, MessageInfo: []*gmproto.MessageInfo{{Data: &gmproto.MessageInfo_MessageContent{MessageContent: &gmproto.MessageContent{Content: o.Request.Text}}}}}}
	res, err := g.Client.SendMessage(ctx, req)
	if err != nil {
		// Transport errors, relay 4xx/5xx, timeouts and cancellation all arrive
		// here. None proves the phone did not act, so the outcome stays unknown.
		return ErrAmbiguous
	}
	return SendOutcome(res.GetStatus())
}

// SendOutcome maps the phone's reply. Only explicit failure codes are rejections.
func SendOutcome(status gmproto.SendMessageResponse_Status) error {
	switch status {
	case gmproto.SendMessageResponse_SUCCESS:
		return nil
	case gmproto.SendMessageResponse_FAILURE_2, gmproto.SendMessageResponse_FAILURE_3, gmproto.SendMessageResponse_FAILURE_4:
		return ErrRejected
	default:
		return ErrAmbiguous
	}
}
func (g *Google) MarkRead(ctx context.Context, conv, id string) error {
	if err := g.Client.MarkRead(ctx, conv, id); err != nil {
		return ErrUnavailable
	}
	return nil
}

// Attachment downloads at most MaxAttachmentBytes. Downloads are serialized to
// bound memory and upstream connections.
func (g *Google) Attachment(ctx context.Context, data []byte) ([]byte, error) {
	var media gmproto.MediaContent
	if err := unmarshalPrivate(data, &media); err != nil {
		return nil, err
	}
	if media.GetSize() > MaxAttachmentBytes {
		return nil, ErrTooLarge
	}
	if len(media.GetMediaData()) > 0 {
		if len(media.GetMediaData()) > MaxAttachmentBytes {
			return nil, ErrTooLarge
		}
		return media.GetMediaData(), nil
	}
	if media.GetMediaID() == "" || len(media.GetDecryptionKey()) != 32 {
		return nil, ErrUnavailable
	}
	g.mediaOnce.Do(func() {
		if g.mediaSlot == nil {
			g.mediaSlot = make(chan struct{}, 1)
		}
	})
	select {
	case g.mediaSlot <- struct{}{}:
		defer func() { <-g.mediaSlot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	r, err := g.Client.DownloadMediaContext(ctx, media.GetMediaID(), media.GetDecryptionKey())
	if err != nil {
		return nil, ErrUnavailable
	}
	return ReadBounded(ctx, r)
}

// ReadBounded reads up to MaxAttachmentBytes and aborts the body on cancellation.
func ReadBounded(ctx context.Context, r io.ReadCloser) ([]byte, error) {
	stopWatcher := make(chan struct{})
	watcherDone := make(chan struct{})
	var closeOnce sync.Once
	closeBody := func() { closeOnce.Do(func() { _ = r.Close() }) }
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			closeBody()
		case <-stopWatcher:
		}
	}()
	out, err := io.ReadAll(io.LimitReader(r, MaxAttachmentBytes+1))
	close(stopWatcher)
	closeBody()
	<-watcherDone
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	if len(out) > MaxAttachmentBytes {
		return nil, ErrTooLarge
	}
	return out, nil
}

func marshalPrivate(media *gmproto.MediaContent) ([]byte, error) { return protojson.Marshal(media) }
func unmarshalPrivate(data []byte, media *gmproto.MediaContent) error {
	return protojson.Unmarshal(data, media)
}
