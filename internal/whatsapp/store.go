package whatsapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	local "github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
	wa "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/util/keys"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Credentials implements whatsmeow's stores in the bridge's encrypted database.
type Credentials struct {
	DB        *local.Store
	Namespace string
	Failure   func(error)
}

var _ wa.AllStores = (*Credentials)(nil)
var _ wa.DeviceContainer = (*Credentials)(nil)

func NewCredentials(db *local.Store, namespace string) *Credentials {
	return &Credentials{DB: db, Namespace: namespace}
}
func NewNamespace() string                        { var id [16]byte; rand.Read(id[:]); return hex.EncodeToString(id[:]) }
func (s *Credentials) key(kind, id string) string { return "wa:" + s.Namespace + ":" + kind + ":" + id }
func (s *Credentials) failed(err error) error {
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && s.Failure != nil {
		s.Failure(err)
	}
	return err
}
func (s *Credentials) txn(ctx context.Context, fn func(context.Context) error) error {
	var callbackErr error
	err := s.DB.NetworkTxn(ctx, func(ctx context.Context) error { callbackErr = fn(ctx); return callbackErr })
	if callbackErr != nil {
		return err
	}
	return s.failed(err)
}
func (s *Credentials) put(ctx context.Context, kind, id string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.failed(s.DB.NetworkPut(ctx, s.key(kind, id), data))
}
func (s *Credentials) get(ctx context.Context, kind, id string, value any) (bool, error) {
	data, err := s.DB.NetworkGet(ctx, s.key(kind, id))
	if err != nil || data == nil {
		return false, s.failed(err)
	}
	return true, s.failed(json.Unmarshal(data, value))
}
func (s *Credentials) del(ctx context.Context, kind, id string) error {
	return s.failed(s.DB.NetworkPut(ctx, s.key(kind, id), nil))
}
func (s *Credentials) scan(ctx context.Context, kind string) (map[string][]byte, error) {
	rows, err := s.DB.NetworkScan(ctx, s.key(kind, ""))
	out := map[string][]byte{}
	for k, v := range rows {
		out[strings.TrimPrefix(k, s.key(kind, ""))] = v
	}
	return out, s.failed(err)
}
func (s *Credentials) deletePrefix(ctx context.Context, kind, prefix string) error {
	return s.txn(ctx, func(ctx context.Context) error {
		rows, err := s.scan(ctx, kind)
		if err != nil {
			return err
		}
		for k := range rows {
			if strings.HasPrefix(k, prefix) {
				if err = s.del(ctx, kind, k); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
func compound(parts ...string) string { data, _ := json.Marshal(parts); return string(data) }

type deviceRecord struct {
	Noise, Identity                 *keys.KeyPair
	Signed                          *keys.PreKey
	Registration                    uint32
	Secret                          []byte
	ID                              *types.JID
	LID                             types.JID
	Account                         json.RawMessage
	Platform, Business, Push, Nonce string
	Migration                       int64
}

func (s *Credentials) Device(ctx context.Context) (*wa.Device, error) {
	var r deviceRecord
	exists, err := s.get(ctx, "device", "current", &r)
	if err != nil {
		return nil, err
	}
	d := &wa.Device{Log: waLog.Noop, Container: s, LIDs: s}
	d.SetAllStores(s)
	if !exists {
		var registration [4]byte
		rand.Read(registration[:])
		d.NoiseKey = keys.NewKeyPair()
		d.IdentityKey = keys.NewKeyPair()
		d.SignedPreKey = d.IdentityKey.CreateSignedPreKey(1)
		d.RegistrationID = uint32(registration[0])<<24 | uint32(registration[1])<<16 | uint32(registration[2])<<8 | uint32(registration[3])
		d.AdvSecretKey = make([]byte, 32)
		rand.Read(d.AdvSecretKey)
		return d, nil
	}
	d.NoiseKey, d.IdentityKey, d.SignedPreKey, d.RegistrationID, d.AdvSecretKey = r.Noise, r.Identity, r.Signed, r.Registration, r.Secret
	d.ID, d.LID, d.Platform, d.BusinessName, d.PushName = r.ID, r.LID, r.Platform, r.Business, r.Push
	d.CompanionMetaNonce, d.LIDMigrationTimestamp, d.Initialized = r.Nonce, r.Migration, true
	if len(r.Account) > 0 {
		if err = json.Unmarshal(r.Account, &d.Account); err != nil {
			return nil, err
		}
	}
	if d.NoiseKey == nil || d.IdentityKey == nil || d.SignedPreKey == nil {
		return nil, errors.New("invalid device keys")
	}
	return d, nil
}
func (s *Credentials) PutDevice(ctx context.Context, d *wa.Device) error {
	account, err := json.Marshal(d.Account)
	if err != nil {
		return err
	}
	d.Initialized = true
	return s.put(ctx, "device", "current", deviceRecord{d.NoiseKey, d.IdentityKey, d.SignedPreKey, d.RegistrationID, d.AdvSecretKey, d.ID, d.LID, account, d.Platform, d.BusinessName, d.PushName, d.CompanionMetaNonce, d.LIDMigrationTimestamp})
}
func (s *Credentials) DeleteDevice(ctx context.Context, d *wa.Device) error {
	return s.del(ctx, "device", "current")
}
func (s *Credentials) PutIdentity(ctx context.Context, a string, k [32]byte) error {
	return s.put(ctx, "identity", a, k)
}
func (s *Credentials) DeleteIdentity(ctx context.Context, a string) error {
	return s.del(ctx, "identity", a)
}
func (s *Credentials) DeleteAllIdentities(ctx context.Context, p string) error {
	return s.deletePrefix(ctx, "identity", p+":")
}
func (s *Credentials) IsTrustedIdentity(ctx context.Context, a string, k [32]byte) (bool, error) {
	var old [32]byte
	found, err := s.get(ctx, "identity", a, &old)
	return !found || old == k, err
}
func (s *Credentials) GetSession(ctx context.Context, a string) ([]byte, error) {
	var v []byte
	_, err := s.get(ctx, "session", a, &v)
	return v, err
}
func (s *Credentials) HasSession(ctx context.Context, a string) (bool, error) {
	v, err := s.GetSession(ctx, a)
	return v != nil, err
}
func (s *Credentials) PutSession(ctx context.Context, a string, v []byte) error {
	return s.put(ctx, "session", a, v)
}
func (s *Credentials) DeleteSession(ctx context.Context, a string) error {
	return s.del(ctx, "session", a)
}
func (s *Credentials) DeleteAllSessions(ctx context.Context, p string) error {
	return s.deletePrefix(ctx, "session", p+":")
}
func (s *Credentials) GetManySessions(ctx context.Context, addresses []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, a := range addresses {
		v, err := s.GetSession(ctx, a)
		if err != nil {
			return nil, err
		}
		if v != nil {
			out[a] = v
		}
	}
	return out, nil
}
func (s *Credentials) PutManySessions(ctx context.Context, values map[string][]byte) error {
	return s.txn(ctx, func(ctx context.Context) error {
		for k, v := range values {
			if err := s.PutSession(ctx, k, v); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Credentials) MigratePNToLID(ctx context.Context, pn, lid types.JID) error {
	return s.txn(ctx, func(ctx context.Context) error {
		for _, kind := range []string{"session", "identity", "sender"} {
			rows, err := s.scan(ctx, kind)
			if err != nil {
				return err
			}
			for k, v := range rows {
				next := k
				if kind == "sender" {
					var parts []string
					if err = json.Unmarshal([]byte(k), &parts); err != nil {
						return err
					}
					if len(parts) == 2 && strings.HasPrefix(parts[1], pn.SignalAddressUser()+":") {
						parts[1] = lid.SignalAddressUser() + strings.TrimPrefix(parts[1], pn.SignalAddressUser())
						next = compound(parts...)
					}
				} else if strings.HasPrefix(k, pn.SignalAddressUser()+":") {
					next = lid.SignalAddressUser() + strings.TrimPrefix(k, pn.SignalAddressUser())
				}
				if next == k {
					continue
				}
				if err = s.DB.NetworkPut(ctx, s.key(kind, next), v); err != nil {
					return err
				}
				if err = s.del(ctx, kind, k); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

type preKeyRecord struct {
	Key      *keys.PreKey
	Uploaded bool
}

func (s *Credentials) generate(ctx context.Context, uploaded bool) (*keys.PreKey, error) {
	var next uint32
	_, err := s.get(ctx, "meta", "prekey", &next)
	if err != nil {
		return nil, err
	}
	next++
	k := keys.NewPreKey(next)
	if err = s.put(ctx, "prekey", fmt.Sprint(next), preKeyRecord{k, uploaded}); err != nil {
		return nil, err
	}
	return k, s.put(ctx, "meta", "prekey", next)
}
func (s *Credentials) GenOnePreKey(ctx context.Context) (k *keys.PreKey, err error) {
	err = s.txn(ctx, func(ctx context.Context) error { k, err = s.generate(ctx, true); return err })
	return
}
func (s *Credentials) GetOrGenPreKeys(ctx context.Context, count uint32) (out []*keys.PreKey, err error) {
	err = s.txn(ctx, func(ctx context.Context) error {
		rows, e := s.scan(ctx, "prekey")
		if e != nil {
			return e
		}
		for _, raw := range rows {
			var r preKeyRecord
			if e = json.Unmarshal(raw, &r); e != nil {
				return e
			}
			if !r.Uploaded {
				out = append(out, r.Key)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].KeyID < out[j].KeyID })
		if uint32(len(out)) > count {
			out = out[:count]
		}
		for uint32(len(out)) < count {
			k, e := s.generate(ctx, false)
			if e != nil {
				return e
			}
			out = append(out, k)
		}
		return nil
	})
	return
}
func (s *Credentials) GetPreKey(ctx context.Context, id uint32) (*keys.PreKey, error) {
	var r preKeyRecord
	_, err := s.get(ctx, "prekey", fmt.Sprint(id), &r)
	return r.Key, err
}
func (s *Credentials) RemovePreKey(ctx context.Context, id uint32) error {
	return s.del(ctx, "prekey", fmt.Sprint(id))
}
func (s *Credentials) MarkPreKeysAsUploaded(ctx context.Context, id uint32) error {
	return s.txn(ctx, func(ctx context.Context) error {
		rows, err := s.scan(ctx, "prekey")
		if err != nil {
			return err
		}
		for k, raw := range rows {
			var r preKeyRecord
			if err = json.Unmarshal(raw, &r); err != nil {
				return err
			}
			if r.Key.KeyID <= id {
				r.Uploaded = true
				if err = s.put(ctx, "prekey", k, r); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
func (s *Credentials) UploadedPreKeyCount(ctx context.Context) (int, error) {
	rows, err := s.scan(ctx, "prekey")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, raw := range rows {
		var r preKeyRecord
		if err = json.Unmarshal(raw, &r); err != nil {
			return 0, err
		}
		if r.Uploaded {
			n++
		}
	}
	return n, nil
}
func (s *Credentials) PutSenderKey(ctx context.Context, g, u string, v []byte) error {
	return s.put(ctx, "sender", compound(g, u), v)
}
func (s *Credentials) GetSenderKey(ctx context.Context, g, u string) ([]byte, error) {
	var v []byte
	_, err := s.get(ctx, "sender", compound(g, u), &v)
	return v, err
}
