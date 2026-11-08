//go:build linux

package tmux

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

// A relay-wrapped pane reports gc-osc7501 as pane_current_command for its
// whole life, so its start waits until a look finds the agent among the
// pane's descendants. On a host whose full process-table snapshot is failing
// (ps killed or overrunning its bound: the 2026-11-03 shadow's 18:33-19:59Z
// window took Claude start p95 from 9.9 s to 14.3 s) the look still finds the
// agent, because it reads only the pane's own subtree from /proc and runs no
// ps at all. Kills a look that depends on the full snapshot: under the
// failing shim it never sees the agent and the wait times out.
func TestWaitForCommandWrappedPaneFindsAgentWhileSnapshotFails(t *testing.T) {
	const depth = 5
	pid := startWrappedChain(t, depth)
	readsSince := shimProcessTableTools(t, true)

	tm := NewTmux()
	tm.exec = &wrappedWaitExecutor{paneCommand: programstatus.CompanionName, deadPID: pid, livePID: pid}
	var logged []string
	tm.snapshotFailures.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	start := time.Now()
	if err := tm.WaitForCommand(context.Background(), "gc-test-relay-degraded", supportedShells, 5*time.Second); err != nil {
		t.Fatalf("WaitForCommand = %v while the full snapshot fails, want the agent found %d levels below the wrapper", err, depth)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("WaitForCommand took %v to find a running agent, want one look", took)
	}
	if reads := readsSince(); len(reads) != 0 {
		t.Errorf("the wrapped look ran ps/pgrep %d times %q, want none: the pane's subtree is read from /proc", len(reads), reads)
	}
	if len(logged) != 0 {
		t.Errorf("logged %q, want no failed-snapshot line: no full snapshot was needed", logged)
	}
	if running, err := tm.runtimeRunning("gc-test-relay-degraded", []string{"sleep"}); !running || err != nil {
		t.Errorf("runtimeRunning = (%v, %v) while the full snapshot fails, want (true, nil)", running, err)
	}
	// The subtree is a verified answer, not an unread table: an agent the
	// subtree does not hold is absent, whatever ps would have said.
	if running, err := tm.runtimeRunning("gc-test-relay-degraded", []string{"no-such-agent"}); running || err != nil {
		t.Errorf("runtimeRunning(no-such-agent) = (%v, %v), want (false, nil) from the read subtree", running, err)
	}
}

// readPaneSubtree reads the pane process and its descendants only, each with
// its parent, comm and argv, as the full snapshot would list them.
func TestReadPaneSubtreeMatchesFullSnapshot(t *testing.T) {
	const depth = 3
	pid := startWrappedChain(t, depth)

	sub, err := readPaneSubtree(pid)
	if err != nil {
		t.Fatalf("readPaneSubtree(%s): %v", pid, err)
	}
	full, err := fetchProcessSnapshot(context.Background())
	if err != nil {
		t.Fatalf("fetchProcessSnapshot: %v", err)
	}
	if got := len(sub.byPID); got != depth+1 {
		t.Fatalf("subtree holds %d processes %v, want the root and %d descendants", got, sub.byPID, depth)
	}
	for p, got := range sub.byPID {
		want, ok := full.byPID[p]
		if !ok {
			t.Fatalf("subtree process %s is not in the full snapshot", p)
		}
		if got.Command != want.Command || got.Args != want.Args || (p != pid && got.PPID != want.PPID) {
			t.Errorf("subtree process %s = %+v, want %+v", p, got, want)
		}
	}
	if _, err := readPaneSubtree("999999999"); err == nil {
		t.Error("readPaneSubtree of a pid that does not exist = nil error, want an error so the probe falls back")
	}
	if _, err := readPaneSubtree("not-a-pid"); err == nil {
		t.Error("readPaneSubtree of a malformed pid = nil error, want an error")
	}
}
