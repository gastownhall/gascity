package herdr

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// TestProcessTreeAliveFindsDescendantBehindWrapper reproduces the
// live-confirmed caffeinate bug: an always-on agent launched via
// `caffeinate -i <agent>` leaves caffeinate as the pane's only reported
// foreground process, with the agent running underneath it as an
// undiscovered child. Foreground-only matching (the pre-fix behavior) never
// sees the agent name and reports Alive=false forever, driving the mayor's
// continuation-reset loop. processTreeAlive must walk the host process table
// from the pane's shell/foreground PIDs so a wanted name found on a
// descendant still counts as alive.
func TestProcessTreeAliveFindsDescendantBehindWrapper(t *testing.T) {
	restore := stubSnapshotProcesses(t, []proctable.ProcessRecord{
		{PID: 100, PPID: 1, Name: "caffeinate"},
		{PID: 101, PPID: 100, Name: "claude"}, // the actual agent, hidden under caffeinate
	})
	defer restore()

	fg := []proc{{PID: 100, Name: "caffeinate"}}
	if !processTreeAlive(100, fg, []string{"claude"}, "") {
		t.Error("processTreeAlive = false; want true for the agent found beneath the caffeinate wrapper")
	}
}

// TestProcessTreeAliveNoMatchStaysDead is the required negative case: a
// genuinely-absent agent process must still report false, so a real crash is
// not masked by the new fallback.
func TestProcessTreeAliveNoMatchStaysDead(t *testing.T) {
	restore := stubSnapshotProcesses(t, []proctable.ProcessRecord{
		{PID: 100, PPID: 1, Name: "caffeinate"},
	})
	defer restore()

	fg := []proc{{PID: 100, Name: "caffeinate"}}
	if processTreeAlive(100, fg, []string{"claude"}, "") {
		t.Error("processTreeAlive = true; want false when the agent process is genuinely gone")
	}
}

// TestProcessAliveFallsBackToProcessTree exercises ProcessAlive end to end
// against a stubbed process-info + process-table pair: the pane's foreground
// only shows caffeinate, but the wanted name is present two hops down in the
// stubbed host snapshot.
func TestProcessAliveFallsBackToProcessTree(t *testing.T) {
	restore := stubSnapshotProcesses(t, []proctable.ProcessRecord{
		{PID: 100, PPID: 1, Name: "caffeinate"},
		{PID: 101, PPID: 100, Name: "sh"},
		{PID: 102, PPID: 101, Name: "claude"},
	})
	defer restore()

	if !processTreeAlive(100, []proc{{PID: 100, Name: "caffeinate"}}, []string{"claude"}, "") {
		t.Error("processTreeAlive = false; want true via multi-hop descendant match")
	}
}

// TestProcessTreeAliveFindsReparentedBySessionID reproduces the 4/13 miss
// band: the agent is NOT a descendant of the pane's shell/foreground PIDs
// (it was reparented, e.g. after its immediate parent exited), so the
// shell-rooted DescendantAlive walk alone misses it. But its process env
// still carries the session's GC_SESSION_ID (env survives reparenting; only
// ppid changes), so the session-scoped root-widening must find it anyway.
func TestProcessTreeAliveFindsReparentedBySessionID(t *testing.T) {
	restore := stubSnapshotProcesses(t, []proctable.ProcessRecord{
		{PID: 100, PPID: 1, Name: "caffeinate"},
		{PID: 999, PPID: 1, Name: "gc", SessionID: "sess-abc"}, // reparented to init, not under the shell
	})
	defer restore()

	fg := []proc{{PID: 100, Name: "caffeinate"}}
	if processTreeAlive(100, fg, []string{"gc"}, "sess-abc") == false {
		t.Error("processTreeAlive = false; want true for a reparented agent found via GC_SESSION_ID")
	}
	if processTreeAlive(100, fg, []string{"gc"}, "") {
		t.Error("processTreeAlive = true with no sessionID; want false (no shell/fg path finds the reparented process)")
	}
	if processTreeAlive(100, fg, []string{"gc"}, "sess-other") {
		t.Error("processTreeAlive = true; want false for a non-matching sessionID")
	}
}

func stubSnapshotProcesses(t *testing.T, records []proctable.ProcessRecord) (restore func()) {
	t.Helper()
	prev := snapshotProcesses
	snapshotProcesses = func() ([]proctable.ProcessRecord, error) { return records, nil }
	return func() { snapshotProcesses = prev }
}
