package herdr

import (
	"fmt"
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

// stubPartialSnapshot stands in a snapshot that saw every process but could
// not inspect those marked SessionIDUnknown.
func stubPartialSnapshot(t *testing.T, records []proctable.ProcessRecord) {
	t.Helper()
	prev := snapshotProcesses
	snapshotProcesses = func() ([]proctable.ProcessRecord, error) {
		return records, fmt.Errorf("%w: 1 process(es) could be opened but not read", proctable.ErrScanIncomplete)
	}
	t.Cleanup(func() { snapshotProcesses = prev })
}

// TestProcessTreeAliveTrustsAPartialSnapshotsMatch pins that an incomplete
// snapshot is still evidence: the agent found under the pane's shell is alive
// whatever else could not be inspected.
func TestProcessTreeAliveTrustsAPartialSnapshotsMatch(t *testing.T) {
	stubPartialSnapshot(t, []proctable.ProcessRecord{
		{PID: 100, PPID: 1, Name: "caffeinate"},
		{PID: 101, PPID: 100, Name: "claude"},
		{PID: 500, PPID: 1, Name: "svchost.exe", SessionIDUnknown: true},
	})

	fg := []proc{{PID: 100, Name: "caffeinate"}}
	if !processTreeAlive(100, fg, []string{"claude"}, "sess-abc") {
		t.Error("processTreeAlive = false; want true for an agent found in the snapshot, complete or not")
	}
}

// TestProcessTreeAliveDoesNotCallAnUnreadCandidateDead covers the reparented
// agent whose environment could not be read: its GC_SESSION_ID is unknown, not
// absent, so the walk cannot rule it out.
func TestProcessTreeAliveDoesNotCallAnUnreadCandidateDead(t *testing.T) {
	stubPartialSnapshot(t, []proctable.ProcessRecord{
		{PID: 100, PPID: 1, Name: "caffeinate"},
		{PID: 999, PPID: 1, Name: "gc", SessionIDUnknown: true},
	})

	fg := []proc{{PID: 100, Name: "caffeinate"}}
	if !processTreeAlive(100, fg, []string{"gc"}, "sess-abc") {
		t.Error("processTreeAlive = false; want true while an unread process that could be the reparented agent is running")
	}
}

// TestProcessTreeAliveRulesOutUnreadProcessesThatCannotMatch pins the other
// half: incompleteness only protects a process that could be the agent. Unread
// processes with no wanted name in their subtree cannot hide it.
func TestProcessTreeAliveRulesOutUnreadProcessesThatCannotMatch(t *testing.T) {
	stubPartialSnapshot(t, []proctable.ProcessRecord{
		{PID: 100, PPID: 1, Name: "caffeinate"},
		{PID: 500, PPID: 1, Name: "svchost.exe", SessionIDUnknown: true},
		{PID: 501, PPID: 500, Name: "conhost.exe"},
	})

	fg := []proc{{PID: 100, Name: "caffeinate"}}
	if processTreeAlive(100, fg, []string{"gc"}, "sess-abc") {
		t.Error("processTreeAlive = true; want false when no unread process could lead to the agent")
	}
}

// TestProcessTreeAliveWithoutASessionIDIgnoresIncompleteness: with no session
// to widen by, nothing that needs inspection is consulted, so the walk from the
// pane is the whole answer.
func TestProcessTreeAliveWithoutASessionIDIgnoresIncompleteness(t *testing.T) {
	stubPartialSnapshot(t, []proctable.ProcessRecord{
		{PID: 100, PPID: 1, Name: "caffeinate"},
		{PID: 999, PPID: 1, Name: "gc", SessionIDUnknown: true},
	})

	fg := []proc{{PID: 100, Name: "caffeinate"}}
	if processTreeAlive(100, fg, []string{"gc"}, "") {
		t.Error("processTreeAlive = true with no sessionID; want false (no shell/fg path finds the process)")
	}
}

// TestProcessTreeAliveReadsAFailedSnapshotAsDead pins the behavior this change
// keeps: a read that produced no snapshot at all reports false, as before.
func TestProcessTreeAliveReadsAFailedSnapshotAsDead(t *testing.T) {
	prev := snapshotProcesses
	snapshotProcesses = func() ([]proctable.ProcessRecord, error) { return nil, proctable.ErrUnsupported }
	t.Cleanup(func() { snapshotProcesses = prev })

	if processTreeAlive(100, []proc{{PID: 100, Name: "claude"}}, []string{"claude"}, "sess-abc") {
		t.Error("processTreeAlive = true for a failed snapshot; want false")
	}
}
