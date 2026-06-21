// Package dolttest is a lean, test-only reaper for orphaned
// `dolt sql-server` processes spawned by the real-dolt integration and
// acceptance suites. It fills the role the cmd/gc leak-guard
// (doltLeakGuardedTestingM) plays for cmd/gc tests, which cannot be imported
// because it lives in package main. See issue #3640.
//
// Discovery is Linux-only via /proc and a no-op elsewhere, mirroring the
// scan test/integration already uses (readProcessSnapshot); CI runs on Linux.
package dolttest

import (
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// reap sends SIGTERM, then SIGKILL to survivors, to every `dolt sql-server`
// whose --config path is within root.
func reap(root string) {
	if root == "" {
		return
	}
	root = filepath.Clean(root)
	var pids []int
	for pid, cmd := range scanDoltSQLServers() {
		if pathWithin(root, doltConfigPath(cmd)) {
			pids = append(pids, pid)
		}
	}
	reapPIDs(pids)
}

// SweepStale reaps `dolt sql-server` orphans left by *prior* runs whose per-run
// temp dir is named "<parent>/<prefix><pid>-<rand>". For each dolt process whose
// --config path lives under such a run dir, the owner pid is parsed from the
// run-dir name and the process is reaped only if that pid is no longer alive —
// sparing live concurrent runs. Because SIGKILL is uncatchable in-process, a
// next-run startup sweep is the only reaper for those orphans; call this at
// suite startup, before the run spawns any dolt of its own.
func SweepStale(parent, prefix string) {
	if parent == "" || prefix == "" {
		return
	}
	parent = filepath.Clean(parent)
	var stale []int
	for pid, cmd := range scanDoltSQLServers() {
		runDir, ok := runDirUnder(doltConfigPath(cmd), parent, prefix)
		if !ok {
			continue
		}
		owner, ok := ownerPIDFromRunDir(filepath.Base(runDir), prefix)
		if !ok || pidAlive(owner) {
			continue
		}
		stale = append(stale, pid)
	}
	reapPIDs(stale)
}

// Guard installs a signal handler for SIGINT, SIGTERM, and SIGQUIT (the signal
// `go test -timeout` raises) that reaps dolt under runDir then re-raises, so an
// interrupted or timed-out run does not leak. The returned stop removes the
// handler and does a final reap; call it after m.Run().
func Guard(runDir string) (stop func()) {
	sig := make(chan os.Signal, 2)
	done := make(chan struct{})
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		select {
		case s := <-sig:
			reap(runDir)
			signal.Stop(sig)
			if ss, ok := s.(syscall.Signal); ok {
				signal.Reset(ss)
				_ = syscall.Kill(os.Getpid(), ss)
			}
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
		reap(runDir)
	}
}

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

func looksLikeDoltSQLServer(fields []string) bool {
	for i := 0; i+1 < len(fields); i++ {
		if filepath.Base(fields[i]) == "dolt" && fields[i+1] == "sql-server" {
			return true
		}
	}
	return false
}

func doltConfigPath(cmd string) string {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		if f == "--config" {
			if i+1 < len(fields) {
				return fields[i+1]
			}
			return ""
		}
		if strings.HasPrefix(f, "--config=") {
			return strings.TrimPrefix(f, "--config=")
		}
	}
	return ""
}

func pathWithin(root, p string) bool {
	if root == "" || p == "" {
		return false
	}
	root = filepath.Clean(root)
	if root == "." || root == string(os.PathSeparator) {
		return false
	}
	p = filepath.Clean(p)
	return p == root || strings.HasPrefix(p, root+string(os.PathSeparator))
}

// runDirUnder returns the "<parent>/<prefix>..." run-dir ancestor of configPath.
func runDirUnder(configPath, parent, prefix string) (string, bool) {
	if configPath == "" {
		return "", false
	}
	configPath = filepath.Clean(configPath)
	parent = filepath.Clean(parent)
	leader := parent + string(os.PathSeparator)
	if !strings.HasPrefix(configPath, leader) {
		return "", false
	}
	seg := strings.TrimPrefix(configPath, leader)
	if i := strings.IndexByte(seg, os.PathSeparator); i >= 0 {
		seg = seg[:i]
	}
	if !strings.HasPrefix(seg, prefix) {
		return "", false
	}
	return filepath.Join(parent, seg), true
}

// ownerPIDFromRunDir parses <pid> from a "<prefix><pid>-<rand>" dir name.
func ownerPIDFromRunDir(name, prefix string) (int, bool) {
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	tok := strings.TrimPrefix(name, prefix)
	if i := strings.IndexByte(tok, '-'); i >= 0 {
		tok = tok[:i]
	}
	pid, err := strconv.Atoi(tok)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Signal 0 probes existence without delivering a signal; EPERM means the
	// process exists but is not ours to signal — treat as alive (don't reap).
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func reapPIDs(pids []int) {
	if len(pids) == 0 {
		return
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	time.Sleep(250 * time.Millisecond)
	for _, pid := range pids {
		// Re-confirm the pid is STILL a dolt sql-server before escalating to
		// SIGKILL: a process that exited during the grace period may have had
		// its pid reused by something unrelated (likelier on a loaded host).
		if pidIsDoltSQLServer(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}
