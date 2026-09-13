package store

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	bolt "go.etcd.io/bbolt"
)

type Event struct {
	ID       uint64          `json:"id"`
	Type     string          `json:"type"`
	EntityID string          `json:"entity_id,omitempty"`
	Time     time.Time       `json:"time"`
	Data     json.RawMessage `json:"data"`
}

type Store struct {
	db     *bolt.DB
	cipher cipher.AEAD
}

func Open(path string, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, errors.New("storage key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, cipher: aead}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"meta", "events", "latest", "versions", "outbox", "private", "media", "outbox-txn", "push"} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		versions := tx.Bucket([]byte("versions"))
		if versions.Sequence() < tx.Bucket([]byte("events")).Sequence() {
			if err := versions.SetSequence(tx.Bucket([]byte("events")).Sequence()); err != nil {
				return err
			}
		}
		b := tx.Bucket([]byte("meta"))
		if value := b.Get([]byte("key-check")); value != nil {
			_, err := s.decrypt(value, "key-check")
			return err
		}
		return b.Put([]byte("key-check"), s.encrypt([]byte("google-messages-multidevice-bridge-v1"), "key-check"))
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open encrypted store: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) encrypt(data []byte, context string) []byte {
	nonce := make([]byte, s.cipher.NonceSize())
	rand.Read(nonce)
	return s.cipher.Seal(nonce, nonce, data, []byte(context))
}

func (s *Store) decrypt(data []byte, context string) ([]byte, error) {
	n := s.cipher.NonceSize()
	if len(data) < n {
		return nil, errors.New("truncated encrypted record")
	}
	return s.cipher.Open(nil, data[:n], data[n:], []byte(context))
}

func (s *Store) SaveSession(data []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("meta")).Put([]byte("session"), s.encrypt(data, "session"))
	})
}

func (s *Store) Session() ([]byte, error) {
	var data []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte("meta")).Get([]byte("session"))
		if v == nil {
			return nil
		}
		var err error
		data, err = s.decrypt(v, "session")
		return err
	})
	return data, err
}

func sequence(id uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, id); return b }

// Append suppresses identical consecutive snapshots, but retains later changes.
func (s *Store) Append(event Event) (bool, error) {
	if event.Type == "" || event.EntityID == "" || !json.Valid(event.Data) {
		return false, errors.New("invalid event")
	}
	return s.appendConditional(event, nil)
}

// AppendIfUnchanged fences history against live observations made after
// HistoryWatermark, including observations suppressed by event deduplication.
func (s *Store) AppendIfUnchanged(event Event, watermark uint64) (bool, error) {
	return s.appendConditional(event, &watermark)
}
func (s *Store) appendConditional(event Event, watermark *uint64) (bool, error) {
	return s.Apply(event, nil, watermark)
}

// Apply commits the snapshot, private attachment metadata and any matching send
// observation together. watermark is captured before a history request begins.
func (s *Store) Apply(event Event, private map[string][]byte, watermark *uint64) (bool, error) {
	changed := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		key := event.Type + ":" + event.EntityID
		stale := false
		if watermark != nil {
			v := tx.Bucket([]byte("versions")).Get([]byte(key))
			stale = len(v) == 8 && binary.BigEndian.Uint64(v) > *watermark
		}
		if !stale {
			if err := stampEpoch(tx, event.Type, event.EntityID); err != nil {
				return err
			}
			var err error
			changed, err = s.appendTx(tx, event)
			if err != nil {
				return err
			}
			revision, err := tx.Bucket([]byte("versions")).NextSequence()
			if err != nil {
				return err
			}
			if err = tx.Bucket([]byte("versions")).Put([]byte(key), sequence(revision)); err != nil {
				return err
			}
			for id, data := range private {
				if err = tx.Bucket([]byte("private")).Put([]byte(id), s.encrypt(data, "private:"+id)); err != nil {
					return err
				}
			}
		}
		if event.Type == "message" {
			var m model.Message
			if err := json.Unmarshal(event.Data, &m); err != nil {
				return err
			}
			observed, err := s.confirmSendTx(tx, m)
			if err != nil {
				return err
			}
			changed = changed || observed
		}
		return nil
	})
	return changed, err
}
func (s *Store) appendTx(tx *bolt.Tx, event Event) (bool, error) {
	if event.Type == "" || event.EntityID == "" || !json.Valid(event.Data) {
		return false, errors.New("invalid event")
	}
	var canonical bytes.Buffer
	if err := json.Compact(&canonical, event.Data); err != nil {
		return false, err
	}
	event.Data = canonical.Bytes()
	latest := tx.Bucket([]byte("latest"))
	key := event.Type + ":" + event.EntityID
	if previous := latest.Get([]byte(key)); previous != nil {
		plain, err := s.decrypt(previous, key)
		if err != nil {
			return false, err
		}
		if bytes.Equal(plain, event.Data) {
			return false, nil
		}
	}
	b := tx.Bucket([]byte("events"))
	id, err := b.NextSequence()
	if err != nil {
		return false, err
	}
	event.ID, event.Time = id, time.Now().UTC()
	data, err := json.Marshal(event)
	if err != nil {
		return false, err
	}
	if err = b.Put(sequence(id), s.encrypt(data, fmt.Sprint("event:", id))); err != nil {
		return false, err
	}
	if err = latest.Put([]byte(key), s.encrypt(event.Data, key)); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) Events(after uint64, limit int) ([]Event, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("limit must be 1..1000")
	}
	result := make([]Event, 0)
	if after == math.MaxUint64 {
		return result, nil
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte("events")).Cursor()
		for k, v := c.Seek(sequence(after + 1)); k != nil && len(result) < limit; k, v = c.Next() {
			id := binary.BigEndian.Uint64(k)
			plain, err := s.decrypt(v, fmt.Sprint("event:", id))
			if err != nil {
				return err
			}
			var event Event
			if err := json.Unmarshal(plain, &event); err != nil {
				return err
			}
			result = append(result, event)
		}
		return nil
	})
	return result, err
}
