package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func listTestSessionBead(id, status string, meta map[string]string) beads.Bead {
	return beads.Bead{
		ID:       id,
		Title:    "worker",
		Type:     BeadType,
		Status:   status,
		Labels:   []string{LabelSession, "agent:rig/worker"},
		Metadata: meta,
	}
}

func TestListTransitionsCreateAnnouncesOnce(t *testing.T) {
	lt := NewListTransitions(16)
	b := listTestSessionBead("gc-1", "open", map[string]string{"template": "rig/worker", "alias": "w1", "agent_name": "rig/worker-1"})

	change, info := lt.Observe(BeadChangeCreated, b)
	if change != ListChangeCreated {
		t.Fatalf("first create: change = %v, want ListChangeCreated", change)
	}
	if info.ID != "gc-1" || info.Template != "rig/worker" || info.Alias != "w1" || info.AgentName != "rig/worker-1" {
		t.Fatalf("first create: info = %+v, want the bead's id, template, alias and agent name", info)
	}
	// The same create announced again (a refresh that installed the row, then
	// the create's own notification) is not a second transition.
	if change, _ := lt.Observe(BeadChangeCreated, b); change != ListChangeNone {
		t.Fatalf("repeated create: change = %v, want ListChangeNone", change)
	}
	// A metadata update of an open session is not a list change.
	if change, _ := lt.Observe(BeadChangeUpdated, b); change != ListChangeNone {
		t.Fatalf("update of an open session: change = %v, want ListChangeNone", change)
	}
}

func TestListTransitionsCloseAnnouncesOnce(t *testing.T) {
	lt := NewListTransitions(16)
	lt.Observe(BeadChangeCreated, listTestSessionBead("gc-1", "open", nil))
	closed := listTestSessionBead("gc-1", "closed", map[string]string{"template": "rig/worker", "close_reason": "drained"})

	change, info := lt.Observe(BeadChangeClosed, closed)
	if change != ListChangeClosed {
		t.Fatalf("first close: change = %v, want ListChangeClosed", change)
	}
	if info.ID != "gc-1" || info.Template != "rig/worker" {
		t.Fatalf("first close: info = %+v, want the bead's id and template", info)
	}
	if got := CloseReasonOf(closed); got != "drained" {
		t.Fatalf("CloseReasonOf = %q, want drained", got)
	}
	// A second close (the writer's own event and the cache's inferred one, or
	// a double close) is not a second transition.
	if change, _ := lt.Observe(BeadChangeClosed, closed); change != ListChangeNone {
		t.Fatalf("double close: change = %v, want ListChangeNone", change)
	}
	// Metadata settled on the closed row later (the usage lane's clears) is
	// not a list change either.
	if change, _ := lt.Observe(BeadChangeUpdated, closed); change != ListChangeNone {
		t.Fatalf("update of a closed session: change = %v, want ListChangeNone", change)
	}
}

// A close of a session this observer never saw open (created before it
// started) is still a transition: the event says the row just closed.
func TestListTransitionsCloseOfUnseenSessionAnnounces(t *testing.T) {
	lt := NewListTransitions(16)
	if change, _ := lt.Observe(BeadChangeClosed, listTestSessionBead("gc-9", "closed", nil)); change != ListChangeClosed {
		t.Fatalf("close of an unseen session: change = %v, want ListChangeClosed", change)
	}
}

// An update of a closed row the observer never saw open says nothing about a
// transition: it must not announce a close for a session long gone, nor a
// create for a session that was open all along.
func TestListTransitionsUpdatesOfUnseenSessionsAreSilent(t *testing.T) {
	lt := NewListTransitions(16)
	if change, _ := lt.Observe(BeadChangeUpdated, listTestSessionBead("gc-1", "closed", nil)); change != ListChangeNone {
		t.Fatalf("update of an unseen closed session: change = %v, want ListChangeNone", change)
	}
	if change, _ := lt.Observe(BeadChangeUpdated, listTestSessionBead("gc-2", "open", nil)); change != ListChangeNone {
		t.Fatalf("update of an unseen open session: change = %v, want ListChangeNone", change)
	}
}

// A reopened session (a configured named session's closed row reopened) joins
// the open set again, so it is announced as created; its next close is
// announced too.
func TestListTransitionsReopenAnnouncesCreatedAgain(t *testing.T) {
	lt := NewListTransitions(16)
	lt.Observe(BeadChangeCreated, listTestSessionBead("gc-1", "open", nil))
	lt.Observe(BeadChangeClosed, listTestSessionBead("gc-1", "closed", nil))

	if change, _ := lt.Observe(BeadChangeUpdated, listTestSessionBead("gc-1", "open", nil)); change != ListChangeCreated {
		t.Fatalf("reopen: change = %v, want ListChangeCreated", change)
	}
	if change, _ := lt.Observe(BeadChangeClosed, listTestSessionBead("gc-1", "closed", nil)); change != ListChangeClosed {
		t.Fatalf("close after reopen: change = %v, want ListChangeClosed", change)
	}
}

// A bead.updated that carries the close (status closed) is the close when the
// observer knew the row open; the bead.closed that follows is not a second one.
func TestListTransitionsCloseByUpdateOfKnownOpenSession(t *testing.T) {
	lt := NewListTransitions(16)
	lt.Observe(BeadChangeCreated, listTestSessionBead("gc-1", "open", nil))
	if change, _ := lt.Observe(BeadChangeUpdated, listTestSessionBead("gc-1", "closed", nil)); change != ListChangeClosed {
		t.Fatalf("closing update: change = %v, want ListChangeClosed", change)
	}
	if change, _ := lt.Observe(BeadChangeClosed, listTestSessionBead("gc-1", "closed", nil)); change != ListChangeNone {
		t.Fatalf("close after closing update: change = %v, want ListChangeNone", change)
	}
}

// Non-session beads, a failed create (no row, so no notification carries a
// session bead) and an undecodable notification change nothing.
func TestListTransitionsIgnoreNonSessionBeads(t *testing.T) {
	lt := NewListTransitions(16)
	for _, b := range []beads.Bead{
		{ID: "gc-2", Type: "task", Status: "open"},
		{ID: "gc-3", Type: "message", Status: "closed"},
		{ID: "", Type: BeadType, Status: "open"},
	} {
		for _, c := range []BeadChange{BeadChangeCreated, BeadChangeUpdated, BeadChangeClosed} {
			if change, _ := lt.Observe(c, b); change != ListChangeNone {
				t.Fatalf("Observe(%v, %+v) = %v, want ListChangeNone", c, b, change)
			}
		}
	}
}

// A session row created already closed never joined the open set.
func TestListTransitionsCreatedClosedIsSilent(t *testing.T) {
	lt := NewListTransitions(16)
	b := listTestSessionBead("gc-1", "closed", nil)
	if change, _ := lt.Observe(BeadChangeCreated, b); change != ListChangeNone {
		t.Fatalf("create of a closed row: change = %v, want ListChangeNone", change)
	}
	if change, _ := lt.Observe(BeadChangeClosed, b); change != ListChangeNone {
		t.Fatalf("close of a row created closed: change = %v, want ListChangeNone", change)
	}
}

// A delete forgets the row: an id the store reuses afterwards is a new session.
func TestListTransitionsDeleteForgetsTheRow(t *testing.T) {
	lt := NewListTransitions(16)
	lt.Observe(BeadChangeCreated, listTestSessionBead("gc-1", "open", nil))
	lt.Observe(BeadChangeDeleted, beads.Bead{ID: "gc-1"})
	if change, _ := lt.Observe(BeadChangeCreated, listTestSessionBead("gc-1", "open", nil)); change != ListChangeCreated {
		t.Fatalf("create after delete: change = %v, want ListChangeCreated", change)
	}
}

// The remembered set is bounded: overflow forgets everything, which costs at
// most one repeat per id, never a missed transition.
func TestListTransitionsBoundedMemory(t *testing.T) {
	lt := NewListTransitions(2)
	lt.Observe(BeadChangeCreated, listTestSessionBead("gc-1", "open", nil))
	lt.Observe(BeadChangeCreated, listTestSessionBead("gc-2", "open", nil))
	if change, _ := lt.Observe(BeadChangeCreated, listTestSessionBead("gc-3", "open", nil)); change != ListChangeCreated {
		t.Fatalf("create past capacity: change = %v, want ListChangeCreated", change)
	}
	if got := lt.size(); got > 2 {
		t.Fatalf("remembered %d ids, want at most 2", got)
	}
	if change, _ := lt.Observe(BeadChangeClosed, listTestSessionBead("gc-1", "closed", nil)); change != ListChangeClosed {
		t.Fatalf("close of a forgotten id: change = %v, want ListChangeClosed", change)
	}
}
