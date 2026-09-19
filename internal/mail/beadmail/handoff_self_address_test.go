package beadmail

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

// poolSessionDirectory resolves selectors against a fixed session set. It
// models the production shape of a pool member: the session bead carries a
// session_name ("builder-<id>") but no alias, so the only address its own
// commands can name it by is the raw session ID.
type poolSessionDirectory struct {
	infos []session.Info
}

func (d poolSessionDirectory) resolve(selector string) (session.Info, error) {
	for _, info := range d.infos {
		for _, route := range session.RecipientRoutesFromInfo(info) {
			if route == selector {
				return info, nil
			}
		}
	}
	return session.Info{}, session.ErrSessionNotFound
}

func (d poolSessionDirectory) ResolveAddress(selector string, _ bool) (session.Info, error) {
	return d.resolve(selector)
}

func (d poolSessionDirectory) ResolveMailboxAddress(selector string, _ bool) (session.Info, error) {
	return d.resolve(selector)
}

func (d poolSessionDirectory) ListAddresses(bool) ([]session.Info, error) {
	return d.infos, nil
}

var (
	pooledBuilder = session.Info{
		ID:                  "gm-wisp-gpenji",
		SessionNameMetadata: "builder-gm-wisp-gpenji",
	}
	namedMayor = session.Info{
		ID:                  "gm-wisp-mayor01",
		Alias:               "mayor",
		SessionNameMetadata: "mayor",
	}
	fleetDirectory = poolSessionDirectory{infos: []session.Info{pooledBuilder, namedMayor}}
)

// TestSendHandoffSelfAddressedPersistsOneAddress proves that a self-handoff --
// the shape gc handoff --auto sends from the PreCompact hook, where the caller
// passes ONE address as both From and To -- must not persist as two different
// addresses.
//
// SendHandoff normalizes only the From side (resolveSenderRoute ->
// senderDisplayAddress, beadmail.go:189/233) and stores intent.To verbatim as
// the bead Assignee (beadmail.go:197/213). For a pooled session with no alias,
// addressed by its own raw session ID, senderDisplayAddress falls through to
// the SessionNameMetadata branch (beadmail.go:249-251) and rewrites From to
// "builder-<id>" while Assignee stays "<id>".
//
// Both strings name the same session and delivery is unaffected --
// matchesRecipientRoute compares the assignee against
// session.RecipientRoutesFromInfo, which contains the ID -- but the persisted
// row now reads as agent-to-agent mail to every consumer that compares the two
// fields.
func TestSendHandoffSelfAddressedPersistsOneAddress(t *testing.T) {
	store := beads.NewMemStore()
	p := NewWithSessionDirectory(store, fleetDirectory)

	const selfAddress = "gm-wisp-gpenji"
	msg, err := p.SendHandoff(mail.HandoffIntent{
		From:        selfAddress,
		To:          selfAddress,
		Subject:     "context cycle",
		ThreadID:    "thread-deadbeef",
		ExtraLabels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel},
	})
	if err != nil {
		t.Fatalf("SendHandoff: %v", err)
	}

	b, err := store.Get(msg.ID)
	if err != nil {
		t.Fatalf("Get %s: %v", msg.ID, err)
	}
	if b.From != b.Assignee {
		t.Errorf("self-handoff persisted as cross-agent mail: bead From = %q, Assignee = %q; want both to name the same address",
			b.From, b.Assignee)
	}
	if msg.From != msg.To {
		t.Errorf("self-handoff message From = %q, To = %q; want both to name the same address", msg.From, msg.To)
	}
}

// TestSelfHandoffFromPooledSessionStaysInItsOwnInbox falsifies the delivery
// half of ga-28neu4: the claim that a compaction marker whose From and
// Assignee differ "lands in someone else's inbox and fires a real 'you have
// mail' notification".
//
// It does not. matchesRecipientRoute compares the bead assignee against
// session.RecipientRoutesFromInfo by exact string equality, and that route set
// contains the session's own ID -- so the marker is delivered to exactly the
// session that sent it, and is invisible to every other mailbox.
func TestSelfHandoffFromPooledSessionStaysInItsOwnInbox(t *testing.T) {
	store := beads.NewMemStore()
	p := NewWithSessionDirectory(store, fleetDirectory)

	msg, err := p.SendHandoff(mail.HandoffIntent{
		From:        pooledBuilder.ID,
		To:          pooledBuilder.ID,
		Subject:     "context cycle",
		ThreadID:    "thread-deadbeef",
		ExtraLabels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel},
	})
	if err != nil {
		t.Fatalf("SendHandoff: %v", err)
	}

	// The sending session sees its own marker, addressed by either of the two
	// renderings of its address.
	for _, selector := range []string{pooledBuilder.ID, pooledBuilder.SessionNameMetadata} {
		got, err := p.Inbox(selector)
		if err != nil {
			t.Fatalf("Inbox(%q): %v", selector, err)
		}
		if len(got) != 1 || got[0].ID != msg.ID {
			t.Errorf("Inbox(%q) = %d message(s), want exactly the marker %s", selector, len(got), msg.ID)
		}
	}

	// No other mailbox sees it. This is the claim ga-28neu4 rests on.
	others, err := p.Inbox("mayor")
	if err != nil {
		t.Fatalf("Inbox(mayor): %v", err)
	}
	if len(others) != 0 {
		t.Errorf("Inbox(mayor) = %d message(s), want 0; a self-addressed compaction marker leaked into another agent's inbox", len(others))
	}
}
