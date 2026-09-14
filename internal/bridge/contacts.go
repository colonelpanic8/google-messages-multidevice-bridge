package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
)

// contactsFresh is how long a read of the phone's address book stands in for
// the next one. Contacts change on the phone, rarely and never urgently.
const contactsFresh = 15 * time.Minute

// storedContacts reads the address book saved by the last successful read.
func (b *Bridge) storedContacts() (model.ContactBook, error) {
	var book model.ContactBook
	raw, err := b.Store.Contacts()
	if err != nil || len(raw) == 0 {
		return model.ContactBook{Schema: model.Schema, Contacts: []model.Contact{}}, err
	}
	if err := json.Unmarshal(raw, &book); err != nil {
		return model.ContactBook{Schema: model.Schema, Contacts: []model.Contact{}}, nil
	}
	if book.Contacts == nil {
		book.Contacts = []model.Contact{}
	}
	return book, nil
}

// Contacts hands back the phone's address book, reading it again only when the
// stored copy has aged out or the caller asked for a fresh one. A phone that
// cannot be reached yields the stored copy marked stale rather than nothing,
// because completing a recipient is still useful offline.
func (b *Bridge) Contacts(ctx context.Context, refresh bool) (model.ContactBook, error) {
	b.contactsMu.Lock()
	defer b.contactsMu.Unlock()
	stored, err := b.storedContacts()
	if err != nil {
		return stored, err
	}
	if !refresh && !stored.Updated.IsZero() && time.Since(stored.Updated) < contactsFresh {
		return stored, nil
	}
	stale := func(cause error) (model.ContactBook, error) {
		if stored.Updated.IsZero() {
			return model.ContactBook{}, cause
		}
		stored.Stale = true
		return stored, nil
	}
	if b.PairingActive() {
		return stale(ErrPairing)
	}
	p, ctx, release := b.borrowProvider(ctx)
	defer release()
	if p == nil || b.Status().State != "connected" {
		return stale(provider.ErrUnavailable)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	contacts, err := p.Contacts(ctx)
	if err != nil {
		return stale(provider.ErrUnavailable)
	}
	sortContacts(contacts)
	book := model.ContactBook{Schema: model.Schema, Contacts: contacts, Updated: time.Now().UTC()}
	data, err := json.Marshal(book)
	if err != nil {
		return stale(err)
	}
	if err := b.Store.SaveContacts(data); err != nil {
		b.storageFailure(err)
		return book, nil
	}
	return book, nil
}

// sortContacts puts the address book in the order the picker shows it: the
// phone's frequent contacts first, then everyone else by name.
func sortContacts(contacts []model.Contact) {
	key := func(c model.Contact) string {
		if c.Name != "" {
			return strings.ToLower(c.Name)
		}
		return c.Address
	}
	sort.SliceStable(contacts, func(i, j int) bool {
		if contacts[i].Frequent != contacts[j].Frequent {
			return contacts[i].Frequent
		}
		if left, right := key(contacts[i]), key(contacts[j]); left != right {
			return left < right
		}
		return contacts[i].Address < contacts[j].Address
	})
}

var errNoAddress = fmt.Errorf("%w: a participant has no dialable address", ErrInvalid)

// ConversationRecipients is the addressable set behind a stored conversation,
// which is what starting a conversation with more people is built from.
func (b *Bridge) ConversationRecipients(id string) ([]string, error) {
	raw, err := b.Store.Record("conversation", id)
	if err != nil {
		return nil, err
	}
	var conv model.Conversation
	if err := json.Unmarshal(raw, &conv); err != nil {
		return nil, err
	}
	self := map[string]bool{}
	for _, participant := range conv.Participants {
		if participant.IsMe && participant.Address != "" {
			self[participant.Address] = true
		}
	}
	seen := map[string]bool{}
	recipients := make([]string, 0, len(conv.Participants))
	for _, participant := range conv.Participants {
		if participant.IsMe || self[participant.Address] || seen[participant.Address] {
			continue
		}
		if participant.Address == "" {
			return nil, errNoAddress
		}
		seen[participant.Address] = true
		recipients = append(recipients, participant.Address)
	}
	return recipients, nil
}
