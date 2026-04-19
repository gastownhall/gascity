package beadmail

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
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

// --- Compile-time interface check ---

var _ mail.Provider = (*Provider)(nil)
