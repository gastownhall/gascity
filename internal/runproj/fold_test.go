package runproj

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// beadEvent builds a bead.* event with the REAL producer payload shape — the
// raw bead snapshot json.Marshal(b) emits (never the wrapped {"bead":...} form,
// which no producer writes). See CachingStore.notifyChange.
func beadEvent(seq uint64, typ, id, status string) events.Event {
	payload, _ := json.Marshal(beads.Bead{ID: id, Status: status, Type: "task"})
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

	last, _ := Apply(state, []events.Event{
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
	last, _ := Apply(state, []events.Event{{Seq: 99, Type: events.SessionStopped, Subject: "w"}})
	if last != 99 {
		t.Errorf("lastSeq = %d, want 99 (cursor advances past ignored events)", last)
	}
	if len(state) != 0 {
		t.Errorf("fold size = %d, want 0", len(state))
	}
}

func TestDecodeBeadAcceptsCanonicalRawShape(t *testing.T) {
	// The canonical producer shape is the raw bead snapshot (json.Marshal(b),
	// exactly what CachingStore.notifyChange emits) — NOT the wrapped
	// {"bead": ...} form. The fold must decode it, and an envelope carrying no
	// correlation ids leaves the bead's own metadata untouched.
	raw, _ := json.Marshal(beads.Bead{ID: "canon", Status: "open", Type: "task"})
	b, ok := decodeBead(events.Event{Payload: raw})
	if !ok || b.ID != "canon" {
		t.Fatalf("canonical raw-shape decode failed: ok=%v bead=%+v", ok, b)
	}
}

// TestApplyCountsDecodeMisses proves a bead.* event whose payload does not decode
// to a bead with an id is counted as a decode miss (observable), not swallowed
// silently. Non-bead events do not count.
func TestApplyCountsDecodeMisses(t *testing.T) {
	evts := []events.Event{
		beadEvent(1, events.BeadCreated, "a", "open"),
		{Seq: 2, Type: events.BeadUpdated, Payload: json.RawMessage(`{"status":"open"}`)}, // no id → miss
		{Seq: 3, Type: events.BeadClosed, Payload: json.RawMessage(`garbage`)},            // undecodable → miss
		{Seq: 4, Type: events.SessionWoke, Subject: "w"},                                  // not a bead event → not a miss
	}
	state := map[string]beads.Bead{}
	last, stats := Apply(state, evts)
	if last != 4 {
		t.Errorf("lastSeq = %d, want 4", last)
	}
	if stats.DecodeMisses != 2 {
		t.Errorf("DecodeMisses = %d, want 2 (id-less + garbage bead.* events)", stats.DecodeMisses)
	}
	if len(state) != 1 {
		t.Errorf("fold size = %d, want 1 (only 'a' decoded)", len(state))
	}
}

// TestApplyBackfillsEnvelopeCorrelationIDs proves the correlation-spine
// guardrail: when a bead.* envelope carries step_id/session_id but the decoded
// bead's metadata lacks them, Apply backfills them onto the bead metadata so
// run/step grouping survives the spine work removing the metadata duplication.
func TestApplyBackfillsEnvelopeCorrelationIDs(t *testing.T) {
	// Bead payload deliberately omits gc.step_id / gc.session_id from metadata.
	raw, _ := json.Marshal(beads.Bead{ID: "b1", Status: "open", Type: "task"})
	state := map[string]beads.Bead{}
	Apply(state, []events.Event{{
		Seq:       1,
		Type:      events.BeadCreated,
		Payload:   raw,
		StepID:    "gcg-step-9",
		SessionID: "sess-9",
	}})
	got := state["b1"]
	if got.Metadata[beadmeta.StepIDMetadataKey] != "gcg-step-9" {
		t.Errorf("gc.step_id = %q, want gcg-step-9 (backfilled from envelope)", got.Metadata[beadmeta.StepIDMetadataKey])
	}
	if got.Metadata[beadmeta.SessionIDMetadataKey] != "sess-9" {
		t.Errorf("gc.session_id = %q, want sess-9 (backfilled from envelope)", got.Metadata[beadmeta.SessionIDMetadataKey])
	}
}

// TestApplyEnvelopeBackfillDoesNotClobberPayloadMetadata proves the backfill is
// a fill-if-absent: a value already carried in the bead payload metadata wins
// over the envelope copy (the payload is the authoritative snapshot).
func TestApplyEnvelopeBackfillDoesNotClobberPayloadMetadata(t *testing.T) {
	raw, _ := json.Marshal(beads.Bead{
		ID: "b2", Status: "open", Type: "task",
		Metadata: map[string]string{beadmeta.StepIDMetadataKey: "payload-step"},
	})
	state := map[string]beads.Bead{}
	Apply(state, []events.Event{{
		Seq: 1, Type: events.BeadCreated, Payload: raw, StepID: "envelope-step",
	}})
	if got := state["b2"].Metadata[beadmeta.StepIDMetadataKey]; got != "payload-step" {
		t.Errorf("gc.step_id = %q, want payload-step (payload metadata wins over envelope)", got)
	}
}
