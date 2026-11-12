//go:build linux

package proctable

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ScanBySessionIDOutsideTmux is ScanBySessionID for a runtime whose sessions
// never run under a tmux server, such as an exec pack whose boxes share this
// host: it never reports a process that has a tmux server or client among its
// ancestors. A tmux server an agent founds inside its session (`tmux new -d`)
// inherits the session's GC_SESSION_ID, so ScanBySessionID reports each of its
// panes as that session's root, which is right for the tmux runtime, whose
// panes are its sessions, and wrong for any other runtime, whose orphan kill
// would end the pane's process group. A root whose ancestry cannot be read
// (an ancestor vanished mid-walk, or the parent chain loops) is left out with
// an EntryError: it may sit under a tmux server.
func ScanBySessionIDOutsideTmux(id string) ([]runtime.LiveRuntime, error) {
	found, err := ScanBySessionID(id)
	if len(found) == 0 {
		return found, err
	}
	kept, fenceErr := fenceTmuxSubtrees(scanRoot, found)
	return kept, errors.Join(err, fenceErr)
}

// fenceTmuxSubtrees drops every root of found that descends from a tmux
// server or client, and every root whose ancestry is undecidable, joining an
// EntryError for each of the latter.
func fenceTmuxSubtrees(root string, found []runtime.LiveRuntime) ([]runtime.LiveRuntime, error) {
	kept := make([]runtime.LiveRuntime, 0, len(found))
	var errs []error
	for _, live := range found {
		under, err := underTmux(root, live.PID)
		if err != nil {
			errs = append(errs, &EntryError{PID: live.PID, Err: fmt.Errorf("reading the ancestry of pid %d: %w", live.PID, err)})
			continue
		}
		if !under {
			kept = append(kept, live)
		}
	}
	return kept, errors.Join(errs...)
}

// underTmux reports whether a tmux server or client (isInfrastructureCommand)
// is an ancestor of pid, walking parent links up to init.
func underTmux(root string, pid int) (bool, error) {
	seen := map[int]bool{pid: true}
	ppid, ok, err := readParentPID(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("pid %d vanished", pid)
	}
	for p := ppid; p > 1; {
		if seen[p] {
			return false, fmt.Errorf("the parent chain loops at pid %d", p)
		}
		seen[p] = true
		comm, err := os.ReadFile(filepath.Join(root, strconv.Itoa(p), "comm"))
		if errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("ancestor pid %d vanished", p)
		}
		if err != nil {
			return false, err
		}
		if isInfrastructureCommand(string(comm)) {
			return true, nil
		}
		next, ok, err := readParentPID(filepath.Join(root, strconv.Itoa(p), "stat"))
		if err != nil {
			return false, err
		}
		if !ok {
			return false, fmt.Errorf("ancestor pid %d vanished", p)
		}
		p = next
	}
	return false, nil
}
