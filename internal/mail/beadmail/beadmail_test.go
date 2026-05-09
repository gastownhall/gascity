package beadmail

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

// noListScanStore errors when List is called without a filter, proving that
// Inbox/Count/All use targeted type queries instead of broad scans.
type noListScanStore struct {
	*beads.MemStore
}

func (s noListScanStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if !query.HasFilter() {
		return nil, errors.New("unfiltered List() must not be called — use targeted queries")
	}
	return s.MemStore.List(query)
}

type noBroadSessionRouteStore struct {
	*beads.MemStore
	t *testing.T
}

func (s noBroadSessionRouteStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == session.LabelSession && len(query.Metadata) == 0 {
		s.t.Fatalf("recipient routing used broad session scan: %+v", query)
	}
	return s.MemStore.List(query)
}

func TestInboxDoesNotCallBroadList(t *testing.T) {
	base := beads.NewMemStore()
	p := New(noListScanStore{MemStore: base})

	if _, err := p.Send("human", "mayor", "", "targeted"); err != nil {
		t.Fatal(err)
	}

	msgs, err := p.Inbox("mayor")
	if err != nil {
		t.Fatalf("Inbox should use targeted queries: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("Inbox = %d messages, want 1", len(msgs))
	}
}

func TestCheckDoesNotUseMessageLabelSupplement(t *testing.T) {
	runner := func(_ string, name string, args ...string) ([]byte, error) {
		cmd := name + " " + strings.Join(args, " ")
		if strings.Contains(cmd, "--label=gc:message") {
			t.Fatalf("mail check used gc:message label supplement: %s", cmd)
		}
		if strings.Contains(cmd, "--assignee=mayor") && strings.Contains(cmd, "--type=message") && strings.Contains(cmd, "--status=open") {
			return []byte(`[{"id":"msg-1","title":"hello","description":"body","status":"open","issue_type":"message","assignee":"mayor","from":"human","created_at":"2026-01-02T03:04:05Z","labels":["gc:message"]}]`), nil
		}
		return nil, errors.New("unexpected command: " + cmd)
	}
	p := New(beads.NewBdStore(t.TempDir(), runner))

	msgs, err := p.Check("mayor")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != "msg-1" {
		t.Fatalf("Check = %#v, want msg-1", msgs)
	}
}

func TestCountDoesNotCallBroadList(t *testing.T) {
	base := beads.NewMemStore()
	p := New(noListScanStore{MemStore: base})

	if _, err := p.Send("human", "mayor", "", "count me"); err != nil {
		t.Fatal(err)
	}

	total, unread, err := p.Count("mayor")
	if err != nil {
		t.Fatalf("Count should use targeted queries: %v", err)
	}
	if total != 1 || unread != 1 {
		t.Errorf("Count = (%d, %d), want (1, 1)", total, unread)
	}
}

func TestAllDoesNotCallBroadList(t *testing.T) {
	base := beads.NewMemStore()
	p := New(noListScanStore{MemStore: base})

	if _, err := p.Send("human", "mayor", "", "all msg"); err != nil {
		t.Fatal(err)
	}

	msgs, err := p.All("mayor")
	if err != nil {
		t.Fatalf("All should use targeted queries: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("All = %d messages, want 1", len(msgs))
	}
}

// --- Empty-recipient (global) path ---

func TestCountEmptyRecipient(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	if _, err := p.Send("human", "mayor", "", "msg1"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Send("human", "deacon", "", "msg2"); err != nil {
		t.Fatal(err)
	}

	total, unread, err := p.Count("")
	if err != nil {
		t.Fatalf("Count empty recipient: %v", err)
	}
	if total != 2 || unread != 2 {
		t.Errorf("Count(\"\") = (%d, %d), want (2, 2)", total, unread)
	}
}

func TestAllEmptyRecipient(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	if _, err := p.Send("human", "mayor", "", "msg1"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Send("human", "deacon", "", "msg2"); err != nil {
		t.Fatal(err)
	}

	msgs, err := p.All("")
	if err != nil {
		t.Fatalf("All empty recipient: %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("All(\"\") = %d messages, want 2", len(msgs))
	}
}

// --- Send ---

func TestSend(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	m, err := p.Send("human", "mayor", "Hello", "hello there")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if m.ID == "" {
		t.Error("Send returned empty ID")
	}
	if m.From != "human" {
		t.Errorf("From = %q, want %q", m.From, "human")
	}
	if m.To != "mayor" {
		t.Errorf("To = %q, want %q", m.To, "mayor")
	}
	if m.Subject != "Hello" {
		t.Errorf("Subject = %q, want %q", m.Subject, "Hello")
	}
	if m.Body != "hello there" {
		t.Errorf("Body = %q, want %q", m.Body, "hello there")
	}
	if m.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if m.ThreadID == "" {
		t.Error("ThreadID is empty — new messages should get a thread ID")
	}

	// Verify underlying bead.
	b, err := store.Get(m.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Type != "message" {
		t.Errorf("bead Type = %q, want %q", b.Type, "message")
	}
	if b.Status != "open" {
		t.Errorf("bead Status = %q, want %q", b.Status, "open")
	}
	if hasLabel(b.Labels, "gc:message") {
		t.Error("bead should no longer carry the legacy gc:message label")
	}
}

func TestSendStoresStableSessionRouteWithoutChangingDisplaySender(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sender, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "gascity/workflows.codex-min-9",
			"session_name": "workflows__codex-min-mc-sender",
		},
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	msg, err := p.Send("gascity/workflows.codex-min-9", "human", "Approval", "please approve")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if msg.From != "gascity/workflows.codex-min-9" {
		t.Fatalf("message From = %q, want display alias", msg.From)
	}
	b, err := store.Get(msg.ID)
	if err != nil {
		t.Fatalf("Get message: %v", err)
	}
	if b.From != "gascity/workflows.codex-min-9" {
		t.Fatalf("bead From = %q, want display alias", b.From)
	}
	if b.Metadata[fromSessionIDMetadataKey] != sender.ID {
		t.Fatalf("%s = %q, want %q", fromSessionIDMetadataKey, b.Metadata[fromSessionIDMetadataKey], sender.ID)
	}
	if b.Metadata[fromDisplayMetadataKey] != "gascity/workflows.codex-min-9" {
		t.Fatalf("%s = %q, want original display alias", fromDisplayMetadataKey, b.Metadata[fromDisplayMetadataKey])
	}
}

func TestReplyUsesStoredSenderSessionIDAfterAliasRename(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sender, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "old-sender",
			"session_name": "sender-gc-42",
		},
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	original, err := p.Send("old-sender", "human", "Approval", "please approve")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := store.SetMetadataBatch(sender.ID, session.UpdatedAliasMetadata(sender.Metadata, "new-sender")); err != nil {
		t.Fatalf("SetMetadataBatch(alias rename): %v", err)
	}

	reply, err := p.Reply(original.ID, "human", "approved", "approved")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply.To != "old-sender" {
		t.Fatalf("reply To = %q, want original display sender", reply.To)
	}
	b, err := store.Get(reply.ID)
	if err != nil {
		t.Fatalf("Get reply: %v", err)
	}
	if b.Assignee != sender.ID {
		t.Fatalf("reply bead Assignee = %q, want stable sender session ID %q", b.Assignee, sender.ID)
	}
	if b.Metadata[toSessionIDMetadataKey] != sender.ID {
		t.Fatalf("reply %s = %q, want %q", toSessionIDMetadataKey, b.Metadata[toSessionIDMetadataKey], sender.ID)
	}
	if b.Metadata[toDisplayMetadataKey] != "old-sender" {
		t.Fatalf("reply %s = %q, want original display sender", toDisplayMetadataKey, b.Metadata[toDisplayMetadataKey])
	}
	inbox, err := p.Inbox("new-sender")
	if err != nil {
		t.Fatalf("Inbox(new-sender): %v", err)
	}
	if len(inbox) != 1 || inbox[0].ID != reply.ID {
		t.Fatalf("Inbox(new-sender) = %#v, want reply %s", inbox, reply.ID)
	}
	oldInbox, err := p.Inbox("old-sender")
	if err != nil {
		t.Fatalf("Inbox(old-sender): %v", err)
	}
	if len(oldInbox) != 1 || oldInbox[0].ID != reply.ID {
		t.Fatalf("Inbox(old-sender) = %#v, want reply %s", oldInbox, reply.ID)
	}
	total, unread, err := p.Count("new-sender")
	if err != nil {
		t.Fatalf("Count(new-sender): %v", err)
	}
	if total != 1 || unread != 1 {
		t.Fatalf("Count(new-sender) = (%d, %d), want (1, 1)", total, unread)
	}
}

func TestSendFallsBackToLiteralSenderWhenSessionIdentifierIsAmbiguous(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	for i := 0; i < 2; i++ {
		if _, err := store.Create(beads.Bead{
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"alias": "duplicate",
			},
		}); err != nil {
			t.Fatalf("Create session %d: %v", i, err)
		}
	}

	msg, err := p.Send("duplicate", "human", "subject", "body")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if msg.From != "duplicate" {
		t.Fatalf("message From = %q, want literal ambiguous sender", msg.From)
	}
	b, err := store.Get(msg.ID)
	if err != nil {
		t.Fatalf("Get message: %v", err)
	}
	if b.Metadata[fromSessionIDMetadataKey] != "" {
		t.Fatalf("ambiguous sender stored %s = %q, want empty", fromSessionIDMetadataKey, b.Metadata[fromSessionIDMetadataKey])
	}
}

func TestInboxFallsBackToLiteralRecipientWhenSessionIdentifierIsAmbiguous(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	for i := 0; i < 2; i++ {
		if _, err := store.Create(beads.Bead{
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"alias": "duplicate",
			},
		}); err != nil {
			t.Fatalf("Create session %d: %v", i, err)
		}
	}
	msg, err := p.Send("human", "duplicate", "subject", "body")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	inbox, err := p.Inbox("duplicate")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(inbox) != 1 || inbox[0].ID != msg.ID {
		t.Fatalf("Inbox = %#v, want literal recipient message %s", inbox, msg.ID)
	}
}

func TestSendRejectsEmptyRecipient(t *testing.T) {
	p := New(beads.NewMemStore())
	if _, err := p.Send("human", "", "subject", "body"); err == nil {
		t.Fatal("Send with empty recipient should error")
	}
}

func TestGetRejectsNonMessageType(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	b, err := store.Create(beads.Bead{Title: "task", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(b.ID); err == nil {
		t.Error("Get should reject non-message bead")
	}

	untyped, err := store.Create(beads.Bead{Title: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(untyped.ID); err == nil {
		t.Error("Get should reject bead with empty type (Type=\"message\" is now required)")
	}
}

// --- Inbox ---

func TestInboxEmpty(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	msgs, err := p.Inbox("mayor")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("Inbox = %d messages, want 0", len(msgs))
	}
}

func TestInboxFilters(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	// Message to mayor.
	if _, err := p.Send("human", "mayor", "", "for mayor"); err != nil {
		t.Fatal(err)
	}
	// Message to worker.
	if _, err := p.Send("human", "worker", "", "for worker"); err != nil {
		t.Fatal(err)
	}
	// Task bead (not a message).
	store.Create(beads.Bead{Title: "a task"}) //nolint:errcheck

	msgs, err := p.Inbox("mayor")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Inbox = %d messages, want 1", len(msgs))
	}
	if msgs[0].Body != "for mayor" {
		t.Errorf("Body = %q, want %q", msgs[0].Body, "for mayor")
	}
}

func TestInboxExcludesRead(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	m, err := p.Send("human", "mayor", "", "will be read")
	if err != nil {
		t.Fatal(err)
	}
	// Read (marks as read, NOT closed).
	if _, err := p.Read(m.ID); err != nil {
		t.Fatal(err)
	}

	msgs, err := p.Inbox("mayor")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("Inbox = %d messages, want 0 (read messages excluded)", len(msgs))
	}
}

// --- Get ---

func TestGet(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("human", "mayor", "Subject", "body")
	if err != nil {
		t.Fatal(err)
	}

	m, err := p.Get(sent.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.Subject != "Subject" {
		t.Errorf("Subject = %q, want %q", m.Subject, "Subject")
	}
	if m.Body != "body" {
		t.Errorf("Body = %q, want %q", m.Body, "body")
	}
	if m.Read {
		t.Error("Get should not mark as read")
	}
}

func TestGetNotFound(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	_, err := p.Get("gc-999")
	if err == nil {
		t.Error("Get should fail for nonexistent ID")
	}
}

// --- Read ---

func TestRead(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("human", "mayor", "Sub", "read me")
	if err != nil {
		t.Fatal(err)
	}

	m, err := p.Read(sent.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if m.Body != "read me" {
		t.Errorf("Body = %q, want %q", m.Body, "read me")
	}
	if !m.Read {
		t.Error("Read should set Read = true")
	}

	// Bead should still be open (not closed).
	b, err := store.Get(sent.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Status != "open" {
		t.Errorf("bead Status = %q, want %q (Read must not close beads)", b.Status, "open")
	}
	if !hasLabel(b.Labels, "read") {
		t.Error("bead missing 'read' label")
	}
}

func TestReadDoesNotClose(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("human", "mayor", "", "still accessible")
	if err != nil {
		t.Fatal(err)
	}

	// Read it.
	if _, err := p.Read(sent.ID); err != nil {
		t.Fatal(err)
	}

	// Get should still return it.
	m, err := p.Get(sent.ID)
	if err != nil {
		t.Fatalf("Get after Read: %v", err)
	}
	if m.Body != "still accessible" {
		t.Errorf("Body = %q, want %q", m.Body, "still accessible")
	}
}

func TestReadAlreadyRead(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("human", "mayor", "", "old news")
	if err != nil {
		t.Fatal(err)
	}
	// Mark as read via label.
	store.Update(sent.ID, beads.UpdateOpts{Labels: []string{"read"}}) //nolint:errcheck

	// Reading already-read message should still return it.
	m, err := p.Read(sent.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if m.Body != "old news" {
		t.Errorf("Body = %q, want %q", m.Body, "old news")
	}
}

func TestReadNotFound(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	_, err := p.Read("gc-999")
	if err == nil {
		t.Error("Read should fail for nonexistent ID")
	}
}

// --- MarkRead / MarkUnread ---

func TestMarkReadMarkUnread(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("human", "mayor", "", "toggle me")
	if err != nil {
		t.Fatal(err)
	}

	// MarkRead.
	if err := p.MarkRead(sent.ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	msgs, err := p.Inbox("mayor")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("Inbox after MarkRead = %d, want 0", len(msgs))
	}

	// MarkUnread.
	if err := p.MarkUnread(sent.ID); err != nil {
		t.Fatalf("MarkUnread: %v", err)
	}
	msgs, err = p.Inbox("mayor")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Errorf("Inbox after MarkUnread = %d, want 1", len(msgs))
	}
}

// --- Archive ---

func TestArchive(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("human", "mayor", "", "dismiss me")
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Archive(sent.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Bead should be closed.
	b, err := store.Get(sent.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Status != "closed" {
		t.Errorf("bead Status = %q, want %q", b.Status, "closed")
	}
}

func TestArchiveNonMessage(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	// Create a task bead (not a message).
	b, err := store.Create(beads.Bead{Title: "a task"})
	if err != nil {
		t.Fatal(err)
	}

	err = p.Archive(b.ID)
	if err == nil {
		t.Error("Archive should fail for non-message beads")
	}
}

func TestArchiveAlreadyClosed(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("human", "mayor", "", "old")
	if err != nil {
		t.Fatal(err)
	}
	store.Close(sent.ID) //nolint:errcheck

	// Archiving already-closed message returns ErrAlreadyArchived.
	err = p.Archive(sent.ID)
	if !errors.Is(err, mail.ErrAlreadyArchived) {
		t.Errorf("Archive already closed: got %v, want ErrAlreadyArchived", err)
	}
}

func TestArchiveNotFound(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	err := p.Archive("gc-999")
	if err == nil {
		t.Error("Archive should fail for nonexistent ID")
	}
}

// TestArchiveStampsCloseReason verifies that Archive stamps
// close_reason=MailArchivedCloseReason on the closed message bead.
// Without this, bd's validation.on-close=error rejects the close and
// leaves the message open silently.
func TestArchiveStampsCloseReason(t *testing.T) {
	if got := len(MailArchivedCloseReason); got < 20 {
		t.Fatalf("MailArchivedCloseReason = %q (%d chars), want >=20", MailArchivedCloseReason, got)
	}

	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("human", "mayor", "", "dismiss me")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Archive(sent.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	b, err := store.Get(sent.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := b.Metadata["close_reason"]; got != MailArchivedCloseReason {
		t.Errorf("close_reason = %q, want %q", got, MailArchivedCloseReason)
	}
}

// TestArchiveManyStampsCloseReason verifies the batch-archive path also
// stamps close_reason on every closed bead.
func TestArchiveManyStampsCloseReason(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	a, err := p.Send("human", "mayor", "", "first")
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Send("human", "mayor", "", "second")
	if err != nil {
		t.Fatal(err)
	}

	results, err := p.ArchiveMany([]string{a.ID, b.ID})
	if err != nil {
		t.Fatalf("ArchiveMany: %v", err)
	}
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("ArchiveMany[%d].Err = %v", i, r.Err)
		}
	}
	for _, id := range []string{a.ID, b.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("store.Get(%s): %v", id, err)
		}
		if got.Status != "closed" {
			t.Errorf("bead %s status = %q, want closed", id, got.Status)
		}
		if reason := got.Metadata["close_reason"]; reason != MailArchivedCloseReason {
			t.Errorf("bead %s close_reason = %q, want %q", id, reason, MailArchivedCloseReason)
		}
	}
}

// --- Delete ---

func TestDelete(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("human", "mayor", "", "delete me")
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Delete(sent.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	b, err := store.Get(sent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Errorf("bead Status = %q, want %q", b.Status, "closed")
	}
}

// --- Reply ---

func TestReply(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("alice", "bob", "Hello", "first message")
	if err != nil {
		t.Fatal(err)
	}

	reply, err := p.Reply(sent.ID, "bob", "RE: Hello", "reply body")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	if reply.To != "alice" {
		t.Errorf("Reply To = %q, want %q (original sender)", reply.To, "alice")
	}
	if reply.From != "bob" {
		t.Errorf("Reply From = %q, want %q", reply.From, "bob")
	}
	if reply.ThreadID != sent.ThreadID {
		t.Errorf("Reply ThreadID = %q, want %q (inherited)", reply.ThreadID, sent.ThreadID)
	}
	if reply.ReplyTo != sent.ID {
		t.Errorf("Reply ReplyTo = %q, want %q", reply.ReplyTo, sent.ID)
	}
}

// TestReplyDerivesSubjectFromOriginal ensures an empty subject is replaced
// with "Re: <original-subject>", so underlying stores that require a
// non-empty title (e.g. BdStore → `bd create`) don't reject the reply.
func TestReplyDerivesSubjectFromOriginal(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("alice", "bob", "Hello", "first message")
	if err != nil {
		t.Fatal(err)
	}

	reply, err := p.Reply(sent.ID, "bob", "", "reply body")
	if err != nil {
		t.Fatalf("Reply with empty subject: %v", err)
	}
	if reply.Subject != "Re: Hello" {
		t.Errorf("Reply Subject = %q, want %q", reply.Subject, "Re: Hello")
	}
}

// TestReplyPreservesExplicitSubject ensures an explicit subject is passed
// through unchanged — no automatic "Re:" prefixing.
func TestReplyPreservesExplicitSubject(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("alice", "bob", "Hello", "first message")
	if err != nil {
		t.Fatal(err)
	}

	reply, err := p.Reply(sent.ID, "bob", "Custom subject", "reply body")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply.Subject != "Custom subject" {
		t.Errorf("Reply Subject = %q, want %q", reply.Subject, "Custom subject")
	}
}

// TestReplyAvoidsDoubleRePrefix ensures that replying to a message whose
// subject already starts with "Re:" does not produce "Re: Re: ..." when
// the caller omits the subject.
func TestReplyAvoidsDoubleRePrefix(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("alice", "bob", "Re: Hello", "body")
	if err != nil {
		t.Fatal(err)
	}

	reply, err := p.Reply(sent.ID, "bob", "", "reply body")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply.Subject != "Re: Hello" {
		t.Errorf("Reply Subject = %q, want %q (no double prefix)", reply.Subject, "Re: Hello")
	}
}

// TestReplyFallsBackToBodyWhenOriginalTitleEmpty covers the degenerate case
// where an original message somehow has no title (possible in stores that
// don't enforce title). The reply still gets a non-empty title.
func TestReplyFallsBackToBodyWhenOriginalTitleEmpty(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	// Create a message bead directly without a title.
	orig, err := store.Create(beads.Bead{
		Type:     "message",
		Assignee: "bob",
		From:     "alice",
		Labels:   []string{"thread:t1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	reply, err := p.Reply(orig.ID, "bob", "", "a terse reply body")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply.Subject == "" {
		t.Error("Reply Subject is empty; must be non-empty so bd create won't reject")
	}
	if reply.Subject != "a terse reply body" {
		t.Errorf("Reply Subject = %q, want %q (first line of body)", reply.Subject, "a terse reply body")
	}
}

// TestReplyAgainstBdStoreValidatesTitle is a regression test that exercises
// the real BdStore code path: the fake runner emulates `bd create`'s
// title-required validation. Without a derived title, Reply would fail here.
func TestReplyAgainstBdStoreValidatesTitle(t *testing.T) {
	// Fake runner that rejects `bd create` with empty positional title,
	// the same way the real bd binary does.
	runner := func(_ string, name string, args ...string) ([]byte, error) {
		if name != "bd" {
			return nil, errors.New("unexpected command: " + name)
		}
		switch args[0] {
		case "create":
			// args: create --json <title> -t <type> [flags...]
			if len(args) < 3 {
				return nil, errors.New("bd create: too few args")
			}
			title := args[2]
			if title == "" {
				return nil, errors.New(`exit status 1: {"error":"validation failed for issue : title is required"}`)
			}
			// Return a minimal issue JSON.
			id := "bd-" + title
			return []byte(`{"id":"` + id + `","title":"` + title + `","status":"open","issue_type":"message","created_at":"2026-04-24T00:00:00Z"}`), nil
		case "show":
			// bd show --json returns a JSON array.
			return []byte(`[{"id":"bd-Hello","title":"Hello","status":"open","issue_type":"message","assignee":"bob","from":"alice","created_at":"2026-04-24T00:00:00Z","labels":["thread:t1"]}]`), nil
		case "update":
			return []byte(`{}`), nil
		case "list":
			return []byte(`[]`), nil
		}
		return nil, errors.New("unexpected bd subcommand: " + args[0])
	}
	p := New(beads.NewBdStore(t.TempDir(), runner))

	// Reply with empty subject — must succeed because the provider derives
	// "Re: Hello" from the original message.
	reply, err := p.Reply("bd-Hello", "bob", "", "reply body")
	if err != nil {
		t.Fatalf("Reply should derive a non-empty title to pass bd validation: %v", err)
	}
	if reply.Subject != "Re: Hello" {
		t.Errorf("Reply Subject = %q, want %q", reply.Subject, "Re: Hello")
	}
}

func TestReplyPrefersStoredSenderSessionID(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sender, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "gascity/workflows.codex-min-9",
			"session_name": "workflows__codex-min-mc-sender",
		},
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	responder, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "gascity/workflows.codex-min-10",
			"session_name": "workflows__codex-min-mc-responder",
		},
	})
	if err != nil {
		t.Fatalf("Create responder session: %v", err)
	}
	original, err := store.Create(beads.Bead{
		Title:       "Approval needed",
		Description: "please approve",
		Type:        "message",
		Assignee:    "human",
		From:        "gascity/workflows.codex-min-9",
		Labels:      []string{"thread:stable-route"},
		Metadata: map[string]string{
			fromSessionIDMetadataKey: sender.ID,
			fromDisplayMetadataKey:   "gascity/workflows.codex-min-9",
		},
	})
	if err != nil {
		t.Fatalf("Create original message: %v", err)
	}

	reply, err := p.Reply(original.ID, "gascity/workflows.codex-min-10", "approved", "approved")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	if reply.To != "gascity/workflows.codex-min-9" {
		t.Fatalf("reply To = %q, want sender display alias", reply.To)
	}
	if reply.From != "gascity/workflows.codex-min-10" {
		t.Fatalf("reply From = %q, want display alias", reply.From)
	}
	b, err := store.Get(reply.ID)
	if err != nil {
		t.Fatalf("Get reply: %v", err)
	}
	if b.Metadata[fromSessionIDMetadataKey] != responder.ID {
		t.Fatalf("reply %s = %q, want %q", fromSessionIDMetadataKey, b.Metadata[fromSessionIDMetadataKey], responder.ID)
	}
	if b.Metadata[fromDisplayMetadataKey] != "gascity/workflows.codex-min-10" {
		t.Fatalf("reply %s = %q, want responder display alias", fromDisplayMetadataKey, b.Metadata[fromDisplayMetadataKey])
	}
	if b.Assignee != sender.ID {
		t.Fatalf("reply bead Assignee = %q, want stable sender session ID %q", b.Assignee, sender.ID)
	}
	if b.Metadata[toSessionIDMetadataKey] != sender.ID {
		t.Fatalf("reply %s = %q, want %q", toSessionIDMetadataKey, b.Metadata[toSessionIDMetadataKey], sender.ID)
	}
	if b.Metadata[toDisplayMetadataKey] != "gascity/workflows.codex-min-9" {
		t.Fatalf("reply %s = %q, want sender display alias", toDisplayMetadataKey, b.Metadata[toDisplayMetadataKey])
	}
}

func TestReplyToClosedSenderSessionIsDiscoverableByHistoricalAlias(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sender, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "gascity/workflows.codex-min-9",
			"alias_history": "gascity/workflows.codex-min-8",
			"session_name":  "workflows__codex-min-mc-sender",
		},
	})
	if err != nil {
		t.Fatalf("Create sender session: %v", err)
	}
	responder, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "gascity/workflows.codex-min-10",
			"session_name": "workflows__codex-min-mc-responder",
		},
	})
	if err != nil {
		t.Fatalf("Create responder session: %v", err)
	}
	original, err := store.Create(beads.Bead{
		Title:       "Approval needed",
		Description: "please approve",
		Type:        "message",
		Assignee:    "human",
		From:        "gascity/workflows.codex-min-8",
		Labels:      []string{"thread:closed-sender-route"},
		Metadata: map[string]string{
			fromSessionIDMetadataKey: sender.ID,
			fromDisplayMetadataKey:   "gascity/workflows.codex-min-8",
		},
	})
	if err != nil {
		t.Fatalf("Create original message: %v", err)
	}
	if err := store.Close(sender.ID); err != nil {
		t.Fatalf("Close sender session: %v", err)
	}

	reply, err := p.Reply(original.ID, "gascity/workflows.codex-min-10", "approved", "approved")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply.To != "gascity/workflows.codex-min-8" {
		t.Fatalf("reply To = %q, want historical sender display alias", reply.To)
	}
	if reply.From != "gascity/workflows.codex-min-10" {
		t.Fatalf("reply From = %q, want responder display alias", reply.From)
	}
	b, err := store.Get(reply.ID)
	if err != nil {
		t.Fatalf("Get reply: %v", err)
	}
	if b.Assignee != sender.ID {
		t.Fatalf("reply bead Assignee = %q, want closed sender session ID %q", b.Assignee, sender.ID)
	}
	if b.Metadata[fromSessionIDMetadataKey] != responder.ID {
		t.Fatalf("reply %s = %q, want %q", fromSessionIDMetadataKey, b.Metadata[fromSessionIDMetadataKey], responder.ID)
	}

	msgs, err := p.Inbox("gascity/workflows.codex-min-8")
	if err != nil {
		t.Fatalf("Inbox by historical alias: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Inbox by historical alias returned %d messages, want 1", len(msgs))
	}
	if msgs[0].ID != reply.ID {
		t.Fatalf("Inbox by historical alias returned %s, want reply %s", msgs[0].ID, reply.ID)
	}
}

func TestRecipientRoutesPreferLiveSessionOverClosedHistory(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	closed, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "old-worker",
			"alias_history": "worker",
			"session_name":  "workflows__codex-min-mc-old",
		},
	})
	if err != nil {
		t.Fatalf("Create closed session: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close session: %v", err)
	}
	live, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "worker",
			"session_name": "workflows__codex-min-mc-live",
		},
	})
	if err != nil {
		t.Fatalf("Create live session: %v", err)
	}
	closedReply, err := store.Create(beads.Bead{
		Title:    "old reply",
		Type:     "message",
		Assignee: closed.ID,
		From:     "human",
	})
	if err != nil {
		t.Fatalf("Create closed reply: %v", err)
	}
	liveMail, err := store.Create(beads.Bead{
		Title:    "live mail",
		Type:     "message",
		Assignee: live.ID,
		From:     "human",
	})
	if err != nil {
		t.Fatalf("Create live mail: %v", err)
	}

	msgs, err := p.Inbox("worker")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Inbox returned %d messages, want 1", len(msgs))
	}
	if msgs[0].ID != liveMail.ID {
		t.Fatalf("Inbox returned %s, want live message %s; closed reply was %s", msgs[0].ID, liveMail.ID, closedReply.ID)
	}
}

func TestInboxByCurrentSessionAliasAvoidsBroadSessionScan(t *testing.T) {
	store := noBroadSessionRouteStore{MemStore: beads.NewMemStore(), t: t}
	p := New(store)

	closed, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "old-worker",
			"alias_history": "worker",
			"session_name":  "workflows__codex-min-mc-old",
		},
	})
	if err != nil {
		t.Fatalf("Create closed session: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close session: %v", err)
	}
	live, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "worker",
			"session_name": "workflows__codex-min-mc-live",
		},
	})
	if err != nil {
		t.Fatalf("Create live session: %v", err)
	}
	closedReply, err := store.Create(beads.Bead{
		Title:    "old reply",
		Type:     "message",
		Assignee: closed.ID,
		From:     "human",
	})
	if err != nil {
		t.Fatalf("Create closed reply: %v", err)
	}
	liveMail, err := store.Create(beads.Bead{
		Title:    "live mail",
		Type:     "message",
		Assignee: live.ID,
		From:     "human",
	})
	if err != nil {
		t.Fatalf("Create live mail: %v", err)
	}

	msgs, err := p.Inbox("worker")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Inbox returned %d messages, want 1", len(msgs))
	}
	if msgs[0].ID != liveMail.ID {
		t.Fatalf("Inbox returned %s, want live message %s; closed reply was %s", msgs[0].ID, liveMail.ID, closedReply.ID)
	}
}

func TestInboxByClosedCurrentSessionAliasAvoidsBroadSessionScan(t *testing.T) {
	store := noBroadSessionRouteStore{MemStore: beads.NewMemStore(), t: t}
	p := New(store)

	closed, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "worker",
			"session_name": "workflows__codex-min-mc-closed",
		},
	})
	if err != nil {
		t.Fatalf("Create closed session: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close session: %v", err)
	}
	closedMail, err := store.Create(beads.Bead{
		Title:    "closed mail",
		Type:     "message",
		Assignee: closed.ID,
		From:     "human",
	})
	if err != nil {
		t.Fatalf("Create closed mail: %v", err)
	}

	msgs, err := p.Inbox("worker")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Inbox returned %d messages, want 1", len(msgs))
	}
	if msgs[0].ID != closedMail.ID {
		t.Fatalf("Inbox returned %s, want closed mail %s", msgs[0].ID, closedMail.ID)
	}
}

func TestInboxByHistoricalAliasFallsBackToSessionScan(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	live, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "new-worker",
			"alias_history": "worker",
			"session_name":  "workflows__codex-min-mc-live",
		},
	})
	if err != nil {
		t.Fatalf("Create live session: %v", err)
	}
	liveMail, err := store.Create(beads.Bead{
		Title:    "live mail",
		Type:     "message",
		Assignee: live.ID,
		From:     "human",
	})
	if err != nil {
		t.Fatalf("Create live mail: %v", err)
	}

	msgs, err := p.Inbox("worker")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Inbox returned %d messages, want 1", len(msgs))
	}
	if msgs[0].ID != liveMail.ID {
		t.Fatalf("Inbox returned %s, want live message %s", msgs[0].ID, liveMail.ID)
	}
}

func TestRecipientRoutesPreferCurrentAddressOverHistoricalAliasAmbiguity(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	historical, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "new-worker",
			"alias_history": "worker",
			"session_name":  "workflows__codex-min-mc-history",
		},
	})
	if err != nil {
		t.Fatalf("Create historical session: %v", err)
	}
	current, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "worker",
			"session_name": "workflows__codex-min-mc-current",
		},
	})
	if err != nil {
		t.Fatalf("Create current session: %v", err)
	}
	historicalMail, err := store.Create(beads.Bead{
		Title:    "historical mail",
		Type:     "message",
		Assignee: historical.ID,
		From:     "human",
	})
	if err != nil {
		t.Fatalf("Create historical mail: %v", err)
	}
	currentMail, err := store.Create(beads.Bead{
		Title:    "current mail",
		Type:     "message",
		Assignee: current.ID,
		From:     "human",
	})
	if err != nil {
		t.Fatalf("Create current mail: %v", err)
	}

	msgs, err := p.Inbox("worker")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Inbox returned %d messages, want 1", len(msgs))
	}
	if msgs[0].ID != currentMail.ID {
		t.Fatalf("Inbox returned %s, want current mail %s; historical mail was %s", msgs[0].ID, currentMail.ID, historicalMail.ID)
	}
}

func TestRecipientRoutesPreferClosedCurrentAddressOverLiveHistoricalAlias(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	liveHistorical, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "new-worker",
			"alias_history": "worker",
			"session_name":  "workflows__codex-min-mc-live",
		},
	})
	if err != nil {
		t.Fatalf("Create live historical session: %v", err)
	}
	closedCurrent, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "worker",
			"session_name": "workflows__codex-min-mc-closed",
		},
	})
	if err != nil {
		t.Fatalf("Create closed current session: %v", err)
	}
	if err := store.Close(closedCurrent.ID); err != nil {
		t.Fatalf("Close current session: %v", err)
	}
	liveMail, err := store.Create(beads.Bead{
		Title:    "live historical mail",
		Type:     "message",
		Assignee: liveHistorical.ID,
		From:     "human",
	})
	if err != nil {
		t.Fatalf("Create live mail: %v", err)
	}
	closedMail, err := store.Create(beads.Bead{
		Title:    "closed current mail",
		Type:     "message",
		Assignee: closedCurrent.ID,
		From:     "human",
	})
	if err != nil {
		t.Fatalf("Create closed mail: %v", err)
	}

	msgs, err := p.Inbox("worker")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Inbox returned %d messages, want 1", len(msgs))
	}
	if msgs[0].ID != closedMail.ID {
		t.Fatalf("Inbox returned %s, want closed current mail %s; live historical mail was %s", msgs[0].ID, closedMail.ID, liveMail.ID)
	}
}

// --- Thread ---

func TestThread(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("alice", "bob", "Hello", "first")
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Reply(sent.ID, "bob", "RE: Hello", "second")
	if err != nil {
		t.Fatal(err)
	}

	msgs, err := p.Thread(sent.ThreadID)
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Thread = %d messages, want 2", len(msgs))
	}
	// First should be the original (earlier CreatedAt).
	if msgs[0].Body != "first" {
		t.Errorf("Thread[0].Body = %q, want %q", msgs[0].Body, "first")
	}
	if msgs[1].Body != "second" {
		t.Errorf("Thread[1].Body = %q, want %q", msgs[1].Body, "second")
	}
}

func TestThreadEmpty(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	msgs, err := p.Thread("nonexistent")
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("Thread = %d messages, want 0", len(msgs))
	}
}

// TestThreadAcceptsMessageIDOfOriginal locks in the fix for #1526. Callers
// (notably `gc mail thread <id>` from cmd/gc/cmd_mail.go) pass a *message*
// bead-ID, not the underlying thread-ID. Provider.Thread must resolve the
// message-ID to its thread label and return the thread.
func TestThreadAcceptsMessageIDOfOriginal(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("alice", "bob", "Hello", "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Reply(sent.ID, "bob", "Re: Hello", "second"); err != nil {
		t.Fatal(err)
	}

	msgs, err := p.Thread(sent.ID)
	if err != nil {
		t.Fatalf("Thread(%q): %v", sent.ID, err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Thread(messageID) = %d messages, want 2", len(msgs))
	}
	if msgs[0].Body != "first" || msgs[1].Body != "second" {
		t.Errorf("Thread(messageID) bodies = [%q, %q], want [first, second]", msgs[0].Body, msgs[1].Body)
	}
}

// TestThreadSurfacesNonNotFoundStoreErrors verifies that a real store I/O
// failure during message-id resolution propagates to the caller instead of
// being silently swallowed as "treat input as thread-id".
func TestThreadSurfacesNonNotFoundStoreErrors(t *testing.T) {
	mem := beads.NewMemStore()
	failing := &getErrorStore{MemStore: mem, getErr: errors.New("simulated I/O failure")}
	p := New(failing)

	_, err := p.Thread("anything")
	if err == nil {
		t.Fatal("Thread: expected error from underlying store, got nil")
	}
	if !strings.Contains(err.Error(), "simulated I/O failure") {
		t.Errorf("Thread: error %q does not wrap underlying store error", err)
	}
}

func TestThreadRejectsNonMessageBeadID(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)
	task, err := store.Create(beads.Bead{
		Title:  "not mail",
		Type:   "task",
		Labels: []string{"thread:looks-mail-like"},
	})
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}

	_, err = p.Thread(task.ID)
	if err == nil {
		t.Fatal("Thread(non-message bead ID): expected error, got nil")
	}
	if !strings.Contains(err.Error(), `bead "`) || !strings.Contains(err.Error(), "want message") {
		t.Fatalf("Thread(non-message bead ID) error = %q, want clear non-message diagnostic", err)
	}
}

// getErrorStore returns a custom error from Get; List defers to MemStore.
type getErrorStore struct {
	*beads.MemStore
	getErr error
}

func (s *getErrorStore) Get(_ string) (beads.Bead, error) {
	return beads.Bead{}, s.getErr
}

// TestThreadAcceptsMessageIDOfReply ensures the resolution works regardless
// of which message in the thread the caller hands us — the parent OR any
// reply should both surface the full thread.
func TestThreadAcceptsMessageIDOfReply(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("alice", "bob", "Hello", "first")
	if err != nil {
		t.Fatal(err)
	}
	reply, err := p.Reply(sent.ID, "bob", "Re: Hello", "second")
	if err != nil {
		t.Fatal(err)
	}

	msgs, err := p.Thread(reply.ID)
	if err != nil {
		t.Fatalf("Thread(%q): %v", reply.ID, err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Thread(replyID) = %d messages, want 2", len(msgs))
	}
	if msgs[0].Body != "first" || msgs[1].Body != "second" {
		t.Errorf("Thread(replyID) bodies = [%q, %q], want [first, second]", msgs[0].Body, msgs[1].Body)
	}
}

// --- Count ---

func TestCount(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	if _, err := p.Send("alice", "bob", "", "msg1"); err != nil {
		t.Fatal(err)
	}
	m2, err := p.Send("alice", "bob", "", "msg2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Send("alice", "charlie", "", "not bob's"); err != nil {
		t.Fatal(err)
	}

	// Mark one as read.
	if err := p.MarkRead(m2.ID); err != nil {
		t.Fatal(err)
	}

	total, unread, err := p.Count("bob")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if unread != 1 {
		t.Errorf("unread = %d, want 1", unread)
	}
}

func TestCountRecipientsEmptyDoesNotCountAllMessages(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)
	if _, err := p.Send("human", "mayor", "", "msg"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	total, unread, err := p.CountRecipients(nil)
	if err != nil {
		t.Fatalf("CountRecipients(nil): %v", err)
	}
	if total != 0 || unread != 0 {
		t.Fatalf("CountRecipients(nil) = (%d,%d), want (0,0)", total, unread)
	}
}

// --- Check ---

func TestCheck(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	if _, err := p.Send("human", "mayor", "", "check me"); err != nil {
		t.Fatal(err)
	}

	msgs, err := p.Check("mayor")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Check = %d messages, want 1", len(msgs))
	}
	if msgs[0].Body != "check me" {
		t.Errorf("Body = %q, want %q", msgs[0].Body, "check me")
	}

	// Check should NOT mark as read (bead still open, no read label).
	b, err := store.Get(msgs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "open" {
		t.Errorf("bead Status = %q, want %q (Check must not close beads)", b.Status, "open")
	}
	if hasLabel(b.Labels, "read") {
		t.Error("Check should not add read label")
	}
}

// --- Provider session-list cache (ga-q6ct) ---

// countingSessionListStore counts broad gc:session List calls and forwards
// the rest. Used to pin that Provider memoizes the gc:session enumeration
// across multiple Inbox calls in a single command invocation.
type countingSessionListStore struct {
	*beads.MemStore
	sessionListCalls int
}

func (s *countingSessionListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == session.LabelSession && len(query.Metadata) == 0 {
		s.sessionListCalls++
	}
	return s.MemStore.List(query)
}

func TestProvider_DefaultProviderSeesNewHistoricalAliasSessionAcrossCalls(t *testing.T) {
	// Pin: the default Provider is safe for long-lived shared use. If a lookup
	// runs before the matching session exists, later lookups must see newly
	// created sessions instead of reusing a stale provider-lifetime snapshot.
	store := &countingSessionListStore{MemStore: beads.NewMemStore()}
	p := New(store)

	if _, err := p.Inbox("old-route"); err != nil {
		t.Fatalf("initial Inbox(old-route): %v", err)
	}

	sessionBead, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "worker-a",
			"alias_history": "old-route",
			"session_name":  "wf__a",
		},
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	if _, err := p.Send("human", sessionBead.Metadata["alias"], "", "for old route"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	msgs, err := p.Inbox("old-route")
	if err != nil {
		t.Fatalf("second Inbox(old-route): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Inbox(old-route) = %d messages, want 1", len(msgs))
	}
	if msgs[0].Body != "for old route" {
		t.Fatalf("Inbox(old-route) body = %q, want %q", msgs[0].Body, "for old route")
	}
	if store.sessionListCalls != 2 {
		t.Errorf("broad gc:session List calls = %d, want 2 (default provider must refetch per call to avoid stale shared state)", store.sessionListCalls)
	}
}

func TestProviderCached_BroadSessionListCachedAcrossInboxCalls(t *testing.T) {
	// Pin: the command-scoped cached Provider still dedupes the broad
	// historical-alias session scan within one provider lifetime.
	store := &countingSessionListStore{MemStore: beads.NewMemStore()}

	// Two live sessions with alias_history that includes the route we'll
	// search for. AliasHistory lookup is the path that does the broad scan.
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "worker-a",
			"alias_history": "old-route",
			"session_name":  "wf__a",
		},
	}); err != nil {
		t.Fatalf("Create session A: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "worker-b",
			"alias_history": "old-route-2",
			"session_name":  "wf__b",
		},
	}); err != nil {
		t.Fatalf("Create session B: %v", err)
	}

	p := NewCached(store)

	// Exercise three independent Inbox calls that each force the
	// alias-history fallback (no current alias matches "old-route" or
	// "old-route-2"). Without the cache: 3 broad scans. With cache: 1.
	for _, recipient := range []string{"old-route", "old-route-2", "old-route"} {
		if _, err := p.Inbox(recipient); err != nil {
			t.Fatalf("Inbox(%q): %v", recipient, err)
		}
	}

	if store.sessionListCalls != 1 {
		t.Errorf("broad gc:session List calls = %d, want 1 (Provider must cache the enumeration)", store.sessionListCalls)
	}
}

// --- Compile-time interface check ---

var _ mail.Provider = (*Provider)(nil)
