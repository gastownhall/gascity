package beads

import (
	"testing"
	"time"
)

// A large molecule pour must get more transaction budget than a single bd
// command: the per-edge cycle checks made a 67-node plan blow the flat 120s
// deadline mid-transaction and fall back to per-bead creates (2026-07-17).
func TestNativeGraphApplyDeadlineScalesWithPlanSize(t *testing.T) {
	t.Parallel()

	if got := nativeGraphApplyDeadline(nil); got != bdCommandTimeout {
		t.Fatalf("nil plan deadline = %v, want flat %v", got, bdCommandTimeout)
	}
	small := &GraphApplyPlan{Nodes: make([]GraphApplyNode, 1)}
	if got := nativeGraphApplyDeadline(small); got <= bdCommandTimeout {
		t.Fatalf("small plan deadline = %v, want > flat %v", got, bdCommandTimeout)
	}
	big := &GraphApplyPlan{
		Nodes: make([]GraphApplyNode, 67),
		Edges: make([]GraphApplyEdge, 100),
	}
	got := nativeGraphApplyDeadline(big)
	want := bdCommandTimeout + 167*2*time.Second
	if got != want {
		t.Fatalf("67-node/100-edge plan deadline = %v, want %v", got, want)
	}
}
