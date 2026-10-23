package runproj

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
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

// TestProjectorIDOnlyDeleteDropsRunFromCensus pins what a cache's delete
// tombstone relies on (#5934): a bead.deleted whose payload is the bare bead
// id — all a cache can announce for a row it holds no snapshot of — removes
// the run root, so the census stops counting it as pending. The fold only
// ever removes a row on bead.deleted, and ColdLoad replays the retained log on
// every restart, so without the event the root reads pending forever.
func TestProjectorIDOnlyDeleteDropsRunFromCensus(t *testing.T) {
	root := beads.Bead{
		ID: "gc-wisp-1", Title: "patrol", Type: "molecule", Status: "open",
		Metadata: beads.StringMap{
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.KindMetadataKey:            beadmeta.KindRun,
			beadmeta.FormulaMetadataKey:         "mol-patrol",
		},
	}
	created, err := beads.EncodeBeadEventPayload(root)
	if err != nil {
		t.Fatal(err)
	}
	tombstone, err := beads.EncodeBeadEventPayload(beads.Bead{ID: root.ID})
	if err != nil {
		t.Fatal(err)
	}

	p := NewProjector()
	p.Apply([]events.Event{{Seq: 1, Type: events.BeadCreated, Subject: root.ID, Payload: created}})
	if got := projectedRunCensus(p); got.Pending != 1 {
		t.Fatalf("precondition: census = %+v, want the run pending", got)
	}

	if !p.Apply([]events.Event{{Seq: 2, Type: events.BeadDeleted, Subject: root.ID, Payload: tombstone}}) {
		t.Fatal("Apply reported no change for the id-only bead.deleted")
	}

	if got := projectedRunCensus(p); got != (CanonicalRunStatusCounts{}) {
		t.Fatalf("census after the id-only bead.deleted = %+v, want no runs", got)
	}
	if got := idsOf(p.Beads()); len(got) != 0 {
		t.Fatalf("folded beads = %v, want none", got)
	}
	if p.DecodeMisses() != 0 {
		t.Fatalf("DecodeMisses = %d, want 0: an id-only payload decodes", p.DecodeMisses())
	}
}

// projectedRunCensus classifies the folded runs as the run tailer and the
// runs API do.
func projectedRunCensus(p *Projector) CanonicalRunStatusCounts {
	beadList := FilterRunBeads(p.Beads())
	_, lanes := BuildRunSummaryWithAllLanes(beadList)
	return CountCanonicalRunStatuses(beadList, lanes)
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
