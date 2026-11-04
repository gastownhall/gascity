package pidutil

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// This file is the shared answer to one question: "has this process been
// ORPHANED — i.e. is its parent a subreaper that would only own it because the
// process that spawned it has already exited?"
//
// The naive form of that test is `ppid <= 1`, and it is wrong on the
// topologies this fleet actually runs. A process that sets
// PR_SET_CHILD_SUBREAPER adopts every orphan among its descendants, so an
// orphan reparents to the nearest such ancestor of its dead parent, not to
// pid 1: `systemd --user` under a user@UID.service, Bazel's local
// process-wrapper around a test action, a container init such as tini. Its
// ppid is then a large live pid and every `<= 1` test reads it as "still
// owned by a live parent".
//
// Another process's subreaper flag cannot be read (PR_GET_CHILD_SUBREAPER
// answers only for the caller), so the set is approximated from the caller's
// side: a process the caller spawned descends from the caller, so its orphan
// is adopted by pid 1 or by one of the caller's own ancestors. Those
// ancestors are the set (DetectSubreapers).
//
// That distinction is load-bearing wherever the answer gates a kill. A daemon
// that inherited an agent's environment and then outlived its spawner (the
// managed-Dolt scope watchdog is the in-tree instance) is an orphan under both
// models; a test that only recognizes the plain-init model treats it as a live,
// owned child of the session and will happily signal its process group.
//
// The logic originated in internal/workspacesvc's orphan sweep, which
// discovered the subreaper model first; it lives here so the runtime and CLI
// layers can share it rather than re-deriving a `<= 1` test each time.

// ParentPID returns the parent process ID from /proc/<pid>/stat.
//
// The comm field is parsed by scanning to the LAST ')' because a process name
// may itself contain parentheses and spaces.
func ParentPID(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	idx := bytes.LastIndexByte(data, ')')
	if idx < 0 {
		return 0, fmt.Errorf("malformed stat for pid %d", pid)
	}
	fields := strings.Fields(string(data[idx+1:]))
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed stat for pid %d", pid)
	}
	return strconv.Atoi(fields[1])
}

// Comm returns the executable name from /proc/<pid>/comm, or "" if it cannot
// be read.
func Comm(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Subreapers is the set of processes that may adopt an orphan of a process
// the caller spawned: the caller's ancestors above pid 1 (DetectSubreapers).
// Pid 1 is never a member; IsReparentedOrphan counts it on its own. The zero
// value is the empty set, which leaves only pid 1 as an adopter.
type Subreapers struct {
	pids map[int]struct{}
}

// NewSubreapers returns the set holding pids. A pid of 1 or less is never a
// member: 1 is init, which IsReparentedOrphan always counts, and 0 or below
// names no process.
func NewSubreapers(pids ...int) Subreapers {
	var s Subreapers
	for _, pid := range pids {
		s.add(pid)
	}
	return s
}

func (s *Subreapers) add(pid int) {
	if pid <= 1 {
		return
	}
	if s.pids == nil {
		s.pids = make(map[int]struct{})
	}
	s.pids[pid] = struct{}{}
}

// Contains reports whether pid is in the set.
func (s Subreapers) Contains(pid int) bool {
	_, ok := s.pids[pid]
	return ok
}

// PIDs returns the set's pids in ascending order.
func (s Subreapers) PIDs() []int {
	out := make([]int, 0, len(s.pids))
	for pid := range s.pids {
		out = append(out, pid)
	}
	sort.Ints(out)
	return out
}

// String renders the set as its sorted pids, for journals and test failures.
func (s Subreapers) String() string {
	return fmt.Sprint(s.PIDs())
}

// maxAncestorDepth bounds the ancestry walk against malformed /proc data.
const maxAncestorDepth = 64

// DetectSubreapers returns the processes that may adopt an orphan of a
// process self spawned: self's ancestors above pid 1. It returns the empty
// set on a plain-init host where self's parent is pid 1, and on any platform
// without /proc.
//
// An orphan is adopted by the nearest subreaper ancestor of its dead parent.
// When self spawned that parent, the adopter is pid 1 or an ancestor of self,
// whichever process set PR_SET_CHILD_SUBREAPER: the `systemd --user` manager,
// Bazel's process-wrapper, a container init. Self is never a member: it is
// the live parent of everything it spawns directly.
//
// A tmux server is never a member either. tmux never sets
// PR_SET_CHILD_SUBREAPER, and it is the live parent of every pane root it
// serves, so a caller started from a pane would otherwise read those roots
// as orphans.
//
// Callers should resolve this ONCE per sweep and pass it down: it walks the
// parent chain on every call.
func DetectSubreapers(self int) Subreapers {
	return DetectSubreapersWith(self, ParentPID, Comm)
}

// DetectSubreapersWith is DetectSubreapers with its two /proc reads injected,
// so the ancestry walk can be tested without a real process tree. A parent
// that cannot be read ends the walk with the ancestors already read.
func DetectSubreapersWith(self int, parentOf func(int) (int, error), commOf func(int) string) Subreapers {
	var s Subreapers
	pid := self
	for depth := 0; depth < maxAncestorDepth; depth++ {
		ppid, err := parentOf(pid)
		if err != nil || ppid <= 1 || ppid == self || s.Contains(ppid) {
			break
		}
		if !isTmuxComm(commOf(ppid)) {
			s.add(ppid)
		}
		pid = ppid
	}
	return s
}

// isTmuxComm reports whether comm is a tmux process: the bare executable, or
// the "tmux: server" / "tmux: client" titles tmux sets on Linux.
func isTmuxComm(comm string) bool {
	switch comm {
	case "tmux", "tmux: server", "tmux: client":
		return true
	}
	return false
}

// IsReparentedOrphan reports whether ppid identifies a subreaper that would own
// a process only after its real parent exited: init (pid 1), or a member of
// subreapers.
//
// An empty set collapses this to the plain `ppid == 1` rule. A live parent — a
// still-running supervisor, or the tmux server that owns a pane's root process
// — is never a member, so a genuinely owned child never matches. A ppid of 0
// means the parent could not be read, which proves nothing, so it never
// matches either.
func IsReparentedOrphan(ppid int, subreapers Subreapers) bool {
	if ppid == 1 {
		return true
	}
	return subreapers.Contains(ppid)
}
