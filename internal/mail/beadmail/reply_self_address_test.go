package beadmail

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// No session identity anywhere: proves the defect is NOT specific to the
// session-id machinery, but to the unconditional target choice.
func TestReplyBySenderBareTargetsParentRecipient(t *testing.T) {
	store := beads.NewMemStore()
	p := newTestProvider(store)

	sent, err := p.Send("alice", "bob", "Hello", "first message")
	if err != nil {
		t.Fatal(err)
	}
	// alice is the parent's SENDER, replying on her own thread.
	reply, err := p.Reply(sent.ID, "alice", "Re: Hello", "follow-up")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply.To == "alice" {
		t.Fatalf("SELF-ADDRESSED: reply To = %q (the replier itself); want parent recipient %q", reply.To, "bob")
	}
	if reply.To != "bob" {
		t.Fatalf("reply To = %q, want parent recipient %q", reply.To, "bob")
	}
}

// Production shape from the report: reproduces to_session_id == from_session_id.
func TestReplyBySenderWithSessionsDoesNotSelfAddress(t *testing.T) {
	store := beads.NewMemStore()
	p := newTestProvider(store)

	sender, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "pack-author.pack-author",
			"session_name": "pack-author__pack-author",
		},
	})
	if err != nil {
		t.Fatalf("Create sender session: %v", err)
	}

	// Exactly what `gc mail send mayor ...` produces: Assignee is the role
	// name and only the SENDER's session is stamped.
	original, err := store.Create(beads.Bead{
		Title:       "time-sensitive correction",
		Description: "please read",
		Type:        "message",
		Assignee:    "mayor",
		From:        "pack-author.pack-author",
		Labels:      []string{"thread:t-nllk48"},
		Metadata: map[string]string{
			fromSessionIDMetadataKey: sender.ID,
			fromDisplayMetadataKey:   "pack-author.pack-author",
		},
	})
	if err != nil {
		t.Fatalf("Create original message: %v", err)
	}

	// The parent's own sender replies.
	reply, err := p.Reply(original.ID, "pack-author.pack-author", "re: correction", "follow-up")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	b, err := store.Get(reply.ID)
	if err != nil {
		t.Fatalf("Get reply: %v", err)
	}
	if got := b.Metadata[fromSessionIDMetadataKey]; got != "" && got == b.Metadata[toSessionIDMetadataKey] {
		t.Fatalf("SELF-ADDRESSED: to_session_id == from_session_id == %q", got)
	}
	if b.Assignee != "mayor" {
		t.Fatalf("reply Assignee = %q, want the parent's recipient %q", b.Assignee, "mayor")
	}
}

func createAliasSession(t *testing.T, store beads.Store, alias string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        alias,
			"session_name": alias + "__" + alias,
		},
	})
	if err != nil {
		t.Fatalf("Create %s session: %v", alias, err)
	}
	return b
}

// A sender's follow-up to its own earlier reply keeps the other party's
// session route and display name instead of exposing the raw session ID.
func TestReplyBySenderToOwnReplyKeepsRecipientDisplay(t *testing.T) {
	store := beads.NewMemStore()
	p := newTestProvider(store)
	alice := createAliasSession(t, store, "alice")
	createAliasSession(t, store, "bob")

	sent, err := p.Send("alice", "bob", "Hello", "first")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	first, err := p.Reply(sent.ID, "bob", "Re: Hello", "answer")
	if err != nil {
		t.Fatalf("first Reply: %v", err)
	}
	second, err := p.Reply(first.ID, "bob", "Re: Hello", "and another thing")
	if err != nil {
		t.Fatalf("second Reply: %v", err)
	}
	b, err := store.Get(second.ID)
	if err != nil {
		t.Fatalf("Get second reply: %v", err)
	}
	if b.Assignee != alice.ID {
		t.Errorf("second reply Assignee = %q, want alice's session %q", b.Assignee, alice.ID)
	}
	if got := b.Metadata[toSessionIDMetadataKey]; got != alice.ID {
		t.Errorf("second reply %s = %q, want %q", toSessionIDMetadataKey, got, alice.ID)
	}
	if got := b.Metadata[toDisplayMetadataKey]; got != "alice" {
		t.Errorf("second reply %s = %q, want %q", toDisplayMetadataKey, got, "alice")
	}
	if second.To != "alice" {
		t.Errorf("second reply To = %q, want %q", second.To, "alice")
	}
}

// A parent sent before its sender resolved to a session carries no session
// identity; once the sender resolves, its follow-up still goes to the recipient.
func TestReplyBySenderToPreSessionMessageGoesToRecipient(t *testing.T) {
	store := beads.NewMemStore()
	p := newTestProvider(store)

	original, err := store.Create(beads.Bead{
		Title:       "Hello",
		Description: "first",
		Type:        "message",
		Assignee:    "bob",
		From:        "alice",
	})
	if err != nil {
		t.Fatalf("Create original message: %v", err)
	}
	createAliasSession(t, store, "alice")

	reply, err := p.Reply(original.ID, "alice", "Re: Hello", "follow-up")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	b, err := store.Get(reply.ID)
	if err != nil {
		t.Fatalf("Get reply: %v", err)
	}
	if b.Assignee != "bob" {
		t.Errorf("reply Assignee = %q, want %q", b.Assignee, "bob")
	}
}

// Replying to a message one sent to oneself has no other party and errors.
func TestReplyToSelfAddressedMessageErrors(t *testing.T) {
	p := newTestProvider(beads.NewMemStore())

	sent, err := p.Send("alice", "alice", "Note", "to self")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := p.Reply(sent.ID, "alice", "Re: Note", "again"); err == nil {
		t.Fatal("Reply to a self-addressed message succeeded; want an error")
	}
}
