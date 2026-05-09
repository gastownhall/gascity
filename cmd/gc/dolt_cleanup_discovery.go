package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// loadRigDoltPorts reads each rig's <rigRoot>/.beads/dolt-server.port file and
// returns a port→rig-name map for the reaper's protection check. Missing or
// malformed files are silently skipped — they just won't contribute to the
// protected set, and the reaper will fall back to its config-path filter.
//
// If two rigs claim the same port (pathological — operator misconfiguration),
// the later-listed rig wins. The function is still safe: any port match
// protects, regardless of which rig name is attributed.
func loadRigDoltPorts(rigs []resolverRig, fs fsys.FS) map[int]string {
	out := map[int]string{}
	for _, rig := range rigs {
		path := filepath.Join(rig.Path, ".beads", "dolt-server.port")
		data, err := fs.ReadFile(path)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			continue
		}
		port, err := strconv.Atoi(text)
		if err != nil || !validDoltPort(port) {
			continue
		}
		out[port] = rig.Name
	}
	return out
}

// procEnumerationTimeout caps the per-PID I/O during /proc walks so a stuck
// kernel thread or hung process can't make the reaper hang.
const procEnumerationTimeout = 2 * time.Second

// discoverDoltProcesses walks /proc to find live `dolt sql-server` processes
// and reports their argv and listening ports. Returns nil + nil on hosts
// without /proc (the reaper degrades to "no candidates found", which is
// indistinguishable from a healthy host with no orphans).
//
// The function is intentionally Linux-specific. macOS/BSD hosts would need
// `ps -ax -o pid,command` and `lsof -i -P -nFn` — left as future work since
// the architect's spec scopes this to Linux test infrastructure.
func discoverDoltProcesses() ([]DoltProcInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, nil
	}

	pidPorts := portsByPID()

	var out []DoltProcInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		argv, ok := readDoltSQLServerArgv(pid)
		if !ok {
			continue
		}
		out = append(out, DoltProcInfo{
			PID:            pid,
			Argv:           argv,
			Ports:          pidPorts[pid],
			RSSBytes:       readProcRSSBytes(pid),
			StartTimeTicks: readProcStartTimeTicks(pid),
		})
	}
	return out, nil
}

func discoverActiveTestRoots(homeDir, tempDir string) []string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	var roots []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := readWithTimeout(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
		if err != nil || len(data) == 0 {
			continue
		}
		argv := splitCmdline(data)
		if looksLikeDoltSQLServer(argv) {
			continue
		}
		for _, arg := range argv {
			root, ok := activeTestRootFromPath(arg, homeDir, tempDir)
			if !ok {
				continue
			}
			if _, exists := seen[root]; exists {
				continue
			}
			seen[root] = struct{}{}
			roots = append(roots, root)
		}
	}
	return roots
}

func activeTestRootFromPath(path, homeDir, tempDir string) (string, bool) {
	clean := filepath.Clean(path)
	for _, root := range []string{"/tmp", tempDir} {
		if testRoot, ok := activeTestRootUnder(clean, root, testConfigPathPrefixes()); ok {
			return testRoot, true
		}
	}
	if homeDir == "" {
		return "", false
	}
	return activeTestRootUnder(clean, filepath.Join(homeDir, ".gotmp"), []string{"Test"})
}

func activeTestRootUnder(cleanPath, root string, prefixes []string) (string, bool) {
	if root == "" {
		return "", false
	}
	cleanRoot := filepath.Clean(root)
	if cleanRoot == "." || cleanRoot == string(filepath.Separator) {
		return "", false
	}
	rootPrefix := cleanRoot + string(filepath.Separator)
	if !strings.HasPrefix(cleanPath, rootPrefix) {
		return "", false
	}
	child := strings.TrimPrefix(cleanPath, rootPrefix)
	for _, prefix := range prefixes {
		if !strings.HasPrefix(child, prefix) {
			continue
		}
		nextSep := strings.IndexRune(child, filepath.Separator)
		if nextSep < 0 {
			return filepath.Join(cleanRoot, child), true
		}
		return filepath.Join(cleanRoot, child[:nextSep]), true
	}
	return "", false
}

func readProcStartTimeTicks(pid int) uint64 {
	data, err := readWithTimeout(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	return parseProcStartTimeTicks(data)
}

func parseProcStartTimeTicks(data []byte) uint64 {
	text := string(data)
	closeParen := strings.LastIndex(text, ")")
	if closeParen < 0 {
		return 0
	}
	fields := strings.Fields(text[closeParen+1:])
	if len(fields) <= 19 {
		return 0
	}
	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	return startTime
}

func readProcRSSBytes(pid int) int64 {
	data, err := readWithTimeout(filepath.Join("/proc", strconv.Itoa(pid), "statm"))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || pages <= 0 {
		return 0
	}
	return pages * int64(os.Getpagesize())
}

// readDoltSQLServerArgv reads /proc/<pid>/cmdline and returns the NUL-split
// argv if and only if the process looks like `dolt sql-server`. The boolean
// is false for any non-dolt process so callers can skip cheaply.
func readDoltSQLServerArgv(pid int) ([]string, bool) {
	data, err := readWithTimeout(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil || len(data) == 0 {
		return nil, false
	}
	argv := splitCmdline(data)
	if !looksLikeDoltSQLServer(argv) {
		return nil, false
	}
	return argv, true
}

// splitCmdline parses a /proc/<pid>/cmdline blob (NUL-separated argv with
// trailing NUL) into a string slice. Empty trailing element is dropped.
func splitCmdline(data []byte) []string {
	parts := strings.Split(string(data), "\x00")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// looksLikeDoltSQLServer reports whether argv invokes `dolt sql-server`. The
// match is intentionally permissive: argv[0] basename must be "dolt" (allowing
// /usr/local/bin/dolt or just "dolt") and argv[1] must be "sql-server".
func looksLikeDoltSQLServer(argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	if filepath.Base(argv[0]) != "dolt" {
		return false
	}
	return argv[1] == "sql-server"
}

// portsByPID returns a map from PID to its listening TCP ports by reading
// /proc/net/tcp{,6} and cross-referencing /proc/<pid>/fd/ socket inodes. On
// hosts without /proc/net the map is empty (the reaper falls back to argv-
// only protection).
func portsByPID() map[int][]int {
	out := map[int][]int{}
	listenInodes := listenInodesByPort()
	if len(listenInodes) == 0 {
		return out
	}
	inodeToPort := map[string]int{}
	for port, inodes := range listenInodes {
		for _, inode := range inodes {
			inodeToPort[inode] = port
		}
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			if !strings.HasPrefix(target, "socket:[") {
				continue
			}
			inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
			if port, ok := inodeToPort[inode]; ok {
				out[pid] = appendUniqueInt(out[pid], port)
			}
		}
	}
	return out
}

// listenInodesByPort reads /proc/net/tcp{,6} and returns a port → []inode map
// for sockets in LISTEN state (TCP state 0A). Each inode is a unique kernel
// socket identifier that appears as the target of a /proc/<pid>/fd/<n>
// symlink ("socket:[<inode>]"); cross-referencing those gives port→pid.
func listenInodesByPort() map[int][]string {
	out := map[int][]string{}
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			_, portHex, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			port, err := strconv.ParseUint(portHex, 16, 16)
			if err != nil {
				continue
			}
			out[int(port)] = appendUniqueString(out[int(port)], fields[9])
		}
	}
	return out
}

func appendUniqueInt(s []int, v int) []int {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// readWithTimeout reads a file with a deadline so a stuck /proc entry (a
// kernel thread that's blocked) can't hang the discovery walk.
func readWithTimeout(path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), procEnumerationTimeout)
	defer cancel()
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := os.ReadFile(path)
		ch <- result{data, err}
	}()
	select {
	case r := <-ch:
		return r.data, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// killProcess sends a signal to a PID. Wraps syscall.Kill so the reaper can
// inject a no-op for tests. Errors are returned verbatim; ESRCH (no such
// process) is the caller's responsibility to interpret as "already gone".
func killProcess(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}
