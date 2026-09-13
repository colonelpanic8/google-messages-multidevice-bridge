package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	bolt "go.etcd.io/bbolt"
)

func (s *Store) SaveUpload(u model.Upload, data []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		raw, err := json.Marshal(u)
		if err != nil {
			return err
		}
		if err = tx.Bucket([]byte("media")).Put([]byte("upload:"+u.ID), s.encrypt(data, "media:upload:"+u.ID)); err != nil {
			return err
		}
		_, err = s.appendTx(tx, Event{Type: "upload", EntityID: u.ID, Data: raw})
		return err
	})
}
func (s *Store) FinishConversation(id string, event Event, private map[string][]byte, watermark uint64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		o, err := s.readOutbox(tx, []byte(id))
		if err != nil {
			return err
		}
		if o.State != "sending" || o.Request.Kind != "conversation" {
			return ErrConflict
		}
		if event.Type != "conversation" || event.EntityID == "" {
			return errors.New("invalid conversation result")
		}
		version := tx.Bucket([]byte("versions")).Get([]byte("conversation:" + event.EntityID))
		if len(version) != 8 || binary.BigEndian.Uint64(version) <= watermark {
			if _, err = s.appendTx(tx, event); err != nil {
				return err
			}
			revision, err := tx.Bucket([]byte("versions")).NextSequence()
			if err != nil {
				return err
			}
			if err = tx.Bucket([]byte("versions")).Put([]byte("conversation:"+event.EntityID), sequence(revision)); err != nil {
				return err
			}
			for key, data := range private {
				if err = tx.Bucket([]byte("private")).Put([]byte(key), s.encrypt(data, "private:"+key)); err != nil {
					return err
				}
			}
		}
		if err := stampEpoch(tx, "conversation", event.EntityID); err != nil {
			return err
		}
		o.State, o.ConversationID, o.Updated = "accepted", event.EntityID, time.Now().UTC()
		o.Detail = "Conversation is ready; no message was sent"
		return s.writeOutbox(tx, o)
	})
}
func (s *Store) historyTx(tx *bolt.Tx, id string) (model.HistoryJob, error) {
	var job model.HistoryJob
	raw := tx.Bucket([]byte("latest")).Get([]byte("history:" + id))
	if raw == nil {
		return job, ErrNotFound
	}
	data, err := s.decrypt(raw, "history:"+id)
	if err != nil {
		return job, err
	}
	err = json.Unmarshal(data, &job)
	return job, err
}
func (s *Store) writeHistory(tx *bolt.Tx, job *model.HistoryJob) error {
	job.Updated = time.Now().UTC()
	raw, err := json.Marshal(job)
	if err != nil {
		return err
	}
	_, err = s.appendTx(tx, Event{Type: "history", EntityID: job.ID, Data: raw})
	return err
}
func (s *Store) QueueHistory(job model.HistoryJob, restart bool) (model.HistoryJob, error) {
	err := s.db.Update(func(tx *bolt.Tx) error {
		old, err := s.historyTx(tx, job.ID)
		if err == nil {
			job = old
			if !restart && (job.State == "complete" || job.State == "queued") {
				return nil
			}
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		if restart {
			job.Pages, job.Records = 0, 0
			if err := tx.Bucket([]byte("private")).Delete([]byte("history:" + job.ID)); err != nil {
				return err
			}
			if seen := tx.Bucket([]byte("history-pages")); seen != nil {
				prefix := historyPrefix(job.ID)
				c := seen.Cursor()
				for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
					if err := c.Delete(); err != nil {
						return err
					}
				}
			}
		}
		job.Generation++
		job.Updated = time.Now().UTC()
		job.Schema, job.State, job.Detail, job.RetryAt = model.Schema, "queued", "", time.Time{}
		return s.writeHistory(tx, &job)
	})
	return job, err
}
func (s *Store) PauseHistory(id string) (model.HistoryJob, error) {
	var job model.HistoryJob
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		job, err = s.historyTx(tx, id)
		if err != nil {
			return err
		}
		if job.State == "complete" {
			return nil
		}
		job.State = "paused"
		job.Updated = time.Now().UTC()
		return s.writeHistory(tx, &job)
	})
	return job, err
}
func (s *Store) HistoryCursor(id string) ([]byte, error) {
	data, err := s.Private("history:" + id)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return data, err
}

// CheckpointHistory advances only the page read by the worker. Pausing while a
// request is in flight retains the previous cursor, making resume a safe re-read.
func (s *Store) CheckpointHistory(id string, generation uint64, previous, next []byte, records int, detail string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		job, err := s.historyTx(tx, id)
		if err != nil {
			return err
		}
		if job.State != "queued" || job.Generation != generation {
			return nil
		}
		key := "history:" + id
		var current []byte
		if enc := tx.Bucket([]byte("private")).Get([]byte(key)); enc != nil {
			current, err = s.decrypt(enc, "private:"+key)
			if err != nil {
				return err
			}
		}
		if !bytes.Equal(current, previous) {
			return ErrConflict
		}
		job.Detail = detail
		if detail != "" {
			job.RetryAt = time.Now().UTC().Add(30 * time.Second)
			return s.writeHistory(tx, &job)
		}
		seen, err := tx.CreateBucketIfNotExists([]byte("history-pages"))
		if err != nil {
			return err
		}
		hash := func(cursor []byte) []byte {
			sum := sha256.Sum256(cursor)
			return append(historyPrefix(id), []byte(hex.EncodeToString(sum[:]))...)
		}
		if err = seen.Put(hash(previous), []byte{1}); err != nil {
			return err
		}
		job.Pages++
		job.Records += int64(records)
		job.RetryAt = time.Time{}
		if len(next) == 0 {
			job.State = "complete"
		} else if seen.Get(hash(next)) != nil {
			job.State = "failed"
			job.Detail = "Provider repeated a history cursor; import stopped"
		}
		if err = tx.Bucket([]byte("private")).Put([]byte(key), s.encrypt(next, "private:"+key)); err != nil {
			return err
		}
		return s.writeHistory(tx, &job)
	})
}

func historyPrefix(id string) []byte {
	sum := sha256.Sum256([]byte(id))
	return []byte(hex.EncodeToString(sum[:]) + ":")
}

func (s *Store) FailHistory(id string, generation uint64, detail string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		job, err := s.historyTx(tx, id)
		if err != nil {
			return err
		}
		if job.State != "queued" || job.Generation != generation {
			return nil
		}
		job.State, job.Detail, job.RetryAt = "failed", detail, time.Time{}
		return s.writeHistory(tx, &job)
	})
}
