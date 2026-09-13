package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
)

var phoneNumber = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

func validateRequest(req *model.SendRequest) error {
	if req.Kind == "message" {
		req.Kind = ""
	}
	if req.Kind == "conversation" {
		if req.ConversationID != "" || req.Text != "" || len(req.AttachmentIDs) > 0 || req.MessageID != "" || req.Emoji != "" || req.Remove || len(req.Recipients) < 1 || len(req.Recipients) > 20 {
			return ErrInvalid
		}
		req.Recipients = append([]string(nil), req.Recipients...)
		sort.Strings(req.Recipients)
		for i, number := range req.Recipients {
			if !phoneNumber.MatchString(number) || (i > 0 && number == req.Recipients[i-1]) {
				return ErrInvalid
			}
		}
		return nil
	}
	if req.ConversationID == "" || len(req.ConversationID) > 256 || len(req.Recipients) > 0 {
		return ErrInvalid
	}
	if req.Kind == "reaction" {
		if req.MessageID == "" || len(req.MessageID) > 256 || strings.TrimSpace(req.Emoji) == "" || len(req.Emoji) > 64 || !utf8.ValidString(req.Emoji) || req.Text != "" || len(req.AttachmentIDs) > 0 {
			return ErrInvalid
		}
		return nil
	}
	if req.Kind != "" || req.MessageID != "" || req.Emoji != "" || req.Remove || len(req.Text) > 16000 || !utf8.ValidString(req.Text) || len(req.AttachmentIDs) > 10 {
		return ErrInvalid
	}
	// The phone drops media when text rides in the same send, so a message
	// carries either text or attachments; clients queue a caption separately.
	if (strings.TrimSpace(req.Text) == "") == (len(req.AttachmentIDs) == 0) {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, id := range req.AttachmentIDs {
		if len(id) != 64 || seen[id] {
			return ErrInvalid
		}
		seen[id] = true
	}
	return nil
}
func (b *Bridge) upload(id string) (model.Upload, error) {
	var u model.Upload
	raw, err := b.Store.Record("upload", id)
	if err != nil {
		return u, err
	}
	err = json.Unmarshal(raw, &u)
	return u, err
}
func (b *Bridge) SaveUpload(name, contentType string, data []byte) (model.Upload, error) {
	var u model.Upload
	typ, _, err := mime.ParseMediaType(contentType)
	if err != nil || name == "" || len(name) > 255 || strings.ContainsAny(name, "\r\n\x00") || len(data) == 0 {
		return u, ErrInvalid
	}
	if len(data) > provider.MaxAttachmentBytes {
		return u, provider.ErrTooLarge
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	raw, _ := json.Marshal([]string{name, typ})
	sum := sha256.Sum256(append(raw, data...))
	id := hex.EncodeToString(sum[:])
	if existing, err := b.upload(id); err == nil {
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return u, err
	}
	u = model.Upload{Schema: model.Schema, ID: id, Name: name, MIME: typ, Size: int64(len(data)), Created: time.Now().UTC()}
	err = b.Store.SaveUpload(u, data)
	if err == nil {
		b.Hub.Notify()
	}
	return u, err
}
func (b *Bridge) prepareMedia(ctx context.Context, p provider.Provider, target provider.SendTarget, req model.SendRequest) (provider.SendTarget, error) {
	for _, id := range req.AttachmentIDs {
		raw, err := b.Store.Private("upload:" + id)
		if errors.Is(err, store.ErrNotFound) {
			u, err := b.upload(id)
			if err != nil {
				return target, fmt.Errorf("%w: %v", ErrStorage, err)
			}
			data, err := b.Store.Media("upload:" + id)
			if err != nil {
				return target, fmt.Errorf("%w: %v", ErrStorage, err)
			}
			raw, err = p.Upload(ctx, data, u.Name, u.MIME)
			if err != nil {
				if errors.Is(err, provider.ErrRejected) || errors.Is(err, provider.ErrTooLarge) {
					return target, provider.ErrRejected
				}
				return target, provider.ErrUnavailable
			}
			if err = b.Store.PutPrivate("upload:"+id, raw); err != nil {
				return target, fmt.Errorf("%w: %v", ErrStorage, err)
			}
		} else if err != nil {
			return target, fmt.Errorf("%w: %v", ErrStorage, err)
		}
		target.Media = append(target.Media, raw)
	}
	return target, nil
}
func (b *Bridge) Typing(ctx context.Context, conv string) error {
	b.mutationMu.Lock()
	defer b.mutationMu.Unlock()
	if b.PairingActive() {
		return ErrPairing
	}
	if current, err := b.Store.EntityCurrent("conversation", conv); err != nil {
		return err
	} else if !current {
		return ErrInvalid
	}
	if conv == "" || len(conv) > 256 {
		return ErrInvalid
	}
	if _, err := b.Store.Record("conversation", conv); err != nil {
		return err
	}
	p, ctx, release := b.borrowProvider(ctx)
	defer release()
	if p == nil || b.Status().State != "connected" {
		return provider.ErrUnavailable
	}
	b.mu.Lock()
	now := time.Now()
	if last := b.typingSent[conv]; now.Sub(last) < 4*time.Second {
		b.mu.Unlock()
		return nil
	}
	if b.typingSent == nil {
		b.typingSent = make(map[string]time.Time)
	}
	b.typingSent[conv] = now
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	target, err := p.Prepare(ctx, conv)
	if err != nil {
		return provider.ErrUnavailable
	}
	return p.Typing(ctx, target)
}
