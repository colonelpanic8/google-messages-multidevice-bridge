package store

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryEncryptionAndDeduplication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	key := bytes.Repeat([]byte{7}, 32)
	s, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte(`{"cookie":"private-session-cookie"}`)
	if err := s.SaveSession(secret); err != nil {
		t.Fatal(err)
	}
	a := Event{Type: "message", EntityID: "m1", Data: json.RawMessage(`{"body":"private-message-body","read":false}`)}
	for i, want := range []bool{true, false} {
		added, err := s.Append(a)
		if err != nil || added != want {
			t.Fatalf("append %d: %v, %v", i, added, err)
		}
	}
	b := a
	b.Data = json.RawMessage(`{"body":"private-message-body","read":true}`)
	if added, err := s.Append(b); !added || err != nil {
		t.Fatalf("changed status: %v %v", added, err)
	}
	if added, err := s.Append(a); !added || err != nil {
		t.Fatalf("return to prior state: %v %v", added, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"private-session-cookie", "private-message-body"} {
		if bytes.Contains(file, []byte(text)) {
			t.Fatalf("plaintext leaked: %s", text)
		}
	}
	if wrong, err := Open(path, bytes.Repeat([]byte{8}, 32)); err == nil {
		wrong.Close()
		t.Fatal("wrong key accepted")
	}
	s, err = Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loaded, err := s.Session()
	if err != nil || !bytes.Equal(secret, loaded) {
		t.Fatalf("session recovery: %s %v", loaded, err)
	}
	batch, err := s.Events(1, 1)
	if err != nil || len(batch) != 1 || batch[0].ID != 2 {
		t.Fatalf("cursor: %+v %v", batch, err)
	}
	batch, err = s.Events(2, 100)
	if err != nil || len(batch) != 1 || batch[0].ID != 3 {
		t.Fatalf("next page: %+v %v", batch, err)
	}
	batch, err = s.Events(math.MaxUint64, 10)
	if err != nil || len(batch) != 0 {
		t.Fatal("cursor overflow")
	}
}

func TestAuthenticationRejectsTampering(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bridge.db"), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ciphertext := s.encrypt([]byte("secret"), "session")
	if _, err := s.decrypt(ciphertext, "another-record"); err == nil {
		t.Fatal("record substitution accepted")
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := s.decrypt(ciphertext, "session"); err == nil {
		t.Fatal("tampered payload accepted")
	}
}
