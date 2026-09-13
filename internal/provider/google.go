package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
var ErrInvalidCursor = errors.New("invalid provider cursor")
var ErrInvalidFolder = errors.New("invalid conversation folder")

type googleClient interface {
	ListConversations(context.Context, *gmproto.ListConversationsRequest) (*gmproto.ListConversationsResponse, error)
	FetchMessages(context.Context, string, int64, *gmproto.Cursor) (*gmproto.ListMessagesResponse, error)
	GetOrCreateConversation(context.Context, *gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error)
	GetConversation(context.Context, string) (*gmproto.Conversation, error)
	SendMessage(context.Context, *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error)
	SendReaction(context.Context, *gmproto.SendReactionRequest) (*gmproto.SendReactionResponse, error)
	SetTyping(context.Context, string, *gmproto.SIMPayload) error
	MarkRead(context.Context, string, string) error
}

type contextMediaUploader interface {
	UploadMediaContext(context.Context, []byte, string, string) (*gmproto.MediaContent, error)
}

type contextMediaDownloader interface {
	DownloadMediaContext(context.Context, string, []byte) (io.ReadCloser, error)
}

type Google struct {
	Client    googleClient
	listMu    sync.Mutex
	mediaSlot chan struct{}
	mediaOnce sync.Once
}

var (
	_ Provider               = (*Google)(nil)
	_ googleClient           = (*libgm.Client)(nil)
	_ contextMediaUploader   = (*libgm.Client)(nil)
	_ contextMediaDownloader = (*libgm.Client)(nil)
)

func NewGoogle(client *libgm.Client) *Google {
	return newGoogle(client)
}

func newGoogle(client googleClient) *Google {
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
				remoteID, _ := remoteMedia(media)
				available := remoteID != "" || len(media.GetMediaData()) > 0
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
	out, _, err := g.ConversationPage(ctx, "inbox", nil)
	return out, err
}

func (g *Google) Messages(ctx context.Context, id string) ([]Snapshot, error) {
	out, _, err := g.MessagePage(ctx, id, nil)
	return out, err
}

type pageCursor struct {
	Version  int    `json:"version"`
	Scope    string `json:"scope"`
	Key      string `json:"key"`
	Position []byte `json:"position"`
}

func encodeCursor(scope, key string, cursor *gmproto.Cursor) ([]byte, error) {
	if cursor == nil {
		return nil, nil
	}
	if cursor.GetLastItemID() == "" || cursor.GetLastItemTimestamp() <= 0 {
		return nil, ErrUnavailable
	}
	position, err := proto.Marshal(cursor)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pageCursor{Version: 1, Scope: scope, Key: key, Position: position})
}

func decodeCursor(data []byte, scope, key string) (*gmproto.Cursor, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var token pageCursor
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&token); err != nil {
		return nil, ErrInvalidCursor
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidCursor
	}
	if token.Version != 1 || token.Scope != scope || token.Key != key || len(token.Position) == 0 {
		return nil, ErrInvalidCursor
	}
	var cursor gmproto.Cursor
	if err := proto.Unmarshal(token.Position, &cursor); err != nil || cursor.GetLastItemID() == "" || cursor.GetLastItemTimestamp() <= 0 {
		return nil, ErrInvalidCursor
	}
	return &cursor, nil
}

func snapshotsOf[T proto.Message](messages []T) ([]Snapshot, error) {
	out := make([]Snapshot, 0, len(messages))
	for _, message := range messages {
		s, err := SnapshotOf(message)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (g *Google) MessagePage(ctx context.Context, conversationID string, cursorData []byte) ([]Snapshot, []byte, error) {
	cursor, err := decodeCursor(cursorData, "messages", conversationID)
	if err != nil {
		return nil, nil, err
	}
	res, err := g.Client.FetchMessages(ctx, conversationID, RecentMessages, cursor)
	if err != nil || res == nil {
		return nil, nil, ErrUnavailable
	}
	out, err := snapshotsOf(res.GetMessages())
	if err != nil {
		return nil, nil, err
	}
	next, err := encodeCursor("messages", conversationID, res.GetCursor())
	if err != nil {
		return nil, nil, err
	}
	return out, next, nil
}

func conversationFolder(folder string) (gmproto.ListConversationsRequest_Folder, error) {
	switch folder {
	case "inbox":
		return gmproto.ListConversationsRequest_INBOX, nil
	case "archive":
		return gmproto.ListConversationsRequest_ARCHIVE, nil
	case "spam":
		return gmproto.ListConversationsRequest_SPAM_BLOCKED, nil
	default:
		return gmproto.ListConversationsRequest_UNKNOWN, ErrInvalidFolder
	}
}

func (g *Google) ConversationPage(ctx context.Context, folder string, cursorData []byte) ([]Snapshot, []byte, error) {
	protocolFolder, err := conversationFolder(folder)
	if err != nil {
		return nil, nil, err
	}
	cursor, err := decodeCursor(cursorData, "conversations", folder)
	if err != nil {
		return nil, nil, err
	}
	// libgm's first-list flag is not synchronized.
	g.listMu.Lock()
	defer g.listMu.Unlock()
	res, err := g.Client.ListConversations(ctx, &gmproto.ListConversationsRequest{Count: RecentConversations, Folder: protocolFolder, Cursor: cursor})
	if err != nil || res == nil {
		return nil, nil, ErrUnavailable
	}
	if res.GetCursor() == nil && len(res.GetCursorBytes()) > 0 {
		return nil, nil, ErrUnsupportedCursor
	}
	out, err := snapshotsOf(res.GetConversations())
	if err != nil {
		return nil, nil, err
	}
	next, err := encodeCursor("conversations", folder, res.GetCursor())
	if err != nil {
		return nil, nil, err
	}
	return out, next, nil
}

func (g *Google) CreateConversation(ctx context.Context, recipients []string) (Snapshot, error) {
	if len(recipients) == 0 {
		return Snapshot{}, ErrRejected
	}
	numbers := make([]*gmproto.ContactNumber, len(recipients))
	for i, recipient := range recipients {
		if recipient == "" {
			return Snapshot{}, ErrRejected
		}
		numbers[i] = &gmproto.ContactNumber{MysteriousInt: 2, Number: recipient, Number2: recipient}
	}
	req := &gmproto.GetOrCreateConversationRequest{Numbers: numbers}
	res, err := g.Client.GetOrCreateConversation(ctx, req)
	if err != nil {
		return Snapshot{}, ErrAmbiguous
	}
	if res.GetStatus() == gmproto.GetOrCreateConversationResponse_CREATE_RCS {
		name, create := "", true
		req.RCSGroupName, req.CreateRCSGroup = &name, &create
		res, err = g.Client.GetOrCreateConversation(ctx, req)
		if err != nil {
			return Snapshot{}, ErrAmbiguous
		}
	}
	if res.GetStatus() != gmproto.GetOrCreateConversationResponse_SUCCESS || res.GetConversation().GetConversationID() == "" {
		return Snapshot{}, ErrAmbiguous
	}
	snapshot, err := SnapshotOf(res.GetConversation())
	if err != nil {
		return Snapshot{}, ErrAmbiguous
	}
	return snapshot, nil
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
	// Media entries lead and a text entry follows only when nonblank, as
	// upstream sends; a leading empty text entry drops the media on the phone.
	messageInfo := make([]*gmproto.MessageInfo, 0, len(target.Media)+1)
	var mediaBytes int64
	for _, data := range target.Media {
		var media gmproto.MediaContent
		if err := unmarshalPrivate(data, &media); err != nil || media.GetMediaID() == "" || len(media.GetDecryptionKey()) != 32 || media.GetSize() < 0 || media.GetSize() > MaxAttachmentBytes-mediaBytes {
			return ErrRejected
		}
		mediaBytes += media.GetSize()
		messageInfo = append(messageInfo, &gmproto.MessageInfo{Data: &gmproto.MessageInfo_MediaContent{MediaContent: &media}})
	}
	if strings.TrimSpace(o.Request.Text) != "" || len(messageInfo) == 0 {
		messageInfo = append(messageInfo, &gmproto.MessageInfo{Data: &gmproto.MessageInfo_MessageContent{MessageContent: &gmproto.MessageContent{Content: o.Request.Text}}})
	}
	req := &gmproto.SendMessageRequest{ConversationID: o.Request.ConversationID, TmpID: o.TransactionID, SIMPayload: target.sim, MessagePayload: &gmproto.MessagePayload{TmpID: o.TransactionID, TmpID2: o.TransactionID, ConversationID: o.Request.ConversationID, ParticipantID: target.participantID, MessageInfo: messageInfo}}
	res, err := g.Client.SendMessage(ctx, req)
	if err != nil {
		// Transport errors, relay 4xx/5xx, timeouts and cancellation all arrive
		// here. None proves the phone did not act, so the outcome stays unknown.
		return ErrAmbiguous
	}
	return SendOutcome(res.GetStatus())
}

func (g *Google) React(ctx context.Context, target SendTarget, messageID, emoji string, remove bool) error {
	if target.ConversationID == "" || target.participantID == "" || target.sim == nil || messageID == "" || emoji == "" {
		return ErrRejected
	}
	action := gmproto.SendReactionRequest_ADD
	var sim *gmproto.SIMPayload
	if remove {
		action = gmproto.SendReactionRequest_REMOVE
	} else {
		sim = target.sim
	}
	res, err := g.Client.SendReaction(ctx, &gmproto.SendReactionRequest{
		MessageID:    messageID,
		ReactionData: gmproto.MakeReactionData(emoji),
		Action:       action,
		SIMPayload:   sim,
	})
	if err != nil || res == nil {
		return ErrAmbiguous
	}
	if !res.GetSuccess() {
		return ErrRejected
	}
	return nil
}

func (g *Google) Typing(ctx context.Context, target SendTarget) error {
	if target.ConversationID == "" || target.participantID == "" || target.sim == nil {
		return ErrRejected
	}
	if err := g.Client.SetTyping(ctx, target.ConversationID, target.sim); err != nil {
		return ErrAmbiguous
	}
	return nil
}

func (g *Google) Upload(ctx context.Context, data []byte, name, mime string) ([]byte, error) {
	if len(data) > MaxAttachmentBytes {
		return nil, ErrTooLarge
	}
	uploader, ok := g.Client.(contextMediaUploader)
	if !ok {
		return nil, ErrUnavailable
	}
	media, err := uploader.UploadMediaContext(ctx, data, name, mime)
	if err != nil || media == nil || media.GetMediaID() == "" || len(media.GetDecryptionKey()) != 32 || media.GetSize() < 0 || media.GetSize() > MaxAttachmentBytes {
		return nil, ErrUnavailable
	}
	private, err := marshalPrivate(media)
	if err != nil {
		return nil, ErrUnavailable
	}
	return private, nil
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
	mediaID, key := remoteMedia(&media)
	if mediaID == "" {
		if inline := media.GetMediaData(); len(inline) > 0 {
			if len(inline) > MaxAttachmentBytes {
				return nil, ErrTooLarge
			}
			return inline, nil
		}
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
	downloader, ok := g.Client.(contextMediaDownloader)
	if !ok {
		return nil, ErrUnavailable
	}
	r, err := downloader.DownloadMediaContext(ctx, mediaID, key)
	if err != nil {
		return nil, fmt.Errorf("%w: download: %v", ErrUnavailable, err)
	}
	return ReadBounded(ctx, r)
}

// remoteMedia prefers the full upload over its thumbnail, matching upstream.
// Inline mediaData is only a preview, so it is the last resort.
func remoteMedia(media *gmproto.MediaContent) (string, []byte) {
	if media.GetMediaID() != "" && len(media.GetDecryptionKey()) == 32 {
		return media.GetMediaID(), media.GetDecryptionKey()
	}
	if media.GetThumbnailMediaID() != "" && len(media.GetThumbnailDecryptionKey()) == 32 {
		return media.GetThumbnailMediaID(), media.GetThumbnailDecryptionKey()
	}
	return "", nil
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
		return nil, fmt.Errorf("%w: read: %v", ErrUnavailable, err)
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
