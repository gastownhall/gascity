//go:build windows

package proctable

import (
	"os"
	"testing"
)

func findProcessRecord(records []ProcessRecord, pid int) (ProcessRecord, bool) {
	for _, record := range records {
		if record.PID == pid {
			return record, true
		}
	}
	return ProcessRecord{}, false
}

func TestSnapshotProcessesReportsThisProcess(t *testing.T) {
	records, err := snapshotProcesses()
	if err != nil {
		t.Fatalf("snapshotProcesses: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("snapshotProcesses returned no records; an empty snapshot is indistinguishable from an idle machine")
	}
	self, ok := findProcessRecord(records, os.Getpid())
	if !ok {
		t.Fatalf("snapshot of %d processes omitted this process %d", len(records), os.Getpid())
	}
	if self.StartTime == "" {
		t.Error("this process has no start identity in the snapshot; teardown could not fence on it")
	}
	if self.Name == "" {
		t.Error("this process has no image name in the snapshot")
	}
	identity, err := ProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatalf("ProcessIdentity for this process: %v", err)
	}
	if self.StartTime != identity {
		t.Errorf("snapshot start identity %q and ProcessIdentity %q disagree; callers compare the two directly", self.StartTime, identity)
	}
}

// TestSnapshotProcessesReportsAChildsParent is the PPID contract: a live parent
// this snapshot actually saw is reported, and it is the right one.
func TestSnapshotProcessesReportsAChildsParent(t *testing.T) {
	sessionID := uniqueSessionID(t, "snapshot-child")
	helper := startHelper(t, sessionID, t.TempDir())

	records, err := snapshotProcesses()
	if err != nil {
		t.Fatalf("snapshotProcesses: %v", err)
	}
	child, ok := findProcessRecord(records, helper.pid)
	if !ok {
		t.Fatalf("snapshot of %d processes omitted helper PID %d", len(records), helper.pid)
	}
	if child.PPID != os.Getpid() {
		t.Errorf("helper PID %d reports PPID %d, want this test process %d", helper.pid, child.PPID, os.Getpid())
	}
	if child.SessionID != sessionID {
		t.Errorf("helper PID %d reports session %q, want %q", helper.pid, child.SessionID, sessionID)
	}
	if child.StartTime != helper.identity {
		t.Errorf("helper PID %d reports start identity %q, want %q", helper.pid, child.StartTime, helper.identity)
	}
}

// TestSnapshotProcessesReportsNoProcessGroup pins the deliberate choice
// documented in descendants_windows.go: Windows has no Unix process group, so
// every row reports none rather than inventing one out of its own PID.
func TestSnapshotProcessesReportsNoProcessGroup(t *testing.T) {
	records, err := snapshotProcesses()
	if err != nil {
		t.Fatalf("snapshotProcesses: %v", err)
	}
	for _, record := range records {
		if record.PGID != 0 {
			t.Fatalf("PID %d reports PGID %d; Windows has no process group to report and a non-zero value invites a group kill", record.PID, record.PGID)
		}
	}
}

// TestSnapshotProcessesFeedsDescendantAlive checks the snapshot is shaped the
// way its one consumer walks it: parent edges that reach a spawned child.
func TestSnapshotProcessesFeedsDescendantAlive(t *testing.T) {
	helper := startHelper(t, uniqueSessionID(t, "snapshot-descendant"), t.TempDir())

	records, err := snapshotProcesses()
	if err != nil {
		t.Fatalf("snapshotProcesses: %v", err)
	}
	child, ok := findProcessRecord(records, helper.pid)
	if !ok {
		t.Fatalf("snapshot omitted helper PID %d", helper.pid)
	}
	if !DescendantAlive(records, []int{os.Getpid()}, []string{child.Name}) {
		t.Errorf("DescendantAlive could not reach helper %q (PID %d) from this process %d", child.Name, helper.pid, os.Getpid())
	}
	if DescendantAlive(records, []int{os.Getpid()}, []string{"definitely-not-a-real-process"}) {
		t.Error("DescendantAlive matched a process name that does not exist")
	}
}

// TestDescendantAliveMatchesWindowsImageNamesAgainstBareNames is the regression
// test for configured process_names never matching on Windows. A snapshot names
// each process by its image file, ".exe" included and in whatever case the file
// has, while configurations name processes the Unix way, so an exact match read
// every live agent as dead.
func TestDescendantAliveMatchesWindowsImageNamesAgainstBareNames(t *testing.T) {
	records := []ProcessRecord{
		{PID: 100, PPID: 1, Name: "pwsh.exe"},
		{PID: 101, PPID: 100, Name: "CLAUDE.EXE"},
		{PID: 102, PPID: 100, Name: "gc.exe"},
	}
	for _, names := range [][]string{{"claude"}, {"gc"}, {"Claude"}, {"claude.exe"}} {
		if !DescendantAlive(records, []int{100}, names) {
			t.Errorf("DescendantAlive(%v) = false, want true: that process is running", names)
		}
	}
	if DescendantAlive(records, []int{100}, []string{"claud"}) {
		t.Error("DescendantAlive matched a name that is only a prefix of a running image")
	}
}

// TestSnapshotFeedsDescendantAliveOnlyVouchedLinks pins that the descendant
// walk inherits the snapshot's creation-time fence. Windows never clears a
// parent PID, so a process whose parent exited still names that PID after it
// is given to someone else. The snapshot reports such a row with no parent, so
// DescendantAlive, walking from the PID's new holder, does not reach the
// earlier holder's orphan and read it as a live agent.
func TestSnapshotFeedsDescendantAliveOnlyVouchedLinks(t *testing.T) {
	rows := []windowsProcess{
		{pid: 200, ppid: 1, image: "powershell.exe", creation: 5_000, creationOK: true, env: map[string]string{}, envOK: true, inspection: inspected},
		{pid: 300, ppid: 200, image: "claude.exe", creation: 4_000, creationOK: true, env: map[string]string{}, envOK: true, inspection: inspected},
		{pid: 301, ppid: 200, image: "codex.exe", creation: 6_000, creationOK: true, env: map[string]string{}, envOK: true, inspection: inspected},
	}
	table, err := processTableFromRows(rows, 200)
	if err != nil {
		t.Fatalf("processTableFromRows: %v", err)
	}
	records, err := snapshotFromTable(table)
	if err != nil {
		t.Fatalf("snapshotFromTable: %v", err)
	}
	if orphan, _ := findProcessRecord(records, 300); orphan.PPID != 0 {
		t.Errorf("the orphan of an earlier PID 200 reports PPID %d, want 0", orphan.PPID)
	}
	if DescendantAlive(records, []int{200}, []string{"claude"}) {
		t.Error("DescendantAlive walked from PID 200 to the orphan of an earlier process that held it")
	}
	if !DescendantAlive(records, []int{200}, []string{"codex"}) {
		t.Error("DescendantAlive missed a child created after its parent")
	}
}
