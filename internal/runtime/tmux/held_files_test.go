package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// treeHeldFiles names every file the pane's process tree holds open, once
// each, and nothing another tree holds; a process in the tree whose files
// cannot be read fails the read rather than answering partially.
func TestTreeHeldFiles(t *testing.T) {
	const (
		log   = "/home/user/.config/seat/sessions/2026/11/08/rollout-a.jsonl"
		other = "/home/user/.codex/sessions/2026/11/08/rollout-b.jsonl"
	)
	// 4242 is the pane's shell, 4300 a launcher, 4301 the agent; 5000 is
	// another session's agent.
	records := []proctable.ProcessRecord{
		{PID: 4242, PPID: 1}, {PID: 4300, PPID: 4242}, {PID: 4301, PPID: 4300}, {PID: 5000, PPID: 1},
	}
	for _, tt := range []struct {
		name    string
		files   map[int][]string
		err     map[int]bool
		want    []string
		wantErr bool
	}{
		{"held across the tree", map[int][]string{4242: {"/dev/pts/3"}, 4301: {"/w", log, log}, 5000: {other}}, nil, []string{"/dev/pts/3", "/w", log}, false},
		{"held twice", map[int][]string{4300: {log}, 4301: {log}}, nil, []string{log}, false},
		{"nothing held", nil, nil, nil, false},
		{"an unreadable descendant", map[int][]string{4301: {log}}, map[int]bool{4300: true}, nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			openFiles := func(pid int) ([]string, error) {
				if tt.err[pid] {
					return nil, errors.New("permission denied")
				}
				return tt.files[pid], nil
			}
			got, err := treeHeldFiles(records, 4242, openFiles)
			if (err != nil) != tt.wantErr {
				t.Fatalf("treeHeldFiles error = %v, wantErr %v", err, tt.wantErr)
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
	tm.paneHeldFiles = func(int) ([]string, error) { return nil, errors.New("permission denied") }
	tm.heldFileOwned = func(string) bool { return true }
	if got, err := tm.HeldFiles("seat"); err == nil {
		t.Fatalf("HeldFiles = %q, nil; want an error", got)
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
