package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	bolt "go.etcd.io/bbolt"
)

var ErrConflict = errors.New("idempotency key already used for a different request")
var ErrNotFound = errors.New("record not found")

// HistoryWatermark tracks observations, including identical live snapshots.
// It is intentionally separate from the public durable-event cursor.
func (s *Store) HistoryWatermark() (uint64, error) {
	var n uint64
	err := s.db.View(func(tx *bolt.Tx) error { n = tx.Bucket([]byte("versions")).Sequence(); return nil })
	return n, err
}
func (s *Store) Watermark() (uint64, error) {
	var n uint64
	err := s.db.View(func(tx *bolt.Tx) error { n = tx.Bucket([]byte("events")).Sequence(); return nil })
	return n, err
}
func (s *Store) Latest(kind string) ([]json.RawMessage, error) {
	result := make([]json.RawMessage, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte("latest")).Cursor()
		prefix := []byte(kind + ":")
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			plain, err := s.decrypt(v, string(k))
			if err != nil {
				return err
			}
			result = append(result, json.RawMessage(plain))
		}
		return nil
	})
	return result, err
}
func (s *Store) Record(kind, id string) (json.RawMessage, error) {
	return s.get("latest", kind+":"+id)
}
func recordContext(bucket, key string) string {
	if bucket == "latest" {
		return key
	}
	return bucket + ":" + key
}
func (s *Store) get(bucket, key string) ([]byte, error) {
	var result []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucket)).Get([]byte(key))
		if v == nil {
			return ErrNotFound
		}
		var err error
		result, err = s.decrypt(v, recordContext(bucket, key))
		return err
	})
	return result, err
}
func (s *Store) PutPrivate(id string, data []byte) error { return s.put("private", id, data) }
func (s *Store) Private(id string) ([]byte, error)       { return s.get("private", id) }
func (s *Store) Media(id string) ([]byte, error)         { return s.get("media", id) }
func (s *Store) PutMedia(id string, data []byte) error   { return s.put("media", id, data) }
func (s *Store) put(bucket, key string, data []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucket)).Put([]byte(key), s.encrypt(data, recordContext(bucket, key)))
	})
}
func (s *Store) readOutbox(tx *bolt.Tx, key []byte) (model.Outbox, error) {
	var o model.Outbox
	v := tx.Bucket([]byte("outbox")).Get(key)
	if v == nil {
		return o, ErrNotFound
	}
	p, err := s.decrypt(v, "outbox:"+string(key))
	if err != nil {
		return o, err
	}
	err = json.Unmarshal(p, &o)
	return o, err
}
func (s *Store) writeOutbox(tx *bolt.Tx, o model.Outbox) error {
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	if err = tx.Bucket([]byte("outbox")).Put([]byte(o.ID), s.encrypt(data, "outbox:"+o.ID)); err != nil {
		return err
	}
	_, err = s.appendTx(tx, Event{Type: "outbox", EntityID: o.ID, Data: data})
	return err
}
func (s *Store) Enqueue(id, transactionID string, req model.SendRequest) (model.Outbox, bool, error) {
	var o model.Outbox
	created := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		o, err = s.readOutbox(tx, []byte(id))
		if err == nil {
			if o.Request != req {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		now := time.Now().UTC()
		o = model.Outbox{Schema: model.Schema, ID: id, Request: req, TransactionID: transactionID, State: "queued", Created: now, Updated: now}
		created = true
		if err := tx.Bucket([]byte("outbox-txn")).Put([]byte(transactionID), []byte(id)); err != nil {
			return err
		}
		return s.writeOutbox(tx, o)
	})
	return o, created, err
}

// Claim commits sending before any provider operation. Only queued records are eligible.
func (s *Store) Claim() (*model.Outbox, error) {
	var result *model.Outbox
	err := s.db.Update(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte("outbox")).Cursor()
		var oldest *model.Outbox
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			o, err := s.readOutbox(tx, k)
			if err != nil {
				return err
			}
			if o.State == "queued" && (oldest == nil || o.Created.Before(oldest.Created)) {
				oldest = &o
			}
		}
		if oldest == nil {
			return nil
		}
		oldest.State, oldest.Updated = "sending", time.Now().UTC()
		if err := s.writeOutbox(tx, *oldest); err != nil {
			return err
		}
		result = oldest
		return nil
	})
	return result, err
}
func (s *Store) Finish(id, state, detail string) error {
	if state != "accepted" && state != "rejected" && state != "ambiguous" {
		return errors.New("invalid send result state")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		o, err := s.readOutbox(tx, []byte(id))
		if err != nil {
			return err
		}
		if o.State != "sending" {
			return nil
		} // A matching echo may already have arrived.
		o.State, o.Detail, o.Updated = state, detail, time.Now().UTC()
		return s.writeOutbox(tx, o)
	})
}
func (s *Store) RecoverSending() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var records []model.Outbox
		err := tx.Bucket([]byte("outbox")).ForEach(func(k, v []byte) error {
			o, err := s.readOutbox(tx, k)
			if err != nil {
				return err
			}
			if err := tx.Bucket([]byte("outbox-txn")).Put([]byte(o.TransactionID), []byte(o.ID)); err != nil {
				return err
			}
			if o.State == "sending" {
				o.State = "ambiguous"
				o.Detail = "Service stopped during send; inspect phone before sending again"
				o.Updated = time.Now().UTC()
				records = append(records, o)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, o := range records {
			if err := s.writeOutbox(tx, o); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Store) confirmSendTx(tx *bolt.Tx, m model.Message) (bool, error) {
	if m.TransactionID == "" || m.ID == "" {
		return false, nil
	}
	id := tx.Bucket([]byte("outbox-txn")).Get([]byte(m.TransactionID))
	if id == nil {
		return false, nil
	}
	o, err := s.readOutbox(tx, id)
	if err != nil {
		return false, err
	}
	if o.Request.ConversationID != m.ConversationID || o.State == "queued" || o.State == "canceled" || o.State == "confirmed" {
		return false, nil
	}
	o.State, o.MessageID, o.Detail, o.Updated = "confirmed", m.ID, "Observed in Google history; see message status for delivery", time.Now().UTC()
	return true, s.writeOutbox(tx, o)
}
func (s *Store) Outbox(id string) (model.Outbox, error) {
	var o model.Outbox
	err := s.db.View(func(tx *bolt.Tx) error { var err error; o, err = s.readOutbox(tx, []byte(id)); return err })
	return o, err
}
func (s *Store) CancelQueued(id string) (model.Outbox, error) {
	var o model.Outbox
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		o, err = s.readOutbox(tx, []byte(id))
		if err != nil {
			return err
		}
		if o.State == "canceled" {
			return nil
		}
		if o.State != "queued" {
			return ErrConflict
		}
		o.State, o.Updated = "canceled", time.Now().UTC()
		return s.writeOutbox(tx, o)
	})
	return o, err
}

// Snapshot and its cursor are from one read transaction. Clients apply events
// after this cursor to avoid missing updates while loading their initial view.
func (s *Store) Snapshot(kind string) ([]json.RawMessage, uint64, error) {
	result := make([]json.RawMessage, 0)
	var watermark uint64
	err := s.db.View(func(tx *bolt.Tx) error {
		watermark = tx.Bucket([]byte("events")).Sequence()
		c := tx.Bucket([]byte("latest")).Cursor()
		prefix := []byte(kind + ":")
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			plain, err := s.decrypt(v, string(k))
			if err != nil {
				return err
			}
			result = append(result, plain)
		}
		return nil
	})
	return result, watermark, err
}

// ClaimQueued is the mutation boundary after read-only provider preparation.
func (s *Store) ClaimQueued(id string) (model.Outbox, error) {
	var o model.Outbox
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		o, err = s.readOutbox(tx, []byte(id))
		if err != nil {
			return err
		}
		if o.State != "queued" {
			return ErrConflict
		}
		o.State, o.Updated = "sending", time.Now().UTC()
		return s.writeOutbox(tx, o)
	})
	return o, err
}
func (s *Store) RejectQueued(id, detail string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		o, err := s.readOutbox(tx, []byte(id))
		if err != nil {
			return err
		}
		if o.State != "queued" {
			return ErrConflict
		}
		o.State, o.Detail, o.Updated = "rejected", detail, time.Now().UTC()
		return s.writeOutbox(tx, o)
	})
}
