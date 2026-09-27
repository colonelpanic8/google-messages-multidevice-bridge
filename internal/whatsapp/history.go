package whatsapp

import (
	"context"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type whatsmeowHistory struct{}

func (*whatsmeowHistory) request(info *types.MessageInfo) *waE2E.Message {
	return (&whatsmeow.Client{}).BuildHistorySyncRequest(info, 100)
}
func whatsmeowPeer() whatsmeow.SendRequestExtra { return whatsmeow.SendRequestExtra{Peer: true} }
func (p *Provider) History(ctx context.Context, e *events.HistorySync) error {
	for _, mapping := range e.Data.GetPhoneNumberToLidMappings() {
		pn, err := types.ParseJID(mapping.GetPnJID())
		if err != nil {
			return err
		}
		lid, err := types.ParseJID(mapping.GetLidJID())
		if err != nil {
			return err
		}
		if err = p.Keys.PutLIDMapping(ctx, lid, pn); err != nil {
			return err
		}
	}
	for _, c := range e.Data.GetConversations() {
		if err := p.Ingest(ctx, "history-chat", historyChat{Conversation: c, OnDemand: e.Data.GetSyncType() == waHistorySync.HistorySync_ON_DEMAND}); err != nil {
			return err
		}
	}
	return nil
}

type historyChat struct {
	Conversation *waHistorySync.Conversation
	OnDemand     bool
}

func (p *Provider) HistoryChat(ctx context.Context, h *historyChat) ([]provider.Snapshot, error) {
	parser := &whatsmeow.Client{Store: p.Device}
	c := h.Conversation
	chat, err := types.ParseJID(c.GetID())
	if err != nil {
		return nil, err
	}
	if c.GetPnJID() != "" && c.GetLidJID() != "" {
		pn, e := types.ParseJID(c.GetPnJID())
		if e != nil {
			return nil, e
		}
		lid, e := types.ParseJID(c.GetLidJID())
		if e != nil {
			return nil, e
		}
		if err = p.Keys.PutLIDMapping(ctx, lid, pn); err != nil {
			return nil, err
		}
	}
	canonical, err := p.Canonical(ctx, chat)
	if err != nil {
		return nil, err
	}
	settings, err := p.Keys.GetChatSettings(ctx, canonical)
	if err != nil {
		return nil, err
	}
	if !settings.Found {
		if err = p.Keys.PutArchived(ctx, canonical, c.GetArchived()); err != nil {
			return nil, err
		}
		if err = p.Keys.PutPinned(ctx, canonical, c.GetPinned() != 0); err != nil {
			return nil, err
		}
		if c.GetMuteEndTime() > 0 {
			if err = p.Keys.PutMutedUntil(ctx, canonical, time.Unix(int64(c.GetMuteEndTime()), 0)); err != nil {
				return nil, err
			}
		}
	}
	pending := []string{}
	var oldest types.MessageInfo
	for _, m := range c.GetMessages() {
		parsed, err := parser.ParseWebMessage(chat, m.GetMessage())
		if err != nil {
			return nil, err
		}
		if parsed == nil {
			continue
		}
		id, err := p.ingest(ctx, "message", parsed)
		if err != nil {
			return nil, err
		}
		pending = append(pending, id)
		if oldest.ID == "" || parsed.Info.Timestamp.Before(oldest.Timestamp) {
			oldest = parsed.Info
		}
	}
	var request historyRequest
	found, err := p.Keys.get(ctx, "history-request", canonical.String(), &request)
	if err != nil {
		return nil, err
	}
	if found && h.OnDemand {
		if err = p.Keys.put(ctx, "history-response", canonical.String(), historyResponse{RequestID: request.Oldest.ID, Oldest: oldest, Pending: pending}); err != nil {
			return nil, err
		}
	}
	snap, err := p.Conversation(ctx, canonical, c.GetName())
	if err != nil {
		return nil, err
	}
	return []provider.Snapshot{snap}, nil
}
