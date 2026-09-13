package store

import (
	"bytes"
	"encoding/binary"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"time"

	bolt "go.etcd.io/bbolt"
)

func epoch(tx *bolt.Tx) uint64 {
	v := tx.Bucket([]byte("meta")).Get([]byte("session-epoch"))
	if len(v) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(v)
}
func stampEpoch(tx *bolt.Tx, kind, id string) error {
	if kind != "message" && kind != "conversation" {
		return nil
	}
	b, err := tx.CreateBucketIfNotExists([]byte("entity-epochs"))
	if err != nil {
		return err
	}
	return b.Put([]byte(kind+":"+id), sequence(epoch(tx)))
}
func (s *Store) EntityCurrent(kind, id string) (bool, error) {
	current := true
	err := s.db.View(func(tx *bolt.Tx) error {
		currentEpoch := epoch(tx)
		if currentEpoch == 0 {
			return nil
		}
		b := tx.Bucket([]byte("entity-epochs"))
		if b == nil {
			current = false
			return nil
		}
		v := b.Get([]byte(kind + ":" + id))
		current = len(v) == 8 && binary.BigEndian.Uint64(v) == currentEpoch
		return nil
	})
	return current, err
}
func (s *Store) cancelQueuedForPairing(tx *bolt.Tx) error {
	var pending []model.Outbox
	err := tx.Bucket([]byte("outbox")).ForEach(func(k, v []byte) error {
		o, err := s.readOutbox(tx, k)
		if err != nil {
			return err
		}
		if o.State == "queued" {
			pending = append(pending, o)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, o := range pending {
		o.State, o.Detail, o.Updated = "canceled", "Pairing changed; review the recipient and submit again", time.Now().UTC()
		if err := s.writeOutbox(tx, o); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) CancelQueuedForPairing() error { return s.db.Update(s.cancelQueuedForPairing) }
func (s *Store) SavePairedSession(data []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		if err := meta.Put([]byte("session"), s.encrypt(data, "session")); err != nil {
			return err
		}
		if err := meta.Put([]byte("session-epoch"), sequence(epoch(tx)+1)); err != nil {
			return err
		}
		if err := s.cancelQueuedForPairing(tx); err != nil {
			return err
		}
		private := tx.Bucket([]byte("private"))
		cursor := private.Cursor()
		for k, _ := cursor.Seek([]byte("upload:")); k != nil && bytes.HasPrefix(k, []byte("upload:")); k, _ = cursor.Next() {
			if err := cursor.Delete(); err != nil {
				return err
			}
		}
		var jobs []model.HistoryJob
		c := tx.Bucket([]byte("latest")).Cursor()
		for k, _ := c.Seek([]byte("history:")); k != nil && bytes.HasPrefix(k, []byte("history:")); k, _ = c.Next() {
			job, err := s.historyTx(tx, string(k[len("history:"):]))
			if err != nil {
				return err
			}
			jobs = append(jobs, job)
		}
		for _, job := range jobs {
			job.State = "paused"
			job.Detail = "Pairing changed; start an import for the paired phone"
			job.Generation++
			job.Pages, job.Records = 0, 0
			job.RetryAt = time.Time{}
			if err := private.Delete([]byte("history:" + job.ID)); err != nil {
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
			if err := s.writeHistory(tx, &job); err != nil {
				return err
			}
		}
		return nil
	})
}
