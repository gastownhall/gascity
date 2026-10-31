//go:build windows

package proctable

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestChildProcessesListsALiveChild pins the live read: a child this test
// starts is reported under this process with its image name and the time it
// started, and is gone from the list once it has exited.
func TestChildProcessesListsALiveChild(t *testing.T) {
	before := time.Now()
	helper := startHelper(t, uniqueSessionID(t, "children"), t.TempDir())
	after := time.Now()

	child, ok := findChild(t, os.Getpid(), helper.pid)
	if !ok {
		t.Fatalf("helper PID %d is not among this process's children", helper.pid)
	}
	if got, want := ProcessNameKey(child.Name), ProcessNameKey(filepath.Base(os.Args[0])); got != want {
		t.Errorf("child name = %q (key %q), want the test binary's key %q", child.Name, got, want)
	}
	// The kernel stamps creation inside the start call, so it falls between
	// the two clock reads around it; a second either side absorbs the coarser
	// of the two clocks.
	if child.Created.Before(before.Add(-time.Second)) || child.Created.After(after.Add(time.Second)) {
		t.Errorf("child created at %v, want between %v and %v", child.Created, before, after)
	}

	helper.stop()
	if !helper.awaitExit(helperStopGrace) {
		t.Fatalf("helper PID %d did not exit", helper.pid)
	}
	if _, ok := findChild(t, os.Getpid(), helper.pid); ok {
		t.Errorf("helper PID %d is still listed as a child after it exited", helper.pid)
	}
}

func findChild(t *testing.T, parent, pid int) (ChildProcess, bool) {
	t.Helper()
	children, err := ChildProcesses(parent)
	if err != nil {
		t.Fatalf("ChildProcesses(%d): %v", parent, err)
	}
	for _, child := range children {
		if child.PID == pid {
			return child, true
		}
	}
	return ChildProcess{}, false
}

// TestChildrenInTableVouchesForEachLink pins which rows count as children. A
// row naming the parent is one only if both creation times are known and it
// was created no earlier than the parent; anything else could be the orphan of
// an earlier process that held the same PID.
func TestChildrenInTableVouchesForEachLink(t *testing.T) {
	table := indexProcessTable([]windowsProcess{
		{pid: 200, ppid: 1, image: "pwsh.exe", creation: 5_000, creationOK: true},
		{pid: 300, ppid: 200, image: "child.exe", creation: 6_000, creationOK: true},
		{pid: 301, ppid: 200, image: "orphan-of-an-earlier-200.exe", creation: 4_000, creationOK: true},
		{pid: 302, ppid: 200, image: "unknown-creation.exe"},
		{pid: 303, ppid: 300, image: "grandchild.exe", creation: 7_000, creationOK: true},
		{pid: 304, ppid: 999, image: "someone-elses.exe", creation: 6_000, creationOK: true},
	})

	got := childrenInTable(table, 200)
	if len(got) != 1 {
		t.Fatalf("children of 200 = %+v, want only PID 300", got)
	}
	if want := (ChildProcess{PID: 300, Name: "child.exe", Created: time.Unix(0, 6_000)}); !got[0].Created.Equal(want.Created) || got[0].PID != want.PID || got[0].Name != want.Name {
		t.Errorf("child = %+v, want %+v", got[0], want)
	}
	if got := childrenInTable(table, 777); len(got) != 0 {
		t.Errorf("children of a PID the table does not hold = %+v, want none", got)
	}
}
