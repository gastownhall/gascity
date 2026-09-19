package beadmail

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

func digestKey(order string) map[string]string {
	return map[string]string{mail.SupersedeKeyMetadataKey: order}
}

func unreadSubjects(t *testing.T, p *Provider, recipient string) []string {
	t.Helper()
	msgs, err := p.Inbox(recipient)
	if err != nil {
		t.Fatalf("Inbox(%q): %v", recipient, err)
	}
	subjects := make([]string, 0, len(msgs))
	for _, m := range msgs {
		subjects = append(subjects, m.Subject)
	}
	return subjects
}

// A recurring order reports full current state every hour, so only the newest
// copy carries information. The second send must leave one unread message, and
// it must be the new one.
func TestSupersedeKeyLeavesOnlyTheNewestDigestUnread(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	first, err := p.SendWithMetadata("reviewer", "mayor", "review: 3 actionable", "old state", digestKey("slack-alert-review"))
	if err != nil {
		t.Fatalf("first digest: %v", err)
	}
	if _, err := p.SendWithMetadata("reviewer", "mayor", "review: 1 actionable", "new state", digestKey("slack-alert-review")); err != nil {
		t.Fatalf("second digest: %v", err)
	}

	got := unreadSubjects(t, p, "mayor")
	if len(got) != 1 || got[0] != "review: 1 actionable" {
		t.Fatalf("unread subjects = %v, want exactly [review: 1 actionable]", got)
	}

	// Supersede archives, it never deletes: the older digest is closed, and its
	// body is still on the record for gc mail peek and bd show.
	old, err := store.Get(first.ID)
	if err != nil {
		t.Fatalf("store.Get(superseded %s): %v", first.ID, err)
	}
	if old.Status != "closed" {
		t.Errorf("superseded status = %q, want closed", old.Status)
	}
	if old.Description != "old state" {
		t.Errorf("superseded body = %q, want %q", old.Description, "old state")
	}
}

// Supersede is scoped by key, sender, and recipient. A worker's one-off report
// carries no key, so no later message may retire it — that report is the whole
// point of the mailbox.
func TestSupersedeNeverRetiresUnrelatedMessages(t *testing.T) {
	p := New(beads.NewMemStore())

	if _, err := p.Send("sc-wisp-chkxapn", "mayor", "BLOCKED: merge PR 25", "waiting"); err != nil {
		t.Fatalf("worker report: %v", err)
	}
	if _, err := p.SendWithMetadata("reviewer", "mayor", "other order", "state", digestKey("slack-alert-reader")); err != nil {
		t.Fatalf("other-order digest: %v", err)
	}
	if _, err := p.SendWithMetadata("someone-else", "mayor", "same order, other sender", "state", digestKey("slack-alert-review")); err != nil {
		t.Fatalf("other-sender digest: %v", err)
	}
	if _, err := p.SendWithMetadata("reviewer", "human", "same order, other recipient", "state", digestKey("slack-alert-review")); err != nil {
		t.Fatalf("other-recipient digest: %v", err)
	}
	if _, err := p.SendWithMetadata("reviewer", "mayor", "review: first", "state", digestKey("slack-alert-review")); err != nil {
		t.Fatalf("first digest: %v", err)
	}
	if _, err := p.SendWithMetadata("reviewer", "mayor", "review: second", "state", digestKey("slack-alert-review")); err != nil {
		t.Fatalf("second digest: %v", err)
	}

	got := unreadSubjects(t, p, "mayor")
	want := map[string]bool{
		"BLOCKED: merge PR 25":     true,
		"other order":              true,
		"same order, other sender": true,
		"review: second":           true,
	}
	if len(got) != len(want) {
		t.Fatalf("unread subjects = %v, want %d messages: %v", got, len(want), want)
	}
	for _, s := range got {
		if !want[s] {
			t.Errorf("unexpected unread subject %q (retired the wrong message)", s)
		}
	}
	if subs := unreadSubjects(t, p, "human"); len(subs) != 1 {
		t.Errorf("human inbox = %v, want the other-recipient digest untouched", subs)
	}
}

// The sweep archives a report only on positive evidence that the blocker is
// gone. Open, in-progress, unknown, and non-work beads all keep it visible.
func TestArchiveResolvedBlockersOnlyRetiresClosedBlockers(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	mk := func(status, beadType string) string {
		b, err := store.Create(beads.Bead{Title: "blocker", Type: beadType})
		if err != nil {
			t.Fatalf("create blocker: %v", err)
		}
		if status == "closed" {
			if err := store.Close(b.ID); err != nil {
				t.Fatalf("close blocker: %v", err)
			}
		}
		return b.ID
	}

	cases := []struct {
		subject    string
		blockedOn  string
		wantUnread bool
	}{
		{"resolved work bead", mk("closed", "task"), false},
		{"still open", mk("open", "task"), true},
		{"unknown bead", "sc-does-not-exist", true},
		{"closed session bead", mk("closed", session.BeadType), true},
	}
	for _, c := range cases {
		if _, err := p.SendWithMetadata("worker", "mayor", c.subject, "body", map[string]string{mail.BlockedOnMetadataKey: c.blockedOn}); err != nil {
			t.Fatalf("send %q: %v", c.subject, err)
		}
	}
	// A plain report with no annotation must survive every sweep.
	if _, err := p.Send("worker", "mayor", "unannotated", "body"); err != nil {
		t.Fatalf("send unannotated: %v", err)
	}

	archived, err := p.ArchiveResolvedBlockers([]string{"mayor"})
	if err != nil {
		t.Fatalf("ArchiveResolvedBlockers: %v", err)
	}
	if len(archived) != 1 {
		t.Errorf("archived %d messages, want 1", len(archived))
	}

	unread := map[string]bool{}
	for _, s := range unreadSubjects(t, p, "mayor") {
		unread[s] = true
	}
	for _, c := range cases {
		if unread[c.subject] != c.wantUnread {
			t.Errorf("%q unread = %v, want %v", c.subject, unread[c.subject], c.wantUnread)
		}
	}
	if !unread["unannotated"] {
		t.Error("unannotated report was archived; only annotated messages may be swept")
	}
}

// A read message has already left the inbox. Sweeping it would churn its
// status for no gain, and a second sweep must be a no-op.
func TestArchiveResolvedBlockersSkipsReadAndRepeats(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	blocker, err := store.Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	if err := store.Close(blocker.ID); err != nil {
		t.Fatalf("close blocker: %v", err)
	}

	read, err := p.SendWithMetadata("worker", "mayor", "already read", "body", map[string]string{mail.BlockedOnMetadataKey: blocker.ID})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := p.MarkRead(read.ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}

	archived, err := p.ArchiveResolvedBlockers([]string{"mayor"})
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if len(archived) != 0 {
		t.Errorf("first sweep archived %v, want none (message already read)", archived)
	}

	if _, err := p.ArchiveResolvedBlockers([]string{"mayor"}); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if got, err := p.Get(read.ID); err != nil || got.Body != "body" {
		t.Errorf("Get after sweeps = (%+v, %v), want the read message intact", got, err)
	}
}

var (
	_ mail.MetadataSender = (*Provider)(nil)
	_ mail.BlockerSweeper = (*Provider)(nil)
)
