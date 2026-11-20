//go:build linux

package tmux

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// HeldFiles names the agent's rollout from a pane tree shaped as a sandboxed
// seat runs it: the pane's process is a root-owned wrapper above the agent,
// and the launcher has a child that exited unreaped. The kernel lists neither
// fd directory to the current user, so proctable.OpenFiles reads both as
// permission denied: each is skipped, and the walk goes on through the
// wrapper's children to the agent. A fake procfs stands in for /proc, its two
// unlistable fd directories mode 0o000.
func TestTmuxHeldFilesSkipsTheProcessesItMayNotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lists a directory whatever its mode, so no fd directory here is unlistable")
	}
	rollout := filepath.Join(t.TempDir(), "sessions", "2026", "11", "08", "rollout-a.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollout, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	procRoot := t.TempDir()
	fdDir := func(pid int, mode os.FileMode, held ...string) {
		t.Helper()
		dir := filepath.Join(procRoot, strconv.Itoa(pid), "fd")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for fd, path := range held {
			if err := os.Symlink(path, filepath.Join(dir, strconv.Itoa(fd))); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
	}
	// 4242 is the pane's root-owned wrapper, 4300 the launcher, 4301 the
	// agent and 4302 the launcher's zombie child.
	fdDir(4242, 0o000)
	fdDir(4300, 0o755, "/dev/null")
	fdDir(4301, 0o755, "/dev/null", rollout)
	fdDir(4302, 0o000)
	proctable.SetScanRootForTesting(t, procRoot)
	records := []proctable.ProcessRecord{
		{PID: 4242, PPID: 1}, {PID: 4300, PPID: 4242}, {PID: 4301, PPID: 4300}, {PID: 4302, PPID: 4300},
	}

	tm := NewTmux()
	tm.exec = &fakeExecutor{agentPane: true, out: "4242"}
	tm.paneHeldFiles = func(pid int) ([]string, error) {
		return treeHeldFiles(records, pid, proctable.OpenFiles)
	}
	got, err := tm.HeldFiles("seat")
	if err != nil {
		t.Fatalf("HeldFiles: %v", err)
	}
	if !slices.Equal(got, []string{rollout}) {
		t.Fatalf("HeldFiles = %q, want the agent's %q", got, rollout)
	}
}
