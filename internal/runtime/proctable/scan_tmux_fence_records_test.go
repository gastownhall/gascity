package proctable

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// rootPIDs is the pids of found, in order.
func rootPIDs(found []runtime.LiveRuntime) []int {
	pids := make([]int, 0, len(found))
	for _, r := range found {
		pids = append(pids, r.PID)
	}
	return pids
}

// The Darwin half of TestScanBySessionIDOutsideTmuxFencesTmuxSubtrees, over
// one ps snapshot: nothing under a tmux server or client is a host-sharing
// session's root, the box's program and its adopted leftover are, and an
// ancestry the snapshot cannot complete is left out. The tmux runtime's scan
// (scanRecordsBySessionID) still reports the pane.
func TestFenceTmuxSubtreeRecords(t *testing.T) {
	box := map[string]string{"GC_SESSION_ID": "sid-box"}
	records := map[int]psRecord{
		10: {pid: 10, ppid: 1, command: "holder", env: map[string]string{}},
		11: {pid: 11, ppid: 10, command: "sh", env: box},
		12: {pid: 12, ppid: 11, command: "sleep", env: box},
		13: {pid: 13, ppid: 11, command: "tmux:", env: box},
		20: {pid: 20, ppid: 1, command: "tmux:", env: box},
		21: {pid: 21, ppid: 20, command: "bash", env: box},
		22: {pid: 22, ppid: 21, command: "vim", env: box},
		23: {pid: 23, ppid: 22, command: "agent", env: map[string]string{"GC_SESSION_ID": "sid-other"}},
		30: {pid: 30, ppid: 1, command: "sleep", env: box},
		41: {pid: 41, ppid: 40, command: "agent", env: box},
		50: {pid: 50, ppid: 51, command: "agent", env: box},
		51: {pid: 51, ppid: 50, command: "loop", env: map[string]string{}},
	}
	all := scanRecordsBySessionID(records, "sid-box")
	if got := rootPIDs(all); !slices.Equal(got, []int{11, 21, 30, 41, 50}) {
		t.Fatalf("scanRecordsBySessionID = %v, want the tmux runtime's roots [11 21 30 41 50]", got)
	}
	if got := rootPIDs(fenceTmuxSubtreeRecords(records, all)); !slices.Equal(got, []int{11, 30}) {
		t.Fatalf("fenced roots = %v, want [11 30]", got)
	}
	if got := fenceTmuxSubtreeRecords(records, scanRecordsBySessionID(records, "sid-other")); len(got) != 0 {
		t.Fatalf("fenced sid-other roots = %+v, want none: its root runs in a pane", got)
	}
}
