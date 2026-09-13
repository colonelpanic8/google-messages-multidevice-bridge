package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/util"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var ErrInvalid = errors.New("invalid request")
var validKey = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

func (b *Bridge) connection(transport, phone bool) {
	b.mu.Lock()
	if b.status.State == "storage_failed" || b.status.State == "authentication_required" || b.status.State == "connection_failed" {
		b.mu.Unlock()
		return
	}
	b.status.Detail = ""
	b.status.Transport, b.status.Phone = transport, phone
	b.status.State = "degraded"
	if transport && phone {
		b.status.State = "connected"
	}
	b.status.Updated = time.Now().UTC()
	b.mu.Unlock()
	wake(b.sendWake)
}
func wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
func (b *Bridge) RequestSync() { wake(b.syncWake) }
func (b *Bridge) getProvider() provider.Provider {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.provider
}

func (b *Bridge) setProvider(p provider.Provider) {
	b.mu.Lock()
	b.provider = p
	b.mu.Unlock()
}
func (b *Bridge) borrowProvider(ctx context.Context) (provider.Provider, context.Context, func()) {
	b.mu.Lock()
	p, lifetime := b.provider, b.providerCtx
	if p == nil {
		b.mu.Unlock()
		return nil, ctx, func() {}
	}
	b.providerOps.Add(1)
	b.mu.Unlock()
	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	stop := func() bool { return false }
	if lifetime != nil {
		stop = context.AfterFunc(lifetime, cancel)
	}
	return p, callCtx, func() { stop(); cancel(); b.providerOps.Done() }
}

// Prepare upgrades the prototype's latest snapshots. Original durable events are
// retained; the API normalizes them on read. Re-running this is safe.
func (b *Bridge) Prepare() error {
	b.prepareOnce.Do(func() { b.prepareErr = b.prepare() })
	return b.prepareErr
}
func (b *Bridge) prepare() error {
	if err := b.Store.RecoverSending(); err != nil {
		return err
	}
	for _, kind := range []string{"message", "conversation"} {
		records, err := b.Store.Latest(kind)
		if err != nil {
			return err
		}
		for _, raw := range records {
			var header struct {
				Schema int `json:"schema"`
			}
			if err := json.Unmarshal(raw, &header); err != nil {
				return err
			}
			if header.Schema != 0 {
				continue
			}
			var msg proto.Message = &gmproto.Message{}
			if kind == "conversation" {
				msg = &gmproto.Conversation{}
			}
			if err := protojson.Unmarshal(raw, msg); err != nil {
				return errors.New("cannot migrate legacy snapshot")
			}
			snap, err := provider.SnapshotOf(msg)
			if err != nil {
				return err
			}
			if _, err = b.Store.Apply(snap.Event, snap.Private, nil); err != nil {
				return err
			}
		}
	}
	return nil
}
func (b *Bridge) Queue(id string, req model.SendRequest) (model.Outbox, bool, error) {
	b.mutationMu.Lock()
	defer b.mutationMu.Unlock()
	if b.PairingActive() {
		return model.Outbox{}, false, ErrPairing
	}
	if err := validateRequest(&req); err != nil || !validKey.MatchString(id) {
		return model.Outbox{}, false, ErrInvalid
	}
	if existing, err := b.Store.Outbox(id); err == nil {
		if !existing.Request.Equal(req) {
			return model.Outbox{}, false, store.ErrConflict
		}
		return existing, false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return model.Outbox{}, false, err
	}
	if req.Kind != "conversation" {
		if current, err := b.Store.EntityCurrent("conversation", req.ConversationID); err != nil {
			return model.Outbox{}, false, err
		} else if !current {
			return model.Outbox{}, false, ErrInvalid
		}
		raw, err := b.Store.Record("conversation", req.ConversationID)
		if err != nil {
			return model.Outbox{}, false, err
		}
		var conv model.Conversation
		if err = json.Unmarshal(raw, &conv); err != nil {
			return model.Outbox{}, false, err
		}
		if conv.ReadOnly || conv.State == "deleted" {
			return model.Outbox{}, false, ErrInvalid
		}
		if req.Kind == "reaction" {
			if current, err := b.Store.EntityCurrent("message", req.MessageID); err != nil {
				return model.Outbox{}, false, err
			} else if !current {
				return model.Outbox{}, false, ErrInvalid
			}
			raw, err = b.Store.Record("message", req.MessageID)
			if err != nil {
				return model.Outbox{}, false, err
			}
			var msg model.Message
			if json.Unmarshal(raw, &msg) != nil || msg.ConversationID != req.ConversationID || msg.Deleted {
				return model.Outbox{}, false, ErrInvalid
			}
		}
		var size int64
		for _, id := range req.AttachmentIDs {
			upload, err := b.upload(id)
			if err != nil {
				return model.Outbox{}, false, err
			}
			size += upload.Size
		}
		if size > provider.MaxAttachmentBytes {
			return model.Outbox{}, false, provider.ErrTooLarge
		}
	}
	o, created, err := b.Store.Enqueue(id, util.GenerateTmpID(), req)
	if err == nil {
		b.Hub.Notify()
		wake(b.sendWake)
	}
	return o, created, err
}
func (b *Bridge) sendLoop(ctx context.Context) {
	ticker := time.NewTicker(b.sendRetry)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if b.Status().State == "connected" {
			sent, err := b.sendOne(ctx)
			if err != nil {
				b.storageFailure(err)
				return
			}
			if sent {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-b.sendWake:
		}
	}
}
func (b *Bridge) sendOne(ctx context.Context) (bool, error) {
	p := b.getProvider()
	if p == nil || ctx.Err() != nil {
		return false, nil
	}
	records, err := b.Store.Latest("outbox")
	if err != nil {
		return false, err
	}
	var queued []model.Outbox
	for _, raw := range records {
		var item model.Outbox
		if err := json.Unmarshal(raw, &item); err != nil {
			return false, err
		}
		if item.State == "queued" {
			queued = append(queued, item)
		}
	}
	sort.Slice(queued, func(i, j int) bool { return queued[i].Created.Before(queued[j].Created) })
	blocked := make(map[string]bool)
	for _, candidate := range queued {
		if blocked[candidate.Request.ConversationID] {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var target provider.SendTarget
		var err error
		if candidate.Request.Kind != "conversation" {
			target, err = p.Prepare(callCtx, candidate.Request.ConversationID)
		}
		if err == nil {
			target, err = b.prepareMedia(callCtx, p, target, candidate.Request)
		}
		cancel()
		if ctx.Err() != nil {
			return false, nil
		}
		if errors.Is(err, provider.ErrRejected) {
			err = b.Store.RejectQueued(candidate.ID, "Conversation has no usable outgoing sender or is read-only")
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			if err != nil {
				return false, err
			}
			b.Hub.Notify()
			return true, nil
		}
		if errors.Is(err, ErrStorage) {
			return false, err
		}
		if err != nil {
			blocked[candidate.Request.ConversationID] = true
			continue
		}
		if b.PairingActive() {
			return false, nil
		}
		watermark, err := b.Store.HistoryWatermark()
		if err != nil {
			return false, err
		}
		o, err := b.claimSend(candidate.ID)
		if errors.Is(err, ErrPairing) {
			return false, nil
		}
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			return false, err
		}
		b.Hub.Notify()
		callCtx, cancel = context.WithTimeout(ctx, b.sendTimeout)
		switch o.Request.Kind {
		case "conversation":
			var snap provider.Snapshot
			snap, err = p.CreateConversation(callCtx, o.Request.Recipients)
			if err == nil {
				cancel()
				if err = b.Store.FinishConversation(o.ID, snap.Event, snap.Private, watermark); err != nil {
					return true, err
				}
				b.Hub.Notify()
				b.RequestSync()
				return true, nil
			}
		case "reaction":
			err = p.React(callCtx, target, o.Request.MessageID, o.Request.Emoji, o.Request.Remove)
		default:
			err = p.Send(callCtx, target, o)
		}
		cancel()
		state, detail := "accepted", "Google accepted the request; delivery is not confirmed"
		if o.Request.Kind == "reaction" {
			detail = "Google accepted the reaction update"
		}
		if errors.Is(err, provider.ErrRejected) {
			state, detail = "rejected", "Provider explicitly rejected the send request"
		} else if err != nil {
			state, detail = "ambiguous", "Send outcome unknown; inspect phone before sending again"
		}
		if err = b.Store.Finish(o.ID, state, detail); err != nil {
			return true, err
		}
		b.Hub.Notify()
		b.RequestSync()
		return true, nil
	}
	return false, nil
}
func (b *Bridge) syncLoop(ctx context.Context) {
	ticker := time.NewTicker(b.syncEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-b.syncWake:
		}
		if ctx.Err() != nil {
			return
		}
		if b.Status().State != "connected" {
			continue
		}
		b.mu.Lock()
		b.status.SyncState = "running"
		b.mu.Unlock()
		err := b.reconcile(ctx)
		b.mu.Lock()
		if err != nil {
			b.status.SyncState = "failed"
		} else {
			now := time.Now().UTC()
			b.status.SyncState = "recent_window_complete"
			b.status.LastSync = &now
		}
		b.mu.Unlock()
	}
}
func (b *Bridge) apply(snap provider.Snapshot, watermark uint64) error {
	added, err := b.Store.Apply(snap.Event, snap.Private, &watermark)
	if added {
		b.Hub.Notify()
	}
	if err != nil {
		b.storageFailure(err)
		return fmt.Errorf("%w: %v", ErrStorage, err)
	}
	return nil
}
func (b *Bridge) reconcile(ctx context.Context) error {
	p := b.getProvider()
	if p == nil {
		return provider.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	watermark, err := b.Store.HistoryWatermark()
	if err != nil {
		b.storageFailure(err)
		return err
	}
	callCtx, done := context.WithTimeout(ctx, 30*time.Second)
	convs, err := p.Conversations(callCtx)
	done()
	if err != nil {
		return err
	}
	for _, c := range convs {
		if err = b.apply(c, watermark); err != nil {
			return err
		}
		mark, err := b.Store.HistoryWatermark()
		if err != nil {
			b.storageFailure(err)
			return err
		}
		callCtx, done := context.WithTimeout(ctx, 30*time.Second)
		messages, err := p.Messages(callCtx, c.Event.EntityID)
		done()
		if err != nil {
			return err
		}
		for _, m := range messages {
			if err = b.apply(m, mark); err != nil {
				return err
			}
		}
	}
	return nil
}
func (b *Bridge) MarkRead(ctx context.Context, conv, id string) error {
	b.mutationMu.Lock()
	defer b.mutationMu.Unlock()
	if b.PairingActive() {
		return ErrPairing
	}
	if current, err := b.Store.EntityCurrent("message", id); err != nil {
		return err
	} else if !current {
		return ErrInvalid
	}
	raw, err := b.Store.Record("message", id)
	if err != nil {
		return err
	}
	var m model.Message
	if json.Unmarshal(raw, &m) != nil || m.ConversationID != conv {
		return ErrInvalid
	}
	p, ctx, release := b.borrowProvider(ctx)
	defer release()
	if p == nil || b.Status().State != "connected" {
		return provider.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = p.MarkRead(ctx, conv, id); err != nil {
		return provider.ErrUnavailable
	}
	b.RequestSync()
	return nil
}
func (b *Bridge) Attachment(ctx context.Context, id string) ([]byte, error) {
	if len(id) != 64 {
		return nil, ErrInvalid
	}
	data, err := b.Store.Media(id)
	if err == nil {
		return data, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	metadata, err := b.Store.Private(id)
	if err != nil {
		return nil, err
	}
	p, ctx, release := b.borrowProvider(ctx)
	defer release()
	if p == nil {
		return nil, provider.ErrUnavailable
	}
	data, err = p.Attachment(ctx, metadata)
	if err != nil {
		return nil, err
	}
	if len(data) > provider.MaxAttachmentBytes {
		return nil, ErrInvalid
	}
	if err = b.Store.PutMedia(id, data); err != nil {
		return nil, err
	}
	return data, nil
}

func (b *Bridge) claimSend(id string) (model.Outbox, error) {
	b.mutationMu.Lock()
	defer b.mutationMu.Unlock()
	if b.PairingActive() {
		return model.Outbox{}, ErrPairing
	}
	return b.Store.ClaimQueued(id)
}
