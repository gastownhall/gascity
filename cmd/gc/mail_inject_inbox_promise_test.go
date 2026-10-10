package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

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

// injectedBulletLines returns the bullet lines ("- ...") of an inject block in
// render order. Every rendered message occupies exactly one of them, alone or
// as part of the one line that stands for the collapsed auto-handoff group.
func injectedBulletLines(out string) []string {
	var bullets []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "- ") {
			bullets = append(bullets, line)
		}
	}
	return bullets
}

// injectedLinesNaming returns the bullet lines of an inject block that name id
// as a whole token, so "gc-1" does not match a line that only names "gc-10".
func injectedLinesNaming(out, id string) []string {
	named := regexp.MustCompile(`(^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(id) + `($|[^A-Za-z0-9_-])`)
	var lines []string
	for _, line := range injectedBulletLines(out) {
		if named.MatchString(line) {
			lines = append(lines, line)
		}
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
	ordinary, err := mp.Send("human", "mayor", "ordinary", "still open")
	if err != nil {
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

	if len(injectedLinesNaming(inject.String(), auto.ID)) == 0 {
		t.Fatalf("inject block does not show auto handoff %s:\n%s", auto.ID, inject.String())
	}
	for _, id := range []string{ordinary.ID, auto.ID} {
		lines := injectedLinesNaming(inject.String(), id)
		if len(lines) == 0 || strings.Contains(inbox.String(), id) {
			continue
		}
		for _, line := range lines {
			if !strings.Contains(strings.ToLower(line), "archived") {
				t.Errorf("inject block shows %s, the next `gc mail inbox` cannot list it (archived on delivery), and its line does not say so: %q", id, line)
			}
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
	if len(injectedLinesNaming(inject.String(), ordinary.ID)) == 0 {
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
	afterInject := inboxIDs()
	for _, id := range all {
		wasShown := len(injectedLinesNaming(inject.String(), id)) > 0
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

// TestMailCheckInjectRendersAutoHandoffBacklogAsOneLine pins the rendering half
// of ga-8gdkfy Rule C (round-1 review, ga-geys12): the empty-body auto-handoffs
// share ONE preview slot, and that slot is ONE line naming every one of them.
// The slot budget and the archive set cannot tell one line from N — a backlog
// rendered one line per message passes both while re-inflating the injected
// block, which is the crowding Rule C exists to stop.
func TestMailCheckInjectRendersAutoHandoffBacklogAsOneLine(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	ordinary, err := mp.Send("gascity/investigator", "mayor", "Handoff: routed work", "routed to you")
	if err != nil {
		t.Fatalf("Send ordinary: %v", err)
	}
	const backlog = mailInjectMaxMessages + 2
	autoIDs := make([]string, 0, backlog)
	for i := 0; i < backlog; i++ {
		autoIDs = append(autoIDs, createUnreadAutoHandoff(t, store).ID)
	}

	var inject, stderr bytes.Buffer
	if code := doMailCheck(mp, "mayor", true, &inject, &stderr); code != 0 {
		t.Fatalf("doMailCheck --inject = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := inject.String()

	if bullets := injectedBulletLines(out); len(bullets) != 2 {
		t.Fatalf("inject block has %d bullet lines, want 2 (one for all %d auto-handoffs, one for the ordinary mail):\n%s", len(bullets), backlog, out)
	}
	groupLines := injectedLinesNaming(out, autoIDs[0])
	if len(groupLines) != 1 {
		t.Fatalf("auto handoff %s is named on %d lines, want 1:\n%s", autoIDs[0], len(groupLines), out)
	}
	group := groupLines[0]
	for _, id := range autoIDs {
		if got := injectedLinesNaming(out, id); len(got) != 1 || got[0] != group {
			t.Errorf("auto handoff %s is named on %d line(s), want only the shared group line %q:\n%s", id, len(got), group, out)
		}
	}
	if !strings.Contains(strings.ToLower(group), "archived") {
		t.Errorf("group line does not say archived: %q", group)
	}
	if got := injectedLinesNaming(out, ordinary.ID); len(got) != 1 || got[0] == group {
		t.Errorf("ordinary mail %s must keep its own line apart from the group line %q, got %q", ordinary.ID, group, got)
	}

	for _, id := range autoIDs {
		assertAutoHandoffRetainedAddressable(t, store, id)
	}
	b, err := store.Get(ordinary.ID)
	if err != nil {
		t.Fatalf("Get ordinary: %v", err)
	}
	if b.Status != "open" {
		t.Errorf("ordinary mail status = %q after inject, want open", b.Status)
	}
}

// TestFormatInjectOutputCollapsedAutoHandoffGroup pins the shape of the one
// group line (ga-8gdkfy Rule C) against fixed timestamps: newest first, one
// slot of mailInjectMaxMessages however large the group, and a body-bearing
// auto-handoff left out of it because its body is content the group line
// would drop.
func TestFormatInjectOutputCollapsedAutoHandoffGroup(t *testing.T) {
	base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	handoff := func(id string, minutes int, body string) mail.Message {
		return mail.Message{
			ID: id, From: "mayor", To: "mayor", Subject: "context cycle", Body: body,
			CreatedAt: base.Add(time.Duration(minutes) * time.Minute), Priority: 1, ArchivedOnDelivery: true,
		}
	}
	ordinary := func(id string, minutes int) mail.Message {
		return mail.Message{
			ID: id, From: "human", To: "mayor", Subject: "ordinary " + id, Body: "body of " + id,
			CreatedAt: base.Add(time.Duration(minutes) * time.Minute),
		}
	}

	tests := []struct {
		name         string
		messages     []mail.Message
		wantGroup    []string // IDs the group line names, newest first
		wantOwnLines []string // IDs that keep their own line, in render order
		wantContains []string
	}{
		{
			name:         "one empty-body auto-handoff",
			messages:     []mail.Message{handoff("ah-1", 1, "")},
			wantGroup:    []string{"ah-1"},
			wantContains: []string{"You have 1 unread message(s)."},
		},
		{
			name: "a backlog is one line, newest first",
			messages: []mail.Message{
				handoff("ah-a", 2, ""), handoff("ah-b", 5, ""), handoff("ah-c", 1, ""),
				handoff("ah-d", 4, ""), handoff("ah-e", 3, ""),
			},
			wantGroup:    []string{"ah-b", "ah-d", "ah-e", "ah-a", "ah-c"},
			wantContains: []string{"You have 5 unread message(s)."},
		},
		{
			name: "the group costs one slot and ordinary mail fills the rest",
			messages: []mail.Message{
				handoff("ah-a", 2, ""), handoff("ah-b", 5, ""), handoff("ah-c", 1, ""),
				handoff("ah-d", 4, ""), handoff("ah-e", 3, ""),
				ordinary("o-1", 1), ordinary("o-2", 2), ordinary("o-3", 3), ordinary("o-4", 4),
			},
			wantGroup:    []string{"ah-b", "ah-d", "ah-e", "ah-a", "ah-c"},
			wantOwnLines: []string{"o-3", "o-4"},
			wantContains: []string{"You have 9 unread message(s).", "5 of these are archived on delivery", "2 more unread message(s) are not shown"},
		},
		{
			name: "a body-bearing auto-handoff keeps its own line",
			messages: []mail.Message{
				handoff("ah-body", 3, "run the migration"), handoff("ah-1", 1, ""), handoff("ah-2", 2, ""),
			},
			wantGroup:    []string{"ah-2", "ah-1"},
			wantOwnLines: []string{"ah-body"},
			wantContains: []string{"run the migration"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := formatInjectOutput(tt.messages)
			bullets := injectedBulletLines(out)
			if want := 1 + len(tt.wantOwnLines); len(bullets) != want {
				t.Fatalf("inject block has %d bullet lines, want %d (one group line + %d own):\n%s", len(bullets), want, len(tt.wantOwnLines), out)
			}
			group := bullets[0]
			if !strings.Contains(strings.ToLower(group), "archived") {
				t.Errorf("group line does not say archived: %q", group)
			}
			last := -1
			for _, id := range tt.wantGroup {
				at := strings.Index(group, id)
				if at < 0 {
					t.Errorf("group line does not name %s: %q", id, group)
					continue
				}
				if at < last {
					t.Errorf("group line names %s out of newest-first order: %q", id, group)
				}
				last = at
				if got := injectedLinesNaming(out, id); len(got) != 1 || got[0] != group {
					t.Errorf("%s is named on %d line(s), want only the group line:\n%s", id, len(got), out)
				}
			}
			for i, id := range tt.wantOwnLines {
				if got := bullets[1+i]; len(injectedLinesNaming(got, id)) != 1 {
					t.Errorf("bullet %d = %q, want the own line of %s", 1+i, got, id)
				}
				if strings.Contains(group, id) {
					t.Errorf("group line names %s, which must keep its own line: %q", id, group)
				}
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("inject block missing %q:\n%s", want, out)
				}
			}
			for _, overPromise := range []string{"to see all", "for the full list"} {
				if strings.Contains(out, overPromise) {
					t.Errorf("inject block says 'gc mail inbox' %s although it archives what it shows:\n%s", overPromise, out)
				}
			}
		})
	}
}
