package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waServerSync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/proto/waWeb"
	wa "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

func TestNonDisplayableMessagesDoNotCreateRecords(t *testing.T) {
	controls := []*waE2E.Message{
		{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum()}},
		{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_APP_STATE_SYNC_KEY_SHARE.Enum()}},
		{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_PEER_DATA_OPERATION_REQUEST_MESSAGE.Enum()}},
		{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_EPHEMERAL_SETTING.Enum()}},
		{SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{AxolotlSenderKeyDistributionMessage: []byte("synthetic")}},
		{PollUpdateMessage: &waE2E.PollUpdateMessage{}}, {KeepInChatMessage: &waE2E.KeepInChatMessage{}},
		{PinInChatMessage: &waE2E.PinInChatMessage{}}, {CallLogMesssage: &waE2E.CallLogMessage{}}, {},
	}
	p := providerFixture(t)
	ctx := context.Background()
	for _, body := range controls {
		event := inboxMessage("control", body)
		event.Info.IsFromMe = true
		if err := p.Ingest(ctx, "message", event); err != nil {
			t.Fatal(err)
		}
	}
	for _, server := range []string{types.BroadcastServer, types.NewsletterServer} {
		event := inboxMessage("broadcast", &waE2E.Message{Conversation: proto.String("broadcast content")})
		event.Info.Chat = types.NewJID("status", server)
		if err := p.Ingest(ctx, "message", event); err != nil {
			t.Fatal(err)
		}
	}
	system := inboxMessage("system", &waE2E.Message{Conversation: proto.String("security notice")})
	system.SourceWebMsg.MessageStubType = waWeb.WebMessageInfo_CIPHERTEXT.Enum()
	if err := p.Ingest(ctx, "message", system); err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(ctx, persistFixture(p)); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"message", "conversation"} {
		rows, err := p.DB.Latest(kind)
		if err != nil || len(rows) != 0 {
			t.Fatal(kind, len(rows), err)
		}
	}
	// User content may accompany a sender-key distribution payload.
	event := inboxMessage("visible", &waE2E.Message{Conversation: proto.String("hello"), SenderKeyDistributionMessage: controls[4].SenderKeyDistributionMessage})
	if err := p.Ingest(ctx, "message", event); err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(ctx, persistFixture(p)); err != nil {
		t.Fatal(err)
	}
	if m := readMessage(t, p, "123@lid/visible"); m.Text != "hello" {
		t.Fatal(m)
	}
}

func conversationFixture(t *testing.T, p *Provider) model.Conversation {
	t.Helper()
	raw, err := p.DB.Record("conversation", "123@lid")
	if err != nil {
		t.Fatal(err)
	}
	var c model.Conversation
	if err = json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}
func TestHistoryConversationMetadataAndLivePreview(t *testing.T) {
	p := providerFixture(t)
	ctx := context.Background()
	c := &waHistorySync.Conversation{ID: proto.String("123@lid"), Name: proto.String("chat"), LastMsgTimestamp: proto.Uint64(300), UnreadCount: proto.Uint32(3)}
	for _, n := range []uint64{300, 100, 200} {
		c.Messages = append(c.Messages, &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{ID: proto.String(time.Unix(int64(n), 0).String()), RemoteJID: c.ID, FromMe: proto.Bool(n == 300)}, MessageTimestamp: proto.Uint64(n), Message: &waE2E.Message{Conversation: proto.String(time.Unix(int64(n), 0).String())}}})
	}
	h := &events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_FULL.Enum(), Conversations: []*waHistorySync.Conversation{c}}}
	if err := p.Ingest(ctx, "history", h); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := p.Drain(ctx, persistFixture(p)); err != nil {
			t.Fatal(err)
		}
	}
	got := conversationFixture(t, p)
	if got.Updated.Unix() != 300 || got.Preview != time.Unix(300, 0).String() || got.PreviewDirection != "outgoing" || got.PreviewSenderID == "" || got.UnreadCount != 3 || !got.Unread {
		t.Fatal(got)
	}
	if _, err := p.DB.ReadConversation("123@lid"); err != nil {
		t.Fatal(err)
	}
	if err := p.Ingest(ctx, "history", h); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := p.Drain(ctx, persistFixture(p)); err != nil {
			t.Fatal(err)
		}
	}
	if c := conversationFixture(t, p); c.Unread || c.UnreadCount != 0 {
		t.Fatal("replayed snapshot reset read state", c)
	}
	live := inboxMessage("live", &waE2E.Message{Conversation: proto.String("newest")})
	live.SourceWebMsg = nil
	live.Info.Timestamp = time.Now()
	if err := p.Ingest(ctx, "message", live); err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(ctx, persistFixture(p)); err != nil {
		t.Fatal(err)
	}
	if err := p.Ingest(ctx, "history", h); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := p.Drain(ctx, persistFixture(p)); err != nil {
			t.Fatal(err)
		}
	}
	got = conversationFixture(t, p)
	if got.Preview != "newest" || got.PreviewDirection != "incoming" || got.UnreadCount != 1 || !got.Updated.Equal(live.Info.Timestamp.UTC()) {
		t.Fatal(got)
	}
}

type fakeAppState struct {
	keys  *Credentials
	calls map[appstate.WAPatchName][]bool
	fail  bool
}

func (f *fakeAppState) FetchAppState(ctx context.Context, name appstate.WAPatchName, full, only bool) error {
	if only {
		return errors.New("would skip incomplete snapshot")
	}
	f.calls[name] = append(f.calls[name], full)
	if name == appstate.WAPatchCriticalUnblockLow {
		if f.fail {
			return appstate.ErrKeyNotFound
		}
		return f.keys.PutAllContactNames(ctx, []wa.ContactEntry{{JID: types.NewJID("14155550100", types.DefaultUserServer), FullName: "Address book"}})
	}
	return nil
}
func TestAppStateRetriesMissingKeysAndIncompleteReconnect(t *testing.T) {
	p := providerFixture(t)
	ctx := context.Background()
	client := &fakeAppState{keys: p.Keys, calls: map[appstate.WAPatchName][]bool{}, fail: true}
	sync := &AppStateSync{Provider: p, Client: client}
	if err := sync.Sync(ctx); !errors.Is(err, appstate.ErrKeyNotFound) || len(client.calls) != 0 {
		t.Fatal(err, client.calls)
	}
	if err := p.Keys.PutAppStateSyncKey(ctx, []byte{1}, wa.AppStateSyncKey{Data: []byte("synthetic"), Timestamp: 1}); err != nil {
		t.Fatal(err)
	}
	if err := sync.Sync(ctx); !errors.Is(err, appstate.ErrKeyNotFound) || len(client.calls) != len(appstate.AllPatchNames) {
		t.Fatal(err, client.calls)
	}
	// A reconnect repeats the incomplete snapshot even if a partial version exists.
	if err := p.Keys.PutAppStateVersion(ctx, string(appstate.WAPatchCriticalUnblockLow), 42, [128]byte{}); err != nil {
		t.Fatal(err)
	}
	client.fail = false
	sync = &AppStateSync{Provider: p, Client: client}
	if err := sync.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	calls := client.calls[appstate.WAPatchCriticalUnblockLow]
	if len(calls) != 2 || !calls[0] || !calls[1] {
		t.Fatal(calls)
	}
	if err := sync.Sync(ctx); err != nil || len(client.calls[appstate.WAPatchCriticalUnblockLow]) != 2 {
		t.Fatal("completed fetch repeated", err)
	}
	if err := sync.Retry(ctx, appstate.WAPatchCriticalUnblockLow); err != nil {
		t.Fatal(err)
	}
	sync = &AppStateSync{Provider: p, Client: client}
	if err := sync.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := client.calls[appstate.WAPatchCriticalUnblockLow]; len(calls) != 3 || !calls[2] {
		t.Fatal("sync error did not persist incomplete state", calls)
	}
	pn := types.NewJID("14155550100", types.DefaultUserServer)
	lid := types.NewJID("123", types.HiddenUserServer)
	if err := p.Keys.PutLIDMapping(ctx, lid, pn); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Keys.PutPushName(ctx, types.NewJID("14155550101", types.DefaultUserServer), "Push only"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Keys.PutBusinessName(ctx, types.NewJID("14155550102", types.DefaultUserServer), "Business only"); err != nil {
		t.Fatal(err)
	}
	contacts, err := p.Contacts(ctx)
	if err != nil || len(contacts) != 3 {
		t.Fatal(contacts, err)
	}
	for _, c := range contacts {
		if c.Name == "" || c.Address == "" {
			t.Fatal(c)
		}
	}
}

func TestRepairedEpochDoesNotReadoptOldConversation(t *testing.T) {
	p := providerFixture(t)
	ctx := context.Background()
	if err := p.DB.SavePairedSession([]byte("old"), true); err != nil {
		t.Fatal(err)
	}
	event := inboxMessage("old", &waE2E.Message{Conversation: proto.String("old preview")})
	event.Info.Timestamp = time.Unix(500, 0)
	if err := p.Ingest(ctx, "message", event); err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(ctx, persistFixture(p)); err != nil {
		t.Fatal(err)
	}
	if err := p.DB.SavePairedSession([]byte("new"), true); err != nil {
		t.Fatal(err)
	}
	snapshots, err := p.Conversations(ctx)
	if err != nil || len(snapshots) != 0 {
		t.Fatal("old chat adopted by metadata refresh", err)
	}
	fresh := inboxMessage("fresh", &waE2E.Message{Conversation: proto.String("new epoch preview")})
	fresh.Info.Timestamp = time.Unix(100, 0)
	if err = p.Ingest(ctx, "message", fresh); err != nil {
		t.Fatal(err)
	}
	if err = p.Drain(ctx, persistFixture(p)); err != nil {
		t.Fatal(err)
	}
	c := conversationFixture(t, p)
	if c.Updated.Unix() != 100 || c.Preview != "new epoch preview" {
		t.Fatal("old epoch preview leaked", c)
	}
	if current, err := p.DB.EntityCurrent("message", "123@lid/old"); err != nil || current {
		t.Fatal("old message adopted", err)
	}
	// Only an actual observation may make the old message current again.
	if err = p.Ingest(ctx, "message", event); err != nil {
		t.Fatal(err)
	}
	if err = p.Drain(ctx, persistFixture(p)); err != nil {
		t.Fatal(err)
	}
	if current, err := p.DB.EntityCurrent("message", "123@lid/old"); err != nil || !current {
		t.Fatal("observed message not adopted", err)
	}
}

func TestFreshPairingNamespaceHasIndependentKeys(t *testing.T) {
	p := providerFixture(t)
	ctx := context.Background()
	old, err := p.Keys.Device(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Keys.PutDevice(ctx, old); err != nil {
		t.Fatal(err)
	}
	next, err := NewCredentials(p.DB, NewNamespace()).Device(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != nil || *next.IdentityKey.Priv == *old.IdentityKey.Priv || *next.NoiseKey.Priv == *old.NoiseKey.Priv {
		t.Fatal("pairing reused identity")
	}
}

func TestAppStateContactActionsReachAddressBook(t *testing.T) {
	p := providerFixture(t)
	ctx := context.Background()
	client := whatsmeow.NewClient(p.Device, waLog.Noop)
	//lint:ignore SA1019 Exercise the pinned contact mapper without a network connection.
	internal := client.DangerousInternals()
	for _, full := range []bool{true, false} {
		var emitted []any
		mutation := appstate.Mutation{Index: []string{appstate.IndexContact, "14155550103@s.whatsapp.net"}, Operation: waServerSync.SyncdMutation_SET, Action: &waSyncAction.SyncActionValue{ContactAction: &waSyncAction.ContactAction{FullName: proto.String("Synthetic contact")}}}
		if err := internal.CollectEventsToDispatch(ctx, appstate.WAPatchCriticalUnblockLow, []appstate.Mutation{mutation}, full, &emitted); err != nil {
			t.Fatal(err)
		}
		contacts, err := p.Contacts(ctx)
		if err != nil || len(contacts) != 1 || contacts[0].Address != "+14155550103" || contacts[0].Name != "Synthetic contact" {
			t.Fatal(contacts, err)
		}
		if !full {
			found := false
			for _, e := range emitted {
				if _, ok := e.(*events.Contact); ok {
					found = true
				}
			}
			if !found {
				t.Fatal("incremental contact event missing")
			}
		}
	}
}

func TestMutationsDoNotCreateMessagesOrIncrementUnread(t *testing.T) {
	p := providerFixture(t)
	ctx := context.Background()
	original := inboxMessage("target", &waE2E.Message{Conversation: proto.String("original")})
	original.SourceWebMsg = nil
	original.Info.Timestamp = time.Now()
	apply := func(e *events.Message) {
		t.Helper()
		if err := p.Ingest(ctx, "message", e); err != nil {
			t.Fatal(err)
		}
		if err := p.Drain(ctx, persistFixture(p)); err != nil {
			t.Fatal(err)
		}
	}
	apply(original)
	for _, body := range []*waE2E.Message{
		{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("target")}, Text: proto.String("👍")}},
		{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String("target")}, EditedMessage: &waE2E.Message{Conversation: proto.String("edited")}}},
		{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("target")}}},
	} {
		event := inboxMessage("mutation", body)
		event.SourceWebMsg = nil
		event.Info.Timestamp = time.Now()
		apply(event)
	}
	records, err := p.DB.Latest("message")
	if err != nil || len(records) != 1 {
		t.Fatal("mutation created a new record", err)
	}
	c := conversationFixture(t, p)
	if c.UnreadCount != 1 || c.Preview != "Message deleted" {
		t.Fatal(c)
	}
	if _, err := p.DB.ReadConversation("123@lid"); err != nil {
		t.Fatal(err)
	}
	if c = conversationFixture(t, p); c.Unread || c.UnreadCount != 0 {
		t.Fatal(c)
	}
}
