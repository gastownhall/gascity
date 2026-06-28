package main

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
)

// TestCreateHandoffMailRoutesThroughProviderSeam proves the handoff command
// produces its mail through the mail.Provider domain seam: createHandoffMail
// returns a mail.Message (not a raw *beads.Bead), and the resulting message is
// identical to what the former direct store.Create produced — same Title, Type,
// Assignee, From, Description, Ephemeral, thread label, and extra labels.
func TestCreateHandoffMailRoutesThroughProviderSeam(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	var stderr bytes.Buffer

	// The seam return type is mail.Message; this assignment fails to compile if
	// createHandoffMail still leaks *beads.Bead at the call site.
	var msg mail.Message
	msg, ok := createHandoffMail(store, rec, "mayor", "mayor",
		[]string{"HANDOFF: context full", "drain now"}, "HANDOFF: context cycle",
		[]string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel}, &stderr)
	if !ok {
		t.Fatalf("createHandoffMail failed: %s", stderr.String())
	}

	if msg.Subject != "HANDOFF: context full" {
		t.Errorf("Subject = %q, want %q", msg.Subject, "HANDOFF: context full")
	}
	if msg.Body != "drain now" {
		t.Errorf("Body = %q, want %q", msg.Body, "drain now")
	}
	if msg.From != "mayor" {
		t.Errorf("From = %q, want %q", msg.From, "mayor")
	}
	if msg.To != "mayor" {
		t.Errorf("To = %q, want %q", msg.To, "mayor")
	}
	if msg.ThreadID == "" {
		t.Errorf("ThreadID = empty, want a thread id")
	}

	// The mail must be byte-identical to today's direct-create shape.
	all := listOpenMessagesBothTiers(t, store)
	if len(all) != 1 {
		t.Fatalf("got %d message beads, want 1", len(all))
	}
	b := all[0]
	if b.ID != msg.ID {
		t.Errorf("bead ID = %q, want %q", b.ID, msg.ID)
	}
	if b.Type != "message" {
		t.Errorf("bead Type = %q, want %q", b.Type, "message")
	}
	if b.Title != "HANDOFF: context full" {
		t.Errorf("bead Title = %q, want %q", b.Title, "HANDOFF: context full")
	}
	if b.Description != "drain now" {
		t.Errorf("bead Description = %q, want %q", b.Description, "drain now")
	}
	if b.Assignee != "mayor" {
		t.Errorf("bead Assignee = %q, want %q", b.Assignee, "mayor")
	}
	if b.From != "mayor" {
		t.Errorf("bead From = %q, want %q", b.From, "mayor")
	}
	if !b.Ephemeral {
		t.Errorf("bead Ephemeral = false, want true")
	}
	if !hasString(b.Labels, "thread:"+msg.ThreadID) {
		t.Errorf("bead labels = %#v, missing thread label", b.Labels)
	}
	for _, want := range []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel} {
		if !hasString(b.Labels, want) {
			t.Errorf("bead labels = %#v, missing %q", b.Labels, want)
		}
	}

	// The MailSent event must still fire with the sender display as actor and
	// the new message's ID as subject (byte-identical to the legacy path).
	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1 (MailSent)", len(rec.Events))
	}
	if rec.Events[0].Type != events.MailSent {
		t.Errorf("event Type = %q, want %q", rec.Events[0].Type, events.MailSent)
	}
	if rec.Events[0].Actor != "mayor" {
		t.Errorf("event Actor = %q, want %q", rec.Events[0].Actor, "mayor")
	}
	if rec.Events[0].Subject != msg.ID {
		t.Errorf("event Subject = %q, want %q", rec.Events[0].Subject, msg.ID)
	}
}
