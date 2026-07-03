package runproj

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func TestProjectorPreservesFirstSeenOrder(t *testing.T) {
	p := NewProjector()
	changed := p.Apply([]events.Event{
		beadEvent(1, events.BeadCreated, "b", "open"),
		beadEvent(2, events.BeadCreated, "a", "open"),
		beadEvent(3, events.BeadCreated, "c", "open"),
		// An update to an existing bead must NOT reorder it.
		beadEvent(4, events.BeadUpdated, "b", "in_progress"),
	})
	if !changed {
		t.Fatal("Apply reported no change for bead events")
	}

	got := idsOf(p.Beads())
	want := []string{"b", "a", "c"}
	if !equalIDs(got, want) {
		t.Errorf("order = %v, want %v (first-seen order, stable across updates)", got, want)
	}
	if p.LastSeq() != 4 {
		t.Errorf("lastSeq = %d, want 4", p.LastSeq())
	}
}

func TestProjectorIncrementalApplyAndDelete(t *testing.T) {
	p := NewProjector()
	p.Apply([]events.Event{
		beadEvent(1, events.BeadCreated, "a", "open"),
		beadEvent(2, events.BeadCreated, "b", "open"),
	})

	// Incremental tail: a new bead appends at the end; a delete drops its slot.
	changed := p.Apply([]events.Event{
		beadEvent(3, events.BeadCreated, "c", "open"),
		beadEvent(4, events.BeadDeleted, "a", "open"),
	})
	if !changed {
		t.Fatal("Apply reported no change")
	}

	got := idsOf(p.Beads())
	want := []string{"b", "c"}
	if !equalIDs(got, want) {
		t.Errorf("order after delete = %v, want %v", got, want)
	}
	if p.LastSeq() != 4 {
		t.Errorf("lastSeq = %d, want 4", p.LastSeq())
	}
}

func TestProjectorNoOpTickReportsUnchanged(t *testing.T) {
	p := NewProjector()
	p.Apply([]events.Event{beadEvent(1, events.BeadCreated, "a", "open")})

	// A tick carrying only non-bead events advances the cursor but changes no
	// bead, so the tailer can skip the rebuild.
	changed := p.Apply([]events.Event{{Seq: 7, Type: events.SessionWoke, Subject: "w"}})
	if changed {
		t.Error("non-bead event should not report a change")
	}
	if p.LastSeq() != 7 {
		t.Errorf("lastSeq = %d, want 7 (cursor advances past ignored events)", p.LastSeq())
	}
}

func idsOf(bl []beads.Bead) []string {
	out := make([]string, len(bl))
	for i, b := range bl {
		out[i] = b.ID
	}
	return out
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
