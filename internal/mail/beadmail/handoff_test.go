package beadmail

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

// TestSendHandoffConfinesBeadSerialization proves the handoff domain seam:
// SendHandoff speaks mail.Message in/out while the type=message bead, the
// caller-supplied thread label, and the extra labels are confined inside the
// beadmail implementation. The resulting bead must be identical to what the
// former direct store.Create at the cmd/gc call site produced.
func TestSendHandoffConfinesBeadSerialization(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	msg, _, err := p.SendHandoff(mail.HandoffIntent{
		From:     "mayor",
		To:       "mayor",
		Subject:  "HANDOFF: context full",
		Body:     "drain now",
		ThreadID: "thread-deadbeef",
		ExtraLabels: []string{
			mail.AutoHandoffLabel,
			mail.ArchiveAfterInjectLabel,
		},
	})
	if err != nil {
		t.Fatalf("SendHandoff: %v", err)
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
	if msg.ThreadID != "thread-deadbeef" {
		t.Errorf("ThreadID = %q, want %q", msg.ThreadID, "thread-deadbeef")
	}

	// Verify the confined bead matches the legacy direct-create shape.
	b, err := store.Get(msg.ID)
	if err != nil {
		t.Fatalf("Get %s: %v", msg.ID, err)
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
	for _, want := range []string{
		"thread:thread-deadbeef",
		mail.AutoHandoffLabel,
		mail.ArchiveAfterInjectLabel,
	} {
		if !hasLabel(b.Labels, want) {
			t.Errorf("bead labels = %#v, missing %q", b.Labels, want)
		}
	}
}

// TestSendHandoffResolvesSenderRoute proves SendHandoff applies the same
// sender-route metadata as the normal Send path, so reply routing keeps working
// for handoff mail.
func TestSendHandoffResolvesSenderRoute(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sess, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "sky",
			"session_name": "gc-sky",
		},
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	msg, _, err := p.SendHandoff(mail.HandoffIntent{
		From:     sess.ID,
		To:       "human",
		Subject:  "HANDOFF: context cycle",
		ThreadID: "thread-cafe",
	})
	if err != nil {
		t.Fatalf("SendHandoff: %v", err)
	}
	if msg.From != "sky" {
		t.Errorf("From = %q, want display alias %q", msg.From, "sky")
	}

	b, err := store.Get(msg.ID)
	if err != nil {
		t.Fatalf("Get %s: %v", msg.ID, err)
	}
	if got := b.Metadata[mail.FromSessionIDMetadataKey]; got != sess.ID {
		t.Errorf("from-session metadata = %q, want %q", got, sess.ID)
	}
	if got := b.Metadata[mail.FromDisplayMetadataKey]; got != "sky" {
		t.Errorf("from-display metadata = %q, want %q", got, "sky")
	}
}

func autoHandoffIntent() mail.HandoffIntent {
	return mail.HandoffIntent{
		From:        "mayor",
		To:          "mayor",
		Subject:     "HANDOFF: context full",
		Body:        "drain now",
		ThreadID:    "thread-deadbeef",
		DedupKey:    "precompact:s1",
		ExtraLabels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel},
	}
}

func TestSendHandoffSuppressesWhileUnreadHandoffPending(t *testing.T) {
	p := New(beads.NewMemStore())

	first, suppressed, err := p.SendHandoff(autoHandoffIntent())
	if err != nil || suppressed {
		t.Fatalf("first SendHandoff = suppressed %v, err %v; want created", suppressed, err)
	}
	second, suppressed, err := p.SendHandoff(autoHandoffIntent())
	if err != nil {
		t.Fatalf("second SendHandoff: %v", err)
	}
	if !suppressed {
		t.Fatal("second send while first is unread not suppressed; want suppressed")
	}
	if second.ID != first.ID {
		t.Fatalf("suppressed send returned %s, want the pending %s", second.ID, first.ID)
	}
}

func TestSendHandoffResendsAfterPendingHandoffRead(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	first, _, err := p.SendHandoff(autoHandoffIntent())
	if err != nil {
		t.Fatalf("first SendHandoff: %v", err)
	}
	if err := p.MarkRead(first.ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	b, err := store.Get(first.ID)
	if err != nil || b.Status != "open" {
		t.Fatalf("read handoff = status %q, err %v; want it still open", b.Status, err)
	}

	second, suppressed, err := p.SendHandoff(autoHandoffIntent())
	if err != nil {
		t.Fatalf("second SendHandoff: %v", err)
	}
	if suppressed {
		t.Fatal("send after the pending handoff was read suppressed; want a fresh unread handoff")
	}
	if second.ID == first.ID {
		t.Fatalf("second send reused %s; want a new message", first.ID)
	}

	pending, err := p.CheckAutoHandoffs([]string{"mayor"})
	if err != nil {
		t.Fatalf("CheckAutoHandoffs: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != second.ID {
		t.Fatalf("injectable handoffs = %v, want exactly the new %s", pending, second.ID)
	}
}
