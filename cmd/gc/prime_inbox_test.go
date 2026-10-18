package main

import (
	"bytes"
	"slices"
	"strings"
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
	inbox, err := p.InboxForSession(info)
	if err != nil {
		t.Fatalf("InboxForSession: %v", err)
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

// TestSessionStartWithoutAProvenSessionServesNothingForAClosedSession pins the
// SessionStart mail path for a caller that proved no session: when the hook's
// own session bead is closed, the ordinary-mail block surfaces nothing either,
// rather than falling back to the identity candidates, where GC_ALIAS reaches
// whichever session holds the alias now.
func TestSessionStartWithoutAProvenSessionServesNothingForAClosedSession(t *testing.T) {
	f := newHookBudgetFixture(t)
	f.sendMail(t, "status please")
	store, err := openCityStoreAt(f.cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	if err := store.Close(f.sessionID); err != nil {
		t.Fatalf("Close(session): %v", err)
	}
	resetHookBudgetProcessState(t)

	var stderr bytes.Buffer
	injection := primeHookContextSuffix(f.cityDir, nil, nil, true, primeHookContext{HookEventName: "SessionStart"}, &stderr, false)
	if strings.Contains(injection.text, "status please") {
		t.Fatalf("SessionStart for a closed session surfaced its alias's mail:\n%s\nstderr=%s", injection.text, stderr.String())
	}
}
