package pidutil

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
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
// answers only for the caller), and being an ancestor is no proof of it: a
// live ancestor that never set the flag is the real parent of every child it
// spawns. So the adopter is observed instead (DetectSubreapers): the caller
// orphans a probe process of its own and reads whom the kernel reparented it
// to. Every orphan whose dead parent descends from the caller goes to that
// same process, the nearest subreaper ancestor of the caller, or to pid 1.
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
//
// It answers only where procfs does. On Windows it reports ErrUnsupported
// rather than a fabricated ancestry: the kernel there does not reparent an
// orphan onto a subreaper at all — a dead parent leaves a dangling parent ID
// that the PID allocator may later hand to an unrelated process — so the
// orphan model this function serves has no Windows equivalent to report.
func ParentPID(pid int) (int, error) {
	data, err := readProcFile(pid, "stat")
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
	data, err := readProcFile(pid, "comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Subreapers is the set of processes besides pid 1 that adopt an orphan of a
// process the caller spawned (DetectSubreapers). Pid 1 is never a member;
// IsReparentedOrphan counts it on its own. The zero value is the empty set,
// which leaves only pid 1 as an adopter.
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

// DetectSubreapers returns the process that adopts an orphan of a process
// this process spawned, when that is not pid 1: the nearest ancestor that set
// PR_SET_CHILD_SUBREAPER, such as the `systemd --user` manager, Bazel's
// process-wrapper or a container init. It returns the empty set on a host
// where such orphans go to pid 1, and on any platform without the Linux
// subreaper model.
//
// The adopter is observed, not inferred. The caller orphans a probe process
// (a shell's background child, whose shell exits at once), reads the probe's
// parent while the probe blocks on a pipe, then closes the pipe so the probe
// exits. The kernel picks the adopter by walking up from the probe's dead
// parent, which is the caller's child, so it is the process that adopts
// every orphan whose dead parent descends from the caller. An ancestor that
// never set the flag is never the adopter, so its own children stay owned.
// The caller itself is never a member: when the caller is the subreaper it
// is the live parent of the agents it spawns.
//
// The result is remembered for this process's ancestry, so a sweep pays for
// one probe per change of ancestry, not one per call: an ancestor's exit
// reparents its child and so changes the chain. A failed probe is not
// remembered; it returns the empty set with the error, and the next call
// probes again.
func DetectSubreapers() (Subreapers, error) {
	return processAdopter.detect(os.Getpid(), ParentPID, probeOrphanAdopter)
}

// processAdopter holds the last successful detection for this process.
var processAdopter adopterCache

// adopterCache remembers the subreaper set detected for one ancestry.
type adopterCache struct {
	mu    sync.Mutex
	valid bool
	chain []int
	set   Subreapers
}

// detect returns the subreaper set for self, whose ancestry parentOf reads,
// running probe (which reports the observed adopter of an orphan of self's
// child) only when the ancestry differs from the remembered one.
func (c *adopterCache) detect(self int, parentOf func(int) (int, error), probe func() (int, error)) (Subreapers, error) {
	chain := ancestry(self, parentOf)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && slices.Equal(c.chain, chain) {
		return c.set, nil
	}
	set, err := adopterSet(self, chain, probe)
	if err != nil {
		return Subreapers{}, err
	}
	c.valid, c.chain, c.set = true, chain, set
	return set, nil
}

// ancestry returns self's ancestors above pid 1, nearest first. A parent that
// cannot be read, a loop, or the depth bound ends the walk.
func ancestry(self int, parentOf func(int) (int, error)) []int {
	var chain []int
	pid := self
	for depth := 0; depth < maxAncestorDepth; depth++ {
		ppid, err := parentOf(pid)
		if err != nil || ppid <= 1 || ppid == self || slices.Contains(chain, ppid) {
			break
		}
		chain = append(chain, ppid)
		pid = ppid
	}
	return chain
}

// adopterSet runs probe and turns the adopter it observed into the set. The
// kernel reparents an orphan to pid 1, to the caller when the caller is a
// subreaper, or to a subreaper ancestor; any other answer means the probe
// did not measure what it should, and fails closed.
func adopterSet(self int, chain []int, probe func() (int, error)) (Subreapers, error) {
	adopter, err := probe()
	if err != nil {
		return Subreapers{}, fmt.Errorf("probing the orphan adopter of pid %d: %w", self, err)
	}
	switch {
	case adopter == 1 || adopter == self:
		return Subreapers{}, nil
	case adopter <= 0:
		return Subreapers{}, fmt.Errorf("probe orphan of pid %d has no visible parent", self)
	case !slices.Contains(chain, adopter):
		return Subreapers{}, fmt.Errorf("probe orphan of pid %d was adopted by pid %d, which is not one of its ancestors %v", self, adopter, chain)
	}
	return NewSubreapers(adopter), nil
}

// IsReparentedOrphan reports whether ppid identifies a subreaper that would own
// a process only after its real parent exited: init (pid 1), or a member of
// subreapers.
//
// An empty set collapses this to the plain `ppid == 1` rule. A live parent — a
// still-running supervisor, a shell that spawned the process itself, or the
// tmux server that owns a pane's root process — never set
// PR_SET_CHILD_SUBREAPER and so is never a member, and a genuinely owned
// child never matches. A ppid of 0 means the parent could not be read, which
// proves nothing, so it never matches either.
func IsReparentedOrphan(ppid int, subreapers Subreapers) bool {
	if ppid == 1 {
		return true
	}
	return subreapers.Contains(ppid)
}
