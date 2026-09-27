package store

import (
	"bytes"
	"context"
	"errors"

	bolt "go.etcd.io/bbolt"
)

type networkTransaction struct {
	owner *Store
	tx    *bolt.Tx
}
type networkTransactionKey struct{}

// NetworkTxn makes a provider's ratchet update and decrypted-event buffer atomic.
func (s *Store) NetworkTxn(ctx context.Context, fn func(context.Context) error) error {
	if t, ok := ctx.Value(networkTransactionKey{}).(networkTransaction); ok && t.owner == s {
		return fn(ctx)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists([]byte("network")); err != nil {
			return err
		}
		return fn(context.WithValue(ctx, networkTransactionKey{}, networkTransaction{s, tx}))
	})
}

func (s *Store) NetworkPut(ctx context.Context, key string, value []byte) error {
	return s.NetworkTxn(ctx, func(ctx context.Context) error {
		tx := ctx.Value(networkTransactionKey{}).(networkTransaction).tx
		b := tx.Bucket([]byte("network"))
		if value == nil {
			return b.Delete([]byte(key))
		}
		return b.Put([]byte(key), s.encrypt(value, "network:"+key))
	})
}

func (s *Store) NetworkGet(ctx context.Context, key string) ([]byte, error) {
	var value []byte
	read := func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("network"))
		if b == nil {
			return nil
		}
		raw := b.Get([]byte(key))
		if raw == nil {
			return nil
		}
		var err error
		value, err = s.decrypt(raw, "network:"+key)
		return err
	}
	if t, ok := ctx.Value(networkTransactionKey{}).(networkTransaction); ok && t.owner == s {
		err := read(t.tx)
		return value, err
	}
	err := s.db.View(read)
	return value, err
}

func (s *Store) NetworkScan(ctx context.Context, prefix string) (map[string][]byte, error) {
	result := map[string][]byte{}
	read := func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("network"))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.Seek([]byte(prefix)); k != nil && bytes.HasPrefix(k, []byte(prefix)); k, v = c.Next() {
			plain, err := s.decrypt(v, "network:"+string(k))
			if err != nil {
				return err
			}
			result[string(k)] = plain
		}
		return nil
	}
	if t, ok := ctx.Value(networkTransactionKey{}).(networkTransaction); ok && t.owner == s {
		return result, read(t.tx)
	}
	err := s.db.View(read)
	return result, err
}

// BindNetwork prevents opening a Google database as a different network.
func (s *Store) BindNetwork(network string) error {
	return s.NetworkTxn(context.Background(), func(ctx context.Context) error {
		old, err := s.NetworkGet(ctx, "network")
		if err != nil {
			return err
		}
		if len(old) == 0 {
			tx := ctx.Value(networkTransactionKey{}).(networkTransaction).tx
			if network != "google-messages" && (tx.Bucket([]byte("meta")).Get([]byte("session")) != nil || tx.Bucket([]byte("events")).Sequence() > 0) {
				return errors.New("existing database belongs to google-messages")
			}
			return s.NetworkPut(ctx, "network", []byte(network))
		}
		if string(old) != network {
			return errors.New("database network does not match --network")
		}
		return nil
	})
}
