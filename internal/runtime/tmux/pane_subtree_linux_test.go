//go:build linux

package tmux

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/fstest"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

// fakeProc is a /proc file system whose reads of the files errs names fail
// with their errors, as reads of a reaped or a refused process do.
type fakeProc struct {
	fstest.MapFS
	errs map[string]error
}

func (p fakeProc) ReadFile(name string) ([]byte, error) {
	if err, ok := p.errs[name]; ok {
		return nil, &fs.PathError{Op: "read", Path: name, Err: err}
	}
	return p.MapFS.ReadFile(name)
}

// addProc adds process pid to proc: its comm, its NUL-separated argv, and
// the children its one thread lists.
func addProc(proc fstest.MapFS, pid, comm, cmdline, children string) {
	proc[pid+"/comm"] = &fstest.MapFile{Data: []byte(comm + "\n")}
	proc[pid+"/cmdline"] = &fstest.MapFile{Data: []byte(cmdline)}
	proc[pid+"/task/"+pid+"/children"] = &fstest.MapFile{Data: []byte(children)}
}

// A descendant that exits during the walk is left out, and the rest of the
// tree is still read, whether its /proc entry is already gone (ENOENT) or it
// was reaped between a file's open and its read (ESRCH, which a comm or
// cmdline read returns on Linux); a root that cannot be read, or a refused
// read, fails the walk so the probe takes the full snapshot. Kills an ESRCH
// that fails the whole walk instead of leaving one process out.
func TestReadProcSubtreeSkipsProcessesThatExitMidWalk(t *testing.T) {
	tree := fstest.MapFS{}
	addProc(tree, "100", "gc-osc7501", "/opt/gc/bin/gc-osc7501\x00relay\x00", "101 ")
	addProc(tree, "101", "bash", "bash\x00-c\x00claude --x\x00", "102 103 104 105")
	addProc(tree, "102", "git", "git\x00status\x00", "")
	addProc(tree, "103", "sleep", "sleep\x001\x00", "")
	// 104 is listed, but its /proc entry is already gone.
	addProc(tree, "105", "claude", "claude\x00--x\x00", "")
	reaped := map[string]error{"102/cmdline": syscall.ESRCH, "103/comm": syscall.ESRCH}

	got, err := readProcSubtree(fakeProc{MapFS: tree, errs: reaped}, "100")
	if err != nil {
		t.Fatalf("readProcSubtree with descendants exiting mid-walk: %v, want them left out", err)
	}
	want := map[string]processRuntimeState{
		"100": {PID: "100", Command: "gc-osc7501", Args: "/opt/gc/bin/gc-osc7501 relay"},
		"101": {PID: "101", PPID: "100", Command: "bash", Args: "bash -c claude --x"},
		"105": {PID: "105", PPID: "101", Command: "claude", Args: "claude --x"},
	}
	if !maps.Equal(got.byPID, want) {
		t.Fatalf("subtree = %v, want %v", got.byPID, want)
	}

	for _, tc := range []struct {
		name string
		errs map[string]error
	}{
		{"the root reaped", map[string]error{"100/comm": syscall.ESRCH}},
		{"a read refused", map[string]error{"105/cmdline": syscall.EACCES}},
	} {
		if _, err := readProcSubtree(fakeProc{MapFS: tree, errs: tc.errs}, "100"); err == nil {
			t.Errorf("%s: readProcSubtree = nil error, want the walk failed so the probe takes the full snapshot", tc.name)
		}
	}
}

// A pane probe whose pane's last subtree walk has not returned (its read
// still blocked in the kernel) starts no second walk: it takes the full
// snapshot at once, and the refused walk is logged, rate-limited, so a host
// whose walks keep failing shows why its probes pay the full snapshot. Once
// the blocked read returns, the next probe walks the subtree again and runs
// no ps. Kills a probe that walks past a held slot, and a fallback that
// leaves no trace.
func TestPaneProbeTakesFullSnapshotWhileItsWalkIsHeld(t *testing.T) {
	readsSince := shimProcessTableTools(t, true)
	const pid = "999999990"
	var walked atomic.Int32
	walks := &paneSubtreeWalks{limit: maxPaneSubtreeWalks, timeout: paneSubtreeTimeout, read: func(root string) (processSnapshot, error) {
		walked.Add(1)
		return newProcessSnapshot([]processRuntimeState{
			{PID: root, Command: programstatus.CompanionName, Args: programstatus.CompanionName + " relay"},
			{PID: "999999991", PPID: root, Command: "sleep", Args: "sleep 60"},
		}), nil
	}}
	tm := NewTmux()
	tm.exec = &wrappedWaitExecutor{paneCommand: programstatus.CompanionName, deadPID: pid, livePID: pid}
	tm.subtreeWalks = walks
	var logged []string
	tm.subtreeFailures.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	tm.snapshotFailures.logf = func(string, ...any) {}

	// The pane's last walk has not returned.
	if err := walks.claim(pid); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if running, err := tm.runtimeRunning("gc-test-held", []string{"sleep"}); running || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("runtimeRunning while the walk is held = (%v, %v), want the failing full snapshot's answer", running, err)
		}
	}
	if n := walked.Load(); n != 0 {
		t.Fatalf("probes started %d walks past a held slot, want none", n)
	}
	if reads := readsSince(); len(reads) != 3 {
		t.Fatalf("probes ran ps/pgrep %d times %q, want one full snapshot each", len(reads), reads)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "pane subtree read failed") || !strings.Contains(logged[0], "its last walk has not returned") {
		t.Fatalf("logged %q, want one line naming the refused walk", logged)
	}

	// The blocked read returns.
	walks.release(pid)
	if running, err := tm.runtimeRunning("gc-test-held", []string{"sleep"}); !running || err != nil {
		t.Fatalf("runtimeRunning once the walk returned = (%v, %v), want the agent found in the subtree", running, err)
	}
	if n := walked.Load(); n != 1 {
		t.Fatalf("walks once the slot freed = %d, want 1", n)
	}
	if reads := readsSince(); len(reads) != 0 {
		t.Fatalf("a probe whose walk answered ran ps/pgrep %q, want none", reads)
	}
}
