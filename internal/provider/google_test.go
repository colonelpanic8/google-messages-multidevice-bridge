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

type fakeGoogleClient struct {
	list          func(context.Context, *gmproto.ListConversationsRequest) (*gmproto.ListConversationsResponse, error)
	fetch         func(context.Context, string, int64, *gmproto.Cursor) (*gmproto.ListMessagesResponse, error)
	create        func(context.Context, *gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error)
	conversation  func(context.Context, string) (*gmproto.Conversation, error)
	send          func(context.Context, *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error)
	react         func(context.Context, *gmproto.SendReactionRequest) (*gmproto.SendReactionResponse, error)
	typing        func(context.Context, string, *gmproto.SIMPayload) error
	markRead      func(context.Context, string, string) error
	upload        func(context.Context, []byte, string, string) (*gmproto.MediaContent, error)
	downloadMedia func(context.Context, string, []byte) (io.ReadCloser, error)
}

func (f *fakeGoogleClient) ListConversations(ctx context.Context, req *gmproto.ListConversationsRequest) (*gmproto.ListConversationsResponse, error) {
	return f.list(ctx, req)
}

func (f *fakeGoogleClient) FetchMessages(ctx context.Context, id string, count int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
	return f.fetch(ctx, id, count, cursor)
}

func (f *fakeGoogleClient) GetOrCreateConversation(ctx context.Context, req *gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error) {
	return f.create(ctx, req)
}

func (f *fakeGoogleClient) GetConversation(ctx context.Context, id string) (*gmproto.Conversation, error) {
	return f.conversation(ctx, id)
}

func (f *fakeGoogleClient) SendMessage(ctx context.Context, req *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
	return f.send(ctx, req)
}

func (f *fakeGoogleClient) SendReaction(ctx context.Context, req *gmproto.SendReactionRequest) (*gmproto.SendReactionResponse, error) {
	return f.react(ctx, req)
}

func (f *fakeGoogleClient) SetTyping(ctx context.Context, id string, sim *gmproto.SIMPayload) error {
	return f.typing(ctx, id, sim)
}

func (f *fakeGoogleClient) MarkRead(ctx context.Context, conversationID, messageID string) error {
	return f.markRead(ctx, conversationID, messageID)
}

func (f *fakeGoogleClient) UploadMediaContext(ctx context.Context, data []byte, name, mime string) (*gmproto.MediaContent, error) {
	return f.upload(ctx, data, name, mime)
}

func (f *fakeGoogleClient) DownloadMediaContext(ctx context.Context, id string, key []byte) (io.ReadCloser, error) {
	return f.downloadMedia(ctx, id, key)
}

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

func TestConversationPagesMapFoldersAndBindCursors(t *testing.T) {
	var requests []*gmproto.ListConversationsRequest
	fake := &fakeGoogleClient{list: func(_ context.Context, req *gmproto.ListConversationsRequest) (*gmproto.ListConversationsResponse, error) {
		requests = append(requests, req)
		return &gmproto.ListConversationsResponse{
			Conversations: []*gmproto.Conversation{{ConversationID: "c1"}},
			Cursor:        &gmproto.Cursor{LastItemID: "c1", LastItemTimestamp: 1234},
		}, nil
	}}
	g := newGoogle(fake)

	first, err := g.Conversations(context.Background())
	if err != nil || len(first) != 1 || len(requests) != 1 {
		t.Fatalf("first page: %d snapshots, %d requests, %v", len(first), len(requests), err)
	}
	if req := requests[0]; req.GetCount() != RecentConversations || req.GetFolder() != gmproto.ListConversationsRequest_INBOX || req.GetCursor() != nil {
		t.Fatalf("first-page request: %+v", req)
	}

	_, next, err := g.ConversationPage(context.Background(), "archive", nil)
	if err != nil || len(next) == 0 {
		t.Fatalf("archive page: cursor=%q err=%v", next, err)
	}
	_, _, err = g.ConversationPage(context.Background(), "archive", next)
	if err != nil {
		t.Fatal(err)
	}
	if req := requests[2]; req.GetCount() != RecentConversations || req.GetFolder() != gmproto.ListConversationsRequest_ARCHIVE || req.GetCursor().GetLastItemID() != "c1" || req.GetCursor().GetLastItemTimestamp() != 1234 {
		t.Fatalf("continuation request: %+v", req)
	}

	before := len(requests)
	if _, _, err = g.ConversationPage(context.Background(), "spam", next); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("cross-folder cursor: %v", err)
	}
	if _, _, err = g.ConversationPage(context.Background(), "trash", nil); !errors.Is(err, ErrInvalidFolder) {
		t.Fatalf("unknown folder: %v", err)
	}
	if _, _, err = g.ConversationPage(context.Background(), "inbox", []byte("not a cursor")); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("malformed cursor: %v", err)
	}
	if len(requests) != before {
		t.Fatal("invalid pagination input reached libgm")
	}
}

func TestConversationPageDoesNotTreatUnusableCursorBytesAsComplete(t *testing.T) {
	fake := &fakeGoogleClient{list: func(context.Context, *gmproto.ListConversationsRequest) (*gmproto.ListConversationsResponse, error) {
		return &gmproto.ListConversationsResponse{CursorBytes: []byte("opaque")}, nil
	}}
	if _, _, err := newGoogle(fake).ConversationPage(context.Background(), "inbox", nil); !errors.Is(err, ErrUnsupportedCursor) {
		t.Fatalf("cursorBytes-only response: %v", err)
	}
}

func TestMessagePagesMaintainCountAndBindConversation(t *testing.T) {
	var ids []string
	var counts []int64
	var cursors []*gmproto.Cursor
	fake := &fakeGoogleClient{fetch: func(_ context.Context, id string, count int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
		ids = append(ids, id)
		counts = append(counts, count)
		cursors = append(cursors, cursor)
		return &gmproto.ListMessagesResponse{
			Messages: []*gmproto.Message{{MessageID: "m1", ConversationID: id}},
			Cursor:   &gmproto.Cursor{LastItemID: "m1", LastItemTimestamp: 5678},
		}, nil
	}}
	g := newGoogle(fake)

	first, err := g.Messages(context.Background(), "c1")
	if err != nil || len(first) != 1 || len(ids) != 1 || ids[0] != "c1" || counts[0] != RecentMessages || cursors[0] != nil {
		t.Fatalf("first page: ids=%v counts=%v cursors=%v snapshots=%d err=%v", ids, counts, cursors, len(first), err)
	}
	_, next, err := g.MessagePage(context.Background(), "c1", nil)
	if err != nil || len(next) == 0 {
		t.Fatalf("page cursor=%q err=%v", next, err)
	}
	_, _, err = g.MessagePage(context.Background(), "c1", next)
	if err != nil {
		t.Fatal(err)
	}
	if cursor := cursors[2]; cursor.GetLastItemID() != "m1" || cursor.GetLastItemTimestamp() != 5678 {
		t.Fatalf("continuation cursor: %+v", cursor)
	}
	before := len(ids)
	if _, _, err = g.MessagePage(context.Background(), "c2", next); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("cross-conversation cursor: %v", err)
	}
	if len(ids) != before {
		t.Fatal("invalid cursor reached libgm")
	}
}

func TestCreateConversationMapsRequestAndOutcomes(t *testing.T) {
	var requests []*gmproto.GetOrCreateConversationRequest
	response := &gmproto.GetOrCreateConversationResponse{
		Status:       gmproto.GetOrCreateConversationResponse_SUCCESS,
		Conversation: &gmproto.Conversation{ConversationID: "c1"},
	}
	var responseErr error
	fake := &fakeGoogleClient{create: func(_ context.Context, req *gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error) {
		requests = append(requests, req)
		return response, responseErr
	}}
	g := newGoogle(fake)

	snapshot, err := g.CreateConversation(context.Background(), []string{"+15550001", "+15550002"})
	if err != nil || snapshot.Event.Type != "conversation" || snapshot.Event.EntityID != "c1" {
		t.Fatalf("success: %+v %v", snapshot.Event, err)
	}
	if len(requests) != 1 || len(requests[0].GetNumbers()) != 2 {
		t.Fatalf("requests: %+v", requests)
	}
	for i, number := range requests[0].GetNumbers() {
		if number.GetMysteriousInt() != 2 || number.GetNumber() != []string{"+15550001", "+15550002"}[i] || number.GetNumber2() != number.GetNumber() {
			t.Fatalf("number %d: %+v", i, number)
		}
	}

	response = &gmproto.GetOrCreateConversationResponse{Status: gmproto.GetOrCreateConversationResponse_CREATE_RCS}
	if _, err = g.CreateConversation(context.Background(), []string{"+15550001", "+15550002"}); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("RCS confirmation: %v", err)
	}
	if len(requests) != 3 || !requests[2].GetCreateRCSGroup() || requests[2].RCSGroupName == nil {
		t.Fatal("CREATE_RCS must make exactly one explicit confirmation")
	}
	response = &gmproto.GetOrCreateConversationResponse{Status: gmproto.GetOrCreateConversationResponse_UNKNOWN}
	if _, err = g.CreateConversation(context.Background(), []string{"+15550001"}); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("unknown response: %v", err)
	}
	responseErr = context.DeadlineExceeded
	if _, err = g.CreateConversation(context.Background(), []string{"+15550001"}); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("transport outcome: %v", err)
	}
	before := len(requests)
	if _, err = g.CreateConversation(context.Background(), nil); !errors.Is(err, ErrRejected) {
		t.Fatalf("empty recipients: %v", err)
	}
	if len(requests) != before {
		t.Fatal("invalid recipients reached libgm")
	}
}

func TestReactionAndTypingMapPreparedTarget(t *testing.T) {
	sim := &gmproto.SIMPayload{}
	target := SendTarget{ConversationID: "c1", participantID: "self", sim: sim}
	var reactions []*gmproto.SendReactionRequest
	reactionResponse := &gmproto.SendReactionResponse{Success: true}
	var reactionErr error
	var typingCalls int
	fake := &fakeGoogleClient{
		react: func(_ context.Context, req *gmproto.SendReactionRequest) (*gmproto.SendReactionResponse, error) {
			reactions = append(reactions, req)
			return reactionResponse, reactionErr
		},
		typing: func(_ context.Context, id string, gotSIM *gmproto.SIMPayload) error {
			typingCalls++
			if id != "c1" || gotSIM != sim {
				t.Fatalf("typing mapping: %q %p", id, gotSIM)
			}
			return nil
		},
	}
	g := newGoogle(fake)

	if err := g.React(context.Background(), target, "m1", "👍", false); err != nil {
		t.Fatal(err)
	}
	if req := reactions[0]; req.GetMessageID() != "m1" || req.GetReactionData().GetUnicode() != "👍" || req.GetReactionData().GetType() != gmproto.EmojiType_LIKE || req.GetAction() != gmproto.SendReactionRequest_ADD || req.GetSIMPayload() != sim {
		t.Fatalf("add mapping: %+v", req)
	}
	if err := g.React(context.Background(), target, "m1", "👍", true); err != nil {
		t.Fatal(err)
	}
	if req := reactions[1]; req.GetAction() != gmproto.SendReactionRequest_REMOVE || req.GetSIMPayload() != nil {
		t.Fatalf("remove mapping: %+v", req)
	}
	reactionResponse = &gmproto.SendReactionResponse{Success: false}
	if err := g.React(context.Background(), target, "m1", "👍", false); !errors.Is(err, ErrRejected) {
		t.Fatalf("explicit refusal: %v", err)
	}
	reactionErr = context.DeadlineExceeded
	if err := g.React(context.Background(), target, "m1", "👍", false); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("uncertain outcome: %v", err)
	}
	before := len(reactions)
	if err := g.React(context.Background(), SendTarget{}, "m1", "👍", false); !errors.Is(err, ErrRejected) {
		t.Fatalf("invalid target: %v", err)
	}
	if len(reactions) != before {
		t.Fatal("invalid target reached libgm")
	}
	if err := g.Typing(context.Background(), target); err != nil || typingCalls != 1 {
		t.Fatalf("typing: calls=%d err=%v", typingCalls, err)
	}
}

func TestUploadSerializesPrivateMediaAndSendAppendsIt(t *testing.T) {
	key := bytes.Repeat([]byte{4}, 32)
	var uploadCalls int
	var sent *gmproto.SendMessageRequest
	fake := &fakeGoogleClient{
		upload: func(_ context.Context, data []byte, name, mime string) (*gmproto.MediaContent, error) {
			uploadCalls++
			if string(data) != "file" || name != "photo.jpg" || mime != "image/jpeg" {
				t.Fatalf("upload arguments: %q %q %q", data, name, mime)
			}
			return &gmproto.MediaContent{MediaID: "media-1", MediaName: name, MimeType: mime, Size: int64(len(data)), DecryptionKey: key}, nil
		},
		send: func(_ context.Context, req *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
			sent = req
			return &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS}, nil
		},
	}
	g := newGoogle(fake)
	private, err := g.Upload(context.Background(), []byte("file"), "photo.jpg", "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	var uploaded gmproto.MediaContent
	if err = unmarshalPrivate(private, &uploaded); err != nil || uploaded.GetMediaID() != "media-1" || !bytes.Equal(uploaded.GetDecryptionKey(), key) {
		t.Fatalf("private upload descriptor: %+v %v", &uploaded, err)
	}
	target := SendTarget{ConversationID: "c1", participantID: "self", sim: &gmproto.SIMPayload{}, Media: [][]byte{private}}
	outbox := model.Outbox{TransactionID: "tmp-1", Request: model.SendRequest{ConversationID: "c1", Text: "caption"}}
	if err = g.Send(context.Background(), target, outbox); err != nil {
		t.Fatal(err)
	}
	info := sent.GetMessagePayload().GetMessageInfo()
	if len(info) != 2 || info[0].GetMediaContent().GetMediaID() != "media-1" || info[1].GetMessageContent().GetContent() != "caption" {
		t.Fatalf("message info: %+v", info)
	}
	outbox.Request.Text = ""
	if err = g.Send(context.Background(), target, outbox); err != nil {
		t.Fatal(err)
	}
	if info = sent.GetMessagePayload().GetMessageInfo(); len(info) != 1 || info[0].GetMediaContent().GetMediaID() != "media-1" {
		t.Fatalf("media-only message info: %+v", info)
	}
	before := sent
	target.Media = [][]byte{[]byte("invalid")}
	if err = g.Send(context.Background(), target, outbox); !errors.Is(err, ErrRejected) {
		t.Fatalf("invalid media: %v", err)
	}
	if sent != before {
		t.Fatal("invalid media reached send")
	}
	if _, err = g.Upload(context.Background(), make([]byte, MaxAttachmentBytes+1), "large", "application/octet-stream"); !errors.Is(err, ErrTooLarge) || uploadCalls != 1 {
		t.Fatalf("oversize: calls=%d err=%v", uploadCalls, err)
	}
}

func TestSendRejectsMediaOverCombinedLimit(t *testing.T) {
	key := bytes.Repeat([]byte{2}, 32)
	first, _ := marshalPrivate(&gmproto.MediaContent{MediaID: "one", Size: MaxAttachmentBytes, DecryptionKey: key})
	second, _ := marshalPrivate(&gmproto.MediaContent{MediaID: "two", Size: 1, DecryptionKey: key})
	sendCalls := 0
	fake := &fakeGoogleClient{send: func(context.Context, *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
		sendCalls++
		return &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS}, nil
	}}
	g := newGoogle(fake)
	target := SendTarget{ConversationID: "c1", participantID: "self", sim: &gmproto.SIMPayload{}, Media: [][]byte{first, second}}
	err := g.Send(context.Background(), target, model.Outbox{TransactionID: "tmp", Request: model.SendRequest{ConversationID: "c1"}})
	if !errors.Is(err, ErrRejected) || sendCalls != 0 {
		t.Fatalf("combined media: calls=%d err=%v", sendCalls, err)
	}
}

func TestCreateRCSConfirmationOutcome(t *testing.T) {
	for _, lost := range []bool{false, true} {
		calls := 0
		g := newGoogle(&fakeGoogleClient{create: func(_ context.Context, req *gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error) {
			calls++
			if calls == 1 {
				if req.CreateRCSGroup != nil {
					t.Fatal("premature confirmation")
				}
				return &gmproto.GetOrCreateConversationResponse{Status: gmproto.GetOrCreateConversationResponse_CREATE_RCS}, nil
			}
			if calls != 2 || !req.GetCreateRCSGroup() || req.RCSGroupName == nil || req.GetRCSGroupName() != "" {
				t.Fatal("invalid confirmation")
			}
			if lost {
				return nil, context.DeadlineExceeded
			}
			return &gmproto.GetOrCreateConversationResponse{Status: gmproto.GetOrCreateConversationResponse_SUCCESS, Conversation: &gmproto.Conversation{ConversationID: "group"}}, nil
		}})
		result, err := g.CreateConversation(context.Background(), []string{"+15550001", "+15550002"})
		if calls != 2 || (lost && !errors.Is(err, ErrAmbiguous)) || (!lost && (err != nil || result.Event.EntityID != "group")) {
			t.Fatalf("lost=%v calls=%d result=%+v err=%v", lost, calls, result, err)
		}
	}
}

func TestAttachmentPrefersFullMediaThenThumbnailOverInlinePreview(t *testing.T) {
	full, thumb := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	fake := &fakeGoogleClient{downloadMedia: func(_ context.Context, id string, key []byte) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(id + ":" + string(key[:1]))), nil
	}}
	g := &Google{Client: fake}
	both, _ := marshalPrivate(&gmproto.MediaContent{MediaID: "full", DecryptionKey: full, ThumbnailMediaID: "thumb", ThumbnailDecryptionKey: thumb, MediaData: []byte("preview")})
	if out, err := g.Attachment(context.Background(), both); err != nil || string(out) != "full:\x01" {
		t.Fatalf("%q %v", out, err)
	}
	thumbOnly, _ := marshalPrivate(&gmproto.MediaContent{ThumbnailMediaID: "thumb", ThumbnailDecryptionKey: thumb, MediaData: []byte("preview")})
	if out, err := g.Attachment(context.Background(), thumbOnly); err != nil || string(out) != "thumb:\x02" {
		t.Fatalf("%q %v", out, err)
	}
	fake.downloadMedia = func(context.Context, string, []byte) (io.ReadCloser, error) { return nil, errors.New("http 500") }
	if _, err := g.Attachment(context.Background(), both); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "http 500") {
		t.Fatalf("download failure: %v", err)
	}
}
