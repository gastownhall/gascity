package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// seedRoutedWorkAssignedTo creates an in_progress work bead routed to
// template with the given assignee, mirroring the shape
// releaseOrphanedPoolAssignments expects (gc.routed_to metadata, reloaded
// after the status transition so it reads back realistically).
func seedRoutedWorkAssignedTo(t *testing.T, store beads.Store, title, template, assignee string) beads.Bead {
	t.Helper()

	work, err := store.Create(beads.Bead{
		Title:    title,
		Type:     "task",
		Status:   "open",
		Assignee: assignee,
		Metadata: map[string]string{"gc.routed_to": template},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work bead: %v", err)
	}
	return work
}

// TestReleaseOrphanedPoolAssignments_TemplateAssigneeSkippedWhenSessionLive
// covers the bug on ga-r22k2y: some routing paths write the bare template
// name itself into Assignee rather than a concrete session identity (never a
// real session), so the reconciler must recognize a live ephemeral session
// for that template as backing the claim instead of treating the assignee as
// a dead named session and reopening live routed work out from under it.
func TestReleaseOrphanedPoolAssignments_TemplateAssigneeSkippedWhenSessionLive(t *testing.T) {
	store := beads.NewMemStore()
	work := seedRoutedWorkAssignedTo(t, store, "routed to template name", "worker", "worker")

	openSessions := []session.Info{
		{ID: "sess-live", Template: "worker", Closed: false},
	}

	released := releaseOrphanedPoolAssignments(
		store,
		beads.SessionStore{Store: store},
		testPoolReleaseConfig(),
		"",
		openSessions,
		[]beads.Bead{work},
		nil,
		nil,
		nil,
		nil,
	)

	if len(released) != 0 {
		t.Fatalf("released %v, want none — a live ephemeral session for template %q backs this bead's "+
			"bare-template assignee, so it must stay claimed, not be reopened for the pool to reclaim",
			released, "worker")
	}
}

// TestReleaseOrphanedPoolAssignments_TemplateAssigneeReleasedWhenNoLiveSession
// is the counterpart: the same bare-template-assignee shape, but with no live
// session anywhere for the template. Nothing is serving this route, so the
// pool must still be able to reclaim it — the assignee-is-template shape must
// not become a permanent shield.
func TestReleaseOrphanedPoolAssignments_TemplateAssigneeReleasedWhenNoLiveSession(t *testing.T) {
	store := beads.NewMemStore()
	work := seedRoutedWorkAssignedTo(t, store, "routed to template name, no session", "worker", "worker")

	released := releaseOrphanedPoolAssignments(
		store,
		beads.SessionStore{Store: store},
		testPoolReleaseConfig(),
		"",
		nil,
		[]beads.Bead{work},
		nil,
		nil,
		nil,
		nil,
	)

	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released %v, want exactly [%s] — no live session serves template %q, so the bare-template "+
			"assignee must not permanently shield the bead from reclamation", released, work.ID, "worker")
	}
}

// TestReleaseOrphanedPoolAssignments_DeadNamedAssigneeReleasedDespiteLiveTemplateSession
// pins the regression a naive fix gets wrong: a genuinely dead NAMED-session
// assignee that happens to share a template with an unrelated LIVE sibling
// session must still be released. Scoping liveness to "some live session
// exists for this template" instead of "the assignee itself IS this
// template" would let any live sibling shield an unrelated dead assignee's
// claim forever — this was the round-1 defect on ga-r22k2y.
func TestReleaseOrphanedPoolAssignments_DeadNamedAssigneeReleasedDespiteLiveTemplateSession(t *testing.T) {
	store := beads.NewMemStore()
	work := seedRoutedWorkAssignedTo(t, store, "dead named assignee, live sibling session", "worker", "sess-dead-999")

	openSessions := []session.Info{
		// Live session for the SAME template, under a different identity
		// than the bead's assignee — must not shield "sess-dead-999".
		{ID: "sess-live", Template: "worker", Closed: false},
	}

	released := releaseOrphanedPoolAssignments(
		store,
		beads.SessionStore{Store: store},
		testPoolReleaseConfig(),
		"",
		openSessions,
		[]beads.Bead{work},
		nil,
		nil,
		nil,
		nil,
	)

	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released %v, want exactly [%s] — assignee %q is a distinct dead session, not the template "+
			"name itself; a live sibling session for template %q must not shield its dead claim from reclamation",
			released, work.ID, "sess-dead-999", "worker")
	}
}
