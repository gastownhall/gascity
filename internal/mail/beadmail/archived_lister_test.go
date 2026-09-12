package beadmail

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
)

// TestAllIncludingArchivedListsArchivedMail: Archive closes a message bead
// rather than deleting it, and AllIncludingArchived still lists it (the
// receipt a caller needs after the recipient dismissed the mail) while All
// does not. Another recipient's mail stays out.
func TestAllIncludingArchivedListsArchivedMail(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)
	sent, err := p.Send("controller", "mayor", "PARKED gp-1 [park abc]", "body")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Send("controller", "worker", "PARKED gp-2 [park def]", "other recipient"); err != nil {
		t.Fatal(err)
	}
	if err := p.Archive(sent.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	open, err := p.All("mayor")
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("All after Archive = %d messages, want 0", len(open))
	}
	var lister mail.ArchivedLister = p
	all, err := lister.AllIncludingArchived("mayor")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != sent.ID || all[0].Subject != "PARKED gp-1 [park abc]" {
		t.Fatalf("AllIncludingArchived = %+v, want only the archived message %s", all, sent.ID)
	}
}
