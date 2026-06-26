package workspacesvc

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// Orphaned service processes are survivors of a previous supervisor hard
// exit: the supervisor died without closing its proxy_process children, so
// they re-parented to init (ppid 1) and kept running. Every subsequent
// supervisor start then spawned a fresh duplicate next to them (observed in
// production: ~39 accumulated duplicates, ga-mukg0s). The sweep below runs
// before each proxy_process spawn and terminates those survivors so
// duplicates never accumulate.
//
// Matching queries live process-table state only — no pid or status files.
// A process is signaled only while every rule of the orphan identity holds
// (see orphanIdentity.matchesLive):
//
//  1. it is alive and not a zombie;
//  2. it has re-parented to init (ppid 1), so no live supervisor owns it;
//  3. its command line is exactly the service's configured command; and
//  4. its environment carries GC_SERVICE_NAME=<service> and
//     GC_SERVICE_STATE_ROOT=<this instance's state root>, proving a gc
//     supervisor spawned it as this city's instance of the service rather
//     than a coincidental same-argv process or a sibling city's
//     still-serving orphan of the same service.
//
// Rule 2 is only meaningful when the sweeping process is not itself init,
// so the sweep is skipped entirely when running as pid 1 (see
// findOrphanedServiceProcessesFrom).
//
// A separately-scoped cleanup with different matching rules
// (GC_HOME/registry scoping instead of service-name + state-root +
// exact-argv + ppid 1) runs at supervisor start and warm refresh — see
// cleanupSupervisorWorkspaceServices in cmd/gc/cmd_supervisor_lifecycle.go.
// Keep the two mechanisms in mind when changing either.

// orphanReapTermWait bounds how long the sweep waits after SIGTERM before
// escalating to SIGKILL.
const orphanReapTermWait = proxyProcessShutdownWait

// orphanIdentity is the full identity a process must match before the sweep
// may signal it: the service's name, the spawning instance's absolute state
// root, and the configured command.
type orphanIdentity struct {
	serviceName string
	stateRoot   string
	command     []string
}

// newOrphanIdentity builds the sweep identity for one service instance.
// stateRoot must be the instance's absolute state root — the value the
// spawn path exports as GC_SERVICE_STATE_ROOT. command is normalized with
// the same empty/whitespace-argument rule pidutil.Cmdline applies to live
// process command lines, so a configured command containing such arguments
// still matches its own orphans.
func newOrphanIdentity(serviceName, stateRoot string, command []string) orphanIdentity {
	return orphanIdentity{
		serviceName: serviceName,
		stateRoot:   stateRoot,
		command:     pidutil.NormalizeArgv(command),
	}
}

// matchesLive reports whether pid currently satisfies every identity rule:
// alive and not a zombie, re-parented to init (ppid 1), command line equal
// to the service command, and environment carrying both the service-name
// and state-root markers. The same predicate runs at scan time, on every
// escalation wait pass, and immediately before SIGKILL — mirroring
// cleanupSupervisorWorkspaceServices's re-read-before-kill precedent — so a
// pid recycled at any point is dropped rather than signaled. Any read
// failure counts as a mismatch: the sweep never signals a process it cannot
// prove is still the orphan.
func (id orphanIdentity) matchesLive(pid int) bool {
	if !pidutil.Alive(pid) {
		return false
	}
	if ppid, err := processParentPID(pid); err != nil || ppid != 1 {
		return false
	}
	if !processCmdlineEquals(pid, id.command) {
		return false
	}
	return processEnvironMatchesService(pid, id.serviceName, id.stateRoot)
}

// reapOrphanedServiceProcesses terminates orphaned survivors of previous
// hard exits that match the service instance's identity. Best-effort: scan
// or signal failures are logged and never block the spawn; on hosts without
// /proc the sweep is a no-op.
func reapOrphanedServiceProcesses(id orphanIdentity) {
	pids := findOrphanedServiceProcesses(id)
	if len(pids) == 0 {
		return
	}
	log.Printf("workspacesvc: terminating %d orphaned process(es) for service %q: %v", len(pids), id.serviceName, pids)
	terminateOrphanedProcesses(id, pids)
}

// findOrphanedServiceProcesses scans /proc for processes matching id.
// Processes that exit mid-scan or whose records are unreadable are skipped.
func findOrphanedServiceProcesses(id orphanIdentity) []int {
	return findOrphanedServiceProcessesFrom(os.Getpid(), id)
}

// findOrphanedServiceProcessesFrom is the scan body with the sweeping
// process's pid made explicit so tests can cover the pid-1 guard. When the
// sweeper is itself init (pid 1 — gc as a container entrypoint), every live
// supervised child also has ppid 1, so the orphan test would match healthy
// processes (including a sibling city's children and publication-context
// replacements started before the old instance closes). Orphans of a
// previous supervisor cannot exist in that case — pid 1 dying tears down
// the pid namespace — so the sweep finds nothing by definition. An identity
// without a state root is incomplete and matches nothing rather than
// matching too broadly.
func findOrphanedServiceProcessesFrom(self int, id orphanIdentity) []int {
	if len(id.command) == 0 || id.stateRoot == "" || self == 1 {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		if !id.matchesLive(pid) {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// terminateOrphanedProcesses sends SIGTERM to each pid (preferring its
// process group, which the spawn path creates via Setpgid), waits briefly,
// then SIGKILLs stragglers. The full identity is re-verified on every wait
// pass and once more immediately before SIGKILL, so a pid recycled after
// SIGTERM is dropped rather than SIGKILLed.
func terminateOrphanedProcesses(id orphanIdentity, pids []int) {
	for _, pid := range pids {
		signalProcessOrGroup(pid, syscall.SIGTERM)
	}
	remaining := pids
	deadline := time.Now().Add(orphanReapTermWait)
	for time.Now().Before(deadline) {
		remaining = liveMatchingOrphans(remaining, id)
		if len(remaining) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	remaining = liveMatchingOrphans(remaining, id)
	if len(remaining) == 0 {
		return
	}
	log.Printf("workspacesvc: SIGKILL %d orphaned straggler(s) for service %q: %v", len(remaining), id.serviceName, remaining)
	for _, pid := range remaining {
		signalProcessOrGroup(pid, syscall.SIGKILL)
	}
}

// signalProcessOrGroup signals the process group led by pid, falling back
// to the process itself when pid is not a group leader. Unsafe targets are
// refused outright.
func signalProcessOrGroup(pid int, sig syscall.Signal) {
	if unsafeSignalTarget(pid) {
		log.Printf("workspacesvc: refusing to signal unsafe orphan-reap target pid %d", pid)
		return
	}
	if err := syscall.Kill(-pid, sig); err == nil {
		return
	}
	_ = syscall.Kill(pid, sig)
}

// unsafeSignalTarget reports whether pid must never be signaled: init,
// nonpositive pids (kill(2) broadcast and current-group semantics), and the
// sweeper's own process group. The scan cannot produce such pids today;
// this mirrors processgroup.Terminate's refusal so kill-adjacent code stays
// safe under future refactors.
func unsafeSignalTarget(pid int) bool {
	return pid <= 1 || pid == syscall.Getpgrp()
}

// liveMatchingOrphans filters pids down to those that still match the full
// orphan identity. Re-checking here — not just at scan time — keeps a
// recycled same-uid pid (and via kill(-pid) its group) from being
// SIGKILLed, and lets a slow-reaped zombie drop out instead of pinning the
// full escalation wait.
func liveMatchingOrphans(pids []int, id orphanIdentity) []int {
	live := pids[:0]
	for _, pid := range pids {
		if id.matchesLive(pid) {
			live = append(live, pid)
		}
	}
	return live
}

// processParentPID reads the parent pid from /proc/<pid>/stat. The comm
// field may contain spaces and parentheses, so parsing starts after the
// last ')'.
func processParentPID(pid int) (int, error) {
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

// processCmdlineEquals reports whether the process's command line equals
// command argv-for-argv.
func processCmdlineEquals(pid int, command []string) bool {
	argv, err := pidutil.Cmdline(pid)
	if err != nil {
		return false
	}
	return argvEquals(argv, command)
}

// argvEquals reports whether argv equals command argv-for-argv.
func argvEquals(argv, command []string) bool {
	if len(argv) != len(command) {
		return false
	}
	for i := range argv {
		if argv[i] != command[i] {
			return false
		}
	}
	return true
}

// processEnvironMatchesService reports whether the process was spawned with
// both GC_SERVICE_NAME=<serviceName> and GC_SERVICE_STATE_ROOT=<stateRoot>.
// The state root scopes matching to this city's instance of the service: it
// is config-derived — stable across supervisor restarts but unique per
// city+service — so a sibling city running the same service name and
// command never matches. (GC_SERVICE_SOCKET would not work as the ownership
// key: it is freshly allocated per instance, so no orphan would ever
// match.) /proc/<pid>/environ is readable only for same-uid processes,
// which doubles as the ownership check; unreadable or unmarked processes
// are never reaped.
func processEnvironMatchesService(pid int, serviceName, stateRoot string) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return false
	}
	nameMarker := "GC_SERVICE_NAME=" + serviceName
	rootMarker := "GC_SERVICE_STATE_ROOT=" + stateRoot
	var nameOK, rootOK bool
	for _, kv := range strings.Split(string(data), "\x00") {
		switch kv {
		case nameMarker:
			nameOK = true
		case rootMarker:
			rootOK = true
		}
	}
	return nameOK && rootOK
}
