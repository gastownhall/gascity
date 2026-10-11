package pidutil

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// TCP listener lookups read the kernel's socket tables instead of shelling out
// to lsof. `lsof -iTCP:<port> -sTCP:LISTEN` walks every /proc/<pid>/fd on the
// host before it filters, so on a busy machine (thousands of processes) each
// call costs seconds of kernel CPU, and many concurrent callers saturate it.
// /proc/net/tcp{,6} name a listening socket's inode directly; mapping that
// inode to a process only needs the fd directories of the processes asked
// about. Callers that already know which PID should own a port pass it as a
// candidate so the whole-table walk is only a last resort.
//
// /proc/net is Linux-only. Every lookup reports whether the tables could be
// read at all so callers on hosts without it (darwin) can fall back to lsof.

// procNetTCPTables are the kernel's IPv4 and IPv6 TCP socket tables.
var procNetTCPTables = []string{"/proc/net/tcp", "/proc/net/tcp6"}

const (
	// procNetTCPListen is TCP_LISTEN in the st column, as the kernel prints it.
	procNetTCPListen = "0A"
	// Column indexes in a /proc/net/tcp{,6} row.
	procNetTCPLocalField = 1
	procNetTCPStateField = 3
	procNetTCPInodeField = 9
)

// ListeningSocketInodes returns the inodes of the TCP sockets listening on
// port, and whether any of the kernel's socket tables could be read.
func ListeningSocketInodes(port int) (map[string]struct{}, bool) {
	inodes := map[string]struct{}{}
	checked := visitProcNetTCPListeners(func(listenPort int, inode string) {
		if listenPort == port {
			inodes[inode] = struct{}{}
		}
	})
	return inodes, checked
}

// ListenerPID returns the PID holding a TCP socket listening on port, and
// whether the kernel's socket tables could be read. checked=true with pid 0
// means nothing is listening or the holder's descriptors are not readable
// (another user's process); callers treat both as "no holder found" exactly as
// they would an empty `lsof -t` answer.
//
// The candidates are checked first, in order, and a hit returns without
// walking /proc. Only when none of them holds the socket are the remaining
// processes scanned.
func ListenerPID(port int, candidates ...int) (int, bool) {
	inodes, checked := ListeningSocketInodes(port)
	if !checked {
		return 0, false
	}
	if len(inodes) == 0 {
		return 0, true
	}
	tried := make(map[int]struct{}, len(candidates))
	for _, pid := range candidates {
		if pid <= 0 {
			continue
		}
		tried[pid] = struct{}{}
		if holds, _ := ProcessHoldsSocket(pid, inodes); holds {
			return pid, true
		}
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, true
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if _, ok := tried[pid]; ok {
			continue
		}
		if holds, _ := ProcessHoldsSocket(pid, inodes); holds {
			return pid, true
		}
	}
	return 0, true
}

// ListeningPortsByPID returns the TCP ports each of pids is listening on, and
// whether the kernel's socket tables could be read. Only the given processes'
// descriptors are read; PIDs with no listener (or unreadable descriptors) are
// absent from the map.
func ListeningPortsByPID(pids []int) (map[int][]int, bool) {
	out := map[int][]int{}
	inodePort := map[string]int{}
	checked := visitProcNetTCPListeners(func(port int, inode string) {
		inodePort[inode] = port
	})
	if !checked || len(inodePort) == 0 {
		return out, checked
	}
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		_ = visitProcessSocketInodes(pid, func(inode string) bool {
			if port, ok := inodePort[inode]; ok && !slices.Contains(out[pid], port) {
				out[pid] = append(out[pid], port)
			}
			return false
		})
	}
	return out, true
}

// ProcessHoldsSocket reports whether pid has an open descriptor on one of the
// given socket inodes. err is non-nil when the process's fd directory cannot
// be read (no /proc, the process exited, or it belongs to another user), in
// which case the answer is unknown rather than no.
func ProcessHoldsSocket(pid int, inodes map[string]struct{}) (bool, error) {
	holds := false
	err := visitProcessSocketInodes(pid, func(inode string) bool {
		_, holds = inodes[inode]
		return holds
	})
	return holds, err
}

// visitProcessSocketInodes calls visit with the inode of each socket pid has
// open, stopping early when visit returns true.
func visitProcessSocketInodes(pid int, visit func(inode string) bool) error {
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	fds, err := os.ReadDir(fdDir)
	if err != nil {
		return err
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
		if err != nil {
			continue
		}
		inode, ok := strings.CutPrefix(target, "socket:[")
		if !ok {
			continue
		}
		inode, ok = strings.CutSuffix(inode, "]")
		if !ok {
			continue
		}
		if visit(inode) {
			return nil
		}
	}
	return nil
}

// visitProcNetTCPListeners calls visit for every listening socket in the
// kernel's TCP tables and reports whether any table could be read.
func visitProcNetTCPListeners(visit func(port int, inode string)) bool {
	checked := false
	for _, path := range procNetTCPTables {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		checked = true
		_ = parseProcNetTCPListeners(file, visit)
		_ = file.Close()
	}
	return checked
}

// parseProcNetTCPListeners calls visit with the local port and socket inode of
// every LISTEN row in a /proc/net/tcp or /proc/net/tcp6 table. The header row
// and malformed rows are skipped.
func parseProcNetTCPListeners(r io.Reader, visit func(port int, inode string)) error {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) <= procNetTCPInodeField || fields[procNetTCPStateField] != procNetTCPListen {
			continue
		}
		_, portHex, ok := strings.Cut(fields[procNetTCPLocalField], ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseUint(portHex, 16, 16)
		if err != nil {
			continue
		}
		inode := fields[procNetTCPInodeField]
		if inode == "0" {
			continue
		}
		visit(int(port), inode)
	}
	return scanner.Err()
}
