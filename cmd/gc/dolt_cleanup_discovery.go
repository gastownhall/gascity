package main

import (
	"context"
	"os"
	"os/exec"
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
const procEnumerationTimeout = 5 * time.Second

// psEnumerationTimeout caps the wall-clock budget for the full-system ps -ax
// invocation on Darwin/macOS. ps -ax can take 2–5 s on a busy machine; it
// needs a much larger budget than the per-PID procEnumerationTimeout.
const psEnumerationTimeout = 30 * time.Second

// discoverDoltProcesses finds live `dolt sql-server` processes and reports
// their argv and listening ports. Linux uses /proc for argv, ports, RSS, and
// start ticks. Hosts without /proc (including Darwin/macOS) fall back to ps for
// process enumeration and lsof for best-effort listening ports.
func discoverDoltProcesses() ([]DoltProcInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return discoverDoltProcessesFromPS()
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

func discoverDoltProcessesFromPS() ([]DoltProcInfo, error) {
	lines, err := psLStartCommandLines()
	if err != nil {
		return nil, err
	}
	pidPorts := portsByPID()
	var out []DoltProcInfo
	for _, line := range lines {
		proc, ok := parseDoltPSLine(line, pidPorts)
		if !ok {
			continue
		}
		out = append(out, proc)
	}
	return out, nil
}

func discoverActiveTestRoots(homeDir, tempDir string) []string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return discoverActiveTestRootsFromPS(homeDir, tempDir)
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

func discoverActiveTestRootsFromPS(homeDir, tempDir string) []string {
	lines, err := psLStartCommandLines()
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	var roots []string
	for _, line := range lines {
		argv, ok := argvFromPSLine(line)
		if !ok || looksLikeDoltSQLServer(argv) {
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

// readProcStartIdentity returns `ps -p <pid> -o lstart=` output (the portable
// per-process start timestamp) for the given PID. Empty string on any error
// or non-zero exit — callers treat empty as "identity unavailable" and fall
// back to less-strict verification.
func readProcStartIdentity(pid int) string {
	if pid <= 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), procEnumerationTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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

func psLStartCommandLines() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psEnumerationTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-ax", "-o", "pid=,rss=,lstart=,command=")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, 64)
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func parseDoltPSLine(line string, pidPorts map[int][]int) (DoltProcInfo, bool) {
	fields, command := consumeLeadingFields(line, 7)
	if len(fields) != 7 || command == "" {
		return DoltProcInfo{}, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return DoltProcInfo{}, false
	}
	rssKB, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || rssKB < 0 {
		rssKB = 0
	}
	argv := parseDoltPSCommandLine(command)
	if !looksLikeDoltSQLServer(argv) {
		return DoltProcInfo{}, false
	}
	return DoltProcInfo{
		PID:           pid,
		Argv:          argv,
		Ports:         pidPorts[pid],
		RSSBytes:      rssKB * 1024,
		StartIdentity: strings.Join(fields[2:7], " "),
	}, true
}

func argvFromPSLine(line string) ([]string, bool) {
	_, command := consumeLeadingFields(line, 7)
	if command == "" {
		return nil, false
	}
	return parseDoltPSCommandLine(command), true
}

func consumeLeadingFields(s string, n int) ([]string, string) {
	rest := strings.TrimSpace(s)
	fields := make([]string, 0, n)
	for len(fields) < n {
		if rest == "" {
			return fields, ""
		}
		i := strings.IndexFunc(rest, func(r rune) bool { return r == ' ' || r == '\t' })
		if i < 0 {
			fields = append(fields, rest)
			return fields, ""
		}
		fields = append(fields, rest[:i])
		rest = strings.TrimLeft(rest[i:], " \t")
	}
	return fields, rest
}

func parseDoltPSCommandLine(command string) []string {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}
	tail, ok := doltSQLServerPSTail(command)
	if !ok {
		return strings.Fields(command)
	}
	fields := strings.Fields(tail)
	if len(fields) < 2 {
		return fields
	}
	argv := []string{fields[0], fields[1]}
	if cfg, ok := configPathFromPSCommandLine(tail); ok {
		argv = append(argv, "--config", cfg)
	}
	return argv
}

func doltSQLServerPSTail(command string) (string, bool) {
	const marker = "dolt sql-server"
	for start := 0; start < len(command); {
		i := strings.Index(command[start:], marker)
		if i < 0 {
			return "", false
		}
		i += start
		if i == 0 || command[i-1] == filepath.Separator || command[i-1] == '/' || command[i-1] == '\\' {
			return command[i:], true
		}
		start = i + len("dolt")
	}
	return "", false
}

func configPathFromPSCommandLine(command string) (string, bool) {
	spans := commandFieldSpans(command)
	for i, span := range spans {
		field := command[span.start:span.end]
		if strings.HasPrefix(field, "--config=") {
			value := strings.TrimSpace(strings.TrimPrefix(field, "--config="))
			if value == "" {
				return "", false
			}
			return trimPSConfigValue(value), true
		}
		if field == "--config" {
			if i+1 >= len(spans) {
				return "", false
			}
			value := strings.TrimSpace(command[spans[i+1].start:])
			if value == "" {
				return "", false
			}
			return trimPSConfigValue(value), true
		}
	}
	return "", false
}

type commandFieldSpan struct {
	start int
	end   int
}

func commandFieldSpans(s string) []commandFieldSpan {
	var spans []commandFieldSpan
	inField := false
	start := 0
	for i, r := range s {
		if r == ' ' || r == '\t' {
			if inField {
				spans = append(spans, commandFieldSpan{start: start, end: i})
				inField = false
			}
			continue
		}
		if !inField {
			start = i
			inField = true
		}
	}
	if inField {
		spans = append(spans, commandFieldSpan{start: start, end: len(s)})
	}
	return spans
}

func trimPSConfigValue(value string) string {
	for _, sep := range []string{" --", "\t--"} {
		if i := strings.Index(value, sep); i >= 0 {
			value = value[:i]
		}
	}
	return strings.TrimSpace(value)
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
	listenInodes, checkedProcNet := listenInodesByPortChecked()
	if len(listenInodes) == 0 {
		if checkedProcNet {
			return out
		}
		return portsByPIDFromLsof()
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

func portsByPIDFromLsof() map[int][]int {
	out := map[int][]int{}
	if _, err := exec.LookPath("lsof"); err != nil {
		return out
	}
	data, err := lsofOutput("-nP", "-iTCP", "-sTCP:LISTEN")
	if err != nil {
		return out
	}
	return parseListeningPortsByPIDFromLsof(string(data))
}

func parseListeningPortsByPIDFromLsof(output string) map[int][]int {
	out := map[int][]int{}
	for _, line := range strings.Split(output, "\n") {
		pid, port, ok := parseListeningPortLsofLine(line)
		if !ok {
			continue
		}
		out[pid] = appendUniqueInt(out[pid], port)
	}
	return out
}

func parseListeningPortLsofLine(line string) (int, int, bool) {
	if !strings.Contains(line, "(LISTEN)") {
		return 0, 0, false
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, 0, false
	}
	pid, err := strconv.Atoi(fields[1])
	if err != nil || pid <= 0 {
		return 0, 0, false
	}
	listenIdx := strings.Index(line, "(LISTEN)")
	beforeListen := strings.TrimSpace(line[:listenIdx])
	colon := strings.LastIndex(beforeListen, ":")
	if colon < 0 || colon+1 >= len(beforeListen) {
		return 0, 0, false
	}
	portText := beforeListen[colon+1:]
	portText = strings.TrimRightFunc(portText, func(r rune) bool { return r < '0' || r > '9' })
	port, err := strconv.Atoi(portText)
	if err != nil || !validDoltPort(port) {
		return 0, 0, false
	}
	return pid, port, true
}

func listenInodesByPortChecked() (map[int][]string, bool) {
	out := map[int][]string{}
	checked := false
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		checked = true
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
	return out, checked
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
