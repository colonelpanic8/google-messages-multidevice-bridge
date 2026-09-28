package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecoveryStateIsEncryptedAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	key := bytes.Repeat([]byte{9}, 32)
	s, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	want := PairingRecovery{Enabled: true, PhoneNumber: "+15550000001", Attempts: 2, Paused: true, NextAttempt: time.Now().UTC().Truncate(time.Second)}
	if err = s.UpdatePairingRecovery(func(state *PairingRecovery) { *state = want }); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(want.PhoneNumber)) {
		t.Fatal("phone stored in plaintext")
	}
	s, err = Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.PairingRecovery()
	if err != nil || got != want {
		t.Fatalf("reopened: %+v %v", got, err)
	}
}
