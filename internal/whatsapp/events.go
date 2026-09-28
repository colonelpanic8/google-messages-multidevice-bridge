package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"sort"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
	local "github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func unwrap(m *waE2E.Message) *waE2E.Message {
	for m != nil {
		switch {
		case m.EphemeralMessage != nil:
			m = m.EphemeralMessage.Message
		case m.ViewOnceMessage != nil:
			m = m.ViewOnceMessage.Message
		case m.ViewOnceMessageV2 != nil:
			m = m.ViewOnceMessageV2.Message
		case m.ViewOnceMessageV2Extension != nil:
			m = m.ViewOnceMessageV2Extension.Message
		case m.DocumentWithCaptionMessage != nil:
			m = m.DocumentWithCaptionMessage.Message
		case m.EditedMessage != nil:
			m = m.EditedMessage.Message
		default:
			return m
		}
	}
	return &waE2E.Message{}
}
func mediaSize(m *waE2E.Message) uint64 {
	switch {
	case m.ImageMessage != nil:
		return m.ImageMessage.GetFileLength()
	case m.VideoMessage != nil:
		return m.VideoMessage.GetFileLength()
	case m.AudioMessage != nil:
		return m.AudioMessage.GetFileLength()
	case m.DocumentMessage != nil:
		return m.DocumentMessage.GetFileLength()
	case m.StickerMessage != nil:
		return m.StickerMessage.GetFileLength()
	}
	return 0
}
func content(m *model.Message, body *waE2E.Message) map[string][]byte {
	m.Text = body.GetConversation()
	if body.ExtendedTextMessage != nil {
		m.Text = body.ExtendedTextMessage.GetText()
	}
	mime, name := "", ""
	switch {
	case body.ImageMessage != nil:
		mime = body.ImageMessage.GetMimetype()
		m.Text = body.ImageMessage.GetCaption()
		name = "image"
	case body.VideoMessage != nil:
		mime = body.VideoMessage.GetMimetype()
		m.Text = body.VideoMessage.GetCaption()
		name = "video"
	case body.AudioMessage != nil:
		mime = body.AudioMessage.GetMimetype()
		name = "audio"
	case body.DocumentMessage != nil:
		mime = body.DocumentMessage.GetMimetype()
		m.Text = body.DocumentMessage.GetCaption()
		name = body.DocumentMessage.GetFileName()
	case body.StickerMessage != nil:
		mime = body.StickerMessage.GetMimetype()
		name = "sticker"
	}
	m.Attachments = []model.Attachment{}
	if mime == "" {
		return nil
	}
	id := attachmentID(m.ConversationID, m.ID)
	size := mediaSize(body)
	m.Attachments = append(m.Attachments, model.Attachment{ID: id, Name: name, MIME: mime, Size: int64(size), Available: size <= provider.MaxAttachmentBytes})
	raw, _ := proto.Marshal(body)
	return map[string][]byte{id: raw}
}
func (p *Provider) learnSource(ctx context.Context, source types.MessageSource) error {
	for _, pair := range [][2]types.JID{{source.Sender, source.SenderAlt}, {source.Chat, source.RecipientAlt}} {
		a, b := pair[0].ToNonAD(), pair[1].ToNonAD()
		if a.Server == types.DefaultUserServer {
			a, b = b, a
		}
		if a.Server == types.HiddenUserServer && b.Server == types.DefaultUserServer {
			if err := p.Keys.PutLIDMapping(ctx, a, b); err != nil {
				return err
			}
		}
	}
	return nil
}
func (p *Provider) Message(ctx context.Context, e *events.Message) ([]provider.Snapshot, error) {
	if err := p.learnSource(ctx, e.Info.MessageSource); err != nil {
		return nil, err
	}
	chat, err := p.Canonical(ctx, e.Info.Chat)
	if err != nil {
		return nil, err
	}
	sender, err := p.Canonical(ctx, e.Info.Sender)
	if err != nil {
		return nil, err
	}
	if e.Info.PushName != "" {
		if _, _, err = p.Keys.PutPushName(ctx, sender, e.Info.PushName); err != nil {
			return nil, err
		}
	}
	body := unwrap(e.Message)
	targetID := e.Info.ID
	if pm := body.ProtocolMessage; pm != nil && (pm.GetType() == waE2E.ProtocolMessage_REVOKE || pm.GetType() == waE2E.ProtocolMessage_MESSAGE_EDIT) {
		targetID = pm.GetKey().GetID()
	}
	if body.ReactionMessage != nil {
		targetID = body.ReactionMessage.GetKey().GetID()
	}
	id := messageID(chat.String(), targetID)
	m := model.Message{Schema: 1, ID: id, ConversationID: chat.String(), SenderID: sender.String(), Time: e.Info.Timestamp.UTC(), Direction: "incoming", Status: "received", Attachments: []model.Attachment{}, Reactions: []model.Reaction{}}
	if e.Info.IsFromMe {
		m.Direction = "outgoing"
		m.Status = "server_ack"
		m.TransactionID = targetID
	}
	raw, loadErr := p.DB.Record("message", id)
	existing := loadErr == nil
	if loadErr != nil && !errors.Is(loadErr, local.ErrNotFound) {
		return nil, p.Keys.failed(loadErr)
	}
	old := m
	if existing {
		if err = json.Unmarshal(raw, &old); err != nil {
			return nil, err
		}
	}
	var private map[string][]byte
	switch {
	case body.ProtocolMessage != nil && body.ProtocolMessage.GetType() == waE2E.ProtocolMessage_REVOKE:
		if !existing {
			return nil, provider.ErrUnavailable
		}
		m = old
		m.Deleted = true
		m.Text = ""
		m.Attachments = []model.Attachment{}
	case body.ProtocolMessage != nil && body.ProtocolMessage.GetType() == waE2E.ProtocolMessage_MESSAGE_EDIT:
		if !existing {
			return nil, provider.ErrUnavailable
		}
		m = old
		private = content(&m, unwrap(body.ProtocolMessage.EditedMessage))
	case body.ReactionMessage != nil:
		if !existing {
			return nil, provider.ErrUnavailable
		}
		m = old
		emoji := body.ReactionMessage.GetText()
		reactions := []model.Reaction{}
		for _, r := range m.Reactions {
			people := []string{}
			for _, who := range r.Participants {
				if who != sender.String() {
					people = append(people, who)
				}
			}
			if len(people) > 0 {
				r.Participants = people
				reactions = append(reactions, r)
			}
		}
		if emoji != "" {
			found := false
			for i := range reactions {
				if reactions[i].Emoji == emoji {
					reactions[i].Participants = append(reactions[i].Participants, sender.String())
					found = true
				}
			}
			if !found {
				reactions = append(reactions, model.Reaction{Emoji: emoji, Participants: []string{sender.String()}})
			}
		}
		m.Reactions = reactions
	default:
		if existing {
			if e.Info.IsFromMe && old.TransactionID == "" {
				old.TransactionID = targetID
				snap, err := snapshot("message", id, old)
				return []provider.Snapshot{snap}, err
			}
			return nil, nil
		}
		private = content(&m, body)
	}
	if e.SourceWebMsg != nil && m.Direction == "outgoing" {
		switch e.SourceWebMsg.GetStatus() {
		case waWeb.WebMessageInfo_DELIVERY_ACK:
			m.Status = "delivered"
		case waWeb.WebMessageInfo_READ, waWeb.WebMessageInfo_PLAYED:
			m.Status = "read"
		}
	}
	conv, err := p.Conversation(ctx, chat, "")
	if err != nil {
		return nil, err
	}
	snap, err := snapshot("message", id, m)
	if err != nil {
		return nil, err
	}
	snap.Private = private
	return []provider.Snapshot{conv, snap}, nil
}
func statusRank(s string) int {
	switch s {
	case "server_ack":
		return 1
	case "delivered":
		return 2
	case "read":
		return 3
	}
	return 0
}
func ReceiptStatus(t types.ReceiptType) string {
	switch t {
	case types.ReceiptTypeDelivered:
		return "delivered"
	case types.ReceiptTypeRead, types.ReceiptTypeReadSelf, types.ReceiptTypePlayed:
		return "read"
	case types.ReceiptTypeSender:
		return "server_ack"
	}
	return ""
}
func (p *Provider) Receipt(ctx context.Context, e *events.Receipt) ([]provider.Snapshot, error) {
	status := ReceiptStatus(e.Type)
	if status == "" {
		return nil, nil
	}
	if err := p.learnSource(ctx, e.MessageSource); err != nil {
		return nil, err
	}
	chat, err := p.Canonical(ctx, e.Chat)
	if err != nil {
		return nil, err
	}
	out := []provider.Snapshot{}
	for _, remote := range e.MessageIDs {
		id := messageID(chat.String(), remote)
		raw, err := p.DB.Record("message", id)
		if errors.Is(err, local.ErrNotFound) {
			return nil, provider.ErrUnavailable
		}
		if err != nil {
			return nil, p.Keys.failed(err)
		}
		var m model.Message
		if err = json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		if statusRank(m.Status) >= statusRank(status) {
			continue
		}
		m.Status = status
		s, err := snapshot("message", id, m)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// Ingest stores an event before decoding it, retaining unresolved identities and
// out-of-order reactions/receipts for later processing.
func (p *Provider) Ingest(ctx context.Context, kind string, e any) error {
	_, err := p.ingest(ctx, kind, e)
	return err
}
func (p *Provider) ingest(ctx context.Context, kind string, e any) (id string, err error) {
	encoded, err := encodePending(kind, e)
	if err != nil {
		return "", err
	}
	err = p.Keys.txn(ctx, func(ctx context.Context) error {
		var sequence uint64
		if _, err := p.Keys.get(ctx, "meta", "inbox-sequence", &sequence); err != nil {
			return err
		}
		sequence++
		if err := p.Keys.put(ctx, "meta", "inbox-sequence", sequence); err != nil {
			return err
		}
		id = fmt.Sprintf("%020d", sequence)
		return p.Keys.put(ctx, "inbox", id, encoded)
	})
	return
}

func (p *Provider) Drain(ctx context.Context, persist func(provider.Snapshot) error) error {
	rows, err := p.Keys.scan(ctx, "inbox")
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, event, decodeErr := decodePending(rows[id])
		var snaps []provider.Snapshot
		err = decodeErr
		if err == nil {
			switch e := event.(type) {
			case *events.Message:
				snaps, err = p.Message(ctx, e)
			case *historyChat:
				snaps, err = p.HistoryChat(ctx, e)
			case *events.HistorySync:
				err = p.History(ctx, e)
			case *events.Receipt:
				snaps, err = p.Receipt(ctx, e)
			}
		}
		if errors.Is(err, provider.ErrUnavailable) {
			continue
		}
		if err != nil {
			var storage *storageError
			if errors.As(err, &storage) || ctx.Err() != nil {
				return err
			}
			if err = p.quarantine(ctx, id, rows[id]); err != nil {
				return err
			}
			continue
		}
		for _, snap := range snaps {
			if err = persist(snap); err != nil {
				return err
			}
		}
		if err = p.Keys.del(ctx, "inbox", id); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) MessagePage(ctx context.Context, conversation string, cursor []byte) ([]provider.Snapshot, []byte, error) {
	// On-demand history is asynchronous. The durable request marker suppresses
	// duplicate requests while the corresponding HistorySync response is pending.
	var requested historyRequest
	requestFound, err := p.Keys.get(ctx, "history-request", conversation, &requested)
	if err != nil {
		return nil, nil, err
	}
	var oldest historyBoundary
	if len(cursor) > 0 {
		if json.Unmarshal(cursor, &oldest) != nil {
			return nil, nil, provider.ErrInvalidCursor
		}
	} else if requestFound {
		oldest = requested.Oldest
	} else {
		rows, _, err := p.DB.ConversationMessages(conversation)
		if err != nil {
			return nil, nil, err
		}
		for _, raw := range rows {
			var m model.Message
			if err = json.Unmarshal(raw.Data, &m); err != nil {
				return nil, nil, err
			}
			if oldest.ID == "" || m.Time.Before(oldest.Timestamp) {
				chat, e := types.ParseJID(conversation)
				if e != nil {
					return nil, nil, e
				}
				oldest = historyBoundary{ID: messageRemoteID(m.ID), Timestamp: m.Time, Chat: chat, IsFromMe: m.Direction == "outgoing"}
			}
		}
	}
	if oldest.ID == "" {
		return nil, nil, provider.ErrUnsupportedCursor
	}
	var response historyResponse
	found, err := p.Keys.get(ctx, "history-response", conversation, &response)
	if err != nil {
		return nil, nil, err
	}
	if found && response.RequestID == oldest.ID {
		for _, id := range response.Pending {
			raw, err := p.Keys.DB.NetworkGet(ctx, p.Keys.key("inbox", id))
			if err != nil {
				return nil, nil, err
			}
			if raw != nil {
				return nil, nil, provider.ErrUnavailable
			}
		}

		if response.Oldest.ID == "" || !response.Oldest.Timestamp.Before(oldest.Timestamp) {
			return nil, nil, nil
		}
		next, err := json.Marshal(response.Oldest)
		return nil, next, err
	}
	if requestFound && requested.Oldest.ID == oldest.ID && time.Since(requested.Time) < 2*time.Minute {
		return nil, nil, provider.ErrUnavailable
	}
	requested = historyRequest{Oldest: oldest, Time: time.Now()}
	if err = p.Keys.put(ctx, "history-request", conversation, requested); err != nil {
		return nil, nil, err
	}
	msg := (&whatsmeowHistory{}).request(oldest.info())
	_, err = p.Client.SendMessage(ctx, p.Device.GetJID().ToNonAD(), msg, whatsmeowPeer())
	if err != nil {
		return nil, nil, provider.ErrUnavailable
	}
	return nil, nil, provider.ErrUnavailable
}

type historyRequest struct {
	Oldest historyBoundary
	Time   time.Time
}

type historyResponse struct {
	Pending   []string
	RequestID string
	Oldest    historyBoundary
}

// History boundaries need only the fields used by BuildHistorySyncRequest.
type historyBoundary struct {
	Chat      types.JID
	ID        string
	Timestamp time.Time
	IsFromMe  bool
}

func (h historyBoundary) info() *types.MessageInfo {
	return &types.MessageInfo{ID: h.ID, Timestamp: h.Timestamp, MessageSource: types.MessageSource{Chat: h.Chat, IsFromMe: h.IsFromMe}}
}
