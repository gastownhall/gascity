//go:build unix

package dolttest

import (
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// scanDoltSQLServers returns pid->cmdline for every running `dolt sql-server`.
// Linux-only via /proc; returns nil where /proc is unavailable.
func scanDoltSQLServers() map[int]string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make(map[int]string)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmd, ok := readProcCmdline(pid)
		if !ok || !looksLikeDoltSQLServer(strings.Fields(cmd)) {
			continue
		}
		out[pid] = cmd
	}
	return out
}

// readProcCmdline returns the space-joined argv of pid from /proc.
func readProcCmdline(pid int) (string, bool) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil || len(raw) == 0 {
		return "", false
	}
	cmd := strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " "))
	if cmd == "" {
		return "", false
	}
	return cmd, true
}

// pidIsDoltSQLServer reports whether pid is, right now, a dolt sql-server.
func pidIsDoltSQLServer(pid int) bool {
	cmd, ok := readProcCmdline(pid)
	return ok && looksLikeDoltSQLServer(strings.Fields(cmd))
}

// platformPIDAlive probes existence with signal 0, which delivers nothing.
// EPERM means the process exists but is not ours to signal — treat as alive so
// it is not reaped.
func platformPIDAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func requestStop(pid int) {
	_ = syscall.Kill(pid, syscall.SIGTERM)
}

func forceKill(pid int) {
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// reraise restores the default disposition for s and delivers it to this
// process, so an interrupted run terminates with the status the signal names
// after the reaper has run.
func reraise(s os.Signal) {
	ss, ok := s.(syscall.Signal)
	if !ok {
		return
	}
	signal.Reset(ss)
	_ = syscall.Kill(os.Getpid(), ss)
}
