package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	bolt "go.etcd.io/bbolt"
)

const pairingAttemptKey = "pairing-attempt"

func previousCountKey(kind string) []byte { return []byte("previous-session-" + kind + "s") }

type SessionSummary struct {
	Epoch                 uint64 `json:"session_epoch"`
	PreviousConversations int    `json:"previous_session_conversations"`
	PreviousMessages      int    `json:"previous_session_messages"`
}

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
	current := epoch(tx)
	key := []byte(kind + ":" + id)
	previous := b.Get(key)
	if current > 0 && tx.Bucket([]byte("latest")).Get(key) != nil && (len(previous) != 8 || binary.BigEndian.Uint64(previous) != current) {
		meta := tx.Bucket([]byte("meta"))
		countKey := previousCountKey(kind)
		count := meta.Get(countKey)
		if len(count) == 8 && binary.BigEndian.Uint64(count) > 0 {
			if err := meta.Put(countKey, sequence(binary.BigEndian.Uint64(count)-1)); err != nil {
				return err
			}
		}
	}
	return b.Put(key, sequence(current))
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

func (s *Store) SessionSummary() (SessionSummary, error) {
	var summary SessionSummary
	err := s.db.View(func(tx *bolt.Tx) error {
		summary.Epoch = epoch(tx)
		if summary.Epoch == 0 {
			return nil
		}
		meta := tx.Bucket([]byte("meta"))
		if value := meta.Get(previousCountKey("conversation")); len(value) == 8 {
			summary.PreviousConversations = int(binary.BigEndian.Uint64(value))
		}
		if value := meta.Get(previousCountKey("message")); len(value) == 8 {
			summary.PreviousMessages = int(binary.BigEndian.Uint64(value))
		}
		return nil
	})
	return summary, err
}

// UpgradeSessionMetadata assigns records created before session ownership was
// explicit to the session that was current when this version first opened the
// database. Future re-pairs preserve that ownership instead of guessing.
func (s *Store) UpgradeSessionMetadata() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		current := epoch(tx)
		if current == 0 {
			return nil
		}
		var outbox []model.Outbox
		if err := tx.Bucket([]byte("outbox")).ForEach(func(k, _ []byte) error {
			o, err := s.readOutbox(tx, k)
			if err != nil {
				return err
			}
			if o.SessionEpoch == 0 {
				o.SessionEpoch = current
				outbox = append(outbox, o)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, o := range outbox {
			if err := s.writeOutbox(tx, o); err != nil {
				return err
			}
		}
		var jobs []model.HistoryJob
		latest := tx.Bucket([]byte("latest"))
		c := latest.Cursor()
		for k, _ := c.Seek([]byte("history:")); k != nil && bytes.HasPrefix(k, []byte("history:")); k, _ = c.Next() {
			job, err := s.historyTx(tx, string(k[len("history:"):]))
			if err != nil {
				return err
			}
			if job.SessionEpoch == 0 {
				job.SessionEpoch = current
				jobs = append(jobs, job)
			}
		}
		for i := range jobs {
			if err := s.writeHistory(tx, &jobs[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) BeginPairingAttempt(started, expires time.Time) error {
	marker, err := json.Marshal(struct {
		Started time.Time `json:"started"`
		Expires time.Time `json:"expires"`
	}{Started: started, Expires: expires})
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := s.cancelQueuedForPairing(tx); err != nil {
			return err
		}
		return tx.Bucket([]byte("meta")).Put([]byte(pairingAttemptKey), s.encrypt(marker, pairingAttemptKey))
	})
}

func (s *Store) ClearPairingAttempt() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("meta")).Delete([]byte(pairingAttemptKey))
	})
}

const pairingAgentDigestKey = "pairing-agent-digest"

func (s *Store) SavePairingAgentDigest(digest []byte) error {
	if len(digest) != 32 {
		return errors.New("invalid pairing agent digest")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("meta")).Put([]byte(pairingAgentDigestKey), s.encrypt(digest, pairingAgentDigestKey))
	})
}

func (s *Store) PairingAgentDigest() ([]byte, error) {
	var digest []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket([]byte("meta")).Get([]byte(pairingAgentDigestKey))
		if value == nil {
			return nil
		}
		var err error
		digest, err = s.decrypt(value, pairingAgentDigestKey)
		return err
	})
	return digest, err
}

func (s *Store) RecoverPairingAttempt() (bool, error) {
	interrupted := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		data := meta.Get([]byte(pairingAttemptKey))
		if data == nil {
			return nil
		}
		plain, err := s.decrypt(data, pairingAttemptKey)
		if err != nil {
			return err
		}
		var marker struct {
			Started time.Time `json:"started"`
			Expires time.Time `json:"expires"`
		}
		if err = json.Unmarshal(plain, &marker); err != nil || marker.Started.IsZero() || marker.Expires.IsZero() {
			return errors.New("invalid pairing attempt marker")
		}
		interrupted = true
		return meta.Delete([]byte(pairingAttemptKey))
	})
	return interrupted, err
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

// AdoptStoredRecords claims every stored conversation and message for the
// current session and reports how many it moved. It answers the question the
// bridge used to decide on its own: the stored records describe the phone that
// is attached now, so the read-only boundary is wrong and clearing it avoids
// re-importing history that is already stored.
func (s *Store) AdoptStoredRecords() (int, error) {
	adopted := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		adopted = 0
		current := epoch(tx)
		if current == 0 {
			return nil
		}
		epochs, err := tx.CreateBucketIfNotExists([]byte("entity-epochs"))
		if err != nil {
			return err
		}
		stamp := sequence(current)
		latest := tx.Bucket([]byte("latest"))
		meta := tx.Bucket([]byte("meta"))
		for _, kind := range []string{"conversation", "message"} {
			prefix := []byte(kind + ":")
			c := latest.Cursor()
			for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
				key := append([]byte(nil), k...)
				if v := epochs.Get(key); len(v) == 8 && binary.BigEndian.Uint64(v) == current {
					continue
				}
				if err := epochs.Put(key, stamp); err != nil {
					return err
				}
				adopted++
			}
			if err := meta.Put(previousCountKey(kind), sequence(0)); err != nil {
				return err
			}
		}
		return nil
	})
	return adopted, err
}

// SavePairedSession stores the new session. A re-pair of the same phone keeps
// every stored record writable; newPhone starts a new session epoch so old
// records stay read-only until the replacement phone observes them.
func (s *Store) SavePairedSession(data []byte, newPhone bool) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		if err := meta.Put([]byte("session"), s.encrypt(data, "session")); err != nil {
			return err
		}
		if err := meta.Delete([]byte(pairingAttemptKey)); err != nil {
			return err
		}
		if err := s.cancelQueuedForPairing(tx); err != nil {
			return err
		}
		if !newPhone {
			return nil
		}
		for _, kind := range []string{"conversation", "message"} {
			var count uint64
			prefix := []byte(kind + ":")
			c := tx.Bucket([]byte("latest")).Cursor()
			for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
				count++
			}
			if err := meta.Put(previousCountKey(kind), sequence(count)); err != nil {
				return err
			}
		}
		if err := meta.Put([]byte("session-epoch"), sequence(epoch(tx)+1)); err != nil {
			return err
		}
		// The address book belongs to the phone that was paired, not to the one
		// taking its place.
		if err := meta.Delete([]byte("contacts")); err != nil {
			return err
		}
		private := tx.Bucket([]byte("private"))
		cursor := private.Cursor()
		for k, _ := cursor.First(); k != nil; k, _ = cursor.Next() {
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
