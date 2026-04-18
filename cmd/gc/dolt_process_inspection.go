package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	if strings.TrimSpace(port) == "" {
		return 0
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		return 0
	}
	out, err := exec.Command("lsof", "-i", ":"+strings.TrimSpace(port), "-sTCP:LISTEN", "-t").Output()
	if err != nil {
		return 0
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return 0
	}
	fields := strings.Fields(line)
	pid, err := strconv.Atoi(fields[0])
	if err != nil || !pidAlive(pid) {
		return 0
	}
	return pid
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
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
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
	if err != nil {
		return false
	}
	return samePath(cwd, dataDir)
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
	root := filepath.Clean(dataDir) + string(filepath.Separator)
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err == nil {
		for _, entry := range entries {
			target, readErr := os.Readlink(filepath.Join(fdDir, entry.Name()))
			if readErr != nil || !strings.Contains(target, " (deleted)") {
				continue
			}
			cleanTarget := strings.TrimSuffix(target, " (deleted)")
			if samePath(cleanTarget, dataDir) || strings.HasPrefix(cleanTarget, root) {
				if benignManagedDeletedInodeTarget(cleanTarget) {
					continue
				}
				return true
			}
		}
		return false
	}
	if _, err := exec.LookPath("lsof"); err == nil {
		out, lsofErr := exec.Command("lsof", "-p", strconv.Itoa(pid)).Output()
		if lsofErr == nil {
			cleanDataDir := filepath.Clean(dataDir)
			for _, line := range strings.Split(string(out), "\n") {
				if !strings.Contains(line, " (deleted)") || !strings.Contains(line, cleanDataDir) {
					continue
				}
				idx := strings.Index(line, cleanDataDir)
				if idx >= 0 {
					target := strings.TrimSpace(strings.TrimSuffix(line[idx:], " (deleted)"))
					if benignManagedDeletedInodeTarget(target) {
						continue
					}
				}
				return true
			}
		}
	}
	return false
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
