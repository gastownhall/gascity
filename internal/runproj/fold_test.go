package runproj

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func beadEvent(seq uint64, typ, id, status string) events.Event {
	payload, _ := json.Marshal(struct {
		Bead beads.Bead `json:"bead"`
	}{beads.Bead{ID: id, Status: status, Type: "task"}})
	return events.Event{Seq: seq, Type: typ, Payload: payload}
}

func TestFoldKeepsLatestSnapshotPerID(t *testing.T) {
	evts := []events.Event{
		beadEvent(1, events.BeadCreated, "a", "open"),
		beadEvent(2, events.BeadCreated, "b", "open"),
		beadEvent(3, events.BeadUpdated, "a", "in_progress"),
		beadEvent(4, events.BeadClosed, "b", "closed"),
		{Seq: 5, Type: events.SessionWoke, Subject: "worker-1"}, // ignored
		beadEvent(6, events.BeadCreated, "c", "open"),
		beadEvent(7, events.BeadDeleted, "c", "open"),
	}

	got := Fold(evts)

	if len(got) != 2 {
		t.Fatalf("fold size = %d, want 2 (a + b; c deleted, session ignored)", len(got))
	}
	if got["a"].Status != "in_progress" {
		t.Errorf("a.status = %q, want in_progress (latest snapshot wins)", got["a"].Status)
	}
	if got["b"].Status != "closed" {
		t.Errorf("b.status = %q, want closed", got["b"].Status)
	}
	if _, ok := got["c"]; ok {
		t.Error("c should be removed by bead.deleted")
	}
}

func TestApplyAdvancesCursorAndMutatesInPlace(t *testing.T) {
	state := Fold([]events.Event{beadEvent(10, events.BeadCreated, "a", "open")})

	last := Apply(state, []events.Event{
		beadEvent(11, events.BeadUpdated, "a", "closed"),
		beadEvent(12, events.BeadCreated, "d", "open"),
	})

	if last != 12 {
		t.Errorf("lastSeq = %d, want 12", last)
	}
	if state["a"].Status != "closed" {
		t.Errorf("a.status = %q, want closed after live-tail apply", state["a"].Status)
	}
	if _, ok := state["d"]; !ok {
		t.Error("d should be added by live-tail apply")
	}
}

func TestApplyCursorTracksMaxSeqEvenForIgnoredEvents(t *testing.T) {
	// A non-bead event still advances the cursor so the tailer does not re-read
	// it; only the fold map is unaffected.
	state := map[string]beads.Bead{}
	last := Apply(state, []events.Event{{Seq: 99, Type: events.SessionStopped, Subject: "w"}})
	if last != 99 {
		t.Errorf("lastSeq = %d, want 99 (cursor advances past ignored events)", last)
	}
	if len(state) != 0 {
		t.Errorf("fold size = %d, want 0", len(state))
	}
}

func TestDecodeBeadAcceptsLegacyRawShape(t *testing.T) {
	// Older logs wrote the raw bead snapshot with no {"bead": ...} envelope.
	raw, _ := json.Marshal(beads.Bead{ID: "legacy", Status: "open", Type: "task"})
	b, ok := decodeBead(raw)
	if !ok || b.ID != "legacy" {
		t.Fatalf("legacy raw-shape decode failed: ok=%v bead=%+v", ok, b)
	}
}
