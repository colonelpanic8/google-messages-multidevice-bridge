package whatsapp

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	local "github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
	"go.mau.fi/whatsmeow/types"
)

func credentialFixture(t *testing.T) (*Credentials, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.db")
	db, err := local.Open(path, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewCredentials(db, "synthetic"), path
}
func TestCredentialsEncryptedAndDecryptionRollback(t *testing.T) {
	s, path := credentialFixture(t)
	ctx := context.Background()
	device, err := s.Device(ctx)
	if err != nil {
		t.Fatal(err)
	}
	jid := types.NewJID("14155550100", types.DefaultUserServer)
	device.ID = &jid
	if err = device.Save(ctx); err != nil {
		t.Fatal(err)
	}
	secret := []byte("synthetic-signal-session-never-plaintext")
	if err = s.PutSession(ctx, "peer.0", secret); err != nil {
		t.Fatal(err)
	}
	failures := 0
	s.Failure = func(error) { failures++ }
	failure := errors.New("abort synthetic decryption")
	var h [32]byte
	h[0] = 1
	err = s.DoDecryptionTxn(ctx, func(ctx context.Context) error {
		if err := s.PutSession(ctx, "peer.0", []byte("advanced ratchet")); err != nil {
			return err
		}
		if err := s.PutBufferedEvent(ctx, h, []byte("synthetic plaintext"), time.Now()); err != nil {
			return err
		}
		return failure
	})
	if failures != 0 {
		t.Fatal("semantic decryption error treated as storage failure")
	}
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	old, err := s.GetSession(ctx, "peer.0")
	if err != nil || !bytes.Equal(old, secret) {
		t.Fatal("ratchet escaped rollback", err)
	}
	event, err := s.GetBufferedEvent(ctx, h)
	if err != nil || event != nil {
		t.Fatal("buffer escaped rollback", err)
	}
	if err = s.DB.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, secret) || bytes.Contains(raw, device.IdentityKey.Priv[:]) {
		t.Fatal("plaintext credential in database")
	}
	reopened, err := local.Open(path, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	next := NewCredentials(reopened, s.Namespace)
	restored, err := next.Device(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if *restored.IdentityKey.Priv != *device.IdentityKey.Priv || *restored.ID != jid {
		t.Fatal("device did not survive reopen")
	}
	old, err = next.GetSession(ctx, "peer.0")
	if err != nil || !bytes.Equal(old, secret) {
		t.Fatal("session did not survive reopen", err)
	}
}
func TestPrekeysAndSignalMigration(t *testing.T) {
	s, _ := credentialFixture(t)
	ctx := context.Background()
	keys, err := s.GetOrGenPreKeys(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	subset, err := s.GetOrGenPreKeys(ctx, 1)
	if err != nil || len(subset) != 1 || subset[0].KeyID != 1 {
		t.Fatal(subset, err)
	}
	if err = s.MarkPreKeysAsUploaded(ctx, 3); err != nil {
		t.Fatal(err)
	}
	n, err := s.UploadedPreKeyCount(ctx)
	if err != nil || n != 3 {
		t.Fatal(n, err)
	}
	next, err := s.GenOnePreKey(ctx)
	if err != nil || next.KeyID <= keys[2].KeyID {
		t.Fatal(next, err)
	}
	pn := types.NewJID("14155550100", types.DefaultUserServer)
	lid := types.NewJID("987654", types.HiddenUserServer)
	oldAddress, newAddress := pn.SignalAddress().String(), lid.SignalAddress().String()
	if err = s.PutSession(ctx, oldAddress, []byte("ratchet")); err != nil {
		t.Fatal(err)
	}
	if err = s.PutSenderKey(ctx, "group", oldAddress, []byte("sender")); err != nil {
		t.Fatal(err)
	}
	if err = s.MigratePNToLID(ctx, pn, lid); err != nil {
		t.Fatal(err)
	}
	value, err := s.GetSession(ctx, newAddress)
	if err != nil || string(value) != "ratchet" {
		t.Fatal(string(value), err)
	}
	value, err = s.GetSession(ctx, oldAddress)
	if err != nil || value != nil {
		t.Fatal("old session retained", err)
	}
	value, err = s.GetSenderKey(ctx, "group", newAddress)
	if err != nil || string(value) != "sender" {
		t.Fatal(string(value), err)
	}
}

func TestLIDLookupPreservesDeviceForSignal(t *testing.T) {
	s, _ := credentialFixture(t)
	ctx := context.Background()
	pn := types.NewJID("14155550100", types.DefaultUserServer)
	lid := types.NewJID("123", types.HiddenUserServer)
	if err := s.PutLIDMapping(ctx, lid, pn); err != nil {
		t.Fatal(err)
	}
	pn.Device = 17
	mapped, err := s.GetLIDForPN(ctx, pn)
	if err != nil || mapped.Device != 17 || mapped.User != lid.User {
		t.Fatal(mapped, err)
	}
	mapped, err = s.GetPNForLID(ctx, mapped)
	if err != nil || mapped != pn {
		t.Fatal(mapped, err)
	}
	if err = s.PutSession(ctx, pn.SignalAddress().String(), []byte("synthetic")); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteAllSessions(ctx, pn.SignalAddressUser()); err != nil {
		t.Fatal(err)
	}
	present, err := s.HasSession(ctx, pn.SignalAddress().String())
	if err != nil || present {
		t.Fatal(present, err)
	}
}
