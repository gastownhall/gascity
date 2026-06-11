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
// A process is reaped only when all three hold:
//
//  1. it has re-parented to init (ppid 1), so no live supervisor owns it;
//  2. its command line is exactly the service's configured command; and
//  3. its environment carries GC_SERVICE_NAME=<service>, proving a gc
//     supervisor spawned it as this service rather than a coincidental
//     same-argv process.

// orphanReapTermWait bounds how long the sweep waits after SIGTERM before
// escalating to SIGKILL.
const orphanReapTermWait = proxyProcessShutdownWait

// reapOrphanedServiceProcesses terminates orphaned survivors of previous
// hard exits that match the service's command-line signature. Best-effort:
// scan or signal failures are logged and never block the spawn; on hosts
// without /proc the sweep is a no-op.
func reapOrphanedServiceProcesses(serviceName string, command []string) {
	pids := findOrphanedServiceProcesses(serviceName, command)
	if len(pids) == 0 {
		return
	}
	log.Printf("workspacesvc: terminating %d orphaned process(es) for service %q: %v", len(pids), serviceName, pids)
	terminateOrphanedProcesses(serviceName, pids)
}

// findOrphanedServiceProcesses scans /proc for ppid-1 processes whose
// command line equals command and whose environment marks them as spawns of
// serviceName. Processes that exit mid-scan or whose records are unreadable
// are skipped.
func findOrphanedServiceProcesses(serviceName string, command []string) []int {
	if len(command) == 0 {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		if ppid, err := processParentPID(pid); err != nil || ppid != 1 {
			continue
		}
		if !processCmdlineEquals(pid, command) {
			continue
		}
		if !processEnvironHasServiceMarker(pid, serviceName) {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// terminateOrphanedProcesses sends SIGTERM to each pid (preferring its
// process group, which the spawn path creates via Setpgid), waits briefly,
// then SIGKILLs stragglers.
func terminateOrphanedProcesses(serviceName string, pids []int) {
	for _, pid := range pids {
		signalProcessOrGroup(pid, syscall.SIGTERM)
	}
	remaining := pids
	deadline := time.Now().Add(orphanReapTermWait)
	for time.Now().Before(deadline) {
		remaining = liveProcesses(remaining)
		if len(remaining) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	remaining = liveProcesses(remaining)
	if len(remaining) == 0 {
		return
	}
	log.Printf("workspacesvc: SIGKILL %d orphaned straggler(s) for service %q: %v", len(remaining), serviceName, remaining)
	for _, pid := range remaining {
		signalProcessOrGroup(pid, syscall.SIGKILL)
	}
}

// signalProcessOrGroup signals the process group led by pid, falling back
// to the process itself when pid is not a group leader.
func signalProcessOrGroup(pid int, sig syscall.Signal) {
	if err := syscall.Kill(-pid, sig); err == nil {
		return
	}
	_ = syscall.Kill(pid, sig)
}

// liveProcesses filters pids down to those that still exist.
func liveProcesses(pids []int) []int {
	live := pids[:0]
	for _, pid := range pids {
		if err := syscall.Kill(pid, 0); err == nil || err == syscall.EPERM {
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

// processCmdlineEquals reports whether /proc/<pid>/cmdline equals command
// argv-for-argv.
func processCmdlineEquals(pid int, command []string) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	argv := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
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

// processEnvironHasServiceMarker reports whether the process was spawned
// with GC_SERVICE_NAME=<serviceName>. /proc/<pid>/environ is readable only
// for same-uid processes, which doubles as the ownership check; unreadable
// or unmarked processes are never reaped.
func processEnvironHasServiceMarker(pid int, serviceName string) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return false
	}
	marker := "GC_SERVICE_NAME=" + serviceName
	for _, kv := range strings.Split(string(data), "\x00") {
		if kv == marker {
			return true
		}
	}
	return false
}
