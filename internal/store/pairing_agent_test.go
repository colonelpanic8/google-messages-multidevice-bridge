package store

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestPairingAgentDigestPersistsEncrypted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	key := bytes.Repeat([]byte{9}, 32)
	s, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	digest := bytes.Repeat([]byte{7}, 32)
	if err := s.SavePairingAgentDigest(digest); err != nil {
		t.Fatal(err)
	}
	got, err := s.PairingAgentDigest()
	if err != nil || !bytes.Equal(got, digest) {
		t.Fatalf("digest: %x %v", got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err = s.PairingAgentDigest()
	if err != nil || !bytes.Equal(got, digest) {
		t.Fatalf("reopened digest: %x %v", got, err)
	}
}
