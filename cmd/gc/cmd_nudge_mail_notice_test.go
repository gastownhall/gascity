package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// sessionSenderStore seeds a store with one session bead, so the notice can
// probe the sender address the way the live path does.
func sessionSenderStore(t *testing.T, id, title string) beads.Store {
	t.Helper()
	return beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     id,
		Title:  title,
		Type:   "session",
		Status: "open",
	}}, nil)
}

// A sender that is a session bead id must not reach the notice: readers ran
// "gc mail read sc-wisp-p2yobus" against it and got nothing. The only id the
// notice names is the message id.
func TestMailNudgeNoticeNeverNamesSessionBeadID(t *testing.T) {
	store := sessionSenderStore(t, "sc-wisp-p2yobus", "omp-1")

	got := mailNudgeNotice(store, "sc-wisp-p2yobus", "gc-42")

	if strings.Contains(got, "sc-wisp-p2yobus") {
		t.Fatalf("notice names the session bead id: %q", got)
	}
	if !strings.Contains(got, "omp-1") {
		t.Fatalf("notice drops the sender name: %q", got)
	}
	if !strings.Contains(got, "gc mail read gc-42") {
		t.Fatalf("notice does not point at the message: %q", got)
	}
}

// A session with neither an alias nor a title has no safe name, so the notice
// names nobody rather than falling back to the bead id.
func TestMailNudgeNoticeDropsUntitledSessionSender(t *testing.T) {
	store := sessionSenderStore(t, "sc-wisp-zgm0eu9", "")

	got := mailNudgeNotice(store, "sc-wisp-zgm0eu9", "gc-7")

	if strings.Contains(got, "sc-wisp-zgm0eu9") {
		t.Fatalf("notice names the session bead id: %q", got)
	}
	if !strings.Contains(got, "another session") {
		t.Fatalf("notice = %q, want an anonymous sender", got)
	}
}

// An alias is the same shape as a bead id ("omp-1" against "sc-gkq1b"), so the
// notice must decide by a store probe, not by shape. An alias that matches no
// bead passes through unchanged.
func TestMailNudgeNoticeKeepsAliasSender(t *testing.T) {
	store := sessionSenderStore(t, "sc-wisp-p2yobus", "omp-1")

	got := mailNudgeNotice(store, "mayor", "gc-9")

	if !strings.Contains(got, "You have mail from mayor") {
		t.Fatalf("notice = %q, want the alias named", got)
	}
}

// Every id the notice names must address a bead of type message.
func TestMailNudgeNoticeNamesOnlyMessageBeads(t *testing.T) {
	store := beads.NewMemStoreFrom(2, []beads.Bead{
		{ID: "sc-wisp-8fog4ni", Title: "omp-4", Type: "session", Status: "open"},
		{ID: "gc-13", Title: "status ping", Type: "message", Status: "open"},
	}, nil)

	got := mailNudgeNotice(store, "sc-wisp-8fog4ni", "gc-13")

	for _, field := range strings.Fields(got) {
		b, err := store.Get(strings.Trim(field, "()"))
		if err != nil {
			continue
		}
		if b.Type != "message" {
			t.Fatalf("notice %q names %s, which is type %q", got, b.ID, b.Type)
		}
	}
}

// Several pending messages give a reader no reason to open any one of them
// first, so the drain reports the count and points at the inbox.
func TestQueuedMailNudgesCollapseToInbox(t *testing.T) {
	items := []queuedNudge{
		{ID: "n1", Source: "mail", Message: "You have mail from omp-1 (message gc-1) — run: gc mail read gc-1"},
		{ID: "n2", Source: "wait", Message: "wait ready"},
		{ID: "n3", Source: "mail", Message: "You have mail from mayor (message gc-2) — run: gc mail read gc-2"},
		{ID: "n4", Source: "mail", Message: "You have mail from omp-4 (message gc-3) — run: gc mail read gc-3"},
	}

	out := formatNudgeInjectOutput(items)

	if !strings.Contains(out, "You have 3 unread messages — run: gc mail inbox") {
		t.Fatalf("output does not summarize the backlog:\n%s", out)
	}
	for _, id := range []string{"gc-1", "gc-2", "gc-3"} {
		if strings.Contains(out, id) {
			t.Fatalf("output singles out %s:\n%s", id, out)
		}
	}
	if !strings.Contains(out, "wait ready") {
		t.Fatalf("collapse dropped a non-mail reminder:\n%s", out)
	}
}

// One pending message still names its own id: that is the useful case, and the
// id addresses a message bead.
func TestSingleMailNudgeKeepsItsMessageID(t *testing.T) {
	items := []queuedNudge{
		{ID: "n1", Source: "mail", Message: "You have mail from omp-1 (message gc-1) — run: gc mail read gc-1"},
	}

	out := formatNudgeInjectOutput(items)

	if !strings.Contains(out, "gc mail read gc-1") {
		t.Fatalf("single notice lost its message id:\n%s", out)
	}
}
