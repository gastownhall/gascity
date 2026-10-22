package runproj

import (
	"encoding/json"
	"reflect"
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

// TestProjectorCountsDecodeMisses proves the tailer-driven path (Projector.Apply,
// not the free Apply) counts bead.* payload decode misses instead of swallowing
// them — the observable signal the run tailer logs so a live projection starve
// surfaces. Non-bead events are not misses, and the count is cumulative.
func TestProjectorCountsDecodeMisses(t *testing.T) {
	p := NewProjector()
	p.Apply([]events.Event{
		beadEvent(1, events.BeadCreated, "a", "open"),
		{Seq: 2, Type: events.BeadUpdated, Payload: json.RawMessage(`{"status":"open"}`)}, // no id → miss
		{Seq: 3, Type: events.SessionWoke, Subject: "w"},                                  // not a bead event
	})
	if p.DecodeMisses() != 1 {
		t.Fatalf("DecodeMisses = %d, want 1 after first pass", p.DecodeMisses())
	}
	// A second pass with another undecodable bead.* event accumulates the count.
	p.Apply([]events.Event{{Seq: 4, Type: events.BeadClosed, Payload: json.RawMessage(`garbage`)}})
	if p.DecodeMisses() != 2 {
		t.Errorf("DecodeMisses = %d, want 2 (cumulative across passes)", p.DecodeMisses())
	}
}

// TestProjectorFoldsRepeatedIdenticalUpdateIdempotently pins what makes two
// writers announcing the same change safe: a worker's `gc hook --claim`
// records the claim's bead.updated, and the supervisor may record the same
// snapshot again when it reads the row back. The repeat lands on the same
// snapshot and keeps the bead's first-seen slot, and a close that follows in
// log order still wins.
func TestProjectorFoldsRepeatedIdenticalUpdateIdempotently(t *testing.T) {
	claimed := beads.Bead{
		ID: "step", Status: "in_progress", Type: "task", Assignee: "worker-1",
		Metadata: map[string]string{"gc.root_bead_id": "root"},
	}
	payload, err := beads.EncodeBeadEventPayload(claimed)
	if err != nil {
		t.Fatal(err)
	}
	hookClaim := events.Event{Seq: 3, Type: events.BeadUpdated, Actor: "worker-1", Subject: "step", Payload: payload}
	supervisorRepeat := events.Event{Seq: 4, Type: events.BeadUpdated, Actor: "cache-reconcile", Subject: "step", Payload: payload}

	p := NewProjector()
	p.Apply([]events.Event{
		beadEvent(1, events.BeadCreated, "root", "open"),
		beadEvent(2, events.BeadCreated, "step", "open"),
		hookClaim,
	})
	once := p.Beads()

	p.Apply([]events.Event{supervisorRepeat})
	twice := p.Beads()

	if !equalIDs(idsOf(twice), []string{"root", "step"}) {
		t.Fatalf("beads after the repeat = %v, want [root step]", idsOf(twice))
	}
	if !reflect.DeepEqual(once, twice) {
		t.Fatalf("a repeated identical bead.updated changed the fold:\n once:  %+v\n twice: %+v", once, twice)
	}
	if got := twice[1]; got.Status != "in_progress" || got.Assignee != "worker-1" {
		t.Fatalf("step = status %q assignee %q, want in_progress owned by worker-1", got.Status, got.Assignee)
	}

	closed := claimed
	closed.Status = "closed"
	closedPayload, err := beads.EncodeBeadEventPayload(closed)
	if err != nil {
		t.Fatal(err)
	}
	p.Apply([]events.Event{
		{Seq: 5, Type: events.BeadClosed, Actor: "worker-1", Subject: "step", Payload: closedPayload},
		{Seq: 6, Type: events.BeadClosed, Actor: "cache-reconcile", Subject: "step", Payload: closedPayload},
	})
	if got := p.Beads()[1]; got.Status != "closed" {
		t.Fatalf("step after a repeated close = status %q, want closed", got.Status)
	}
	if p.LastSeq() != 6 {
		t.Fatalf("lastSeq = %d, want 6", p.LastSeq())
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
