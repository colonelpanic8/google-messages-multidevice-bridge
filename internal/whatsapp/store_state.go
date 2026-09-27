package whatsapp

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"time"

	wa "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

func (s *Credentials) PutAppStateSyncKey(ctx context.Context, id []byte, key wa.AppStateSyncKey) error {
	return s.put(ctx, "appkey", hex.EncodeToString(id), key)
}
func (s *Credentials) GetAppStateSyncKey(ctx context.Context, id []byte) (*wa.AppStateSyncKey, error) {
	var v wa.AppStateSyncKey
	ok, err := s.get(ctx, "appkey", hex.EncodeToString(id), &v)
	if !ok {
		return nil, err
	}
	return &v, err
}
func (s *Credentials) GetLatestAppStateSyncKeyID(ctx context.Context) ([]byte, error) {
	rows, err := s.scan(ctx, "appkey")
	if err != nil {
		return nil, err
	}
	var newest int64
	var id string
	for k, raw := range rows {
		var v wa.AppStateSyncKey
		if err = json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		if id == "" || v.Timestamp > newest {
			newest = v.Timestamp
			id = k
		}
	}
	return hex.DecodeString(id)
}
func (s *Credentials) GetAllAppStateSyncKeys(ctx context.Context) ([]*wa.AppStateSyncKey, error) {
	rows, err := s.scan(ctx, "appkey")
	if err != nil {
		return nil, err
	}
	out := make([]*wa.AppStateSyncKey, 0, len(rows))
	for _, raw := range rows {
		var v wa.AppStateSyncKey
		if err = json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		out = append(out, &v)
	}
	return out, nil
}

type appVersion struct {
	Version uint64
	Hash    [128]byte
}

func (s *Credentials) PutAppStateVersion(ctx context.Context, n string, v uint64, h [128]byte) error {
	return s.put(ctx, "appversion", n, appVersion{v, h})
}
func (s *Credentials) GetAppStateVersion(ctx context.Context, n string) (uint64, [128]byte, error) {
	var v appVersion
	_, err := s.get(ctx, "appversion", n, &v)
	return v.Version, v.Hash, err
}
func (s *Credentials) DeleteAppStateVersion(ctx context.Context, n string) error {
	return s.txn(ctx, func(ctx context.Context) error {
		if err := s.del(ctx, "appversion", n); err != nil {
			return err
		}
		return s.deletePrefix(ctx, "mac", n+":")
	})
}
func (s *Credentials) PutAppStateMutationMACs(ctx context.Context, n string, v uint64, mutations []wa.AppStateMutationMAC) error {
	return s.txn(ctx, func(ctx context.Context) error {
		for _, m := range mutations {
			if err := s.put(ctx, "mac", n+":"+hex.EncodeToString(m.IndexMAC), m.ValueMAC); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Credentials) DeleteAppStateMutationMACs(ctx context.Context, n string, ids [][]byte) error {
	return s.txn(ctx, func(ctx context.Context) error {
		for _, id := range ids {
			if err := s.del(ctx, "mac", n+":"+hex.EncodeToString(id)); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Credentials) GetAppStateMutationMAC(ctx context.Context, n string, id []byte) ([]byte, error) {
	var v []byte
	_, err := s.get(ctx, "mac", n+":"+hex.EncodeToString(id), &v)
	return v, err
}

func (s *Credentials) updateContact(ctx context.Context, j types.JID, fn func(*types.ContactInfo)) error {
	return s.txn(ctx, func(ctx context.Context) error {
		v, err := s.GetContact(ctx, j)
		if err != nil {
			return err
		}
		fn(&v)
		v.Found = true
		return s.put(ctx, "contact", j.ToNonAD().String(), v)
	})
}
func (s *Credentials) PutPushName(ctx context.Context, j types.JID, n string) (changed bool, old string, err error) {
	err = s.updateContact(ctx, j, func(v *types.ContactInfo) { old = v.PushName; changed = old != n; v.PushName = n })
	return
}
func (s *Credentials) PutBusinessName(ctx context.Context, j types.JID, n string) (changed bool, old string, err error) {
	err = s.updateContact(ctx, j, func(v *types.ContactInfo) { old = v.BusinessName; changed = old != n; v.BusinessName = n })
	return
}
func (s *Credentials) PutContactName(ctx context.Context, j types.JID, first, full string) error {
	return s.updateContact(ctx, j, func(v *types.ContactInfo) { v.FullName = full; v.FirstName = first })
}
func (s *Credentials) PutAllContactNames(ctx context.Context, entries []wa.ContactEntry) error {
	return s.txn(ctx, func(ctx context.Context) error {
		for _, e := range entries {
			if err := s.PutContactName(ctx, e.JID, e.FirstName, e.FullName); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Credentials) PutManyRedactedPhones(ctx context.Context, entries []wa.RedactedPhoneEntry) error {
	return s.txn(ctx, func(ctx context.Context) error {
		for _, e := range entries {
			if err := s.updateContact(ctx, e.JID, func(v *types.ContactInfo) { v.RedactedPhone = e.RedactedPhone }); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Credentials) GetContact(ctx context.Context, j types.JID) (types.ContactInfo, error) {
	var v types.ContactInfo
	_, err := s.get(ctx, "contact", j.ToNonAD().String(), &v)
	return v, err
}
func (s *Credentials) GetAllContacts(ctx context.Context) (map[types.JID]types.ContactInfo, error) {
	rows, err := s.scan(ctx, "contact")
	if err != nil {
		return nil, err
	}
	out := map[types.JID]types.ContactInfo{}
	for k, raw := range rows {
		j, err := types.ParseJID(k)
		if err != nil {
			return nil, err
		}
		var v types.ContactInfo
		if err = json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		out[j] = v
	}
	return out, nil
}
func (s *Credentials) updateChat(ctx context.Context, j types.JID, fn func(*types.LocalChatSettings)) error {
	j = normalizeJID(j)
	if j.Server == types.DefaultUserServer {
		lid, err := s.GetLIDForPN(ctx, j)
		if err != nil {
			return err
		}
		if !lid.IsEmpty() {
			j = lid
		}
	}
	return s.txn(ctx, func(ctx context.Context) error {
		v, err := s.GetChatSettings(ctx, j)
		if err != nil {
			return err
		}
		fn(&v)
		v.Found = true
		return s.put(ctx, "chat", j.String(), v)
	})
}
func (s *Credentials) PutMutedUntil(ctx context.Context, j types.JID, t time.Time) error {
	return s.updateChat(ctx, j, func(v *types.LocalChatSettings) { v.MutedUntil = t })
}
func (s *Credentials) PutPinned(ctx context.Context, j types.JID, b bool) error {
	return s.updateChat(ctx, j, func(v *types.LocalChatSettings) { v.Pinned = b })
}
func (s *Credentials) PutArchived(ctx context.Context, j types.JID, b bool) error {
	return s.updateChat(ctx, j, func(v *types.LocalChatSettings) { v.Archived = b })
}
func (s *Credentials) GetChatSettings(ctx context.Context, j types.JID) (types.LocalChatSettings, error) {
	var v types.LocalChatSettings
	aliases, err := s.aliases(ctx, j)
	if err != nil {
		return v, err
	}
	for _, alias := range aliases {
		ok, err := s.get(ctx, "chat", alias.String(), &v)
		if err != nil || ok {
			return v, err
		}
	}
	return v, nil
}

func (s *Credentials) PutManyLIDMappings(ctx context.Context, mappings []wa.LIDMapping) error {
	return s.txn(ctx, func(ctx context.Context) error {
		for _, m := range mappings {
			if err := s.PutLIDMapping(ctx, m.LID, m.PN); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Credentials) PutLIDMapping(ctx context.Context, lid, pn types.JID) error {
	lid, pn = normalizeJID(lid), normalizeJID(pn)
	if lid.Server != types.HiddenUserServer || pn.Server != types.DefaultUserServer {
		return nil
	}
	return s.txn(ctx, func(ctx context.Context) error {
		oldPN, err := s.GetPNForLID(ctx, lid)
		if err != nil {
			return err
		}
		oldLID, err := s.GetLIDForPN(ctx, pn)
		if err != nil {
			return err
		}
		if !oldPN.IsEmpty() && oldPN != pn {
			if err = s.del(ctx, "pn", oldPN.String()); err != nil {
				return err
			}
		}
		if !oldLID.IsEmpty() && oldLID != lid {
			if err = s.del(ctx, "lid", oldLID.String()); err != nil {
				return err
			}
		}
		if err := s.put(ctx, "lid", lid.ToNonAD().String(), pn.ToNonAD()); err != nil {
			return err
		}
		return s.put(ctx, "pn", pn.ToNonAD().String(), lid.ToNonAD())
	})
}
func (s *Credentials) GetPNForLID(ctx context.Context, j types.JID) (types.JID, error) {
	var v types.JID
	_, err := s.get(ctx, "lid", normalizeJID(j).String(), &v)
	if !v.IsEmpty() {
		v.Device = j.Device
	}
	return v, err
}
func (s *Credentials) GetLIDForPN(ctx context.Context, j types.JID) (types.JID, error) {
	var v types.JID
	_, err := s.get(ctx, "pn", normalizeJID(j).String(), &v)
	if !v.IsEmpty() {
		v.Device = j.Device
	}
	return v, err
}
func (s *Credentials) GetManyLIDsForPNs(ctx context.Context, js []types.JID) (map[types.JID]types.JID, error) {
	out := map[types.JID]types.JID{}
	for _, j := range js {
		v, err := s.GetLIDForPN(ctx, j)
		if err != nil {
			return nil, err
		}
		if !v.IsEmpty() {
			out[j] = v
		}
	}
	return out, nil
}

func (s *Credentials) PutMessageSecrets(ctx context.Context, entries []wa.MessageSecretInsert) error {
	return s.txn(ctx, func(ctx context.Context) error {
		for _, e := range entries {
			if err := s.PutMessageSecret(ctx, e.Chat, e.Sender, e.ID, e.Secret); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Credentials) PutMessageSecret(ctx context.Context, c, u types.JID, id types.MessageID, secret []byte) error {
	return s.txn(ctx, func(ctx context.Context) error {
		key := compound(c.ToNonAD().String(), u.ToNonAD().String(), id)
		var existing []byte
		found, err := s.get(ctx, "msgsecret", key, &existing)
		if err != nil || found {
			return err
		}
		return s.put(ctx, "msgsecret", key, secret)
	})
}
func (s *Credentials) aliases(ctx context.Context, j types.JID) ([]types.JID, error) {
	out := []types.JID{j.ToNonAD()}
	var alt types.JID
	var err error
	if j.Server == types.DefaultUserServer {
		alt, err = s.GetLIDForPN(ctx, j)
	} else if j.Server == types.HiddenUserServer {
		alt, err = s.GetPNForLID(ctx, j)
	}
	if !alt.IsEmpty() {
		out = append(out, alt.ToNonAD())
	}
	return out, err
}
func (s *Credentials) GetMessageSecret(ctx context.Context, c, u types.JID, id types.MessageID) ([]byte, types.JID, error) {
	chats, err := s.aliases(ctx, c)
	if err != nil {
		return nil, types.EmptyJID, err
	}
	senders, err := s.aliases(ctx, u)
	if err != nil {
		return nil, types.EmptyJID, err
	}
	for _, chat := range chats {
		for _, sender := range senders {
			var v []byte
			ok, err := s.get(ctx, "msgsecret", compound(chat.String(), sender.String(), id), &v)
			if err != nil || ok {
				return v, sender, err
			}
		}
	}
	return nil, types.EmptyJID, nil
}
func (s *Credentials) PutPrivacyTokens(ctx context.Context, entries ...wa.PrivacyToken) error {
	return s.txn(ctx, func(ctx context.Context) error {
		for _, e := range entries {
			old, err := s.GetPrivacyToken(ctx, e.User)
			if err != nil {
				return err
			}
			if old != nil {
				if old.Timestamp.After(e.Timestamp) {
					continue
				}
				if e.SenderTimestamp.IsZero() {
					e.SenderTimestamp = old.SenderTimestamp
				}
			}
			if err = s.put(ctx, "privacy", e.User.ToNonAD().String(), e); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Credentials) GetPrivacyToken(ctx context.Context, j types.JID) (*wa.PrivacyToken, error) {
	aliases, err := s.aliases(ctx, j)
	if err != nil {
		return nil, err
	}
	var best *wa.PrivacyToken
	for _, alias := range aliases {
		var v wa.PrivacyToken
		ok, err := s.get(ctx, "privacy", alias.String(), &v)
		if err != nil {
			return nil, err
		}
		if ok && (best == nil || v.Timestamp.After(best.Timestamp)) {
			best = &v
		}
	}
	return best, nil
}
func (s *Credentials) DeleteExpiredPrivacyTokens(ctx context.Context, cutoff time.Time) (n int64, err error) {
	err = s.txn(ctx, func(ctx context.Context) error {
		rows, e := s.scan(ctx, "privacy")
		if e != nil {
			return e
		}
		for k, raw := range rows {
			var v wa.PrivacyToken
			if e = json.Unmarshal(raw, &v); e != nil {
				return e
			}
			if v.Timestamp.Before(cutoff) {
				if e = s.del(ctx, "privacy", k); e != nil {
					return e
				}
				n++
			}
		}
		return nil
	})
	return
}
func (s *Credentials) PutNCTSalt(ctx context.Context, v []byte) error {
	return s.put(ctx, "meta", "salt", v)
}
func (s *Credentials) GetNCTSalt(ctx context.Context) ([]byte, error) {
	var v []byte
	_, err := s.get(ctx, "meta", "salt", &v)
	return v, err
}
func (s *Credentials) DeleteNCTSalt(ctx context.Context) error { return s.del(ctx, "meta", "salt") }
func (s *Credentials) DoDecryptionTxn(ctx context.Context, fn func(context.Context) error) error {
	return s.txn(ctx, fn)
}
func (s *Credentials) GetBufferedEvent(ctx context.Context, h [32]byte) (*wa.BufferedEvent, error) {
	var v wa.BufferedEvent
	ok, err := s.get(ctx, "buffer", hex.EncodeToString(h[:]), &v)
	if !ok {
		return nil, err
	}
	return &v, err
}
func (s *Credentials) PutBufferedEvent(ctx context.Context, h [32]byte, plain []byte, t time.Time) error {
	return s.put(ctx, "buffer", hex.EncodeToString(h[:]), wa.BufferedEvent{Plaintext: plain, InsertTime: time.Now(), ServerTime: t})
}
func (s *Credentials) ClearBufferedEventPlaintext(ctx context.Context, h [32]byte) error {
	return s.txn(ctx, func(ctx context.Context) error {
		v, err := s.GetBufferedEvent(ctx, h)
		if err != nil || v == nil {
			return err
		}
		v.Plaintext = nil
		return s.put(ctx, "buffer", hex.EncodeToString(h[:]), v)
	})
}
func (s *Credentials) DeleteOldBufferedHashes(ctx context.Context) error {
	return s.txn(ctx, func(ctx context.Context) error {
		rows, err := s.scan(ctx, "buffer")
		if err != nil {
			return err
		}
		for k, raw := range rows {
			var v wa.BufferedEvent
			if err = json.Unmarshal(raw, &v); err != nil {
				return err
			}
			if time.Since(v.InsertTime) > 7*24*time.Hour {
				if err = s.del(ctx, "buffer", k); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

type outgoingEvent struct {
	Format    string
	Plaintext []byte
	Time      time.Time
}

func (s *Credentials) AddOutgoingEvent(ctx context.Context, j types.JID, id types.MessageID, format string, plain []byte) error {
	return s.put(ctx, "outgoing", compound(j.ToNonAD().String(), id), outgoingEvent{format, plain, time.Now()})
}
func (s *Credentials) GetOutgoingEvent(ctx context.Context, j, alt types.JID, id types.MessageID) (string, []byte, error) {
	var v outgoingEvent
	ok, err := s.get(ctx, "outgoing", compound(j.ToNonAD().String(), id), &v)
	if err == nil && !ok && !alt.IsEmpty() {
		_, err = s.get(ctx, "outgoing", compound(alt.ToNonAD().String(), id), &v)
	}
	return v.Format, v.Plaintext, err
}
func (s *Credentials) DeleteOldOutgoingEvents(ctx context.Context) error {
	return s.txn(ctx, func(ctx context.Context) error {
		rows, err := s.scan(ctx, "outgoing")
		if err != nil {
			return err
		}
		for k, raw := range rows {
			var v outgoingEvent
			if err = json.Unmarshal(raw, &v); err != nil {
				return err
			}
			if time.Since(v.Time) > 7*24*time.Hour {
				if err = s.del(ctx, "outgoing", k); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
