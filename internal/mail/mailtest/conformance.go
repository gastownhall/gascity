// Package mailtest provides a conformance test suite for [mail.Provider]
// implementations. Each implementation's test file calls [RunProviderTests]
// with its own factory function.
package mailtest

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/mail"
)

// RunProviderTests runs the full conformance suite against a Provider.
// newProvider returns a fresh, empty provider per test.
func RunProviderTests(t *testing.T, newProvider func(t *testing.T) mail.Provider) {
	t.Helper()

	// --- Group 1: Send ---

	t.Run("Send_ReturnsMatchingFields", func(t *testing.T) {
		p := newProvider(t)
		m, err := p.Send("alice", "bob", "Greetings", "hello")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if m.ID == "" {
			t.Error("Send returned empty ID")
		}
		if m.From != "alice" {
			t.Errorf("From = %q, want %q", m.From, "alice")
		}
		if m.To != "bob" {
			t.Errorf("To = %q, want %q", m.To, "bob")
		}
		if m.Subject != "Greetings" {
			t.Errorf("Subject = %q, want %q", m.Subject, "Greetings")
		}
		if m.Body != "hello" {
			t.Errorf("Body = %q, want %q", m.Body, "hello")
		}
	})

	t.Run("Send_AssignsUniqueIDs", func(t *testing.T) {
		p := newProvider(t)
		m1, err := p.Send("alice", "bob", "", "first")
		if err != nil {
			t.Fatalf("Send 1: %v", err)
		}
		m2, err := p.Send("alice", "bob", "", "second")
		if err != nil {
			t.Fatalf("Send 2: %v", err)
		}
		if m1.ID == m2.ID {
			t.Errorf("two sends produced same ID %q", m1.ID)
		}
	})

	t.Run("Send_SetsRecentCreatedAt", func(t *testing.T) {
		p := newProvider(t)
		before := time.Now().Add(-time.Minute)
		m, err := p.Send("alice", "bob", "", "timestamped")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		after := time.Now().Add(time.Minute)
		if m.CreatedAt.Before(before) || m.CreatedAt.After(after) {
			t.Errorf("CreatedAt = %v, want within ±1 minute of now", m.CreatedAt)
		}
	})

	// --- Group 2: Inbox ---

	t.Run("Inbox_EmptyReturnsNil", func(t *testing.T) {
		p := newProvider(t)
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if msgs != nil {
			t.Errorf("Inbox on empty provider = %v, want nil", msgs)
		}
	})

	t.Run("Inbox_ReturnsMessagesSentToRecipient", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "", "for bob")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("Inbox = %d messages, want 1", len(msgs))
		}
		if msgs[0].ID != sent.ID {
			t.Errorf("Inbox msg ID = %q, want %q", msgs[0].ID, sent.ID)
		}
	})

	t.Run("Inbox_FiltersByRecipient", func(t *testing.T) {
		p := newProvider(t)
		if _, err := p.Send("alice", "bob", "", "for bob"); err != nil {
			t.Fatalf("Send to bob: %v", err)
		}
		if _, err := p.Send("alice", "charlie", "", "for charlie"); err != nil {
			t.Fatalf("Send to charlie: %v", err)
		}
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("Inbox(bob) = %d messages, want 1", len(msgs))
		}
		if msgs[0].To != "bob" {
			t.Errorf("Inbox msg To = %q, want %q", msgs[0].To, "bob")
		}
	})

	t.Run("Inbox_ExcludesReadMessages", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "", "will be read")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if _, err := p.Read(sent.ID); err != nil {
			t.Fatalf("Read: %v", err)
		}
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("Inbox after Read = %d messages, want 0", len(msgs))
		}
	})

	// --- Group 3: Check ---

	t.Run("Check_ReturnsUnreadMessages", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "", "check me")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		msgs, err := p.Check("bob")
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("Check = %d messages, want 1", len(msgs))
		}
		if msgs[0].ID != sent.ID {
			t.Errorf("Check msg ID = %q, want %q", msgs[0].ID, sent.ID)
		}
	})

	t.Run("Check_DoesNotMarkAsRead", func(t *testing.T) {
		p := newProvider(t)
		if _, err := p.Send("alice", "bob", "", "peek"); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if _, err := p.Check("bob"); err != nil {
			t.Fatalf("Check: %v", err)
		}
		// Inbox should still see the message.
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 1 {
			t.Errorf("Inbox after Check = %d messages, want 1", len(msgs))
		}
	})

	// --- Group 4: Read ---

	t.Run("Read_ReturnsCorrectMessage", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "Sub", "read me")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		m, err := p.Read(sent.ID)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if m.ID != sent.ID {
			t.Errorf("ID = %q, want %q", m.ID, sent.ID)
		}
		if m.From != "alice" {
			t.Errorf("From = %q, want %q", m.From, "alice")
		}
		if m.To != "bob" {
			t.Errorf("To = %q, want %q", m.To, "bob")
		}
		if m.Body != "read me" {
			t.Errorf("Body = %q, want %q", m.Body, "read me")
		}
	})

	t.Run("Read_MarksAsRead", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "", "once")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if _, err := p.Read(sent.ID); err != nil {
			t.Fatalf("Read: %v", err)
		}
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("Inbox after Read = %d messages, want 0", len(msgs))
		}
	})

	t.Run("Read_MessageStillAccessible", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "", "still here")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if _, err := p.Read(sent.ID); err != nil {
			t.Fatalf("Read: %v", err)
		}
		// Get should still return the message (not closed).
		m, err := p.Get(sent.ID)
		if err != nil {
			t.Fatalf("Get after Read: %v", err)
		}
		if m.Body != "still here" {
			t.Errorf("Body = %q, want %q", m.Body, "still here")
		}
	})

	t.Run("Read_UnknownIDReturnsError", func(t *testing.T) {
		p := newProvider(t)
		_, err := p.Read("nonexistent")
		if err == nil {
			t.Error("Read(nonexistent) should return error")
		}
	})

	// --- Group 5: Get ---

	t.Run("Get_ReturnsMessage", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "Hi", "body")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		m, err := p.Get(sent.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if m.ID != sent.ID {
			t.Errorf("ID = %q, want %q", m.ID, sent.ID)
		}
		if m.Body != "body" {
			t.Errorf("Body = %q, want %q", m.Body, "body")
		}
	})

	t.Run("Get_DoesNotMarkRead", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "", "peek")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if _, err := p.Get(sent.ID); err != nil {
			t.Fatalf("Get: %v", err)
		}
		// Inbox should still show it.
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 1 {
			t.Errorf("Inbox after Get = %d, want 1 (Get must not mark read)", len(msgs))
		}
	})

	t.Run("Get_UnknownIDErrors", func(t *testing.T) {
		p := newProvider(t)
		_, err := p.Get("nonexistent")
		if err == nil {
			t.Error("Get(nonexistent) should return error")
		}
	})

	// --- Group 6: MarkRead / MarkUnread ---

	t.Run("MarkRead_FiltersFromInbox", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "", "mark me")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if err := p.MarkRead(sent.ID); err != nil {
			t.Fatalf("MarkRead: %v", err)
		}
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("Inbox after MarkRead = %d, want 0", len(msgs))
		}
	})

	t.Run("MarkUnread_RestoresToInbox", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "", "toggle")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if err := p.MarkRead(sent.ID); err != nil {
			t.Fatalf("MarkRead: %v", err)
		}
		if err := p.MarkUnread(sent.ID); err != nil {
			t.Fatalf("MarkUnread: %v", err)
		}
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 1 {
			t.Errorf("Inbox after MarkUnread = %d, want 1", len(msgs))
		}
	})

	// --- Group 7: Reply ---

	t.Run("Reply_InheritsThreadID", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "Hello", "first")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		reply, err := p.Reply(sent.ID, "bob", "RE: Hello", "reply")
		if err != nil {
			t.Fatalf("Reply: %v", err)
		}
		if reply.ThreadID != sent.ThreadID {
			t.Errorf("Reply ThreadID = %q, want %q", reply.ThreadID, sent.ThreadID)
		}
	})

	t.Run("Reply_SetsReplyTo", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "Hello", "first")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		reply, err := p.Reply(sent.ID, "bob", "RE: Hello", "reply")
		if err != nil {
			t.Fatalf("Reply: %v", err)
		}
		if reply.ReplyTo != sent.ID {
			t.Errorf("Reply ReplyTo = %q, want %q", reply.ReplyTo, sent.ID)
		}
	})

	t.Run("Reply_GoesToOriginalSender", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "Hello", "first")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		reply, err := p.Reply(sent.ID, "bob", "RE: Hello", "reply")
		if err != nil {
			t.Fatalf("Reply: %v", err)
		}
		if reply.To != "alice" {
			t.Errorf("Reply To = %q, want %q (original sender)", reply.To, "alice")
		}
		if reply.From != "bob" {
			t.Errorf("Reply From = %q, want %q", reply.From, "bob")
		}
	})

	// --- Group 8: Thread ---

	t.Run("Thread_ReturnsAllInThread", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "Hello", "first")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if _, err := p.Reply(sent.ID, "bob", "RE: Hello", "second"); err != nil {
			t.Fatalf("Reply: %v", err)
		}
		msgs, err := p.Thread(sent.ThreadID)
		if err != nil {
			t.Fatalf("Thread: %v", err)
		}
		if len(msgs) != 2 {
			t.Fatalf("Thread = %d messages, want 2", len(msgs))
		}
	})

	t.Run("Thread_Empty", func(t *testing.T) {
		p := newProvider(t)
		msgs, err := p.Thread("nonexistent-thread")
		if err != nil {
			t.Fatalf("Thread: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("Thread(nonexistent) = %d messages, want 0", len(msgs))
		}
	})

	// --- Group 9: Count ---

	t.Run("Count_TotalAndUnread", func(t *testing.T) {
		p := newProvider(t)
		if _, err := p.Send("alice", "bob", "", "msg1"); err != nil {
			t.Fatalf("Send 1: %v", err)
		}
		m2, err := p.Send("alice", "bob", "", "msg2")
		if err != nil {
			t.Fatalf("Send 2: %v", err)
		}
		if err := p.MarkRead(m2.ID); err != nil {
			t.Fatalf("MarkRead: %v", err)
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
	})

	t.Run("Count_EmptyInbox", func(t *testing.T) {
		p := newProvider(t)
		total, unread, err := p.Count("nobody")
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		if total != 0 || unread != 0 {
			t.Errorf("Count = (%d, %d), want (0, 0)", total, unread)
		}
	})

	// --- Group 10: Delete ---

	t.Run("Delete_RemovesFromAll", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "", "delete me")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if err := p.Delete(sent.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("Inbox after Delete = %d, want 0", len(msgs))
		}
	})

	// --- Group 11: Archive ---

	t.Run("Archive_RemovesFromInbox", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "", "archive me")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if err := p.Archive(sent.ID); err != nil {
			t.Fatalf("Archive: %v", err)
		}
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("Inbox after Archive = %d messages, want 0", len(msgs))
		}
	})

	t.Run("Archive_AlreadyArchivedReturnsError", func(t *testing.T) {
		p := newProvider(t)
		sent, err := p.Send("alice", "bob", "", "double archive")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if err := p.Archive(sent.ID); err != nil {
			t.Fatalf("first Archive: %v", err)
		}
		err = p.Archive(sent.ID)
		if !errors.Is(err, mail.ErrAlreadyArchived) {
			t.Errorf("second Archive = %v, want ErrAlreadyArchived", err)
		}
	})

	t.Run("Archive_UnknownIDReturnsError", func(t *testing.T) {
		p := newProvider(t)
		err := p.Archive("nonexistent")
		if err == nil {
			t.Error("Archive(nonexistent) should return error")
		}
	})

	t.Run("ArchiveMany_AllSucceed", func(t *testing.T) {
		p := newProvider(t)
		var ids []string
		for i := 0; i < 3; i++ {
			m, err := p.Send("alice", "bob", "", "batch")
			if err != nil {
				t.Fatalf("Send %d: %v", i, err)
			}
			ids = append(ids, m.ID)
		}
		results, err := p.ArchiveMany(ids)
		if err != nil {
			t.Fatalf("ArchiveMany: %v", err)
		}
		if len(results) != len(ids) {
			t.Fatalf("results = %d, want %d", len(results), len(ids))
		}
		for i, r := range results {
			if r.ID != ids[i] {
				t.Errorf("results[%d].ID = %q, want %q", i, r.ID, ids[i])
			}
			if r.Err != nil {
				t.Errorf("results[%d].Err = %v, want nil", i, r.Err)
			}
		}
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("Inbox after ArchiveMany = %d, want 0", len(msgs))
		}
	})

	t.Run("ArchiveMany_EmptyReturnsNil", func(t *testing.T) {
		p := newProvider(t)
		results, err := p.ArchiveMany(nil)
		if err != nil {
			t.Fatalf("ArchiveMany(nil): %v", err)
		}
		if len(results) != 0 {
			t.Errorf("results = %d, want 0", len(results))
		}
	})

	t.Run("ArchiveMany_PreservesInputOrder", func(t *testing.T) {
		p := newProvider(t)
		var ids []string
		for i := 0; i < 3; i++ {
			m, err := p.Send("alice", "bob", "", "order")
			if err != nil {
				t.Fatalf("Send %d: %v", i, err)
			}
			ids = append(ids, m.ID)
		}
		reversed := []string{ids[2], ids[0], ids[1]}
		results, err := p.ArchiveMany(reversed)
		if err != nil {
			t.Fatalf("ArchiveMany: %v", err)
		}
		for i, r := range results {
			if r.ID != reversed[i] {
				t.Errorf("results[%d].ID = %q, want %q", i, r.ID, reversed[i])
			}
		}
	})

	t.Run("ArchiveMany_MixedOpenClosed", func(t *testing.T) {
		p := newProvider(t)
		var ids []string
		for i := 0; i < 3; i++ {
			m, err := p.Send("alice", "bob", "", "mixed")
			if err != nil {
				t.Fatalf("Send %d: %v", i, err)
			}
			ids = append(ids, m.ID)
		}
		if err := p.Archive(ids[1]); err != nil {
			t.Fatalf("pre-Archive middle: %v", err)
		}
		results, err := p.ArchiveMany(ids)
		if err != nil {
			t.Fatalf("ArchiveMany: %v", err)
		}
		if len(results) != len(ids) {
			t.Fatalf("results = %d, want %d", len(results), len(ids))
		}
		if results[0].Err != nil {
			t.Errorf("results[0].Err = %v, want nil", results[0].Err)
		}
		if !errors.Is(results[1].Err, mail.ErrAlreadyArchived) {
			t.Errorf("results[1].Err = %v, want ErrAlreadyArchived", results[1].Err)
		}
		if results[2].Err != nil {
			t.Errorf("results[2].Err = %v, want nil", results[2].Err)
		}
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("Inbox after ArchiveMany = %d, want 0", len(msgs))
		}
	})

	t.Run("DeleteMany_AllSucceed", func(t *testing.T) {
		p := newProvider(t)
		var ids []string
		for i := 0; i < 3; i++ {
			m, err := p.Send("alice", "bob", "", "delete batch")
			if err != nil {
				t.Fatalf("Send %d: %v", i, err)
			}
			ids = append(ids, m.ID)
		}
		results, err := p.DeleteMany(ids)
		if err != nil {
			t.Fatalf("DeleteMany: %v", err)
		}
		if len(results) != len(ids) {
			t.Fatalf("results = %d, want %d", len(results), len(ids))
		}
		for i, r := range results {
			if r.ID != ids[i] {
				t.Errorf("results[%d].ID = %q, want %q", i, r.ID, ids[i])
			}
			if r.Err != nil {
				t.Errorf("results[%d].Err = %v, want nil", i, r.Err)
			}
		}
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("Inbox after DeleteMany = %d, want 0", len(msgs))
		}
	})

	t.Run("DeleteMany_MixedOpenClosed", func(t *testing.T) {
		p := newProvider(t)
		var ids []string
		for i := 0; i < 3; i++ {
			m, err := p.Send("alice", "bob", "", "mixed delete")
			if err != nil {
				t.Fatalf("Send %d: %v", i, err)
			}
			ids = append(ids, m.ID)
		}
		if err := p.Delete(ids[1]); err != nil {
			t.Fatalf("pre-Delete middle: %v", err)
		}
		results, err := p.DeleteMany(ids)
		if err != nil {
			t.Fatalf("DeleteMany: %v", err)
		}
		if len(results) != len(ids) {
			t.Fatalf("results = %d, want %d", len(results), len(ids))
		}
		if results[0].Err != nil {
			t.Errorf("results[0].Err = %v, want nil", results[0].Err)
		}
		if !errors.Is(results[1].Err, mail.ErrAlreadyArchived) {
			t.Errorf("results[1].Err = %v, want ErrAlreadyArchived", results[1].Err)
		}
		if results[2].Err != nil {
			t.Errorf("results[2].Err = %v, want nil", results[2].Err)
		}
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("Inbox after DeleteMany = %d, want 0", len(msgs))
		}
	})

	// --- Group 12: Lifecycle ---

	t.Run("Lifecycle_SendInboxReadInboxEmpty", func(t *testing.T) {
		p := newProvider(t)

		// Send.
		sent, err := p.Send("alice", "bob", "", "lifecycle")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}

		// Inbox shows it.
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("Inbox = %d messages, want 1", len(msgs))
		}

		// Read it.
		m, err := p.Read(sent.ID)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if m.Body != "lifecycle" {
			t.Errorf("Body = %q, want %q", m.Body, "lifecycle")
		}

		// Inbox now empty (read messages filtered out).
		msgs, err = p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox after Read: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("Inbox after Read = %d messages, want 0", len(msgs))
		}

		// But message is still accessible via Get.
		m, err = p.Get(sent.ID)
		if err != nil {
			t.Fatalf("Get after Read: %v", err)
		}
		if m.Body != "lifecycle" {
			t.Errorf("Body = %q, want %q", m.Body, "lifecycle")
		}
	})

	t.Run("Lifecycle_SendCheckArchiveInboxEmpty", func(t *testing.T) {
		p := newProvider(t)

		// Send.
		sent, err := p.Send("alice", "bob", "", "check-archive")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}

		// Check (doesn't mark read).
		msgs, err := p.Check("bob")
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("Check = %d messages, want 1", len(msgs))
		}

		// Archive.
		if err := p.Archive(sent.ID); err != nil {
			t.Fatalf("Archive: %v", err)
		}

		// Inbox now empty.
		msgs, err = p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox after Archive: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("Inbox after Archive = %d messages, want 0", len(msgs))
		}
	})

	t.Run("Lifecycle_MarkReadUnreadToggle", func(t *testing.T) {
		p := newProvider(t)

		sent, err := p.Send("alice", "bob", "", "toggle")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}

		// MarkRead → not in inbox.
		if err := p.MarkRead(sent.ID); err != nil {
			t.Fatalf("MarkRead: %v", err)
		}
		msgs, err := p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("Inbox after MarkRead = %d, want 0", len(msgs))
		}

		// MarkUnread → back in inbox.
		if err := p.MarkUnread(sent.ID); err != nil {
			t.Fatalf("MarkUnread: %v", err)
		}
		msgs, err = p.Inbox("bob")
		if err != nil {
			t.Fatalf("Inbox: %v", err)
		}
		if len(msgs) != 1 {
			t.Errorf("Inbox after MarkUnread = %d, want 1", len(msgs))
		}
	})
}
