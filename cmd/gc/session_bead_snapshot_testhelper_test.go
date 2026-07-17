package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// newSessionBeadSnapshot builds a snapshot from fixture beads, mirroring the store
// edge: it projects each bead to a ReconcileSession row (Info + circuit cluster) via
// the session codec — exactly as Store.ListAllForReconcile does in production — then
// builds the Info-fed snapshot and stores the config-change fingerprint over the open
// set. TEST-ONLY: production never constructs a snapshot from raw beads (the raw-bead
// half was deleted in WI-7 W-delete); it builds from the typed reconcile feed
// (loadSessionBeadSnapshot → ListAllForReconcileWithFingerprint) or from Info
// (newSessionBeadSnapshotFromInfos). Tests synthesize fixture beads as the store would
// deserialize them, so projecting them here is the test edge — the codec call below
// lives in a _test.go file and is not counted by the typed-class census.
func newSessionBeadSnapshot(beadsIn []beads.Bead) *sessionBeadSnapshot {
	open := make([]beads.Bead, 0, len(beadsIn))
	for _, b := range beadsIn {
		if b.Status == "closed" {
			continue
		}
		open = append(open, b)
	}
	snap := newSessionBeadSnapshotFromReconcileRows(session.ReconcileRowsFromBeads(beadsIn))
	snap.fingerprint = session.SetFingerprint(open)
	return snap
}
