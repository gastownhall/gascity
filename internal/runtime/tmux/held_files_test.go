package tmux

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// fdTableUnreadable is proctable.OpenFiles' error for a process whose open
// files the current user may not list: on Linux, another user's process (a
// root-owned sandbox wrapper such as sudo or bwrap) or a zombie, whose fd
// directory the kernel shows as root's, mode 0500.
func fdTableUnreadable(pid int) error {
	return fmt.Errorf("reading open files of pid %d: %w", pid, &fs.PathError{Op: "open", Path: fmt.Sprintf("/proc/%d/fd", pid), Err: syscall.EACCES})
}

// fdTableFailed is proctable.OpenFiles' error for a process whose open files
// cannot be read for a reason other than the current user's permission.
func fdTableFailed(pid int) error {
	return fmt.Errorf("reading open files of pid %d: %w", pid, &fs.PathError{Op: "open", Path: fmt.Sprintf("/proc/%d/fd", pid), Err: syscall.EIO})
}

// treeHeldFiles names every file the pane's process tree holds open, once
// each, and nothing another tree holds. A process whose open files the
// current user may not read (a root-owned wrapper above the agent, a zombie)
// is skipped and the walk goes on through its children; a process whose
// files cannot be read for any other reason fails the read rather than
// answering partially.
func TestTreeHeldFiles(t *testing.T) {
	const (
		log   = "/home/user/.config/seat/sessions/2026/11/08/rollout-a.jsonl"
		other = "/home/user/.codex/sessions/2026/11/08/rollout-b.jsonl"
	)
	// 4242 is the pane's process, 4300 a launcher, 4301 the agent and 4302
	// a child of the launcher's that exited unreaped; 5000 is another
	// session's agent.
	records := []proctable.ProcessRecord{
		{PID: 4242, PPID: 1}, {PID: 4300, PPID: 4242}, {PID: 4301, PPID: 4300}, {PID: 4302, PPID: 4300}, {PID: 5000, PPID: 1},
	}
	for _, tt := range []struct {
		name    string
		files   map[int][]string
		err     map[int]error
		want    []string
		wantErr error
	}{
		{"held across the tree", map[int][]string{4242: {"/dev/pts/3"}, 4301: {"/w", log, log}, 5000: {other}}, nil, []string{"/dev/pts/3", "/w", log}, nil},
		{"held twice", map[int][]string{4300: {log}, 4301: {log}}, nil, []string{log}, nil},
		{"nothing held", nil, nil, nil, nil},
		{"a root-owned pane process above the agent", map[int][]string{4301: {log}}, map[int]error{4242: fdTableUnreadable(4242)}, []string{log}, nil},
		{"a zombie in the tree", map[int][]string{4301: {log}}, map[int]error{4302: fdTableUnreadable(4302)}, []string{log}, nil},
		{"a descendant unreadable for another reason", map[int][]string{4301: {log}}, map[int]error{4300: fdTableFailed(4300)}, nil, syscall.EIO},
	} {
		t.Run(tt.name, func(t *testing.T) {
			openFiles := func(pid int) ([]string, error) {
				if err := tt.err[pid]; err != nil {
					return nil, err
				}
				return tt.files[pid], nil
			}
			got, err := treeHeldFiles(records, 4242, openFiles)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("treeHeldFiles error = %v, want %v", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("treeHeldFiles = %q, want %q", got, tt.want)
			}
		})
	}
}

// HeldFiles reads the tree under the session's own agent pane and names only
// the files the current user owns: another user's file is never named, so no
// reader is pointed at it.
func TestTmuxHeldFilesNamesOnlyOwnedFilesOfThePaneTree(t *testing.T) {
	const (
		mine   = "/home/user/.config/seat/sessions/2026/11/08/rollout-a.jsonl"
		theirs = "/home/other/.codex/sessions/2026/11/08/rollout-b.jsonl"
	)
	var readPID int
	tm := NewTmux()
	tm.exec = &fakeExecutor{agentPane: true, out: "4242"}
	tm.paneHeldFiles = func(pid int) ([]string, error) {
		readPID = pid
		return []string{mine, theirs}, nil
	}
	tm.heldFileOwned = func(path string) bool { return path == mine }
	got, err := tm.HeldFiles("seat")
	if err != nil {
		t.Fatalf("HeldFiles: %v", err)
	}
	if readPID != 4242 {
		t.Fatalf("HeldFiles read the tree of pid %d, want the agent pane's 4242", readPID)
	}
	if !slices.Equal(got, []string{mine}) {
		t.Fatalf("HeldFiles = %q, want only the owned %q", got, mine)
	}
}

// A tree that cannot be read is an error, never an empty answer.
func TestTmuxHeldFilesReportsAnUnreadableTree(t *testing.T) {
	tm := NewTmux()
	tm.exec = &fakeExecutor{agentPane: true, out: "4242"}
	tm.paneHeldFiles = func(int) ([]string, error) { return nil, fdTableFailed(4300) }
	tm.heldFileOwned = func(string) bool { return true }
	if got, err := tm.HeldFiles("seat"); !errors.Is(err, syscall.EIO) {
		t.Fatalf("HeldFiles = %q, %v; want the tree's read error", got, err)
	}
}

// ownedRegularFile accepts a regular file this user owns and refuses a
// directory, a missing path, and a file another user owns.
func TestOwnedRegularFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "log.jsonl")
	if err := os.WriteFile(file, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ownedRegularFile(file) {
		t.Errorf("ownedRegularFile(%q) = false, want true for this user's file", file)
	}
	if ownedRegularFile(dir) {
		t.Errorf("ownedRegularFile(%q) = true, want false for a directory", dir)
	}
	if ownedRegularFile(filepath.Join(dir, "missing")) {
		t.Error("ownedRegularFile(missing) = true, want false")
	}
	// A regular file root owns is another user's to an unprivileged one.
	const rootsFile = "/etc/passwd"
	if info, err := os.Stat(rootsFile); err == nil && info.Mode().IsRegular() && os.Getuid() != 0 {
		if ownedRegularFile(rootsFile) {
			t.Errorf("ownedRegularFile(%q) = true, want false for root's file", rootsFile)
		}
	}
}
