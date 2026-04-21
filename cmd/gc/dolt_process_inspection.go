package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	processArgsPSTimeout = time.Second
	lsofCommandTimeout   = 2 * time.Second
)

type managedDoltProcessInspection struct {
	ManagedPID              int
	ManagedSource           string
	ManagedOwned            bool
	ManagedDeletedInodes    bool
	PortHolderPID           int
	PortHolderOwned         bool
	PortHolderDeletedInodes bool
}

func inspectManagedDoltProcess(cityPath, port string) (managedDoltProcessInspection, error) {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return managedDoltProcessInspection{}, err
	}
	info := managedDoltProcessInspection{}
	info.ManagedPID, info.ManagedSource = findManagedDoltPID(layout, port)
	if info.ManagedPID > 0 {
		info.ManagedOwned, info.ManagedDeletedInodes = inspectManagedDoltOwnership(info.ManagedPID, layout)
	}
	info.PortHolderPID = findPortHolderPID(port)
	if info.PortHolderPID > 0 {
		info.PortHolderOwned, info.PortHolderDeletedInodes = inspectManagedDoltOwnership(info.PortHolderPID, layout)
	}
	return info, nil
}

func findManagedDoltPID(layout managedDoltRuntimeLayout, port string) (int, string) {
	if pid := managedPIDFromPIDFile(layout.PIDFile); pid > 0 {
		return pid, "pid-file"
	}
	if pid := findPortHolderPID(port); pid > 0 {
		return pid, "port-holder"
	}
	if pid := managedPIDFromPSByConfig(layout.ConfigFile); pid > 0 {
		return pid, "config"
	}
	if pid := managedPIDFromPSByDataDir(layout.DataDir); pid > 0 {
		return pid, "data-dir"
	}
	return 0, ""
}

func managedPIDFromPIDFile(pidFile string) int {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || !pidAlive(pid) {
		_ = os.Remove(pidFile)
		return 0
	}
	return pid
}

func findPortHolderPID(port string) int {
	port = strings.TrimSpace(port)
	if port == "" {
		return 0
	}
	if pid, checked := findPortHolderPIDFromProc(port); checked {
		return pid
	}
	return findPortHolderPIDFromLsof(port)
}

func findPortHolderPIDFromLsof(port string) int {
	if _, err := exec.LookPath("lsof"); err != nil {
		return 0
	}
	out, err := lsofOutput("-nP", "-iTCP:"+port, "-sTCP:LISTEN", "-t")
	if err == nil {
		if pid := pidFromLsofPIDList(string(out)); pid > 0 {
			return pid
		}
	}

	out, err = lsofOutput("-nP", "-iTCP:"+port, "-sTCP:LISTEN")
	if err != nil {
		return 0
	}
	return pidFromPlainPortLsofOutput(string(out), port)
}

func pidFromLsofPIDList(output string) int {
	for _, field := range strings.Fields(output) {
		pid, err := strconv.Atoi(field)
		if err == nil && pidAlive(pid) {
			return pid
		}
	}
	return 0
}

func pidFromPlainPortLsofOutput(output, port string) int {
	portSuffix := ":" + strings.TrimSpace(port)
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, portSuffix) || !strings.Contains(line, "(LISTEN)") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err == nil && pidAlive(pid) {
			return pid
		}
	}
	return 0
}

func cwdFromFormattedLsofOutput(output string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "n") {
			path := normalizeLsofReportedPath(strings.TrimPrefix(line, "n"))
			if path != "" {
				return path, true
			}
		}
	}
	return "", false
}

func cwdFromPlainLsofOutput(output string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 9 || fields[3] != "cwd" {
			continue
		}
		path := plainLsofPath(fields)
		if path != "" {
			return path, true
		}
	}
	return "", false
}

func deletedDataInodeTargetsFromFormattedLsofOutput(output string) []string {
	var targets []string
	var currentName string
	currentDeleted := false
	flush := func() {
		if currentName != "" && currentDeleted {
			targets = append(targets, currentName)
		}
		currentName = ""
		currentDeleted = false
	}

	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'f':
			flush()
		case 'k':
			links := strings.TrimSpace(strings.TrimPrefix(line, "k"))
			if links == "0" {
				currentDeleted = true
			}
		case 'n':
			if currentName != "" {
				flush()
			}
			target := strings.TrimSpace(strings.TrimPrefix(line, "n"))
			if strings.Contains(target, " (deleted)") {
				currentDeleted = true
				target = strings.TrimSuffix(target, " (deleted)")
			}
			currentName = normalizeLsofReportedPath(target)
		}
	}
	flush()
	return targets
}

func deletedDataInodeTargetsFromPlainLsofOutput(output string) []string {
	var targets []string
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, " (deleted)") {
			continue
		}
		fields := strings.Fields(strings.TrimSuffix(line, " (deleted)"))
		target := plainLsofPath(fields)
		if target != "" {
			targets = append(targets, target)
		}
	}
	return targets
}

func plainLsofPath(fields []string) string {
	if len(fields) < 9 {
		return ""
	}
	return normalizeLsofReportedPath(strings.Join(fields[8:], " "))
}

func normalizeLsofReportedPath(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	switch {
	case path == "/private/tmp":
		return "/tmp"
	case strings.HasPrefix(path, "/private/tmp/"):
		return "/tmp/" + strings.TrimPrefix(path, "/private/tmp/")
	case path == "/private/var":
		return "/var"
	case strings.HasPrefix(path, "/private/var/"):
		return "/var/" + strings.TrimPrefix(path, "/private/var/")
	default:
		return path
	}
}

func processCWDFromLsof(pid int) (string, bool) {
	if _, err := exec.LookPath("lsof"); err != nil {
		return "", false
	}
	out, err := lsofOutput("-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn")
	if err == nil {
		if cwd, ok := cwdFromFormattedLsofOutput(string(out)); ok {
			return cwd, true
		}
	}
	out, err = lsofOutput("-a", "-p", strconv.Itoa(pid), "-d", "cwd")
	if err != nil {
		return "", false
	}
	return cwdFromPlainLsofOutput(string(out))
}

func benignManagedDeletedInodeTarget(target string) bool {
	clean := filepath.Clean(strings.TrimSpace(target))
	return strings.HasSuffix(clean, string(filepath.Separator)+".dolt"+string(filepath.Separator)+"noms"+string(filepath.Separator)+"LOCK")
}

func processHasDeletedDataInodes(pid int, dataDir string) bool {
	if pid <= 0 {
		return false
	}
	if cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd")); err == nil && strings.HasSuffix(cwd, " (deleted)") {
		return true
	}
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err == nil {
		for _, entry := range entries {
			target, readErr := os.Readlink(filepath.Join(fdDir, entry.Name()))
			if readErr != nil || !strings.Contains(target, " (deleted)") {
				continue
			}
			cleanTarget := strings.TrimSuffix(target, " (deleted)")
			if pathWithinOrSame(cleanTarget, dataDir) {
				if benignManagedDeletedInodeTarget(cleanTarget) {
					continue
				}
				return true
			}
		}
		return false
	}
	for _, target := range deletedDataInodeTargetsFromLsof(pid) {
		if pathWithinOrSame(target, dataDir) {
			if benignManagedDeletedInodeTarget(target) {
				continue
			}
			return true
		}
	}
	return false
}

func pathWithinOrSame(path, root string) bool {
	path = normalizePathForCompare(strings.TrimSpace(strings.TrimSuffix(path, " (deleted)")))
	root = normalizePathForCompare(strings.TrimSpace(root))
	if path == "" || root == "" {
		return false
	}
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

func deletedDataInodeTargetsFromLsof(pid int) []string {
	if _, err := exec.LookPath("lsof"); err != nil {
		return nil
	}
	targets := deletedDataInodeTargetsFromFormattedLsof(pid)
	if len(targets) > 0 {
		return targets
	}
	out, err := lsofOutput("-p", strconv.Itoa(pid))
	if err != nil {
		return nil
	}
	return deletedDataInodeTargetsFromPlainLsofOutput(string(out))
}

func deletedDataInodeTargetsFromFormattedLsof(pid int) []string {
	out, err := lsofOutput("-a", "-p", strconv.Itoa(pid), "+L1", "-Fnk")
	if err != nil {
		return nil
	}
	return deletedDataInodeTargetsFromFormattedLsofOutput(string(out))
}

func lsofOutput(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lsofCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", args...)
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
	return cmd.Output()
}

func processHasDeletedDataInodesWithin(pid int, dataDir string, timeout time.Duration) bool {
	if processHasDeletedDataInodes(pid, dataDir) {
		return true
	}
	if timeout <= 0 {
		return false
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if processHasDeletedDataInodes(pid, dataDir) {
			return true
		}
	}
	return false
}

func findPortHolderPIDFromProc(port string) (int, bool) {
	portNum, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0, true
	}
	inodes, checked := listeningSocketInodesFromProc(uint16(portNum))
	if !checked {
		return 0, false
	}
	if len(inodes) == 0 {
		return 0, true
	}
	return processWithSocketInodes(inodes), true
}

func listeningSocketInodesFromProc(port uint16) (map[string]struct{}, bool) {
	inodes := map[string]struct{}{}
	checked := false
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		checked = true
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			_, portHex, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			gotPort, err := strconv.ParseUint(portHex, 16, 16)
			if err != nil || uint16(gotPort) != port {
				continue
			}
			inodes[fields[9]] = struct{}{}
		}
	}
	return inodes, checked
}

func processWithSocketInodes(inodes map[string]struct{}) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !pidAlive(pid) {
			continue
		}
		fdDir := filepath.Join("/proc", entry.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
				continue
			}
			inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
			if _, ok := inodes[inode]; ok {
				return pid
			}
		}
	}
	return 0
}

func managedPIDFromPSByConfig(configFile string) int {
	for _, line := range doltPSLines() {
		if !strings.Contains(line, "dolt sql-server") {
			continue
		}
		if !strings.Contains(line, "--config") || !strings.Contains(line, configFile) {
			continue
		}
		if pid := psLinePID(line); pid > 0 {
			return pid
		}
	}
	return 0
}

func managedPIDFromPSByDataDir(dataDir string) int {
	base := filepath.Base(dataDir)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return 0
	}
	for _, line := range doltPSLines() {
		if !strings.Contains(line, "dolt sql-server") {
			continue
		}
		if !strings.Contains(line, "--data-dir") || !strings.Contains(line, base) {
			continue
		}
		if pid := psLinePID(line); pid > 0 {
			return pid
		}
	}
	return 0
}

func doltPSLines() []string {
	out, err := exec.Command("ps", "ax", "-o", "pid,args").Output()
	if err != nil {
		return nil
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	lines := make([]string, 0, 16)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

func psLinePID(line string) int {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return 0
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || !pidAlive(pid) {
		return 0
	}
	return pid
}

func inspectManagedDoltOwnership(pid int, layout managedDoltRuntimeLayout) (bool, bool) {
	if pid <= 0 {
		return false, false
	}

	stateDir := strings.TrimSpace(loadDoltRuntimeStateDataDir(layout.StateFile))
	if stateDir != "" && !samePath(stateDir, layout.DataDir) {
		return false, processHasDeletedDataInodes(pid, layout.DataDir)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		owned := managedDoltProcessOwnedWithStateDir(pid, layout, stateDir)
		deleted := processHasDeletedDataInodes(pid, layout.DataDir)
		if owned || deleted || !pidAlive(pid) || time.Now().After(deadline) {
			return owned, deleted
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func managedDoltProcessOwnedWithStateDir(pid int, layout managedDoltRuntimeLayout, stateDir string) bool {
	if pid <= 0 {
		return false
	}
	if stateDir != "" && !samePath(stateDir, layout.DataDir) {
		return false
	}

	procArgs, _ := processArgs(pid)
	switch {
	case containsProcessConfig(procArgs, layout.ConfigFile):
		return true
	case hasOtherProcessConfig(procArgs):
		return false
	case processDataDirMatches(procArgs, layout.DataDir):
		return true
	case processCWDMatches(pid, layout.DataDir):
		return true
	default:
		return false
	}
}

func loadDoltRuntimeStateDataDir(path string) string {
	state, err := readDoltRuntimeStateFile(path)
	if err != nil {
		return ""
	}
	return state.DataDir
}

func processArgs(pid int) (string, error) {
	if args, err := processArgsFromProc(pid); err == nil && args != "" {
		return args, nil
	}
	return processArgsFromPS(pid, processArgsPSTimeout)
}

func processArgsFromProc(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid %d", pid)
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return "", err
	}
	args := strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
	if args == "" {
		return "", fmt.Errorf("empty cmdline for pid %d", pid)
	}
	return args, nil
}

func processArgsFromPS(pid int, timeout time.Duration) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid %d", pid)
	}
	if timeout <= 0 {
		timeout = processArgsPSTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "args=")
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("ps args for pid %d: %w", pid, ctx.Err())
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func containsProcessConfig(args, configFile string) bool {
	return strings.Contains(args, "--config "+configFile) || strings.Contains(args, "--config="+configFile)
}

func hasOtherProcessConfig(args string) bool {
	return strings.Contains(args, "--config")
}

func processDataDirMatches(args, dataDir string) bool {
	index := strings.Index(args, "--data-dir")
	if index < 0 {
		return false
	}
	value := extractFlagValue(args[index:], "--data-dir")
	if value == "" {
		return false
	}
	return samePath(value, dataDir)
}

func extractFlagValue(args, flag string) string {
	fields := strings.Fields(args)
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if field == flag {
			if i+1 < len(fields) {
				return strings.TrimSpace(fields[i+1])
			}
			return ""
		}
		prefix := flag + "="
		if strings.HasPrefix(field, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(field, prefix))
		}
	}
	return ""
}

func processCWDMatches(pid int, dataDir string) bool {
	cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err == nil {
		return samePath(cwd, dataDir)
	}
	cwd, ok := processCWDFromLsof(pid)
	return ok && samePath(cwd, dataDir)
}

func doltProcessInspectionFields(info managedDoltProcessInspection) []string {
	return []string{
		fmt.Sprintf("managed_pid\t%d", info.ManagedPID),
		"managed_source\t" + info.ManagedSource,
		fmt.Sprintf("managed_owned\t%t", info.ManagedOwned),
		fmt.Sprintf("managed_deleted_inodes\t%t", info.ManagedDeletedInodes),
		fmt.Sprintf("port_holder_pid\t%d", info.PortHolderPID),
		fmt.Sprintf("port_holder_owned\t%t", info.PortHolderOwned),
		fmt.Sprintf("port_holder_deleted_inodes\t%t", info.PortHolderDeletedInodes),
	}
}
