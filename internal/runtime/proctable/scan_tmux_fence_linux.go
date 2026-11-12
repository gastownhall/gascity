//go:build linux

package proctable

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
	kept, fenceErr := FenceTmuxSubtrees(found)
	return kept, errors.Join(err, fenceErr)
}

// FenceTmuxSubtrees keeps the roots of found that have no tmux server or
// client among their ancestors, reading the same process table the scans
// read. It is ScanBySessionIDOutsideTmux's fence on its own, for a composite
// whose default backend's sessions never run under a tmux server to apply to
// the roots another backend's scan reported (runtime.TmuxSubtreeFence). A
// root whose ancestry cannot be read is left out with an EntryError.
func FenceTmuxSubtrees(found []runtime.LiveRuntime) ([]runtime.LiveRuntime, error) {
	return fenceTmuxSubtrees(scanRoot, found)
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

// underTmux reports whether pid is a tmux server or client, or has one among
// its ancestors (isTmuxComm), walking parent links up to init.
func underTmux(root string, pid int) (bool, error) {
	if tmux, err := isTmuxProcess(root, pid); err != nil || tmux {
		return tmux, err
	}
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
		tmux, err := isTmuxProcess(root, p)
		if err != nil {
			return false, err
		}
		if tmux {
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

// isTmuxProcess reports whether pid's comm names a tmux server or client
// (isTmuxComm).
func isTmuxProcess(root string, pid int) (bool, error) {
	comm, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "comm"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("pid %d vanished", pid)
	}
	if err != nil {
		return false, err
	}
	return isTmuxComm(string(comm)), nil
}

// isTmuxComm reports whether a /proc comm names tmux infrastructure for the
// fence: what isInfrastructureCommand matches, or the "<name>: server" and
// "<name>: client" titles tmux takes from its argv[0], so a tmux run under
// another name (tmux3, a versioned build) is fenced too. The fence errs
// toward reporting less, so it may read more names as tmux than the shared
// scan's root test does.
func isTmuxComm(comm string) bool {
	comm = strings.TrimSpace(comm)
	return isInfrastructureCommand(comm) || strings.HasSuffix(comm, ": server") || strings.HasSuffix(comm, ": client")
}
