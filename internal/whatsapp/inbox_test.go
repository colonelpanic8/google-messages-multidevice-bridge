package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func oneofMessages() []*waE2E.Message {
	return []*waE2E.Message{
		{TemplateMessage: &waE2E.TemplateMessage{Format: &waE2E.TemplateMessage_HydratedFourRowTemplate_{HydratedFourRowTemplate: &waE2E.TemplateMessage_HydratedFourRowTemplate{HydratedContentText: proto.String("template")}}}},
		{InteractiveMessage: &waE2E.InteractiveMessage{InteractiveMessage: &waE2E.InteractiveMessage_NativeFlowMessage_{NativeFlowMessage: &waE2E.InteractiveMessage_NativeFlowMessage{MessageParamsJSON: proto.String(`{"test":true}`)}}}},
	}
}
func inboxMessage(id string, body *waE2E.Message) *events.Message {
	chat := types.NewJID("123", types.HiddenUserServer)
	return &events.Message{Info: types.MessageInfo{ID: id, Timestamp: time.Unix(100, 0), MessageSource: types.MessageSource{Chat: chat, Sender: chat}}, Message: body, RawMessage: body, SourceWebMsg: &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{ID: proto.String(id)}, Message: body}, IsViewOnce: true}
}

func TestInboxProtobufOneofs(t *testing.T) {
	p := providerFixture(t)
	ctx := context.Background()
	history := &events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_FULL.Enum(), Conversations: []*waHistorySync.Conversation{{ID: proto.String("123@lid")}}}}
	for i, body := range oneofMessages() {
		id := []string{"template", "interactive"}[i]
		body.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
		event := inboxMessage(id, body)
		key, err := p.ingest(ctx, "message", event)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := p.DB.NetworkGet(ctx, p.Keys.key("inbox", key))
		if err != nil {
			t.Fatal(err)
		}
		_, decoded, err := decodePending(raw)
		if err != nil {
			t.Fatal(err)
		}
		got := decoded.(*events.Message)
		if !proto.Equal(body, got.Message) || !proto.Equal(body, got.RawMessage) || !proto.Equal(event.SourceWebMsg, got.SourceWebMsg) || !got.IsViewOnce || got.Info.ID != id {
			t.Fatal("message payload or metadata changed")
		}
		history.Data.Conversations[0].Messages = append(history.Data.Conversations[0].Messages, &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
			Key: &waCommon.MessageKey{ID: proto.String("history-" + id), RemoteJID: proto.String("123@lid"), FromMe: proto.Bool(false)}, MessageTimestamp: proto.Uint64(100), Message: body,
		}})
	}
	key, err := p.ingest(ctx, "history", history)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := p.DB.NetworkGet(ctx, p.Keys.key("inbox", key))
	if err != nil {
		t.Fatal(err)
	}
	_, decoded, err := decodePending(raw)
	if err != nil || !proto.Equal(history.Data, decoded.(*events.HistorySync).Data) {
		t.Fatal("history payload changed", err)
	}
	for range 3 {
		if err = p.Drain(ctx, persistFixture(p)); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"template", "interactive", "history-template", "history-interactive"} {
		readMessage(t, p, "123@lid/"+id)
	}
	rows, err := p.Keys.scan(ctx, "inbox")
	if err != nil || len(rows) != 0 {
		t.Fatal("inbox not drained", err)
	}
	count, err := p.Quarantined(ctx)
	if err != nil || count != 0 {
		t.Fatal("valid oneof event quarantined", count, err)
	}
}

func TestInboxQuarantinesMalformedRowsAndContinues(t *testing.T) {
	legacy, err := json.Marshal(struct {
		Kind string
		Data any
	}{"message", inboxMessage("legacy-oneof", oneofMessages()[0])})
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]byte{
		"legacy oneof":     legacy,
		"invalid JSON":     []byte("{"),
		"invalid protobuf": []byte(`{"Version":1,"Kind":"message","Data":{},"Payloads":{"message":"/w=="}}`),
		"unknown version":  []byte(`{"Version":99,"Kind":"message","Data":{}}`),
		"unknown kind":     []byte(`{"Kind":"unknown","Data":{}}`),
	} {
		t.Run(name, func(t *testing.T) {
			p := providerFixture(t)
			ctx := context.Background()
			failures := 0
			p.Keys.Failure = func(error) { failures++ }
			if err := p.DB.NetworkPut(ctx, p.Keys.key("inbox", "00000000000000000000"), bad); err != nil {
				t.Fatal(err)
			}
			// A decodable legacy event must still be processed.
			if err := p.Keys.put(ctx, "inbox", "legacy-good", struct {
				Kind string
				Data any
			}{"message", inboxMessage("legacy-good", &waE2E.Message{Conversation: proto.String("old")})}); err != nil {
				t.Fatal(err)
			}
			if err := p.Ingest(ctx, "message", inboxMessage("new-good", &waE2E.Message{Conversation: proto.String("new")})); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := p.Drain(ctx, persistFixture(p)); err != nil {
					t.Fatal(err)
				}
			}
			if failures != 0 {
				t.Fatal("decode error reported as storage failure")
			}
			for _, id := range []string{"legacy-good", "new-good"} {
				readMessage(t, p, "123@lid/"+id)
			}
			rows, err := p.Keys.scan(ctx, "quarantine")
			if err != nil || len(rows) != 1 || !bytes.Equal(rows["00000000000000000000"], bad) {
				t.Fatal("quarantine did not preserve original row", err)
			}
			restarted := &Provider{Keys: NewCredentials(p.DB, p.Keys.Namespace)}
			if count, err := restarted.Quarantined(ctx); err != nil || count != 1 {
				t.Fatal("quarantine count not durable", count, err)
			}
			rows, err = p.Keys.scan(ctx, "inbox")
			if err != nil || len(rows) != 0 {
				t.Fatal("poisoned inbox retained", err)
			}
		})
	}
}

func TestInboxPersistenceFailureRetainsEvent(t *testing.T) {
	p := providerFixture(t)
	ctx := context.Background()
	if err := p.Ingest(ctx, "message", inboxMessage("persist", &waE2E.Message{Conversation: proto.String("hello")})); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("database write failed")
	if err := p.Drain(ctx, func(provider.Snapshot) error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	rows, err := p.Keys.scan(ctx, "inbox")
	if err != nil || len(rows) != 1 {
		t.Fatal("failed persistence lost event", err)
	}
	if count, err := p.Quarantined(ctx); err != nil || count != 0 {
		t.Fatal("storage failure quarantined", count, err)
	}
}
