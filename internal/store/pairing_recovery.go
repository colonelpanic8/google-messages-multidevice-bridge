package store

import (
	"encoding/json"
	"errors"
	"time"

	bolt "go.etcd.io/bbolt"
)

type PairingRecovery struct {
	Enabled     bool      `json:"enabled"`
	PhoneNumber string    `json:"phone_number,omitempty"`
	Attempts    int       `json:"attempts"`
	Paused      bool      `json:"paused"`
	NextAttempt time.Time `json:"next_attempt"`
}

func (s *Store) PairingRecovery() (PairingRecovery, error) {
	var state PairingRecovery
	data, err := s.get("meta", "whatsapp-pairing-recovery")
	if errors.Is(err, ErrNotFound) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(data, &state)
	return state, err
}

func (s *Store) UpdatePairingRecovery(update func(*PairingRecovery)) error {
	return s.db.Update(func(tx *bolt.Tx) error { return s.updatePairingRecovery(tx, update) })
}

func (s *Store) updatePairingRecovery(tx *bolt.Tx, update func(*PairingRecovery)) error {
	const key = "whatsapp-pairing-recovery"
	raw := tx.Bucket([]byte("meta")).Get([]byte(key))
	var state PairingRecovery
	if raw != nil {
		data, err := s.decrypt(raw, recordContext("meta", key))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(data, &state); err != nil {
			return err
		}
	}
	update(&state)
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return tx.Bucket([]byte("meta")).Put([]byte(key), s.encrypt(data, recordContext("meta", key)))
}
