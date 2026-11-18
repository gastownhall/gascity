//go:build linux || darwin

package tmux

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// TestWaitForCommandWrappedPaneReadsProcessTableOncePerLook pins the cost of
// the look WaitForCommand polls on a wrapped pane. A pane rooted in a wrapper
// (the gc-osc7501 relay, systemd-run) reports the wrapper as
// pane_current_command for its whole life, so each look finds the agent by
// walking the pane's descendants (design §5.3). procps reads the whole process
// table for every ps and pgrep it runs, so a walk that forks them per
// descendant grows with the launch chain: relay -> sudo -> sudo -> bwrap ->
// bwrap -> claude took 17 full reads, 5-7 s on a 4,000-process host, before
// dialog handling and readiness could begin (ga-f4ovjm.19: Claude start p50
// 4 s -> 8 s under the relay). A look reads the process table once, at any
// depth.
func TestWaitForCommandWrappedPaneReadsProcessTableOncePerLook(t *testing.T) {
	readsSince := shimProcessTableTools(t, false)

	// The pane root stands in for the relay. The agent ("sleep") runs five
	// levels below it, as claude does under maintainer-city's launch chain.
	const depth = 5
	pid := startWrappedChain(t, depth)

	// Linux reads only the pane's own subtree from /proc, never the whole
	// table; elsewhere a look takes one full snapshot.
	want := 0
	if !paneSubtreeSupported {
		want = snapshotReads(t, readsSince)
	}
	readsSince()

	tm := NewTmux()
	tm.exec = &wrappedWaitExecutor{paneCommand: programstatus.CompanionName, deadPID: pid, livePID: pid}
	if err := tm.WaitForCommand(context.Background(), "gc-test-relay-deep", supportedShells, 10*time.Second); err != nil {
		t.Fatalf("WaitForCommand: %v (the agent ran %d levels below the wrapper)", err, depth)
	}
	if got := readsSince(); len(got) != want {
		t.Fatalf("one wrapped look ran ps/pgrep %d times %q; want %d, at most one process-table snapshot, at any depth", len(got), got, want)
	}
}

// startWrappedChain starts a process tree standing in for a wrapped pane: a
// root, then depth levels of shells that cannot exec their command away, the
// last running "sleep" (the agent). It returns the root's pid once the agent
// runs depth levels below it, and kills the tree at cleanup.
func startWrappedChain(t *testing.T, depth int) string {
	t.Helper()
	command := "exec sleep 60"
	for i := 0; i < depth; i++ {
		command = "sh -c " + shellquote.Join([]string{command}) + "; :"
	}
	root := osexec.Command("sh", "-c", command+"; :")
	root.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := root.Start(); err != nil {
		t.Fatalf("starting the wrapped chain: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-root.Process.Pid, syscall.SIGKILL)
		_ = root.Wait()
	})
	pid := strconv.Itoa(root.Process.Pid)
	names := proctable.ProcessNameSet([]string{"sleep"})
	deadline := time.Now().Add(10 * time.Second)
	for {
		snapshot, err := fetchProcessSnapshot(context.Background())
		if err == nil && agentLevel(snapshot, pid, names) == depth {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent never ran %d levels below the pane root", depth)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAgentInputTargetDirectMatchReadsNoProcessTable pins the cost of picking
// an unmarked session's agent pane. A pane whose pane_current_command is an
// agent name is picked without reading the process table: the one snapshot is
// taken at the first pane that needs it, so a nudge, input clear or busy probe
// to such a session pays no full-host scan. The second case, whose first pane
// does not match directly, reads it once, which proves the shims see reads.
func TestAgentInputTargetDirectMatchReadsNoProcessTable(t *testing.T) {
	readsSince := shimProcessTableTools(t, false)
	snapshot := snapshotReads(t, readsSince)

	// The pane PIDs name no process, so only a direct match picks a pane.
	for _, tc := range []struct {
		name, panes, want string
		reads             int
	}{
		{"first pane matches", "pane\t@1.%1\tclaude\t999999999\tgc-eol\npane\t@1.%2\tvim\t999999998\tgc-eol", "=w:@1.%1", 0},
		{"second pane matches", "pane\t@1.%1\tvim\t999999999\tgc-eol\npane\t@1.%2\tclaude\t999999998\tgc-eol", "=w:@1.%2", snapshot},
	} {
		tm := NewTmux()
		tm.exec = &fakeExecutor{outs: []string{tc.panes, "GT_PROCESS_NAMES=claude"}}
		got, err := tm.agentInputTarget("w")
		if err != nil || got != tc.want {
			t.Fatalf("%s: agentInputTarget = (%q, %v), want (%q, nil)", tc.name, got, err, tc.want)
		}
		if reads := readsSince(); len(reads) != tc.reads {
			t.Fatalf("%s: agentInputTarget ran ps/pgrep %d times %q, want %d", tc.name, len(reads), reads, tc.reads)
		}
	}
}

// shimProcessTableTools puts ps and pgrep shims first on PATH. Each records
// its run, then runs the real tool, or, when fail is set, exits 1 as a
// process-table read does when it overruns its bound. It returns a function
// reporting the runs recorded since its last call.
func shimProcessTableTools(t *testing.T, fail bool) func() []string {
	t.Helper()
	bin := t.TempDir()
	reads := filepath.Join(bin, "reads.log")
	if err := os.WriteFile(reads, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"ps", "pgrep"} {
		run := "exit 1"
		if !fail {
			toolPath, err := osexec.LookPath(tool)
			if err != nil {
				t.Skipf("%s is not on PATH: %v", tool, err)
			}
			run = "exec " + shellquote.Join([]string{toolPath}) + ` "$@"`
		}
		shim := "#!/bin/sh\necho " + tool + " >>" + shellquote.Join([]string{reads}) + "\n" + run + "\n"
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(shim), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		t.Helper()
		data, err := os.ReadFile(reads)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(reads, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		return strings.Fields(string(data))
	}
}

// snapshotReads is how many shimmed runs one process-table snapshot costs
// here (Darwin's runs ps twice). It leaves readsSince at zero.
func snapshotReads(t *testing.T, readsSince func() []string) int {
	t.Helper()
	readsSince()
	if _, err := fetchProcessSnapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	return len(readsSince())
}

// agentLevel is how many levels below pid the first process named in names
// runs (1: a child), 0 when none does.
func agentLevel(s processSnapshot, pid string, names map[string]struct{}) int {
	for _, child := range s.children[pid] {
		if s.processMatchesNames(child, names) {
			return 1
		}
		if l := agentLevel(s, child, names); l > 0 {
			return l + 1
		}
	}
	return 0
}
