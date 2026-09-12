package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

func TestMessageSnapshotKeepsMediaKeysPrivate(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	msg := &gmproto.Message{
		MessageID: "m1", ConversationID: "c1", ParticipantID: "p2", TmpID: "tmp-1", Timestamp: 1_700_000_000_000_000,
		MessageStatus: &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
		MessageInfo: []*gmproto.MessageInfo{
			{Data: &gmproto.MessageInfo_MessageContent{MessageContent: &gmproto.MessageContent{Content: "hello "}}},
			{Data: &gmproto.MessageInfo_MediaContent{MediaContent: &gmproto.MediaContent{MediaID: "media-1", MediaName: "photo.jpg", MimeType: "image/jpeg", Size: 1234, DecryptionKey: key}}},
			{Data: &gmproto.MessageInfo_MessageContent{MessageContent: &gmproto.MessageContent{Content: "world"}}},
		},
		Reactions: []*gmproto.ReactionEntry{{Data: &gmproto.ReactionData{Unicode: "👍"}, ParticipantIDs: []string{"p2", "p3"}}},
	}
	snap, err := SnapshotOf(msg)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Event.Type != "message" || snap.Event.EntityID != "m1" {
		t.Fatalf("%+v", snap.Event)
	}
	var out model.Message
	if err := json.Unmarshal(snap.Event.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Text != "hello world" || out.Direction != "incoming" || out.Status != "incoming_complete" || out.TransactionID != "tmp-1" || out.Time.Unix() != 1_700_000_000 {
		t.Fatalf("%+v", out)
	}
	if len(out.Attachments) != 1 || !out.Attachments[0].Available || out.Attachments[0].MIME != "image/jpeg" || len(out.Attachments[0].ID) != 64 {
		t.Fatalf("%+v", out.Attachments)
	}
	if len(out.Reactions) != 1 || out.Reactions[0].Emoji != "👍" || len(out.Reactions[0].Participants) != 2 {
		t.Fatalf("%+v", out.Reactions)
	}
	if strings.Contains(string(snap.Event.Data), "media-1") || bytes.Contains(snap.Event.Data, []byte("BwcHBwcH")) {
		t.Fatal("public record leaks media credentials")
	}
	private, ok := snap.Private[out.Attachments[0].ID]
	if !ok {
		t.Fatal("private metadata missing")
	}
	var media gmproto.MediaContent
	if err := unmarshalPrivate(private, &media); err != nil || !bytes.Equal(media.GetDecryptionKey(), key) {
		t.Fatalf("private metadata unusable: %v", err)
	}
}

func TestDeletedAndOutgoingStatusesMap(t *testing.T) {
	for status, want := range map[gmproto.MessageStatusType][2]string{
		gmproto.MessageStatusType_OUTGOING_DISPLAYED: {"outgoing", "false"},
		gmproto.MessageStatusType_MESSAGE_DELETED:    {"system", "true"},
	} {
		snap, err := SnapshotOf(&gmproto.Message{MessageID: "m", MessageStatus: &gmproto.MessageStatus{Status: status}})
		if err != nil {
			t.Fatal(err)
		}
		var out model.Message
		_ = json.Unmarshal(snap.Event.Data, &out)
		if out.Direction != want[0] || (out.Deleted && want[1] != "true") || (!out.Deleted && want[1] == "true") {
			t.Fatalf("%v: %+v", status, out)
		}
	}
}

func TestConversationAndTypingSnapshots(t *testing.T) {
	conv := &gmproto.Conversation{ConversationID: "c1", Name: "Group", ReadOnly: true, Unread: true, Type: gmproto.ConversationType_RCS, Status: gmproto.ConversationStatus_ACTIVE,
		LatestMessage: &gmproto.LatestMessage{DisplayContent: "preview"},
		Participants:  []*gmproto.Participant{{ID: &gmproto.SmallInfo{ParticipantID: "p1", Number: "+15550001"}, FirstName: "A", IsMe: true}, {ID: &gmproto.SmallInfo{ParticipantID: "p2"}, FullName: "Bee"}}}
	snap, err := SnapshotOf(conv)
	if err != nil {
		t.Fatal(err)
	}
	var out model.Conversation
	if err := json.Unmarshal(snap.Event.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "c1" || !out.ReadOnly || out.Protocol != "rcs" || out.State != "active" || out.Preview != "preview" || len(out.Participants) != 2 || out.Participants[0].Name != "A" || !out.Participants[0].IsMe || out.Participants[1].Name != "Bee" {
		t.Fatalf("%+v", out)
	}
	typing, err := SnapshotOf(&gmproto.TypingData{ConversationID: "c1", User: &gmproto.User{Number: "+15550002"}, Type: gmproto.TypingTypes_STARTED_TYPING})
	if err != nil {
		t.Fatal(err)
	}
	var tp model.Typing
	_ = json.Unmarshal(typing.Event.Data, &tp)
	if typing.Event.Type != "typing" || !tp.Active || tp.ParticipantID != "+15550002" {
		t.Fatalf("%+v", tp)
	}
	if _, err := SnapshotOf(&gmproto.EmptyArr{}); err == nil {
		t.Fatal("unsupported type accepted")
	}
}

func TestSendOutcomeIsConservative(t *testing.T) {
	if SendOutcome(gmproto.SendMessageResponse_SUCCESS) != nil {
		t.Fatal("success")
	}
	if !errors.Is(SendOutcome(gmproto.SendMessageResponse_FAILURE_3), ErrRejected) {
		t.Fatal("explicit failure should reject")
	}
	if !errors.Is(SendOutcome(gmproto.SendMessageResponse_Status(99)), ErrAmbiguous) {
		t.Fatal("unknown status must stay ambiguous")
	}
	g := &Google{}
	if err := g.Send(context.Background(), SendTarget{}, model.Outbox{Request: model.SendRequest{ConversationID: "c1"}}); !errors.Is(err, ErrRejected) {
		t.Fatalf("unprepared target must not reach the network: %v", err)
	}
}

type slowBody struct {
	closed chan struct{}
	once   func()
}

func (s *slowBody) Read(p []byte) (int, error) {
	<-s.closed
	return 0, errors.New("closed")
}
func (s *slowBody) Close() error { s.once(); return nil }

func TestReadBoundedLimitsAndCancels(t *testing.T) {
	big := io.NopCloser(bytes.NewReader(make([]byte, MaxAttachmentBytes+1)))
	if _, err := ReadBounded(context.Background(), big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized: %v", err)
	}
	small := io.NopCloser(strings.NewReader("data"))
	if out, err := ReadBounded(context.Background(), small); err != nil || string(out) != "data" {
		t.Fatalf("%q %v", out, err)
	}
	closed := make(chan struct{})
	var closeCalls atomic.Int32
	body := &slowBody{closed: closed, once: func() {
		if closeCalls.Add(1) == 1 {
			close(closed)
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := ReadBounded(ctx, body); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel: %v", err)
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("body closed %d times", got)
	}
}

func TestAttachmentRejectsDeclaredOversizeWithoutNetwork(t *testing.T) {
	g := &Google{}
	data, _ := marshalPrivate(&gmproto.MediaContent{MediaID: "x", Size: MaxAttachmentBytes + 1, DecryptionKey: bytes.Repeat([]byte{1}, 32)})
	if _, err := g.Attachment(context.Background(), data); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	inline, _ := marshalPrivate(&gmproto.MediaContent{MediaData: []byte("inline")})
	if out, err := g.Attachment(context.Background(), inline); err != nil || string(out) != "inline" {
		t.Fatalf("%q %v", out, err)
	}
	missing, _ := marshalPrivate(&gmproto.MediaContent{MediaID: "x"})
	if _, err := g.Attachment(context.Background(), missing); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing key: %v", err)
	}
}
