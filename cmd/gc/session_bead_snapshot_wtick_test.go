package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// wtickSnapshotBead builds an open session bead carrying a session_name and an
// optional circuit-state marker.
func wtickSnapshotBead(id, sessName, circuitState string) beads.Bead {
	meta := map[string]string{"session_name": sessName, "template": "worker"}
	if circuitState != "" {
		meta[sessionpkg.SessionCircuitStateMetadataKey] = circuitState
	}
	return beads.Bead{ID: id, Type: sessionpkg.BeadType, Status: "open", Labels: []string{sessionpkg.LabelSession}, Metadata: meta}
}

// TestOpenForReconcileLockstepAndCircuit pins that OpenForReconcile is lockstep
// with OpenInfos (row i's Info equals OpenInfos()[i]) and carries the persisted
// circuit cluster (row i's Circuit equals CircuitStateFromMetadata of the source
// bead). It is the row-feed equivalent of the OpenInfos/Open lockstep pin.
func TestOpenForReconcileLockstepAndCircuit(t *testing.T) {
	beadsIn := []beads.Bead{
		wtickSnapshotBead("s-1", "worker-1", sessionpkg.SessionCircuitStateOpen),
		wtickSnapshotBead("s-2", "worker-2", ""),
		{ID: "s-closed", Type: sessionpkg.BeadType, Status: "closed", Labels: []string{sessionpkg.LabelSession}, Metadata: map[string]string{"session_name": "worker-3"}},
	}
	snap := newSessionBeadSnapshot(beadsIn)

	rows := snap.OpenForReconcile()
	infos := snap.OpenInfos()
	if len(rows) != len(infos) {
		t.Fatalf("OpenForReconcile len=%d, OpenInfos len=%d — must match (closed filtered from both)", len(rows), len(infos))
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 open rows (closed filtered), got %d", len(rows))
	}
	for i := range rows {
		if rows[i].Info.ID != infos[i].ID {
			t.Fatalf("row %d Info.ID=%q not lockstep with OpenInfos %q", i, rows[i].Info.ID, infos[i].ID)
		}
	}
	// The s-1 row must carry the open-circuit cluster; s-2 the zero cluster.
	if rows[0].Circuit.State != sessionpkg.SessionCircuitStateOpen {
		t.Fatalf("row 0 circuit state = %q, want %q", rows[0].Circuit.State, sessionpkg.SessionCircuitStateOpen)
	}
	if rows[1].Circuit.State != "" {
		t.Fatalf("row 1 circuit state = %q, want empty", rows[1].Circuit.State)
	}
}

// TestApplyOpenInfoPatchFoldsMarker pins the stranded-throttle carrier: after
// ApplyOpenInfoPatch, a REUSED snapshot's OpenForReconcile row carries the folded
// marker (the explicit replacement for the old shared-metadata-map aliasing).
func TestApplyOpenInfoPatchFoldsMarker(t *testing.T) {
	snap := newSessionBeadSnapshot([]beads.Bead{
		wtickSnapshotBead("s-1", "worker-1", ""),
		wtickSnapshotBead("s-2", "worker-2", ""),
	})
	if got := snap.OpenForReconcile()[0].Info.StrandedEventEmittedAt; got != "" {
		t.Fatalf("precondition: marker already set: %q", got)
	}
	snap.ApplyOpenInfoPatch("s-1", sessionpkg.MetadataPatch{strandedEventEmittedKey: "2026-03-08T00:00:00Z"})

	rows := snap.OpenForReconcile()
	if rows[0].Info.StrandedEventEmittedAt != "2026-03-08T00:00:00Z" {
		t.Fatalf("s-1 marker not folded onto reused snapshot row: %q", rows[0].Info.StrandedEventEmittedAt)
	}
	if rows[1].Info.StrandedEventEmittedAt != "" {
		t.Fatalf("s-2 marker wrongly set: %q", rows[1].Info.StrandedEventEmittedAt)
	}
	// Absent id is a no-op (no panic, no change).
	snap.ApplyOpenInfoPatch("missing", sessionpkg.MetadataPatch{strandedEventEmittedKey: "x"})
	if snap.OpenForReconcile()[0].Info.StrandedEventEmittedAt != "2026-03-08T00:00:00Z" {
		t.Fatal("absent-id patch mutated an existing row")
	}
}

// TestNewSessionBeadSnapshotFromReconcileRows pins that the row constructor
// round-trips Info AND Circuit onto OpenForReconcile, and that the typed index
// maps (FindInfoByID) work on a rows-built snapshot (raw open half stays nil).
func TestNewSessionBeadSnapshotFromReconcileRows(t *testing.T) {
	rows := []sessionpkg.ReconcileSession{
		{
			Info:    sessiontest.SeedBead(t, wtickSnapshotBead("s-1", "worker-1", "")),
			Circuit: sessionpkg.CircuitState{State: sessionpkg.SessionCircuitStateOpen, ResetGeneration: "4"},
		},
		{
			Info:    sessiontest.SeedBead(t, wtickSnapshotBead("s-2", "worker-2", "")),
			Circuit: sessionpkg.CircuitState{},
		},
	}
	snap := newSessionBeadSnapshotFromReconcileRows(rows)

	out := snap.OpenForReconcile()
	if len(out) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(out))
	}
	if out[0].Info.ID != "s-1" || out[0].Circuit.State != sessionpkg.SessionCircuitStateOpen || out[0].Circuit.ResetGeneration != "4" {
		t.Fatalf("row 0 not round-tripped: %+v", out[0])
	}
	if info, ok := snap.FindInfoByID("s-2"); !ok || info.ID != "s-2" {
		t.Fatalf("FindInfoByID(s-2) failed on a rows-built snapshot: ok=%v info=%+v", ok, info)
	}
}
