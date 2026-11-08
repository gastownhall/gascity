package tmux

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

var _ runtime.HeldFileProvider = (*Provider)(nil)

// HeldFiles implements [runtime.HeldFileProvider] by forwarding to
// [Tmux.HeldFiles].
func (p *Provider) HeldFiles(name string) ([]string, error) {
	return p.tm.HeldFiles(name)
}

// HeldFiles returns the regular files the process tree rooted at session's
// agent pane holds open that the current user owns, read from the live
// process table: the same tree codexRolloutOf names a rollout from. A process
// in the tree whose open files the current user may not read, such as a
// root-owned sandbox wrapper above the agent or a zombie, is skipped
// (treeHeldFiles). A pane whose PID cannot be read, or a process in its tree
// whose open files cannot be read for any other reason, is an error, never an
// empty answer.
func (t *Tmux) HeldFiles(session string) ([]string, error) {
	out, err := t.GetPanePID(session)
	if err != nil {
		return nil, fmt.Errorf("held files of session %q: %w", session, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || pid <= 1 {
		return nil, fmt.Errorf("held files of session %q: pane pid %q is not an agent process", session, strings.TrimSpace(out))
	}
	read := t.paneHeldFiles
	if read == nil {
		read = paneTreeHeldFiles
	}
	files, err := read(pid)
	if err != nil {
		return nil, fmt.Errorf("held files of session %q: %w", session, err)
	}
	owned := t.heldFileOwned
	if owned == nil {
		owned = ownedRegularFile
	}
	return slices.DeleteFunc(files, func(path string) bool { return !owned(path) }), nil
}

// paneTreeHeldFiles returns the files the process tree rooted at panePID
// holds open: the tree read from the kernel's process table alone
// (proctable.SnapshotProcessTree, which reads no process's environment and
// runs no helper program), then each process's open files.
func paneTreeHeldFiles(panePID int) ([]string, error) {
	records, err := proctable.SnapshotProcessTree()
	if err != nil {
		return nil, fmt.Errorf("snapshotting the process table: %w", err)
	}
	return treeHeldFiles(records, panePID, proctable.OpenFiles)
}

// treeHeldFiles returns, once each in the order first found, the files root
// or any of its descendants in records holds open, per openFiles. A process
// whose open files the current user may not read (fs.ErrPermission) is
// skipped, and the walk goes on through its children: another user's
// process, such as a root-owned sandbox wrapper above the agent (sudo,
// bwrap), or a zombie, which holds no file but whose fd table the kernel
// shows as root's. A process whose open files cannot be read for any other
// reason is an error: the tree's answer would be partial.
func treeHeldFiles(records []proctable.ProcessRecord, root int, openFiles func(int) ([]string, error)) ([]string, error) {
	children := make(map[int][]int, len(records))
	for _, r := range records {
		if r.PID != root {
			children[r.PPID] = append(children[r.PPID], r.PID)
		}
	}
	var held []string
	seenFile := map[string]bool{}
	seen := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		files, err := openFiles(pid)
		if err != nil && !errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("reading the open files of pid %d: %w", pid, err)
		}
		for _, file := range files {
			if !seenFile[file] {
				seenFile[file] = true
				held = append(held, file)
			}
		}
		for _, child := range children[pid] {
			if !seen[child] {
				seen[child] = true
				queue = append(queue, child)
			}
		}
	}
	return held, nil
}
