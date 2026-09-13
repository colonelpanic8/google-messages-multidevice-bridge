package store

import (
	"encoding/json"

	bolt "go.etcd.io/bbolt"
)

// PushKeys is the server's VAPID identity. The private key never leaves the
// database; clients only ever receive PublicKey.
type PushKeys struct {
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
}

// EnsurePushKeys returns the stored VAPID pair, generating one exactly once.
// generate is only called inside the write transaction, so concurrent callers
// cannot produce competing identities.
func (s *Store) EnsurePushKeys(generate func() (PushKeys, error)) (PushKeys, error) {
	var keys PushKeys
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("meta"))
		if raw := bucket.Get([]byte("vapid")); raw != nil {
			data, err := s.decrypt(raw, "vapid")
			if err != nil {
				return err
			}
			return json.Unmarshal(data, &keys)
		}
		fresh, err := generate()
		if err != nil {
			return err
		}
		data, err := json.Marshal(fresh)
		if err != nil {
			return err
		}
		if err = bucket.Put([]byte("vapid"), s.encrypt(data, "vapid")); err != nil {
			return err
		}
		keys = fresh
		return nil
	})
	return keys, err
}

// PutSubscription stores one browser push subscription. The id is derived from
// the endpoint by the caller, so re-subscribing a browser replaces its record
// instead of accumulating duplicates.
func (s *Store) PutSubscription(id string, data []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("push")).Put([]byte(id), s.encrypt(data, recordContext("push", id)))
	})
}

func (s *Store) DeleteSubscription(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("push")).Delete([]byte(id))
	})
}

// Subscriptions returns every stored subscription keyed by id.
func (s *Store) Subscriptions() (map[string][]byte, error) {
	out := map[string][]byte{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("push")).ForEach(func(k, v []byte) error {
			data, err := s.decrypt(v, recordContext("push", string(k)))
			if err != nil {
				return err
			}
			out[string(k)] = data
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
