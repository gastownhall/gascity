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
