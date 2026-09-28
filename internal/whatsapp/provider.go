package whatsapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
	local "github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wa "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type Client interface {
	GetUserInfo(context.Context, []types.JID) (map[types.JID]types.UserInfo, error)
	IsOnWhatsApp(context.Context, []string) ([]types.IsOnWhatsAppResponse, error)
	CreateGroup(context.Context, whatsmeow.ReqCreateGroup) (*types.GroupInfo, error)
	GetGroupInfo(context.Context, types.JID) (*types.GroupInfo, error)
	SendMessage(context.Context, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	Upload(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	DownloadAny(context.Context, *waE2E.Message) ([]byte, error)
	MarkRead(context.Context, []types.MessageID, time.Time, types.JID, types.JID, ...types.ReceiptType) error
	SendChatPresence(context.Context, types.JID, types.ChatPresence, types.ChatPresenceMedia) error
}

type Provider struct {
	Client Client
	Keys   *Credentials
	Device *wa.Device
	DB     *local.Store
}

var _ provider.Provider = (*Provider)(nil)

func normalizeJID(j types.JID) types.JID {
	j = j.ToNonAD()
	switch j.Server {
	case types.LegacyUserServer, types.HostedServer:
		j.Server = types.DefaultUserServer
	case types.HostedLIDServer:
		j.Server = types.HiddenUserServer
	}
	return j
}
func (p *Provider) Canonical(ctx context.Context, j types.JID) (types.JID, error) {
	j = normalizeJID(j)
	if j.Server != types.DefaultUserServer {
		return j, nil
	}
	lid, err := p.Keys.GetLIDForPN(ctx, j)
	if err != nil {
		return types.EmptyJID, err
	}
	if lid.IsEmpty() && p.Client != nil {
		users, e := p.Client.GetUserInfo(ctx, []types.JID{j})
		if e != nil {
			return types.EmptyJID, provider.ErrUnavailable
		}
		lid = users[j].LID
		if !lid.IsEmpty() {
			if err = p.Keys.PutLIDMapping(ctx, lid, j); err != nil {
				return types.EmptyJID, err
			}
		}
	}
	// A PN is never published as a second identity while its LID is unknown.
	if lid.IsEmpty() {
		return types.EmptyJID, provider.ErrUnavailable
	}
	return lid.ToNonAD(), nil
}
func (p *Provider) participant(ctx context.Context, j types.JID) (model.Participant, error) {
	id, err := p.Canonical(ctx, j)
	if err != nil {
		return model.Participant{}, err
	}
	pn := normalizeJID(j)
	if pn.Server == types.HiddenUserServer {
		pn, err = p.Keys.GetPNForLID(ctx, pn)
		if err != nil {
			return model.Participant{}, err
		}
	}
	c, err := p.Keys.GetContact(ctx, id)
	if err != nil {
		return model.Participant{}, err
	}
	if !pn.IsEmpty() {
		other, e := p.Keys.GetContact(ctx, pn)
		if e != nil {
			return model.Participant{}, e
		}
		if other.FullName != "" {
			c.FullName = other.FullName
		}
		if c.PushName == "" {
			c.PushName = other.PushName
		}
		if c.BusinessName == "" {
			c.BusinessName = other.BusinessName
		}
	}
	name := c.FullName
	if name == "" {
		name = c.BusinessName
	}
	if name == "" {
		name = c.PushName
	}
	if name == "" {
		name = c.FirstName
	}
	address := ""
	if pn.Server == types.DefaultUserServer {
		address = "+" + pn.User
	}
	return model.Participant{ID: id.String(), Name: name, Address: address, IsMe: id == p.Device.GetLID().ToNonAD() || j.ToNonAD() == p.Device.GetJID().ToNonAD()}, nil
}
func snapshot(kind, id string, value any) (provider.Snapshot, error) {
	data, err := json.Marshal(value)
	return provider.Snapshot{Event: local.Event{Type: kind, EntityID: id, Data: data}}, err
}
func (p *Provider) Conversation(ctx context.Context, j types.JID, name string) (provider.Snapshot, error) {
	j, err := p.Canonical(ctx, j)
	if err != nil {
		return provider.Snapshot{}, err
	}
	c := model.Conversation{Schema: 1, ID: j.String(), Name: name, Protocol: "whatsapp", State: "active", Participants: []model.Participant{}}
	if raw, e := p.DB.Record("conversation", c.ID); e == nil {
		if err = json.Unmarshal(raw, &c); err != nil {
			return provider.Snapshot{}, err
		}
		if name != "" {
			c.Name = name
		}
	} else if !errors.Is(e, local.ErrNotFound) {
		return provider.Snapshot{}, p.Keys.failed(e)
	}
	settings, err := p.Keys.GetChatSettings(ctx, j)
	if err != nil {
		return provider.Snapshot{}, err
	}
	c.Archived, c.Pinned, c.MutedUntil = settings.Archived, settings.Pinned, settings.MutedUntil
	if j.Server == types.GroupServer {
		if p.Client != nil && (len(c.Participants) == 0 || name != "") {
			g, e := p.Client.GetGroupInfo(ctx, j)
			if e == nil {
				c.Name = g.Name
				c.Participants = nil
				for _, member := range g.Participants {
					if !member.LID.IsEmpty() && !member.PhoneNumber.IsEmpty() {
						if err = p.Keys.PutLIDMapping(ctx, member.LID, member.PhoneNumber); err != nil {
							return provider.Snapshot{}, err
						}
					}
					memberJID := member.JID
					if !member.LID.IsEmpty() {
						memberJID = member.LID
					}
					person, e := p.participant(ctx, memberJID)
					if e != nil {
						return provider.Snapshot{}, e
					}
					c.Participants = append(c.Participants, person)
				}
			}
		}
	} else {
		person, e := p.participant(ctx, j)
		if e != nil {
			return provider.Snapshot{}, e
		}
		c.Participants = []model.Participant{person}
		if person.Name != "" {
			c.Name = person.Name
		} else if c.Name == "" {
			c.Name = person.Address
		}
	}
	return snapshot("conversation", c.ID, c)
}
func (p *Provider) Contacts(ctx context.Context) ([]model.Contact, error) {
	contacts, err := p.Keys.GetAllContacts(ctx)
	if err != nil {
		return nil, err
	}
	out := []model.Contact{}
	seen := map[string]bool{}
	for j, c := range contacts {
		if c.FullName == "" && c.FirstName == "" {
			continue
		}
		person, e := p.participant(ctx, j)
		if errors.Is(e, provider.ErrUnavailable) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if seen[person.ID] {
			continue
		}
		seen[person.ID] = true
		out = append(out, model.Contact{ID: person.ID, Name: person.Name, Address: person.Address})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}
func (p *Provider) Conversations(ctx context.Context) ([]provider.Snapshot, error) {
	rows, err := p.DB.Latest("conversation")
	if err != nil {
		return nil, err
	}
	out := []provider.Snapshot{}
	for _, raw := range rows {
		var c model.Conversation
		if err = json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		j, e := types.ParseJID(c.ID)
		if e != nil {
			return nil, e
		}
		s, e := p.Conversation(ctx, j, c.Name)
		if e != nil {
			return nil, e
		}
		out = append(out, s)
	}
	return out, nil
}
func (p *Provider) Messages(context.Context, string) ([]provider.Snapshot, error) { return nil, nil }
func (p *Provider) ConversationPage(ctx context.Context, folder string, cursor []byte) ([]provider.Snapshot, []byte, error) {
	if len(cursor) > 0 {
		return nil, nil, provider.ErrUnsupportedCursor
	}
	all, err := p.Conversations(ctx)
	if err != nil {
		return nil, nil, err
	}
	out := []provider.Snapshot{}
	for _, s := range all {
		var c model.Conversation
		if err = json.Unmarshal(s.Event.Data, &c); err != nil {
			return nil, nil, err
		}
		if (folder == "archive" && c.Archived) || (folder == "inbox" && !c.Archived) {
			out = append(out, s)
		}
	}
	return out, nil, nil
}
func (p *Provider) Prepare(ctx context.Context, id string) (provider.SendTarget, error) {
	j, err := types.ParseJID(id)
	if err != nil || j.IsEmpty() {
		return provider.SendTarget{}, provider.ErrRejected
	}
	canonical, err := p.Canonical(ctx, j)
	if err != nil {
		return provider.SendTarget{}, err
	}
	return provider.SendTarget{ConversationID: canonical.String()}, nil
}
func Classify(err error) error {
	if err == nil {
		return nil
	}
	// ErrNotConnected is pre-write with our fresh-client lifecycle: a used
	// client never reconnects, so retryFrame cannot reach its socket check.
	for _, refusal := range []error{
		whatsmeow.ErrClientIsNil, whatsmeow.ErrNotConnected, whatsmeow.ErrNotLoggedIn,
		whatsmeow.ErrRecipientADJID, whatsmeow.ErrUnknownServer,
		whatsmeow.ErrInvalidInlineBotID, whatsmeow.ErrBroadcastListUnsupported,
		whatsmeow.ErrServerReturnedError,
	} {
		if errors.Is(err, refusal) {
			return provider.ErrRejected
		}
	}
	var iq *whatsmeow.IQError
	if errors.As(err, &iq) && iq.Code >= 400 && iq.Code < 500 && iq.Code != 408 {
		return provider.ErrRejected
	}
	return provider.ErrAmbiguous
}
func (p *Provider) CreateConversation(ctx context.Context, numbers []string, name string) (provider.Snapshot, error) {
	users, err := p.Client.IsOnWhatsApp(ctx, numbers)
	if err != nil {
		return provider.Snapshot{}, Classify(err)
	}
	if len(users) != len(numbers) {
		return provider.Snapshot{}, provider.ErrRejected
	}
	jids := make([]types.JID, 0, len(users))
	for _, u := range users {
		if !u.IsIn {
			return provider.Snapshot{}, provider.ErrRecipientUnavailable
		}
		if u.JID.Server == types.HiddenUserServer && !u.PhoneNumber.IsEmpty() {
			if err = p.Keys.PutLIDMapping(ctx, u.JID, u.PhoneNumber); err != nil {
				return provider.Snapshot{}, err
			}
		}
		j, e := p.Canonical(ctx, u.JID)
		if e != nil {
			return provider.Snapshot{}, e
		}
		jids = append(jids, j)
	}
	if len(jids) == 1 {
		return p.Conversation(ctx, jids[0], "")
	}
	if name == "" {
		name = "New group"
	}
	g, err := p.Client.CreateGroup(ctx, whatsmeow.ReqCreateGroup{Name: name, Participants: jids})
	if err != nil {
		return provider.Snapshot{}, Classify(err)
	}
	// The group exists even if a subsequent metadata refresh is unavailable.
	c := model.Conversation{Schema: 1, ID: g.JID.String(), Name: g.Name, Protocol: "whatsapp", State: "active", Participants: []model.Participant{}}
	for _, j := range jids {
		person, e := p.participant(ctx, j)
		if e != nil {
			return provider.Snapshot{}, e
		}
		c.Participants = append(c.Participants, person)
	}
	return snapshot("conversation", c.ID, c)
}

func (p *Provider) Upload(ctx context.Context, data []byte, name, mime string) ([]byte, error) {
	kind := whatsmeow.MediaDocument
	switch {
	case strings.HasPrefix(mime, "image/"):
		kind = whatsmeow.MediaImage
	case strings.HasPrefix(mime, "video/"):
		kind = whatsmeow.MediaVideo
	case strings.HasPrefix(mime, "audio/"):
		kind = whatsmeow.MediaAudio
	}
	u, err := p.Client.Upload(ctx, data, kind)
	if err != nil {
		return nil, err
	}
	m := &waE2E.Message{}
	switch kind {
	case whatsmeow.MediaImage:
		m.ImageMessage = &waE2E.ImageMessage{URL: &u.URL, DirectPath: &u.DirectPath, MediaKey: u.MediaKey, FileSHA256: u.FileSHA256, FileEncSHA256: u.FileEncSHA256, FileLength: proto.Uint64(uint64(len(data))), Mimetype: &mime}
	case whatsmeow.MediaVideo:
		m.VideoMessage = &waE2E.VideoMessage{URL: &u.URL, DirectPath: &u.DirectPath, MediaKey: u.MediaKey, FileSHA256: u.FileSHA256, FileEncSHA256: u.FileEncSHA256, FileLength: proto.Uint64(uint64(len(data))), Mimetype: &mime}
	case whatsmeow.MediaAudio:
		m.AudioMessage = &waE2E.AudioMessage{URL: &u.URL, DirectPath: &u.DirectPath, MediaKey: u.MediaKey, FileSHA256: u.FileSHA256, FileEncSHA256: u.FileEncSHA256, FileLength: proto.Uint64(uint64(len(data))), Mimetype: &mime}
	default:
		m.DocumentMessage = &waE2E.DocumentMessage{URL: &u.URL, DirectPath: &u.DirectPath, MediaKey: u.MediaKey, FileSHA256: u.FileSHA256, FileEncSHA256: u.FileEncSHA256, FileLength: proto.Uint64(uint64(len(data))), Mimetype: &mime, FileName: &name}
	}
	return proto.Marshal(m)
}
func (p *Provider) Send(ctx context.Context, t provider.SendTarget, o model.Outbox) error {
	j, err := types.ParseJID(t.ConversationID)
	if err != nil {
		return provider.ErrRejected
	}
	m := &waE2E.Message{Conversation: proto.String(o.Request.Text)}
	// One outbox attempt is one WhatsApp stanza. Albums must be queued separately.
	if len(t.Media) > 1 {
		return provider.ErrRejected
	}
	if len(t.Media) == 1 {
		m = &waE2E.Message{}
		if proto.Unmarshal(t.Media[0], m) != nil {
			return provider.ErrRejected
		}
	}
	mark, err := p.DB.HistoryWatermark()
	if err != nil {
		return err
	}
	response, err := p.Client.SendMessage(ctx, j, m, whatsmeow.SendRequestExtra{ID: o.TransactionID})
	if err != nil {
		return Classify(err)
	}
	message := model.Message{Schema: 1, ID: messageID(j.String(), o.TransactionID), ConversationID: j.String(), SenderID: p.Device.GetLID().ToNonAD().String(), Time: response.Timestamp.UTC(), Direction: "outgoing", Status: "server_ack", Reactions: []model.Reaction{}}
	private := content(&message, m)
	snap, err := snapshot("message", message.ID, message)
	if err != nil {
		return err
	}
	_, err = p.DB.Apply(snap.Event, private, &mark)
	return p.Keys.failed(err)
}
func (p *Provider) React(ctx context.Context, t provider.SendTarget, id, emoji string, remove bool) error {
	raw, err := p.DB.Record("message", id)
	if err != nil {
		return provider.ErrRejected
	}
	var m model.Message
	if json.Unmarshal(raw, &m) != nil {
		return provider.ErrRejected
	}
	j, err := types.ParseJID(t.ConversationID)
	if err != nil {
		return provider.ErrRejected
	}
	if remove {
		emoji = ""
	}
	key := &waCommon.MessageKey{RemoteJID: proto.String(j.String()), FromMe: proto.Bool(m.Direction == "outgoing"), ID: proto.String(messageRemoteID(id))}
	if j.Server == types.GroupServer {
		key.Participant = proto.String(m.SenderID)
	}
	_, err = p.Client.SendMessage(ctx, j, &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: key, Text: proto.String(emoji), SenderTimestampMS: proto.Int64(time.Now().UnixMilli())}}, whatsmeow.SendRequestExtra{ID: t.TransactionID})
	return Classify(err)
}
func (p *Provider) Typing(ctx context.Context, t provider.SendTarget) error {
	j, err := types.ParseJID(t.ConversationID)
	if err != nil {
		return err
	}
	return p.Client.SendChatPresence(ctx, j, types.ChatPresenceComposing, types.ChatPresenceMediaText)
}
func (p *Provider) MarkRead(ctx context.Context, c, id string) error {
	raw, err := p.DB.Record("message", id)
	if err != nil {
		return err
	}
	var m model.Message
	if err = json.Unmarshal(raw, &m); err != nil {
		return err
	}
	chat, err := types.ParseJID(c)
	if err != nil {
		return err
	}
	sender, err := types.ParseJID(m.SenderID)
	if err != nil {
		return err
	}
	return p.Client.MarkRead(ctx, []types.MessageID{messageRemoteID(id)}, m.Time, chat, sender)
}
func (p *Provider) Attachment(ctx context.Context, raw []byte) ([]byte, error) {
	m := &waE2E.Message{}
	if proto.Unmarshal(raw, m) != nil {
		return nil, provider.ErrRejected
	}
	size := mediaSize(m)
	if size > provider.MaxAttachmentBytes {
		return nil, provider.ErrTooLarge
	}
	data, err := p.Client.DownloadAny(ctx, m)
	if len(data) > provider.MaxAttachmentBytes {
		return nil, provider.ErrTooLarge
	}
	return data, err
}
func (p *Provider) RequestMedia(context.Context, []byte) error { return provider.ErrRejected }
func messageID(chat, id string) string                         { return chat + "/" + id }
func messageRemoteID(id string) string                         { _, remote, _ := strings.Cut(id, "/"); return remote }
func attachmentID(chat, id string) string {
	h := sha256.Sum256([]byte("whatsapp:" + chat + ":" + id))
	return hex.EncodeToString(h[:])
}
