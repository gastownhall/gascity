package pidutil

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// This file is the shared answer to one question: "has this process been
// ORPHANED — i.e. is its parent a subreaper that would only own it because the
// process that spawned it has already exited?"
//
// The naive form of that test is `ppid <= 1`, and it is wrong on the topology
// this fleet actually runs. Under a `user@UID.service`, `systemd --user` sets
// PR_SET_CHILD_SUBREAPER, so an orphan reparents to the USER MANAGER, not to
// pid 1 — its ppid is a large live pid and every `<= 1` test reads it as
// "still owned by a live parent".
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

// DetectUserSubreaperPID returns the pid of the `systemd --user` manager acting
// as the child subreaper for this user session, or 0 when there is none (a
// plain-init host, or any platform without /proc).
//
// The caller is itself a descendant of that manager, so the walk goes up the
// caller's own parent chain and returns the nearest ancestor named "systemd"
// whose pid is not 1 — the user manager, as distinct from the system systemd at
// pid 1. Bounded to guard against malformed /proc data, and stops at pid 1.
//
// Callers should resolve this ONCE per sweep and pass it down: it walks the
// parent chain on every call.
func DetectUserSubreaperPID(self int) int {
	return DetectUserSubreaperPIDWith(self, ParentPID, Comm)
}

// DetectUserSubreaperPIDWith is DetectUserSubreaperPID with its two /proc reads
// injected, so the ancestry walk can be tested without a real process tree.
func DetectUserSubreaperPIDWith(self int, parentOf func(int) (int, error), commOf func(int) string) int {
	pid := self
	for depth := 0; depth < 64; depth++ {
		ppid, err := parentOf(pid)
		if err != nil || ppid <= 1 {
			return 0
		}
		if commOf(ppid) == "systemd" {
			return ppid
		}
		pid = ppid
	}
	return 0
}

// IsReparentedOrphan reports whether ppid identifies a subreaper that would own
// a process only after its real parent exited: init (pid 1) on a plain host, or
// the detected `systemd --user` manager under a user session.
//
// subreaperPID of 0 means "none detected", which collapses this to the plain
// `ppid == 1` rule. A live parent — a still-running supervisor, or the tmux
// server that owns a pane's root process — is never a subreaper, so a genuinely
// owned child never matches under either model.
func IsReparentedOrphan(ppid, subreaperPID int) bool {
	if ppid <= 1 {
		return true
	}
	return subreaperPID > 1 && ppid == subreaperPID
}
