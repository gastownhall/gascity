package beadmail

import (
	"errors"
	"slices"
	"strings"
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
	updates, setMetadata, closes, closeAlls int
	// ops records the order of the writes.
	ops []string
	// updateErr, when set, fails every Update after counting it.
	updateErr error
}

func (s *writeCountingStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	s.closeAlls++
	s.ops = append(s.ops, "closeAll")
	return s.MemStore.CloseAll(ids, metadata)
}

func (s *writeCountingStore) Update(id string, opts beads.UpdateOpts) error {
	s.updates++
	s.ops = append(s.ops, "update")
	if s.updateErr != nil {
		return s.updateErr
	}
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
// rows: an injected auto-handoff is stamped read and closed by ONE
// close-with-metadata write, THEN given the "read" label, with no read of the
// row the inbox already holds — so a hook killed mid-archive leaves the
// handoff archived, archived without its label, or still unread, never
// read-but-open — while ordinary mail and ids the inbox did not read are left
// alone.
func TestArchiveFromInboxRetiresWithoutRereading(t *testing.T) {
	store := &writeCountingStore{readCountingStore: &readCountingStore{MemStore: beads.NewMemStore()}}
	p := New(store)
	handoff, _, err := p.SendHandoff(mail.HandoffIntent{
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
	elsewhere, _, err := p.SendHandoff(mail.HandoffIntent{
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
	store.updates, store.setMetadata, store.closes, store.closeAlls = 0, 0, 0, 0

	if err := p.ArchiveInjectedAutoHandoffsFromInbox(inbox, []string{handoff.ID, ordinary.ID, elsewhere.ID}, nil); err != nil {
		t.Fatalf("ArchiveInjectedAutoHandoffsFromInbox: %v", err)
	}
	if store.gets != 0 || store.updates != 1 || store.setMetadata != 0 || store.closes != 0 || store.closeAlls != 1 {
		t.Fatalf("archive made %d gets, %d updates, %d metadata writes, %d closes, %d close-with-metadata writes; want 0, 1, 0, 0, 1",
			store.gets, store.updates, store.setMetadata, store.closes, store.closeAlls)
	}
	if !slices.Equal(store.ops, []string{"closeAll", "update"}) {
		t.Fatalf("archive wrote %v, want the close before the label (closeAll,update)", store.ops)
	}
	b, err := store.MemStore.Get(handoff.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b.Status != "closed" || !hasLabel(b.Labels, "read") || b.Metadata[mail.ReadMetadataKey] != "true" || b.Metadata["close_reason"] != RetentionSweepCloseReason {
		t.Fatalf("archived handoff = status %q labels %v metadata %v", b.Status, b.Labels, b.Metadata)
	}
	if got, err := p.Get(handoff.ID); err != nil || !got.Read {
		t.Fatalf("archived handoff reads back as (read=%v, %v), want a read, still addressable message", got.Read, err)
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

// TestArchiveWhoseLabelWriteFailsStillRetiresTheHandoff pins the window
// between the archive's two writes: when the "read" label cannot be added
// after the close (a kill, or a store that refuses it), the handoff is still
// closed, reads back read through mail.read and is never delivered again, and
// the failed label write is reported rather than swallowed.
func TestArchiveWhoseLabelWriteFailsStillRetiresTheHandoff(t *testing.T) {
	store := &writeCountingStore{readCountingStore: &readCountingStore{MemStore: beads.NewMemStore()}}
	p := New(store)
	handoff, _, err := p.SendHandoff(mail.HandoffIntent{
		From: "human", To: "sky", Subject: "context cycle", ThreadID: "t1",
		ExtraLabels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel},
	})
	if err != nil {
		t.Fatalf("SendHandoff: %v", err)
	}
	inbox, err := p.InboxForRoutes([]string{"sky"})
	if err != nil {
		t.Fatalf("InboxForRoutes: %v", err)
	}
	store.updateErr = errors.New("label write refused")

	err = p.ArchiveInjectedAutoHandoffsFromInbox(inbox, []string{handoff.ID}, nil)
	if err == nil || !strings.Contains(err.Error(), "label write refused") {
		t.Fatalf("ArchiveInjectedAutoHandoffsFromInbox = %v, want the label failure reported", err)
	}
	b, err := store.MemStore.Get(handoff.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b.Status != "closed" || b.Metadata[mail.ReadMetadataKey] != "true" {
		t.Fatalf("handoff after a failed label write = status %q metadata %v, want closed and read", b.Status, b.Metadata)
	}
	if got, err := p.Get(handoff.ID); err != nil || !got.Read {
		t.Fatalf("handoff reads back as (read=%v, %v), want read", got.Read, err)
	}
	again, err := p.CheckAutoHandoffs([]string{"sky"})
	if err != nil {
		t.Fatalf("CheckAutoHandoffs: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("CheckAutoHandoffs = %v, want the closed handoff never delivered again", again)
	}
}

// TestArchiveFromInboxClosesEveryHandoffBeforeAnyLabel pins the archive's two
// phases and the gate between them: every injected handoff is closed first,
// and each "read" label is written only while mayLabel still allows it — so a
// hook whose deadline runs out after the closes writes no label, rather than
// running into the deadline with a handoff already closed.
func TestArchiveFromInboxClosesEveryHandoffBeforeAnyLabel(t *testing.T) {
	for _, tc := range []struct {
		name       string
		labelsLeft int
		wantOps    []string
	}{
		{name: "time for every label", labelsLeft: 2, wantOps: []string{"closeAll", "closeAll", "update", "update"}},
		{name: "deadline passes after the first label", labelsLeft: 1, wantOps: []string{"closeAll", "closeAll", "update"}},
		{name: "deadline passes between close and label", labelsLeft: 0, wantOps: []string{"closeAll", "closeAll"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &writeCountingStore{readCountingStore: &readCountingStore{MemStore: beads.NewMemStore()}}
			p := New(store)
			var ids []string
			for _, thread := range []string{"t1", "t2"} {
				handoff, _, err := p.SendHandoff(mail.HandoffIntent{
					From: "human", To: "sky", Subject: "context cycle " + thread, ThreadID: thread,
					ExtraLabels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel},
				})
				if err != nil {
					t.Fatalf("SendHandoff: %v", err)
				}
				ids = append(ids, handoff.ID)
			}
			inbox, err := p.InboxForRoutes([]string{"sky"})
			if err != nil {
				t.Fatalf("InboxForRoutes: %v", err)
			}
			left := tc.labelsLeft
			mayLabel := func() bool {
				if left == 0 {
					return false
				}
				left--
				return true
			}
			if err := p.ArchiveInjectedAutoHandoffsFromInbox(inbox, ids, mayLabel); err != nil {
				t.Fatalf("ArchiveInjectedAutoHandoffsFromInbox: %v", err)
			}
			if !slices.Equal(store.ops, tc.wantOps) {
				t.Fatalf("archive wrote %v, want %v", store.ops, tc.wantOps)
			}
			for _, id := range ids {
				b, err := store.MemStore.Get(id)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if b.Status != "closed" || b.Metadata[mail.ReadMetadataKey] != "true" {
					t.Fatalf("handoff %s = status %q metadata %v, want closed and read", id, b.Status, b.Metadata)
				}
			}
		})
	}
}

// TestInboxUnreadAutoHandoffsMatchesCheckAutoHandoffs pins the SessionStart
// projection: the inbox's unread archive-after-inject handoffs are exactly
// what CheckAutoHandoffs returns for the session's addresses, in order.
func TestInboxUnreadAutoHandoffsMatchesCheckAutoHandoffs(t *testing.T) {
	_, p, info := newInboxFixture(t)
	labels := []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel}
	for i, to := range []string{"sky", info.ID, "old-sky", "someone-else"} {
		if _, _, err := p.SendHandoff(mail.HandoffIntent{From: "sky", To: to, Subject: "handoff " + to, ThreadID: "t" + to, ExtraLabels: labels}); err != nil {
			t.Fatalf("SendHandoff %d: %v", i, err)
		}
	}
	if _, _, err := p.SendHandoff(mail.HandoffIntent{From: "sky", To: "sky", Subject: "not archive-after-inject", ThreadID: "plain", ExtraLabels: []string{mail.AutoHandoffLabel}}); err != nil {
		t.Fatalf("SendHandoff: %v", err)
	}
	want, err := p.CheckAutoHandoffs(session.MailboxAddressesFromInfo(info))
	if err != nil {
		t.Fatalf("CheckAutoHandoffs: %v", err)
	}
	inbox, err := p.InboxForRoutes(session.RecipientRoutesFromInfo(info))
	if err != nil {
		t.Fatalf("InboxForRoutes: %v", err)
	}
	got := inbox.UnreadAutoHandoffs()
	if !slices.Equal(messageIDs(got), messageIDs(want)) || len(got) != 3 {
		t.Fatalf("UnreadAutoHandoffs = %v, want CheckAutoHandoffs' %v (3 handoffs)", messageIDs(got), messageIDs(want))
	}
}

// newSuccessionFixture seeds the session S that hooks read for — alias "sky",
// session name "runtime-sky", alias history "old-sky" — and returns the store,
// a provider over it, and a function that projects S's current Info.
func newSuccessionFixture(t *testing.T) (*readCountingStore, *Provider, func() session.Info) {
	t.Helper()
	store := &readCountingStore{MemStore: beads.NewMemStore()}
	p := New(store)
	s, err := store.Create(beads.Bead{
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"alias": "sky", "alias_history": "old-sky", "session_name": "runtime-sky"},
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	return store, p, func() session.Info {
		t.Helper()
		info, _, err := session.ResolveSessionRecordByExactID(store.MemStore, s.ID)
		if err != nil {
			t.Fatalf("ResolveSessionRecordByExactID: %v", err)
		}
		return info
	}
}

// createSessionBead creates a session bead carrying metadata, closed when
// closed is set, and returns its id.
func createSessionBead(t *testing.T, store *readCountingStore, metadata map[string]string, closed bool) string {
	t.Helper()
	b, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: metadata})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	if closed {
		if err := store.Close(b.ID); err != nil {
			t.Fatalf("Close session: %v", err)
		}
	}
	return b.ID
}

// sendTo sends one message to each address and returns the ids by address.
func sendTo(t *testing.T, p *Provider, addresses ...string) map[string]string {
	t.Helper()
	ids := make(map[string]string, len(addresses))
	for _, to := range addresses {
		m, err := p.Send("human", to, "", "for "+to)
		if err != nil {
			t.Fatalf("Send(%q): %v", to, err)
		}
		ids[to] = m.ID
	}
	return ids
}

// TestInboxForSessionSuccession pins the session inbox's definition against
// the per-address expansion it replaces, case by case, for the alias a
// session held before (its alias history):
//
//   - held now by another LIVE session: that session's mailbox is its own, so
//     none of its routes join (the per-address expansion leaked them);
//   - held by exactly one CLOSED session, the predecessor: its routes join, so
//     mail a sender routed to the predecessor's id or runtime name is not lost;
//   - held by a live session and one closed session: the live claimant owns
//     the alias, so the closed session is no predecessor and neither's routes
//     join;
//   - claimed by two closed sessions: ambiguous, so neither's routes join.
//
// In every case the alias itself stays one of the session's routes.
func TestInboxForSessionSuccession(t *testing.T) {
	t.Run("history alias held by a live session", func(t *testing.T) {
		store, p, current := newSuccessionFixture(t)
		other := createSessionBead(t, store, map[string]string{"alias": "old-sky", "session_name": "runtime-other"}, false)
		ids := sendTo(t, p, "sky", "old-sky", other, "runtime-other")
		inbox, err := p.InboxForSession(current())
		if err != nil {
			t.Fatalf("InboxForSession: %v", err)
		}
		assertInboxHolds(t, inbox, ids, []string{"sky", "old-sky"}, []string{other, "runtime-other"})
	})
	t.Run("history alias held by a closed predecessor", func(t *testing.T) {
		store, p, current := newSuccessionFixture(t)
		predecessor := createSessionBead(t, store, map[string]string{"alias": "old-sky", "session_name": "runtime-pred"}, true)
		ids := sendTo(t, p, "sky", "old-sky", predecessor, "runtime-pred")
		inbox, err := p.InboxForSession(current())
		if err != nil {
			t.Fatalf("InboxForSession: %v", err)
		}
		assertInboxHolds(t, inbox, ids, []string{"sky", "old-sky", predecessor, "runtime-pred"}, nil)

		// The per-address expansion delivers the same mail here.
		var legacy []mail.Message
		for _, address := range session.MailboxAddressesFromInfo(current()) {
			got, err := p.Check(address)
			if err != nil {
				t.Fatalf("Check(%q): %v", address, err)
			}
			legacy = append(legacy, got...)
		}
		for _, to := range []string{predecessor, "runtime-pred"} {
			if !slices.Contains(messageIDs(legacy), ids[to]) {
				t.Fatalf("the per-address expansion did not deliver mail for %q; the fixture does not pin succession", to)
			}
		}
	})
	t.Run("history alias held by a live session and one closed session", func(t *testing.T) {
		store, p, current := newSuccessionFixture(t)
		predecessor := createSessionBead(t, store, map[string]string{"alias": "old-sky", "session_name": "runtime-pred"}, true)
		live := createSessionBead(t, store, map[string]string{"alias": "old-sky", "session_name": "runtime-other"}, false)
		ids := sendTo(t, p, "sky", "old-sky", predecessor, "runtime-pred", live, "runtime-other")
		inbox, err := p.InboxForSession(current())
		if err != nil {
			t.Fatalf("InboxForSession: %v", err)
		}
		assertInboxHolds(t, inbox, ids, []string{"sky", "old-sky"}, []string{predecessor, "runtime-pred", live, "runtime-other"})
	})
	t.Run("history alias claimed by two closed sessions", func(t *testing.T) {
		store, p, current := newSuccessionFixture(t)
		first := createSessionBead(t, store, map[string]string{"alias": "old-sky", "session_name": "runtime-first"}, true)
		second := createSessionBead(t, store, map[string]string{"alias": "old-sky", "session_name": "runtime-second"}, true)
		ids := sendTo(t, p, "sky", "old-sky", first, "runtime-first", second, "runtime-second")
		inbox, err := p.InboxForSession(current())
		if err != nil {
			t.Fatalf("InboxForSession: %v", err)
		}
		assertInboxHolds(t, inbox, ids, []string{"sky", "old-sky"}, []string{first, "runtime-first", second, "runtime-second"})
	})
}

// assertInboxHolds checks that the inbox's unread mail carries the message
// sent to each address in want and none sent to an address in absent.
func assertInboxHolds(t *testing.T, inbox *Inbox, ids map[string]string, want, absent []string) {
	t.Helper()
	got := messageIDs(inbox.Unread())
	for _, to := range want {
		if !slices.Contains(got, ids[to]) {
			t.Errorf("inbox lacks the mail for %q (routes %v)", to, inbox.Routes())
		}
	}
	for _, to := range absent {
		if slices.Contains(got, ids[to]) {
			t.Errorf("inbox holds the mail for %q, another mailbox's (routes %v)", to, inbox.Routes())
		}
	}
}

// TestInboxForSessionReadBudget pins what defining the inbox costs: a session
// with no alias history reads no session at all, and each alias it held reads
// sessions only by keyed lookups — never a listing of every session.
func TestInboxForSessionReadBudget(t *testing.T) {
	store, p, current := newSuccessionFixture(t)
	info := current()
	info.AliasHistory = nil
	store.reset()
	if _, err := p.InboxForSession(info); err != nil {
		t.Fatalf("InboxForSession: %v", err)
	}
	if store.gets != 0 || store.sessionLists != 0 || store.messageLists != 1 {
		t.Fatalf("no history: %d gets, %d session lists, %d message lists; want 0, 0, 1", store.gets, store.sessionLists, store.messageLists)
	}

	recording := &keyedListStore{readCountingStore: store}
	p = New(recording)
	recording.reset()
	if _, err := p.InboxForSession(current()); err != nil {
		t.Fatalf("InboxForSession: %v", err)
	}
	if recording.unkeyed != 0 {
		t.Fatalf("%d session listings carried no address key", recording.unkeyed)
	}
	if recording.gets+recording.sessionLists > 6 || recording.messageLists != 1 {
		t.Fatalf("one history alias: %d gets, %d session lists, %d message lists; want at most 6 session reads and 1 message list",
			recording.gets, recording.sessionLists, recording.messageLists)
	}
}

// keyedListStore counts session listings that carry no alias or session_name
// key — a listing of every session.
type keyedListStore struct {
	*readCountingStore
	unkeyed int
}

func (s *keyedListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Type != "message" && query.Metadata["alias"] == "" && query.Metadata["session_name"] == "" {
		s.unkeyed++
	}
	return s.readCountingStore.List(query)
}

// TestPinnedSenderWinsASessionNameCollision pins what a pin persists when
// another live session's session name is the pinned session's alias: the
// command proved the sender is the pinned session, so the send records that
// session — where resolving the alias as an address would pick the other one.
func TestPinnedSenderWinsASessionNameCollision(t *testing.T) {
	store, p, current := newSuccessionFixture(t)
	info := current()
	other := createSessionBead(t, store, map[string]string{"alias": "elsewhere", "session_name": "sky"}, false)

	unpinned, err := p.Send("sky", "human", "", "unpinned")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if b, _ := store.MemStore.Get(unpinned.ID); b.Metadata[fromSessionIDMetadataKey] != other {
		t.Fatalf("unpinned send from %q recorded session %q; the fixture expects resolution to pick the colliding %s",
			"sky", b.Metadata[fromSessionIDMetadataKey], other)
	}

	p.PinSender(info)
	pinned, err := p.Send("sky", "human", "", "pinned")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	b, err := store.MemStore.Get(pinned.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b.Metadata[fromSessionIDMetadataKey] != info.ID {
		t.Fatalf("pinned send recorded session %q, want the pinned %s", b.Metadata[fromSessionIDMetadataKey], info.ID)
	}
}

// TestInboxAllAndCountsMatchTheProvider pins the inbox's all-mail and count
// projections against the provider's per-recipient answers for the same
// mailbox, so a reader that counts or lists from one inbox read agrees with
// one that asks address by address.
func TestInboxAllAndCountsMatchTheProvider(t *testing.T) {
	_, p, info := newInboxFixture(t)
	inbox, err := p.InboxForRoutes(session.RecipientRoutesFromInfo(info))
	if err != nil {
		t.Fatalf("InboxForRoutes: %v", err)
	}
	routes := inbox.Routes()

	var want []mail.Message
	for _, route := range routes {
		msgs, err := p.All(route)
		if err != nil {
			t.Fatalf("All(%q): %v", route, err)
		}
		for _, m := range msgs {
			if !slices.ContainsFunc(want, func(w mail.Message) bool { return w.ID == m.ID }) {
				want = append(want, m)
			}
		}
	}
	sortMessagesByArrival(want)
	if got := messageIDs(inbox.All()); !slices.Equal(got, messageIDs(want)) {
		t.Fatalf("Inbox.All = %v, want %v", got, messageIDs(want))
	}

	wantTotal, wantUnread, err := p.CountRecipients(routes)
	if err != nil {
		t.Fatalf("CountRecipients: %v", err)
	}
	if total, unread := inbox.Counts(); total != wantTotal || unread != wantUnread {
		t.Fatalf("Inbox.Counts = (%d, %d), want (%d, %d)", total, unread, wantTotal, wantUnread)
	}
	if total, unread := inbox.Counts(); total != len(inbox.All()) || unread != len(inbox.Unread()) || unread == total {
		t.Fatalf("Inbox.Counts = (%d, %d) for %d messages, %d unread; want them equal and some mail read",
			total, unread, len(inbox.All()), len(inbox.Unread()))
	}
}

// planRecordingStore records the message listings that reach the store.
type planRecordingStore struct {
	*readCountingStore
	messageQueries []beads.ListQuery
}

func (s *planRecordingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Type == "message" {
		s.messageQueries = append(s.messageQueries, query)
	}
	return s.readCountingStore.List(query)
}

// TestInboxForSessionWithPredecessorsStaysKeyed pins that a session inbox
// joining two closed predecessors' routes — more routes than one identity
// carries — still reads the mailbox by keyed requests on the native store
// (beads.NativeListPlan), never by one request that walks the whole ledger.
func TestInboxForSessionWithPredecessorsStaysKeyed(t *testing.T) {
	store, _, current := newSuccessionFixture(t)
	if err := store.SetMetadata(current().ID, "alias_history", "older-sky,old-sky"); err != nil {
		t.Fatalf("SetMetadata(alias_history): %v", err)
	}
	recording := &planRecordingStore{readCountingStore: store}
	p := New(recording)
	var preds []string
	for _, alias := range []string{"old-sky", "older-sky"} {
		preds = append(preds, createSessionBead(t, store, map[string]string{
			"alias": alias, "session_name": "runtime-" + alias, "alias_history": "first-" + alias,
		}, true))
	}
	ids := sendTo(t, p, append([]string{"sky"}, preds...)...)

	recording.messageQueries = nil
	inbox, err := p.InboxForSession(current())
	if err != nil {
		t.Fatalf("InboxForSession: %v", err)
	}
	assertInboxHolds(t, inbox, ids, append([]string{"sky"}, preds...), nil)
	if got := len(inbox.Routes()); got <= 8 {
		t.Fatalf("inbox joins %d routes; the fixture must exceed one identity's routes", got)
	}
	if len(recording.messageQueries) != 1 {
		t.Fatalf("read the mailbox with %d listings, want 1", len(recording.messageQueries))
	}
	plan := beads.NativeListPlan(recording.messageQueries[0])
	for _, req := range plan {
		if !beads.NativeListRequestKeyed(req) {
			t.Fatalf("the inbox listing plans an unkeyed request on the native store: %+v", recording.messageQueries[0])
		}
	}
	if len(plan) != len(inbox.Routes()) {
		t.Fatalf("native plan has %d requests, want one per route (%d)", len(plan), len(inbox.Routes()))
	}
}
