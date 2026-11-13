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

// ScanBySessionIDOutsideTmux is ScanBySessionID for a runtime whose agents
// never run under a tmux server, such as an exec pack whose boxes share this
// host and run their agents outside tmux: it never reports a process that
// is, or descends from, a tmux server or client carrying a GC_SESSION_ID.
// Such a tmux was founded inside a session (an agent's `tmux new -d`, or a
// user's default tmux server first started from a box) and copied its
// founder's environment, so ScanBySessionID reports each of its panes as
// that session's root: right for the tmux runtime, whose panes are its
// sessions, and wrong for any other runtime, whose orphan kill would end the
// pane's process group. A tmux carrying no session's id (a pack's own
// multiplexer, or a user's tmux that gc itself runs in) hosts no such pane,
// so a root under it is still reported. A root whose ancestry cannot be
// decided (an ancestor vanished mid-walk, the parent chain loops, or a tmux
// ancestor's environment cannot be read) is left out with an EntryError,
// since such a tmux may host it; a root that exited after the scan is left
// out silently.
func ScanBySessionIDOutsideTmux(id string) ([]runtime.LiveRuntime, error) {
	found, err := ScanBySessionID(id)
	if len(found) == 0 {
		return found, err
	}
	kept, fenceErr := fenceTmuxSubtrees(scanRoot, found)
	return kept, errors.Join(err, fenceErr)
}

// fenceTmuxSubtrees is ScanBySessionIDOutsideTmux's fence over the procfs at
// root (fenceRoots over hostedBySessionTmux).
func fenceTmuxSubtrees(root string, found []runtime.LiveRuntime) ([]runtime.LiveRuntime, error) {
	return fenceRoots(found, func(pid int) (bool, error) { return hostedBySessionTmux(root, pid) })
}

// errProcessGone marks a process the walk found gone from the procfs.
var errProcessGone = errors.New("the process is gone")

// hostedBySessionTmux reports whether pid, or one of its ancestors up to
// init, is a tmux server or client carrying a GC_SESSION_ID (tmuxStep). A
// pid that is gone is errRootGone; an ancestor that vanished, a parent chain
// that loops, or a failed read is an error.
func hostedBySessionTmux(root string, pid int) (bool, error) {
	seen := make(map[int]bool)
	for p := pid; p > 1; {
		if seen[p] {
			return false, fmt.Errorf("the parent chain loops at pid %d", p)
		}
		seen[p] = true
		hosted, ppid, err := tmuxStep(root, p)
		switch {
		case errors.Is(err, errProcessGone) && p == pid:
			return false, errRootGone
		case errors.Is(err, errProcessGone):
			return false, fmt.Errorf("ancestor pid %d vanished", p)
		case err != nil || hosted:
			return hosted, err
		}
		p = ppid
	}
	return false, nil
}

// tmuxStep reads one process of the walk: whether it is a tmux server or
// client (isTmuxComm) carrying a GC_SESSION_ID and, when it is not, its
// parent. A tmux whose environment cannot be read fails closed with an
// error, since it may carry one. A process that is gone is errProcessGone.
func tmuxStep(root string, pid int) (bool, int, error) {
	dir := filepath.Join(root, strconv.Itoa(pid))
	comm, err := os.ReadFile(filepath.Join(dir, "comm"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, 0, errProcessGone
	}
	if err != nil {
		return false, 0, err
	}
	if isTmuxComm(string(comm)) {
		env, envErr := parseEnvironFile(filepath.Join(dir, "environ"))
		switch {
		case envErr != nil:
			return false, 0, fmt.Errorf("reading the environment of tmux pid %d: %w", pid, envErr)
		case env == nil:
			return false, 0, errProcessGone
		case env["GC_SESSION_ID"] != "":
			return true, 0, nil
		}
	}
	ppid, ok, err := readParentPID(filepath.Join(dir, "stat"))
	if err != nil {
		return false, 0, err
	}
	if !ok {
		return false, 0, errProcessGone
	}
	return false, ppid, nil
}

// isTmuxComm reports whether a /proc comm names tmux infrastructure for the
// fence: what isInfrastructureCommand matches, or a title tmux takes from
// its argv[0], so a tmux run under another name (tmux3, a versioned build)
// is fenced too. tmux titles itself "<name>: server (<socket>)" (or client)
// and, when that overruns comm's 15 bytes, cuts it at its last space: a
// name of up to 6 characters leaves "<name>: server" or "<name>: client",
// one of 7 to 14 characters "<name>:", and a longer one leaves no colon in
// comm, so it is not recognized. Any process whose comm takes one of these
// forms is read as tmux. The fence errs toward reporting less, so it may
// read more names as tmux than the shared scan's root test does.
func isTmuxComm(comm string) bool {
	comm = strings.TrimSpace(comm)
	return isInfrastructureCommand(comm) ||
		strings.HasSuffix(comm, ": server") ||
		strings.HasSuffix(comm, ": client") ||
		strings.HasSuffix(comm, ":")
}
