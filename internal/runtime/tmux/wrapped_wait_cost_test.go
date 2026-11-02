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
	bin := t.TempDir()
	reads := filepath.Join(bin, "reads.log")
	for _, tool := range []string{"ps", "pgrep"} {
		toolPath, err := osexec.LookPath(tool)
		if err != nil {
			t.Skipf("%s is not on PATH: %v", tool, err)
		}
		shim := "#!/bin/sh\necho " + tool + " >>" + shellquote.Join([]string{reads}) + "\nexec " + shellquote.Join([]string{toolPath}) + ` "$@"` + "\n"
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(shim), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The pane root stands in for the relay. The agent ("sleep") runs five
	// levels below it, as claude does under maintainer-city's launch chain;
	// each level is a shell that cannot exec its command away.
	const depth = 5
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
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent never ran %d levels below the pane root", depth)
		}
		time.Sleep(10 * time.Millisecond)
	}

	readsSince := func() []string {
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
	// What one process-table snapshot costs here (Darwin's runs ps twice).
	readsSince()
	if _, err := fetchProcessSnapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := len(readsSince())

	tm := NewTmux()
	tm.exec = &wrappedWaitExecutor{paneCommand: programstatus.CompanionName, deadPID: pid, livePID: pid}
	if err := tm.WaitForCommand(context.Background(), "gc-test-relay-deep", supportedShells, 10*time.Second); err != nil {
		t.Fatalf("WaitForCommand: %v (the agent ran %d levels below the wrapper)", err, depth)
	}
	if got := readsSince(); len(got) != want {
		t.Fatalf("one wrapped look ran ps/pgrep %d times %q; want %d, one process-table snapshot, at any depth", len(got), got, want)
	}
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
