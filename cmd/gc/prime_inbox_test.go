package main

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
)

// TestPrimeUnreadMailFromInboxMatchesPerCandidateCheck pins the SessionStart
// ordinary-mail block reusing the session's inbox: it returns the same unread
// mail, in the same order, as checking every identity candidate — including a
// candidate (here GC_AGENT) that is not one of the session's routes and is
// still checked on its own.
func TestPrimeUnreadMailFromInboxMatchesPerCandidateCheck(t *testing.T) {
	clearGCEnv(t)
	store := beads.NewMemStore()
	p := beadmail.New(store)
	sessionBead, err := store.Create(beads.Bead{
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"alias": "sky", "session_name": "runtime-sky", "alias_history": "old-sky"},
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	for _, to := range []string{"sky", sessionBead.ID, "runtime-sky", "old-sky", "rig/sky-agent", "someone-else"} {
		if _, err := p.Send("human", to, "", "for "+to); err != nil {
			t.Fatalf("Send(%q): %v", to, err)
		}
	}
	t.Setenv("GC_SESSION_ID", sessionBead.ID)
	t.Setenv("GC_ALIAS", "sky")
	t.Setenv("GC_AGENT", "rig/sky-agent")

	want := primeUnreadMailMessages(p)
	info, _, err := session.ResolveSessionRecordByExactID(store, sessionBead.ID)
	if err != nil {
		t.Fatalf("ResolveSessionRecordByExactID: %v", err)
	}
	inbox, err := p.InboxForRoutes(session.RecipientRoutesFromInfo(info))
	if err != nil {
		t.Fatalf("InboxForRoutes: %v", err)
	}
	got := primeUnreadMailMessagesFromInbox(p, inbox)
	ids := func(messages []mail.Message) []string {
		out := make([]string, 0, len(messages))
		for _, m := range messages {
			out = append(out, m.ID)
		}
		return out
	}
	if !slices.Equal(ids(got), ids(want)) {
		t.Fatalf("from inbox = %v, per candidate = %v", ids(got), ids(want))
	}
	if len(got) != 5 {
		t.Fatalf("unread = %d messages, want the 5 addressed to the session's routes and GC_AGENT", len(got))
	}
}
