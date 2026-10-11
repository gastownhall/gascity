package beadmail

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

// readCountingStore counts the reads that reach the store.
type readCountingStore struct {
	*beads.MemStore
	gets         int
	sessionLists int
	messageLists int
}

func (s *readCountingStore) Get(id string) (beads.Bead, error) {
	s.gets++
	return s.MemStore.Get(id)
}

func (s *readCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Type == "message" {
		s.messageLists++
	} else {
		s.sessionLists++
	}
	return s.MemStore.List(query)
}

func (s *readCountingStore) reset() { s.gets, s.sessionLists, s.messageLists = 0, 0, 0 }

// newInboxFixture seeds a session with an alias, a session name and alias
// history, mail to each of its routes (one of it read), and mail to someone
// else.
func newInboxFixture(t *testing.T) (*readCountingStore, *Provider, session.Info) {
	t.Helper()
	store := &readCountingStore{MemStore: beads.NewMemStore()}
	p := New(store)
	sessionBead, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "old-sky",
			"session_name":  "runtime-sky",
		},
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	for _, to := range []string{"sky", sessionBead.ID, "old-sky", "runtime-sky", "someone-else"} {
		if _, err := p.Send("human", to, "", "for "+to); err != nil {
			t.Fatalf("Send(%q): %v", to, err)
		}
	}
	read, err := p.Send("human", "sky", "", "already read")
	if err != nil {
		t.Fatalf("Send(read): %v", err)
	}
	if err := p.MarkRead(read.ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	info, _, err := session.ResolveSessionRecordByExactID(store, sessionBead.ID)
	if err != nil {
		t.Fatalf("ResolveSessionRecordByExactID: %v", err)
	}
	store.reset()
	return store, p, info
}

func messageIDs(messages []mail.Message) []string {
	ids := make([]string, 0, len(messages))
	for _, m := range messages {
		ids = append(ids, m.ID)
	}
	return ids
}

// TestInboxForRoutesMatchesPerAddressCheck pins the equivalence the hooks rely
// on: reading a session's own route set as given returns exactly the unread
// mail that checking each of its mailbox addresses (expanding each) returns,
// in the same order — with one message read and no session reads.
func TestInboxForRoutesMatchesPerAddressCheck(t *testing.T) {
	store, p, info := newInboxFixture(t)

	var want []mail.Message
	seen := map[string]bool{}
	for _, address := range session.MailboxAddressesFromInfo(info) {
		got, err := p.Check(address)
		if err != nil {
			t.Fatalf("Check(%q): %v", address, err)
		}
		for _, m := range got {
			if !seen[m.ID] {
				seen[m.ID] = true
				want = append(want, m)
			}
		}
	}
	sortMessagesByArrival(want)
	store.reset()

	inbox, err := p.InboxForRoutes(session.RecipientRoutesFromInfo(info))
	if err != nil {
		t.Fatalf("InboxForRoutes: %v", err)
	}
	got := inbox.Unread()
	if !slices.Equal(messageIDs(got), messageIDs(want)) {
		t.Fatalf("InboxForRoutes unread = %v, want the per-address check's %v", messageIDs(got), messageIDs(want))
	}
	if len(got) != 4 {
		t.Fatalf("unread = %d messages, want the 4 unread ones addressed to the session's routes", len(got))
	}
	if store.messageLists != 1 || store.sessionLists != 0 || store.gets != 0 {
		t.Fatalf("reads = %d message lists, %d session lists, %d gets; want 1, 0, 0", store.messageLists, store.sessionLists, store.gets)
	}
	if !slices.Equal(inbox.Routes(), session.RecipientRoutesFromInfo(info)) {
		t.Fatalf("Routes() = %v, want %v", inbox.Routes(), session.RecipientRoutesFromInfo(info))
	}
}

func TestInboxForRoutesRefusesNoRoutes(t *testing.T) {
	store, p, _ := newInboxFixture(t)
	for _, routes := range [][]string{nil, {}, {" ", ""}} {
		if _, err := p.InboxForRoutes(routes); err == nil {
			t.Fatalf("InboxForRoutes(%q) succeeded; an empty route set must not read every mailbox", routes)
		}
	}
	if store.messageLists != 0 {
		t.Fatalf("an empty route set read the ledger %d times", store.messageLists)
	}
}

func TestPinnedSenderMatchesResolvedSenderRoute(t *testing.T) {
	for _, from := range []string{"sky", "runtime-sky", "id"} {
		t.Run(from, func(t *testing.T) {
			store, p, info := newInboxFixture(t)
			if from == "id" {
				from = info.ID
			}
			resolved, err := p.Send(from, "human", "", "resolved")
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			wantBead, err := store.MemStore.Get(resolved.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}

			p.PinSender(info)
			store.reset()
			pinned, err := p.Send(from, "human", "", "pinned")
			if err != nil {
				t.Fatalf("Send pinned: %v", err)
			}
			if store.gets != 0 || store.sessionLists != 0 {
				t.Fatalf("pinned send read the session store: %d gets, %d lists", store.gets, store.sessionLists)
			}
			gotBead, err := store.MemStore.Get(pinned.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if gotBead.From != wantBead.From ||
				gotBead.Metadata[fromSessionIDMetadataKey] != wantBead.Metadata[fromSessionIDMetadataKey] ||
				gotBead.Metadata[fromDisplayMetadataKey] != wantBead.Metadata[fromDisplayMetadataKey] {
				t.Fatalf("pinned sender route = (%q, %v), want the resolved (%q, %v)", gotBead.From, gotBead.Metadata, wantBead.From, wantBead.Metadata)
			}
		})
	}
}

func TestPinnedSenderIgnoresOtherAddresses(t *testing.T) {
	store, p, info := newInboxFixture(t)
	p.PinSender(info)
	// A historical alias is not an address ResolveAddress resolves to the
	// session, so it is not taken from the pin.
	msg, err := p.Send("old-sky", "human", "", "from history")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if store.gets == 0 {
		t.Fatal("a sender the pin does not name skipped resolution")
	}
	b, err := store.MemStore.Get(msg.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b.Metadata[fromSessionIDMetadataKey] != "" {
		t.Fatalf("historical-alias sender got session route %q, want the literal sender", b.Metadata[fromSessionIDMetadataKey])
	}

	closed := info
	closed.Closed = true
	p.PinSender(closed)
	store.reset()
	if _, err := p.Send("sky", "human", "", "closed pin"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if store.gets == 0 {
		t.Fatal("a closed pinned session answered for its alias without resolution")
	}
}

// writeCountingStore counts the writes an archive makes.
type writeCountingStore struct {
	*readCountingStore
	updates, setMetadata, closes int
}

func (s *writeCountingStore) Update(id string, opts beads.UpdateOpts) error {
	s.updates++
	return s.MemStore.Update(id, opts)
}

func (s *writeCountingStore) SetMetadata(id, key, value string) error {
	s.setMetadata++
	return s.MemStore.SetMetadata(id, key, value)
}

func (s *writeCountingStore) Close(id string) error {
	s.closes++
	return s.MemStore.Close(id)
}

// TestArchiveFromInboxRetiresWithoutRereading pins the archive from inbox
// rows: an injected auto-handoff is marked read, stamped and closed with one
// update and one close and no read of the row the inbox already holds, while
// ordinary mail and ids the inbox did not read are left alone.
func TestArchiveFromInboxRetiresWithoutRereading(t *testing.T) {
	store := &writeCountingStore{readCountingStore: &readCountingStore{MemStore: beads.NewMemStore()}}
	p := New(store)
	handoff, err := p.SendHandoff(mail.HandoffIntent{
		From: "human", To: "sky", Subject: "context cycle", ThreadID: "t1",
		ExtraLabels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel},
	})
	if err != nil {
		t.Fatalf("SendHandoff: %v", err)
	}
	ordinary, err := p.Send("human", "sky", "", "ordinary")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	elsewhere, err := p.SendHandoff(mail.HandoffIntent{
		From: "human", To: "other", Subject: "not this inbox", ThreadID: "t2",
		ExtraLabels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel},
	})
	if err != nil {
		t.Fatalf("SendHandoff: %v", err)
	}
	inbox, err := p.InboxForRoutes([]string{"sky"})
	if err != nil {
		t.Fatalf("InboxForRoutes: %v", err)
	}
	store.reset()
	store.updates, store.setMetadata, store.closes = 0, 0, 0

	if err := p.ArchiveInjectedAutoHandoffsFromInbox(inbox, []string{handoff.ID, ordinary.ID, elsewhere.ID}); err != nil {
		t.Fatalf("ArchiveInjectedAutoHandoffsFromInbox: %v", err)
	}
	if store.gets != 0 || store.updates != 1 || store.setMetadata != 0 || store.closes != 1 {
		t.Fatalf("archive made %d gets, %d updates, %d metadata writes, %d closes; want 0, 1, 0, 1",
			store.gets, store.updates, store.setMetadata, store.closes)
	}
	b, err := store.MemStore.Get(handoff.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b.Status != "closed" || !hasLabel(b.Labels, "read") || b.Metadata[mail.ReadMetadataKey] != "true" || b.Metadata["close_reason"] != RetentionSweepCloseReason {
		t.Fatalf("archived handoff = status %q labels %v metadata %v", b.Status, b.Labels, b.Metadata)
	}
	for _, id := range []string{ordinary.ID, elsewhere.ID} {
		other, err := store.MemStore.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if other.Status != "open" || hasLabel(other.Labels, "read") {
			t.Fatalf("%s was archived: status %q labels %v", id, other.Status, other.Labels)
		}
	}
}
