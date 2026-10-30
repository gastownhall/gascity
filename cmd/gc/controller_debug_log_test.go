package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The controller printed every assigned work bead and every non-zero
// poolDesired/scaleCheck count to stderr on every tick, about 10k lines/hour of
// a production supervisor journal. Those dumps are operator diagnostics and
// must stay quiet unless GC_DEBUG is on.
func TestAssignedWorkDumpIsGatedOnGCDebug(t *testing.T) {
	rows := []beads.Bead{{ID: "hw-1", Status: "in_progress", Assignee: "who"}}

	t.Setenv("GC_DEBUG", "")
	var quiet bytes.Buffer
	logAssignedWorkBeads(&quiet, rows, 3)
	logAssignedWorkBeads(&quiet, nil, 3)
	if quiet.Len() != 0 {
		t.Fatalf("assigned-work dump printed without GC_DEBUG:\n%s", quiet.String())
	}

	t.Setenv("GC_DEBUG", "1")
	var loud bytes.Buffer
	logAssignedWorkBeads(&loud, rows, 3)
	if !strings.Contains(loud.String(), "assignedWorkBeads: 1 beads found") ||
		!strings.Contains(loud.String(), "hw-1 assignee=who") {
		t.Fatalf("GC_DEBUG dump lost its content:\n%s", loud.String())
	}

	var empty bytes.Buffer
	logAssignedWorkBeads(&empty, nil, 2)
	if !strings.Contains(empty.String(), "assignedWorkBeads: 0 beads (rigStores=2)") {
		t.Fatalf("GC_DEBUG empty-set line missing:\n%s", empty.String())
	}
}

func TestPoolCountLogIsGatedOnGCDebugAndSorted(t *testing.T) {
	counts := map[string]int{"rig/worker": 2, "rig/idle": 0, "city/alpha": 1}

	t.Setenv("GC_DEBUG", "")
	var quiet bytes.Buffer
	logPoolCounts(&quiet, "poolDesired", counts)
	if quiet.Len() != 0 {
		t.Fatalf("pool counts printed without GC_DEBUG:\n%s", quiet.String())
	}

	t.Setenv("GC_DEBUG", "true")
	var loud bytes.Buffer
	logPoolCounts(&loud, "poolDesired", counts)
	want := "poolDesired: city/alpha = 1\npoolDesired: rig/worker = 2\n"
	if loud.String() != want {
		t.Fatalf("GC_DEBUG pool counts = %q, want %q (sorted, zero counts omitted)", loud.String(), want)
	}
}
