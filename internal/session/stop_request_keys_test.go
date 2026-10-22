package session

import "testing"

// TestRestartStopRequestClears (#7635 ruling, v5.9p): a runtime started
// outside PreWake clears whatever stop request or operator's drain mark the
// row holds, and writes nothing for a row that holds none.
func TestRestartStopRequestClears(t *testing.T) {
	meta := map[string]string{DrainAckIncarnationKey: "3", DrainAckAtKey: "x", OperatorDrainKey: "3", "state": "active"}
	got := restartStopRequestClears(meta)
	if len(got) != 3 || got[DrainAckIncarnationKey] != "" || got[OperatorDrainKey] != "" {
		t.Fatalf("clears = %v, want the ack and the operator's mark", got)
	}
	if _, ok := got[OperatorDrainKey]; !ok {
		t.Fatalf("clears = %v, want the operator's mark cleared", got)
	}
	if got := restartStopRequestClears(map[string]string{"state": "active"}); len(got) != 0 {
		t.Fatalf("clears = %v on a bare row, want none", got)
	}
}
