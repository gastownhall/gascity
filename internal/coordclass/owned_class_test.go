package coordclass

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestEveryOwnedShapeClassifiesOffWork pins the agreement between the two
// halves of this package: a bead that ControllerOwned or CoordinationOwned
// says only the controller or a coordination route writes must never classify
// as work. A shape that does is a coordination row the split leaves in the work
// store: migration does not move it, the containment check cannot see it, and
// the `gc bd create` class guard lets it be created there. The startup-health
// episode was exactly that (prod 2026-10: 35 episodes answering to work).
//
// Shapes are listed in their canonical spelling, one per type and label the
// owned predicates name, so adding a type or label there without a class here
// fails this test. One owned label is deliberately absent: a bare order-run:
// label is owned for WRITES (gc bd may not stamp it onto a run record) but
// classifies as work, because agents file real work that inherits it from the
// patrol that found it. TestOrderRunLabelAloneIsWork pins that exception.
func TestEveryOwnedShapeClassifiesOffWork(t *testing.T) {
	shapes := []struct {
		name string
		bead beads.Bead
	}{
		// ControllerOwned (owned.go).
		{"session type", beads.Bead{Type: session.BeadType}},
		{"session label on any type", beads.Bead{Type: "task", Labels: []string{session.LabelSession}}},
		{"session label on an empty type", beads.Bead{Labels: []string{session.LabelSession}}},
		{"startup-health episode", beads.Bead{Type: session.StartupHealthEpisodeType}},
		{"convergence root", beads.Bead{Type: typeConvergence}},
		{"op row", beads.Bead{Type: typeOp}},
		// CoordinationOwned (coordination.go).
		{"mail message", beads.Bead{Type: typeMessage}},
		{"session wait label", beads.Bead{Type: session.WaitBeadType, Labels: []string{session.WaitBeadLabel}}},
		{"typed wait by session label", beads.Bead{Type: session.WaitBeadType, Labels: []string{"session:s-1"}, Metadata: map[string]string{"session_id": "s-1"}}},
		{"legacy typed wait by session label", beads.Bead{Type: session.LegacyWaitBeadType, Labels: []string{"session:s-1"}, Metadata: map[string]string{"session_id": "s-1"}}},
		{"nudge shadow", beads.Bead{Type: "chore", Labels: []string{labelNudge}}},
		{"nudge queue", beads.Bead{Type: "chore", Labels: []string{labelNudgeQueue}}},
		{"order tracking", beads.Bead{Type: "task", Labels: []string{labelOrderTracking}}},
		{"extmsg row", beads.Bead{Type: "task", Labels: []string{labelExtmsgPrefix + "binding"}}},
	}
	for _, s := range shapes {
		t.Run(s.name, func(t *testing.T) {
			if !ControllerOwned(s.bead) && !CoordinationOwned(s.bead) {
				t.Fatalf("the fixture %+v is not an owned shape; the table is stale", s.bead)
			}
			if got := Classify(s.bead); got == ClassWork {
				t.Errorf("Classify(%+v) = work; an owned coordination row must route to its class", s.bead)
			}
		})
	}
}

// TestStartupHealthEpisodeWriterOutputClassifiesAsSessions classifies the bead
// the real writer produces, not a hand-built fixture, so a change to what
// SaveStartupHealthEpisode stamps cannot slip past the class boundary.
func TestStartupHealthEpisodeWriterOutputClassifiesAsSessions(t *testing.T) {
	mem := beads.NewMemStore()
	store := session.NewStore(beads.SessionStore{Store: mem})
	if err := store.SaveStartupHealthEpisode(session.StartupHealthEpisode{SessionName: "worker-1"}); err != nil {
		t.Fatalf("SaveStartupHealthEpisode: %v", err)
	}
	rows, err := mem.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the writer produced %d bead(s), want 1", len(rows))
	}
	if got := Classify(rows[0]); got != ClassSessions {
		t.Errorf("Classify(startup-health episode as written) = %s, want sessions", got)
	}
}

// TestOrderRunLabelAloneIsWork pins the one owned label that stays work: run
// records are matched by order-tracking, and a bead carrying only the run
// label is a patrol-filed bug, handoff or task.
func TestOrderRunLabelAloneIsWork(t *testing.T) {
	b := beads.Bead{Type: "bug", Labels: []string{labelOrderRunPrefix + "technical-health-patrol", "technical-health"}}
	if !CoordinationOwned(b) {
		t.Fatal("the order-run label is no longer write-owned; revisit this exception")
	}
	if got := Classify(b); got != ClassWork {
		t.Errorf("Classify(patrol-filed bug with order-run label) = %s, want work", got)
	}
}
