package storebindingtest

// Bare Messaging class conformance over the typed front-door set.
//
// The class-level assertions are deliberately the ones a provider swap can
// break: a mail round trip, recipient isolation, and the read/unread
// transition that decides whether an agent is told about a message twice or
// not at all. The exhaustive mail.Provider table lives in
// internal/mail/mailtest and is not duplicated here; this suite pins that a
// Messaging front-door SET arrives complete and behaves, which is the part
// the binding composes.

import (
	"testing"

	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/mailtest"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// MessagingSuite configures one bare Messaging class conformance run.
type MessagingSuite struct {
	// NewFrontDoors returns a fresh, empty Messaging front-door set per
	// assertion, already bound to a Sessions address directory.
	NewFrontDoors func(TB) storebinding.MessagingFrontDoors
	// Capability is what the provider declares for the Messaging class.
	Capability storebinding.ClassCapability
}

// RunMessagingFrontDoorTests runs the bare Messaging class conformance suite.
func RunMessagingFrontDoorTests(r Runner, suite MessagingSuite) {
	r.Helper()
	if suite.NewFrontDoors == nil {
		r.Fatalf("storebindingtest: MessagingSuite.NewFrontDoors is required")
	}

	assertClassDeclaredAvailable(r, "Messaging", suite.Capability)

	r.Run("FrontDoorSetArrivesComplete", func(r Runner) {
		fronts := suite.NewFrontDoors(r)
		missing := []string{}
		if fronts.Mail == nil {
			missing = append(missing, "Mail")
		}
		if fronts.Bindings == nil {
			missing = append(missing, "Bindings")
		}
		if fronts.DeliveryContexts == nil {
			missing = append(missing, "DeliveryContexts")
		}
		if fronts.Groups == nil {
			missing = append(missing, "Groups")
		}
		if fronts.Transcripts == nil {
			missing = append(missing, "Transcripts")
		}
		if len(missing) > 0 {
			r.Fatalf("the Messaging front-door set is missing %v; a partial set is a capability loss the composition never announced", missing)
		}
	})

	r.Run("MailRoundTripsToTheRecipient", func(r Runner) {
		provider := mustMail(r, suite)
		sent, err := provider.Send("alice", "bob", "Greetings", "hello")
		if err != nil {
			r.Fatalf("Send: %v", err)
		}
		if sent.ID == "" {
			r.Fatalf("Send returned an empty ID")
		}
		got, err := provider.Get(sent.ID)
		if err != nil {
			r.Fatalf("Get(%q): %v", sent.ID, err)
		}
		if got.From != "alice" || got.To != "bob" || got.Body != "hello" {
			r.Errorf("Get = %+v, want the sent message %+v", got, sent)
		}
	})

	r.Run("InboxIsolatesRecipients", func(r Runner) {
		provider := mustMail(r, suite)
		if _, err := provider.Send("alice", "bob", "", "for bob"); err != nil {
			r.Fatalf("Send to bob: %v", err)
		}
		if _, err := provider.Send("alice", "carol", "", "for carol"); err != nil {
			r.Fatalf("Send to carol: %v", err)
		}
		inbox, err := provider.Inbox("bob")
		if err != nil {
			r.Fatalf("Inbox: %v", err)
		}
		if len(inbox) != 1 || inbox[0].To != "bob" {
			r.Fatalf("Inbox(bob) = %d messages %+v, want exactly bob's", len(inbox), inbox)
		}
	})

	r.Run("ReadRetiresTheMessageFromTheInbox", func(r Runner) {
		provider := mustMail(r, suite)
		sent, err := provider.Send("alice", "bob", "", "read me")
		if err != nil {
			r.Fatalf("Send: %v", err)
		}
		if _, err := provider.Read(sent.ID); err != nil {
			r.Fatalf("Read: %v", err)
		}
		inbox, err := provider.Inbox("bob")
		if err != nil {
			r.Fatalf("Inbox after Read: %v", err)
		}
		if len(inbox) != 0 {
			r.Fatalf("Inbox still holds %d read messages; the recipient is told twice", len(inbox))
		}
		if err := provider.MarkUnread(sent.ID); err != nil {
			r.Fatalf("MarkUnread: %v", err)
		}
		restored, err := provider.Inbox("bob")
		if err != nil {
			r.Fatalf("Inbox after MarkUnread: %v", err)
		}
		if len(restored) != 1 || restored[0].ID != sent.ID {
			r.Fatalf("Inbox after MarkUnread = %d messages, want the restored %q", len(restored), sent.ID)
		}
	})

	r.Run("GetUnknownMessageFails", func(r Runner) {
		provider := mustMail(r, suite)
		if _, err := provider.Get("gcm-does-not-exist"); err == nil {
			r.Fatalf("Get(unknown) succeeded; an absent message is not an empty message")
		}
	})
}

// RunMailProviderTests runs the exhaustive shared mail.Provider table against
// a Messaging front-door set's mail leg. It is a thin bridge onto the existing
// internal/mail/mailtest suite so a storage provider proves the whole mail
// contract without this package restating it.
func RunMailProviderTests(t *testing.T, suite MessagingSuite) {
	t.Helper()
	if suite.NewFrontDoors == nil {
		t.Fatalf("storebindingtest: MessagingSuite.NewFrontDoors is required")
	}
	mailtest.RunProviderTests(t, func(t *testing.T) mail.Provider {
		fronts := suite.NewFrontDoors(t)
		if fronts.Mail == nil {
			t.Fatalf("the Messaging front-door set carries no mail provider")
		}
		return fronts.Mail
	})
}

func mustMail(r Runner, suite MessagingSuite) mail.Provider {
	r.Helper()
	fronts := suite.NewFrontDoors(r)
	if fronts.Mail == nil {
		r.Fatalf("the Messaging front-door set carries no mail provider")
	}
	return fronts.Mail
}
