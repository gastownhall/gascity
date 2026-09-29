package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

// gm-hd3ank: `gc mail check --inject` (the UserPromptSubmit hook) renders the
// unread preview, tells the agent that 'gc mail inbox' shows "all" / "the full
// list", and then archives (mark-read + close, retain-addressable) every
// auto-handoff it just displayed. The agent's next `gc mail inbox` therefore
// cannot list the very messages the hook named, while `gc mail peek <id>` still
// resolves them — which reads as an inbox bug and was filed as one (P1).

var injectedMailLineRE = regexp.MustCompile(`(?m)^- (\S+) from .*$`)

// injectedMailLines maps each message ID rendered in an inject block to its
// rendered preview line.
func injectedMailLines(out string) map[string]string {
	lines := make(map[string]string)
	for _, m := range injectedMailLineRE.FindAllStringSubmatch(out, -1) {
		lines[m[1]] = m[0]
	}
	return lines
}

func createUnreadAutoHandoff(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	// Shape written by `gc handoff --auto "context cycle"` from the PreCompact
	// hook: self-addressed, empty body, both delivery labels, priority:1.
	b, err := store.Create(beads.Bead{
		Title:    "context cycle",
		Type:     "message",
		Assignee: "mayor",
		From:     "mayor",
		Labels:   []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel, "priority:1"},
	})
	if err != nil {
		t.Fatalf("Create auto handoff: %v", err)
	}
	return b
}

// TestMailCheckInjectKeepsInboxPromiseForArchivedAutoHandoffs pins the reported
// symptom: every message the inject block shows must either still be listed by
// the next `gc mail inbox`, or be marked in the block as archived on delivery —
// and the block must not promise that 'gc mail inbox' shows everything when it
// has just archived something it showed.
func TestMailCheckInjectKeepsInboxPromiseForArchivedAutoHandoffs(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	if _, err := mp.Send("human", "mayor", "ordinary", "still open"); err != nil {
		t.Fatalf("Send ordinary: %v", err)
	}
	auto := createUnreadAutoHandoff(t, store)

	var inject, stderr bytes.Buffer
	if code := doMailCheck(mp, "mayor", true, &inject, &stderr); code != 0 {
		t.Fatalf("doMailCheck --inject = %d, want 0; stderr=%s", code, stderr.String())
	}
	var inbox bytes.Buffer
	if code := doMailInbox(mp, "mayor", &inbox, &stderr); code != 0 {
		t.Fatalf("doMailInbox = %d, want 0; stderr=%s", code, stderr.String())
	}

	shown := injectedMailLines(inject.String())
	if _, ok := shown[auto.ID]; !ok {
		t.Fatalf("inject block does not show auto handoff %s:\n%s", auto.ID, inject.String())
	}
	for id, line := range shown {
		if strings.Contains(inbox.String(), id) {
			continue
		}
		if !strings.Contains(strings.ToLower(line), "archived") {
			t.Errorf("inject block shows %s, the next `gc mail inbox` cannot list it (archived on delivery), and its line does not say so: %q", id, line)
		}
		for _, overPromise := range []string{"to see all", "for the full list"} {
			if strings.Contains(inject.String(), overPromise) {
				t.Errorf("inject block says 'gc mail inbox' %s, but shown message %s was archived on delivery and is absent from the inbox", overPromise, id)
			}
		}
	}
	if t.Failed() {
		t.Logf("inject block:\n%s\ngc mail inbox afterwards:\n%s", inject.String(), inbox.String())
	}
}

// TestMailCheckInjectAutoHandoffsDoNotDisplaceOrdinaryMail replays the live
// gm-hd3ank mailbox: four unconsumed PreCompact auto-handoffs (priority:1,
// empty body) around one routed work handoff (priority 0). The priority sort
// before the mailInjectMaxMessages clamp fills every preview slot with
// content-free "context cycle" lines and leaves the real handoff out.
func TestMailCheckInjectAutoHandoffsDoNotDisplaceOrdinaryMail(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	createUnreadAutoHandoff(t, store)
	ordinary, err := mp.Send("gascity/investigator", "mayor", "Handoff: new needs-pack-work bead", "routed to you")
	if err != nil {
		t.Fatalf("Send ordinary: %v", err)
	}
	for i := 0; i < mailInjectMaxMessages; i++ {
		createUnreadAutoHandoff(t, store)
	}

	var inject, stderr bytes.Buffer
	if code := doMailCheck(mp, "mayor", true, &inject, &stderr); code != 0 {
		t.Fatalf("doMailCheck --inject = %d, want 0; stderr=%s", code, stderr.String())
	}
	if _, ok := injectedMailLines(inject.String())[ordinary.ID]; !ok {
		t.Errorf("ordinary unread mail %s was displaced from the %d-slot preview by content-free auto-handoffs:\n%s", ordinary.ID, mailInjectMaxMessages, inject.String())
	}
	b, err := store.Get(ordinary.ID)
	if err != nil {
		t.Fatalf("Get ordinary: %v", err)
	}
	if b.Status != "open" {
		t.Errorf("ordinary mail status = %q after inject, want open (inject must never archive ordinary mail)", b.Status)
	}
}

// TestMailInboxListsAutoHandoffsUntilInjectArchivesThem is the control for the
// two tests above. It rules out the hypotheses in gm-hd3ank's report — an inbox
// page limit, a self-mail filter, an unread/all mismatch in the inbox query —
// by showing the inbox lists every unread auto-handoff until, and only until,
// the injecting check archives it; a plain (non-inject) check changes nothing.
func TestMailInboxListsAutoHandoffsUntilInjectArchivesThem(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	all := []string{createUnreadAutoHandoff(t, store).ID}
	ordinary, err := mp.Send("gascity/investigator", "mayor", "Handoff", "routed to you")
	if err != nil {
		t.Fatalf("Send ordinary: %v", err)
	}
	all = append(all, ordinary.ID)
	for i := 0; i < mailInjectMaxMessages; i++ {
		all = append(all, createUnreadAutoHandoff(t, store).ID)
	}

	inboxIDs := func() string {
		t.Helper()
		var out, stderr bytes.Buffer
		if code := doMailInbox(mp, "mayor", &out, &stderr); code != 0 {
			t.Fatalf("doMailInbox = %d, want 0; stderr=%s", code, stderr.String())
		}
		return out.String()
	}

	before := inboxIDs()
	for _, id := range all {
		if !strings.Contains(before, id) {
			t.Fatalf("inbox before any check is missing unread %s:\n%s", id, before)
		}
	}

	var plain, stderr bytes.Buffer
	if code := doMailCheck(mp, "mayor", false, &plain, &stderr); code != 0 {
		t.Fatalf("doMailCheck (no inject) = %d, want 0; stderr=%s", code, stderr.String())
	}
	afterPlain := inboxIDs()
	for _, id := range all {
		if !strings.Contains(afterPlain, id) {
			t.Fatalf("a non-inject check removed %s from the inbox:\n%s", id, afterPlain)
		}
	}

	var inject bytes.Buffer
	if code := doMailCheck(mp, "mayor", true, &inject, &stderr); code != 0 {
		t.Fatalf("doMailCheck --inject = %d, want 0; stderr=%s", code, stderr.String())
	}
	shown := injectedMailLines(inject.String())
	afterInject := inboxIDs()
	for _, id := range all {
		_, wasShown := shown[id]
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get %s after inject (peek must still resolve it): %v", id, err)
		}
		isAuto := id != ordinary.ID
		switch {
		case wasShown && isAuto:
			if strings.Contains(afterInject, id) {
				t.Errorf("shown auto handoff %s is still in the inbox; expected archive-on-inject", id)
			}
			if b.Status != "closed" {
				t.Errorf("shown auto handoff %s status = %q, want closed", id, b.Status)
			}
		default:
			if !strings.Contains(afterInject, id) {
				t.Errorf("%s (shown=%v, auto=%v) left the inbox, but only shown auto-handoffs are archived", id, wasShown, isAuto)
			}
		}
	}
}
