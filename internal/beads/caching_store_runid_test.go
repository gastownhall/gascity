package beads

import (
	"encoding/json"
	"testing"
)

// TestNotifyChangeResolvesRunSession proves notifyChange resolves the opaque
// run/session correlation ids from the bead's metadata AT the record site and
// passes only those two ids to onChange (never the metadata map). This is the
// typed-at-record-site resolution that lets the redacted export carry run_id
// without ever decoding the payload.
func TestNotifyChangeResolvesRunSession(t *testing.T) {
	var gotType, gotID, gotRun, gotSession string
	cs := NewCachingStore(NewMemStore(), func(eventType, beadID, runID, sessionID string, _ json.RawMessage) {
		gotType, gotID, gotRun, gotSession = eventType, beadID, runID, sessionID
	})

	// gc.root_bead_id resolves the run root; gc.session_id is a direct read.
	cs.notifyChange("bead.closed", Bead{ID: "mc-1", Metadata: map[string]string{
		"gc.root_bead_id": "wf-root-x",
		"gc.session_id":   "sess-y",
	}})
	if gotType != "bead.closed" || gotID != "mc-1" {
		t.Fatalf("event meta: type=%q id=%q, want bead.closed/mc-1", gotType, gotID)
	}
	if gotRun != "wf-root-x" {
		t.Fatalf("runID = %q, want wf-root-x (gc.root_bead_id)", gotRun)
	}
	if gotSession != "sess-y" {
		t.Fatalf("sessionID = %q, want sess-y (gc.session_id)", gotSession)
	}

	// workflow_id wins the run-chain precedence over gc.root_bead_id.
	var run2 string
	cs2 := NewCachingStore(NewMemStore(), func(_, _, runID, _ string, _ json.RawMessage) { run2 = runID })
	cs2.notifyChange("bead.created", Bead{ID: "mc-2", Metadata: map[string]string{
		"workflow_id":     "wf-graph-root",
		"gc.root_bead_id": "wf-root-x",
	}})
	if run2 != "wf-graph-root" {
		t.Fatalf("runID = %q, want wf-graph-root (workflow_id precedence)", run2)
	}

	// No run-chain metadata: run falls back to the bead's own id; session empty.
	var run3, sess3 string
	cs3 := NewCachingStore(NewMemStore(), func(_, _, runID, sessionID string, _ json.RawMessage) { run3, sess3 = runID, sessionID })
	cs3.notifyChange("bead.created", Bead{ID: "mc-3"})
	if run3 != "mc-3" {
		t.Fatalf("runID fallback = %q, want mc-3 (bead's own id)", run3)
	}
	if sess3 != "" {
		t.Fatalf("sessionID = %q, want empty (no gc.session_id)", sess3)
	}
}
