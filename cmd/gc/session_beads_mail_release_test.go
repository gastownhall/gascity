package main

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// ra-59207: the session-close WORK-RELEASE sweep (releaseWorkFromClosedSessionBead)
// and the retired-session/orphan release path (unclaimWorkAssignedToRetiredSessionBead)
// enumerate every bead assigned to the closing/retiring session with status in
// (in_progress, open) and hand each one to ReleaseWorkBead, which clears its
// Assignee. That enumeration has no type filter beyond skipping session beads,
// so a type=message mail wisp — still unread, addressed to the closing session's
// own raw ID (the self-handoff case) — is treated as WORK and stripped. A mail
// bead has no claim/routing semantics: clearing its Assignee does not "release"
// anything, it deletes the wisp's only route to an inbox, silently.
//
// These tests are the falsifiable-check floor demanded by the bead: each MUST
// fail on unpatched source (mail Assignee comes back "") and pass once
// excludeMailMessageBeads (work_assignment.go) filters mail beads out of
// OpenAssignedToBasic/OpenAssignedTo before ReleaseWorkBead ever sees them.

// TestReleaseWorkFromClosedSessionBeadLeavesMailBeadUntouched is the close-path
// falsifiable case: an unread self-handoff-shaped mail wisp, still open,
// assigned to the closing session, must survive with its Assignee unchanged.
func TestReleaseWorkFromClosedSessionBeadLeavesMailBeadUntouched(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker-1",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	// Self-handoff mail: addressed to the raw session ID (current.display falls
	// back to GC_SESSION_ID for an unaliased seat), still unread (status open)
	// when the session closes.
	mailBead, err := store.Create(beads.Bead{
		Title:    "HANDOFF: context filling",
		Type:     "message",
		Status:   "open",
		Assignee: sessionBead.ID,
	})
	if err != nil {
		t.Fatalf("create mail bead: %v", err)
	}

	var stderr bytes.Buffer
	releaseWorkFromClosedSessionBead(store, sessionBead, &stderr)

	got, err := store.Get(mailBead.ID)
	if err != nil {
		t.Fatalf("get mail bead: %v", err)
	}
	if got.Assignee != sessionBead.ID {
		t.Fatalf("mail bead Assignee = %q, want unchanged %q (release must never touch a mail wisp's only route to an inbox)", got.Assignee, sessionBead.ID)
	}
	if got.Status != "open" {
		t.Fatalf("mail bead Status = %q, want unchanged %q", got.Status, "open")
	}
}

// TestReleaseWorkFromClosedSessionBeadStillReleasesRealWork is the companion
// assertion: a genuine WORK bead assigned to the same closing session must
// still be released (assignee cleared, in_progress reset to open) — the mail
// exclusion must not disable the real release behavior.
func TestReleaseWorkFromClosedSessionBeadStillReleasesRealWork(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker-1",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	mailBead, err := store.Create(beads.Bead{
		Title:    "HANDOFF: context filling",
		Type:     "message",
		Status:   "open",
		Assignee: sessionBead.ID,
	})
	if err != nil {
		t.Fatalf("create mail bead: %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:    "real work",
		Status:   "in_progress",
		Assignee: sessionBead.ID,
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}

	var stderr bytes.Buffer
	releaseWorkFromClosedSessionBead(store, sessionBead, &stderr)

	gotMail, err := store.Get(mailBead.ID)
	if err != nil {
		t.Fatalf("get mail bead: %v", err)
	}
	if gotMail.Assignee != sessionBead.ID {
		t.Fatalf("mail bead Assignee = %q, want unchanged %q", gotMail.Assignee, sessionBead.ID)
	}

	gotWork, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if gotWork.Assignee != "" {
		t.Fatalf("work bead Assignee = %q, want cleared", gotWork.Assignee)
	}
	if gotWork.Status != "open" {
		t.Fatalf("work bead Status = %q, want open (in_progress must reset on release)", gotWork.Status)
	}
}

// TestUnclaimWorkAssignedToRetiredSessionBeadLeavesMailBeadUntouched covers the
// same bug class at the retired-session/orphan release site (same unfiltered
// OpenAssignedTo query + ReleaseWorkBead pair, per the bead's own list of
// affected sites), so the class is closed rather than one call site.
func TestUnclaimWorkAssignedToRetiredSessionBeadLeavesMailBeadUntouched(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker-1",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	mailBead, err := store.Create(beads.Bead{
		Title:    "HANDOFF: context filling",
		Type:     "message",
		Status:   "open",
		Assignee: sessionBead.ID,
	})
	if err != nil {
		t.Fatalf("create mail bead: %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:    "real work",
		Status:   "in_progress",
		Assignee: sessionBead.ID,
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}

	var stderr bytes.Buffer
	unclaimWorkAssignedToRetiredSessionBead(store, nil, sessionBead, "fallback/worker", &stderr)

	gotMail, err := store.Get(mailBead.ID)
	if err != nil {
		t.Fatalf("get mail bead: %v", err)
	}
	if gotMail.Assignee != sessionBead.ID {
		t.Fatalf("mail bead Assignee = %q, want unchanged %q (orphan-release must never touch a mail wisp)", gotMail.Assignee, sessionBead.ID)
	}

	gotWork, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if gotWork.Assignee != "" {
		t.Fatalf("work bead Assignee = %q, want cleared (mail exclusion must not disable real orphan release)", gotWork.Assignee)
	}
}
