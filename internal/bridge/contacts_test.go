package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
)

func TestContactsAreCachedAndSurviveAnUnreachablePhone(t *testing.T) {
	b := testBridge(t)
	reads := 0
	b.setProvider(&fakeProvider{contacts: func(context.Context) ([]model.Contact, error) {
		reads++
		return []model.Contact{
			{ID: "p2", Name: "Alan Turing", Address: "+442071838750"},
			{ID: "p1", Name: "Ada Lovelace", Address: "+14155550100", Frequent: true},
		}, nil
	}})
	b.connection(true, true)

	book, err := b.Contacts(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	// The picker shows the phone's frequent contacts before the rest.
	if len(book.Contacts) != 2 || book.Contacts[0].ID != "p1" || book.Stale || book.Updated.IsZero() {
		t.Fatalf("contacts: %+v", book)
	}

	if _, err = b.Contacts(context.Background(), false); err != nil || reads != 1 {
		t.Fatalf("fresh contacts read again: %d %v", reads, err)
	}
	if _, err = b.Contacts(context.Background(), true); err != nil || reads != 2 {
		t.Fatalf("refresh ignored: %d %v", reads, err)
	}

	// Completing a recipient from the last address book beats completing from
	// nothing, so an unreachable phone yields what was stored, marked stale.
	b.setProvider(&fakeProvider{})
	book, err = b.Contacts(context.Background(), true)
	if err != nil || !book.Stale || len(book.Contacts) != 2 {
		t.Fatalf("stale contacts: %+v %v", book, err)
	}
}

func TestContactsWithoutAStoredCopyReportThePhoneIsUnavailable(t *testing.T) {
	b := testBridge(t)
	b.setProvider(&fakeProvider{})
	b.connection(true, true)
	if _, err := b.Contacts(context.Background(), false); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("first read without a phone: %v", err)
	}
}

func TestConversationRecipientsDropTheOwnerAndRefuseUnaddressablePeople(t *testing.T) {
	b := testBridge(t)
	if err := b.persist(snapshot(t, "conversation", "c1", model.Conversation{
		Schema: model.Schema,
		ID:     "c1",
		Participants: []model.Participant{
			{ID: "me", Address: "+15550000000", IsMe: true},
			{ID: "p1", Address: "+14155550100"},
			// Google repeats the owner across SIM legs without flagging the copy.
			{ID: "me-sim", Address: "+15550000000"},
			{ID: "p1-again", Address: "+14155550100"},
		},
	})); err != nil {
		t.Fatal(err)
	}
	got, err := b.ConversationRecipients("c1")
	if err != nil || len(got) != 1 || got[0] != "+14155550100" {
		t.Fatalf("recipients: %+v %v", got, err)
	}

	if err = b.persist(snapshot(t, "conversation", "c2", model.Conversation{
		Schema:       model.Schema,
		ID:           "c2",
		Participants: []model.Participant{{ID: "me", IsMe: true}, {ID: "p1", Name: "No number"}},
	})); err != nil {
		t.Fatal(err)
	}
	if _, err = b.ConversationRecipients("c2"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unaddressable participant: %v", err)
	}
}
