package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type fakeClient struct {
	Client
	sendErr    error
	sends      int
	sentID     string
	users      []types.IsOnWhatsAppResponse
	uploadType whatsmeow.MediaType
	group      *types.GroupInfo
}

func (f *fakeClient) SendMessage(_ context.Context, _ types.JID, _ *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.sends++
	if len(extra) > 0 {
		f.sentID = extra[0].ID
	}
	return whatsmeow.SendResponse{Timestamp: time.Now()}, f.sendErr
}
func (f *fakeClient) IsOnWhatsApp(context.Context, []string) ([]types.IsOnWhatsAppResponse, error) {
	return f.users, nil
}
func (f *fakeClient) GetUserInfo(context.Context, []types.JID) (map[types.JID]types.UserInfo, error) {
	return nil, provider.ErrUnavailable
}
func providerFixture(t *testing.T) *Provider {
	t.Helper()
	keys, _ := credentialFixture(t)
	d, err := keys.Device(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d.LID = types.NewJID("999", types.HiddenUserServer)
	p := &Provider{Keys: keys, Device: d, DB: keys.DB}
	return p
}
func persistFixture(p *Provider) func(provider.Snapshot) error {
	return func(s provider.Snapshot) error { _, err := p.DB.Apply(s.Event, s.Private, nil); return err }
}
func readMessage(t *testing.T, p *Provider, id string) model.Message {
	t.Helper()
	raw, err := p.DB.Record("message", id)
	if err != nil {
		t.Fatal(err)
	}
	var m model.Message
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func TestCanonicalLIDAndPhoneNeverSplit(t *testing.T) {
	p := providerFixture(t)
	ctx := context.Background()
	pn := types.NewJID("14155550100", types.DefaultUserServer)
	lid := types.NewJID("123", types.HiddenUserServer)
	if _, err := p.Canonical(ctx, pn); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatal("unresolved PN published", err)
	}
	if err := p.Keys.PutLIDMapping(ctx, lid, pn); err != nil {
		t.Fatal(err)
	}
	if err := p.Keys.PutContactName(ctx, pn, "Alice", "Alice Example"); err != nil {
		t.Fatal(err)
	}
	pn.Device = 8
	a, err := p.Canonical(ctx, pn)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Canonical(ctx, lid)
	if err != nil || a != b {
		t.Fatal(a, b, err)
	}
	for _, j := range []types.JID{pn, lid} {
		person, err := p.participant(ctx, j)
		if err != nil || person.ID != lid.String() || person.Address != "+14155550100" || person.Name != "Alice Example" {
			t.Fatal(person, err)
		}
	}
}
func TestMessageEditsRevokesReactionsAndReceipts(t *testing.T) {
	p := providerFixture(t)
	ctx := context.Background()
	chat := types.NewJID("123", types.HiddenUserServer)
	event := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: chat}, ID: "A", Timestamp: time.Now()}, Message: &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{Conversation: proto.String("original")}}}}
	apply := func(e *events.Message) {
		t.Helper()
		snaps, err := p.Message(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range snaps {
			if err = persistFixture(p)(s); err != nil {
				t.Fatal(err)
			}
		}
	}
	apply(event)
	id := messageID(chat.String(), "A")
	if m := readMessage(t, p, id); m.Text != "original" || m.Status != "received" {
		t.Fatal(m)
	}
	event.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String("A")}, EditedMessage: &waE2E.Message{Conversation: proto.String("edited")}}}
	apply(event)
	event.Message = &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("A")}, Text: proto.String("👍")}}
	apply(event)
	apply(event)
	m := readMessage(t, p, id)
	if m.Text != "edited" || len(m.Reactions) != 1 || len(m.Reactions[0].Participants) != 1 {
		t.Fatal(m)
	}
	event.Message.ReactionMessage.Text = proto.String("")
	apply(event)
	if len(readMessage(t, p, id).Reactions) != 0 {
		t.Fatal("reaction was not removed")
	}
	for _, typ := range []types.ReceiptType{types.ReceiptTypeRead, types.ReceiptTypeDelivered} {
		snaps, err := p.Receipt(ctx, &events.Receipt{MessageSource: types.MessageSource{Chat: chat}, MessageIDs: []string{"A"}, Type: typ})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range snaps {
			if err = persistFixture(p)(s); err != nil {
				t.Fatal(err)
			}
		}
	}
	if readMessage(t, p, id).Status != "read" {
		t.Fatal("receipt regressed")
	}
	event.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("A")}}}
	apply(event)
	m = readMessage(t, p, id)
	if !m.Deleted || m.Text != "" {
		t.Fatal(m)
	}
}
func TestOutOfOrderReceiptSurvivesDrain(t *testing.T) {
	p := providerFixture(t)
	ctx := context.Background()
	chat := types.NewJID("123", types.HiddenUserServer)
	receipt := &events.Receipt{MessageSource: types.MessageSource{Chat: chat}, MessageIDs: []string{"later"}, Type: types.ReceiptTypeRead}
	if err := p.Ingest(ctx, "receipt", receipt); err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(ctx, persistFixture(p)); err != nil {
		t.Fatal(err)
	}
	pending, err := p.Keys.scan(ctx, "inbox")
	if err != nil || len(pending) != 1 {
		t.Fatal(len(pending), err)
	}
	message := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: chat}, ID: "later", Timestamp: time.Now()}, Message: &waE2E.Message{Conversation: proto.String("late arrival")}}
	if err = p.Ingest(ctx, "message", message); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = p.Drain(ctx, persistFixture(p)); err != nil {
			t.Fatal(err)
		}
	}
	if readMessage(t, p, messageID(chat.String(), "later")).Status != "read" {
		t.Fatal("receipt lost")
	}
}
func TestSingleSendIDAndOutcome(t *testing.T) {
	for _, tt := range []struct {
		name      string
		err, want error
	}{
		{"ack", nil, nil}, {"validation", &whatsmeow.IQError{Code: 403}, provider.ErrRejected},
		{"server refusal", whatsmeow.ErrServerReturnedError, provider.ErrRejected},
		{"timeout", context.DeadlineExceeded, provider.ErrAmbiguous}, {"canceled", context.Canceled, provider.ErrAmbiguous},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := providerFixture(t)
			f := &fakeClient{sendErr: tt.err}
			p.Client = f
			err := p.Send(context.Background(), provider.SendTarget{ConversationID: "123@lid"}, model.Outbox{TransactionID: "PREGENERATED", Request: model.SendRequest{Text: "synthetic"}})
			if !errors.Is(err, tt.want) || f.sends != 1 || f.sentID != "PREGENERATED" {
				t.Fatal(err, f.sends, f.sentID)
			}
		})
	}
}
func TestUnregisteredRecipientRejected(t *testing.T) {
	p := providerFixture(t)
	f := &fakeClient{users: []types.IsOnWhatsAppResponse{{IsIn: false}}}
	p.Client = f
	_, err := p.CreateConversation(context.Background(), []string{"+14155550100"}, "")
	if !errors.Is(err, provider.ErrRejected) || f.sends != 0 {
		t.Fatal(err)
	}
}

func (f *fakeClient) Upload(_ context.Context, _ []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.uploadType = kind
	return whatsmeow.UploadResponse{URL: "https://example.invalid/media", DirectPath: "/synthetic", MediaKey: []byte("private-media-key"), FileSHA256: []byte("hash"), FileEncSHA256: []byte("encrypted-hash")}, nil
}
func (f *fakeClient) DownloadAny(context.Context, *waE2E.Message) ([]byte, error) {
	return []byte("synthetic-media"), nil
}
func (f *fakeClient) GetGroupInfo(context.Context, types.JID) (*types.GroupInfo, error) {
	if f.group == nil {
		return nil, provider.ErrUnavailable
	}
	return f.group, nil
}

func TestMediaPreflightMetadataAndDownload(t *testing.T) {
	for _, tt := range []struct {
		mime string
		kind whatsmeow.MediaType
	}{
		{"image/jpeg", whatsmeow.MediaImage}, {"video/mp4", whatsmeow.MediaVideo}, {"audio/ogg", whatsmeow.MediaAudio}, {"application/pdf", whatsmeow.MediaDocument},
	} {
		t.Run(tt.mime, func(t *testing.T) {
			p := providerFixture(t)
			f := &fakeClient{}
			p.Client = f
			raw, err := p.Upload(context.Background(), []byte("synthetic-media"), "file.ext", tt.mime)
			if err != nil {
				t.Fatal(err)
			}
			if f.uploadType != tt.kind {
				t.Fatal("wrong media type", f.uploadType)
			}
			m := &waE2E.Message{}
			if err = proto.Unmarshal(raw, m); err != nil {
				t.Fatal(err)
			}
			chat := types.NewJID("123", types.HiddenUserServer)
			event := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: chat}, ID: "MEDIA", Timestamp: time.Now()}, Message: &waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: m}}}
			snaps, err := p.Message(context.Background(), event)
			if err != nil {
				t.Fatal(err)
			}
			snap := snaps[len(snaps)-1]
			var msg model.Message
			if err = json.Unmarshal(snap.Event.Data, &msg); err != nil {
				t.Fatal(err)
			}
			if len(msg.Attachments) != 1 || msg.Attachments[0].MIME != tt.mime || !msg.Attachments[0].Available {
				t.Fatal(msg)
			}
			if strings.Contains(string(snap.Event.Data), "private-media-key") || strings.Contains(string(snap.Event.Data), "example.invalid") {
				t.Fatal("media credentials exposed")
			}
			bytes, err := p.Attachment(context.Background(), snap.Private[msg.Attachments[0].ID])
			if err != nil || string(bytes) != "synthetic-media" {
				t.Fatal(err)
			}
		})
	}
}
func TestHistoryWaitsForDurablePageBeforeAdvancing(t *testing.T) {
	p := providerFixture(t)
	f := &fakeClient{}
	p.Client = f
	ctx := context.Background()
	chat := types.NewJID("123", types.HiddenUserServer)
	initial := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: chat}, ID: "new", Timestamp: time.Unix(200, 0)}, Message: &waE2E.Message{Conversation: proto.String("new")}}
	snaps, err := p.Message(ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range snaps {
		if err = persistFixture(p)(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err = p.MessagePage(ctx, chat.String(), nil); !errors.Is(err, provider.ErrUnavailable) || f.sends != 1 {
		t.Fatal(err, f.sends)
	}
	h := &events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_ON_DEMAND.Enum(), Conversations: []*waHistorySync.Conversation{{
		ID: proto.String(chat.String()), Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{ID: proto.String("old"), RemoteJID: proto.String(chat.String()), FromMe: proto.Bool(false)},
			MessageTimestamp: proto.Uint64(100), Message: &waE2E.Message{Conversation: proto.String("old")},
		}}},
	}}}}
	if err = p.Ingest(ctx, "history", h); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = p.Drain(ctx, persistFixture(p)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err = p.MessagePage(ctx, chat.String(), nil); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatal("advanced before persistence", err)
	}
	if err = p.Drain(ctx, persistFixture(p)); err != nil {
		t.Fatal(err)
	}
	_, next, err := p.MessagePage(ctx, chat.String(), nil)
	if err != nil || len(next) == 0 || f.sends != 1 {
		t.Fatal(string(next), err, f.sends)
	}
	if readMessage(t, p, messageID(chat.String(), "old")).Text != "old" {
		t.Fatal("history message absent")
	}
	var boundary types.MessageInfo
	if err = json.Unmarshal(next, &boundary); err != nil || boundary.ID != "old" {
		t.Fatal(boundary, err)
	}
}

func TestGroupSubjectParticipantsAndKnownAddresses(t *testing.T) {
	p := providerFixture(t)
	ctx := context.Background()
	lid := types.NewJID("123", types.HiddenUserServer)
	pn := types.NewJID("14155550100", types.DefaultUserServer)
	group := types.NewJID("12345-6789", types.GroupServer)
	if err := p.Keys.PutContactName(ctx, pn, "Alice", "Alice Example"); err != nil {
		t.Fatal(err)
	}
	p.Client = &fakeClient{group: &types.GroupInfo{JID: group, GroupName: types.GroupName{Name: "Book club"}, Participants: []types.GroupParticipant{{JID: lid, LID: lid, PhoneNumber: pn}}}}
	snap, err := p.Conversation(ctx, group, "old name")
	if err != nil {
		t.Fatal(err)
	}
	var c model.Conversation
	if err = json.Unmarshal(snap.Event.Data, &c); err != nil {
		t.Fatal(err)
	}
	if c.Name != "Book club" || len(c.Participants) != 1 || c.Participants[0].ID != lid.String() || c.Participants[0].Address != "+14155550100" || c.Participants[0].Name != "Alice Example" {
		t.Fatal(c)
	}
}
