// Package dolttest is a lean, test-only reaper for orphaned
// `dolt sql-server` processes spawned by the real-dolt integration and
// acceptance suites. It fills the role the cmd/gc leak-guard
// (doltLeakGuardedTestingM) plays for cmd/gc tests, which cannot be imported
// because it lives in package main. See issue #3640.
//
// Discovery is Linux-only via /proc and a no-op elsewhere, mirroring the
// scan test/integration already uses (readProcessSnapshot); CI runs on Linux.
// The matching, path, and run-dir logic is platform-neutral and is exercised
// on every platform; only the process discovery and signalling primitives are
// per-platform. See dolttest_unix.go and dolttest_windows.go.
package dolttest

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/doltorphan"
)

// reapGracePeriod is how long a reaped process is given to act on the
// cooperative stop request before the forced kill.
const reapGracePeriod = 250 * time.Millisecond

// reap sends SIGTERM, then SIGKILL to survivors, to every `dolt sql-server`
// whose --config or --data-dir path is within root. Both flags are checked
// because a bare `dolt sql-server --data-dir X` (no --config at all, the
// shape the real-dolt integration suite uses) has no --config path for
// doltConfigPath to find.
func reap(root string) {
	if root == "" {
		return
	}
	root = filepath.Clean(root)
	var pids []int
	for pid, cmd := range scanDoltSQLServers() {
		if pathWithin(root, doltConfigPath(cmd)) || pathWithin(root, doltDataDirPath(cmd)) {
			pids = append(pids, pid)
		}
	}
	reapPIDs(pids)
}

// SweepStale reaps `dolt sql-server` orphans left by *prior* runs whose per-run
// temp dir is named "<parent>/<prefix><pid>-<rand>". For each dolt process whose
// --config or --data-dir path lives under such a run dir (see reap's doc for
// why both are checked), the owner pid is parsed from the run-dir name and the
// process is reaped only if that pid is no longer alive — sparing live
// concurrent runs. Because SIGKILL is uncatchable in-process, a next-run
// startup sweep is the only reaper for those orphans; call this at suite
// startup, before the run spawns any dolt of its own.
func SweepStale(parent, prefix string) {
	if parent == "" || prefix == "" {
		return
	}
	parent = filepath.Clean(parent)
	var stale []int
	for pid, cmd := range scanDoltSQLServers() {
		runDir, ok := runDirUnder(doltConfigPath(cmd), parent, prefix)
		if !ok {
			runDir, ok = runDirUnder(doltDataDirPath(cmd), parent, prefix)
		}
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

// SweepOrphanStoreDirs runs the symptom-based fallback sweep
// (internal/doltorphan.Sweep) over root, removing stray dolt store
// directories regardless of what created them: age > 60m, a .dolt marker
// present, and not held open by any live process. It composes with, but
// does not replace, SweepStale and Guard above — those reap live or
// recently-dead *processes* via a specific run-root naming convention;
// this catches the *directory* left behind in cases those miss (a killed
// test binary whose pid was later reused, an ad-hoc dolt invocation
// outside any tracked run-root, etc). Call at suite startup, alongside
// SweepStale, before the run spawns any dolt of its own.
func SweepOrphanStoreDirs(root string) {
	result := doltorphan.Sweep(doltorphan.SweepConfig{Root: root})
	for _, dir := range result.Removed {
		fmt.Fprintf(os.Stderr, "dolttest: startup sweep removed orphaned dolt store dir %s\n", dir)
	}
	for _, err := range result.Errors {
		fmt.Fprintf(os.Stderr, "dolttest: startup sweep error: %v\n", err)
	}
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
			reraise(s)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
		reap(runDir)
	}
}

func looksLikeDoltSQLServer(fields []string) bool {
	for i := 0; i+1 < len(fields); i++ {
		if filepath.Base(fields[i]) == "dolt" && fields[i+1] == "sql-server" {
			return true
		}
	}
	return false
}

// flagValue returns the value of a space-separated ("name value") or
// equals-form ("name=value") occurrence of name in cmd's argv, or "" if
// name isn't present.
func flagValue(cmd, name string) string {
	fields := strings.Fields(cmd)
	prefix := name + "="
	for i, f := range fields {
		if f == name {
			if i+1 < len(fields) {
				return fields[i+1]
			}
			return ""
		}
		if strings.HasPrefix(f, prefix) {
			return strings.TrimPrefix(f, prefix)
		}
	}
	return ""
}

func doltConfigPath(cmd string) string {
	return flagValue(cmd, "--config")
}

// doltDataDirPath returns cmd's --data-dir value, if present. Checked
// alongside doltConfigPath throughout this file because a bare
// `dolt sql-server --data-dir X` (no --config at all) is the exact shape
// the real-dolt integration suite uses, and doltConfigPath alone can never
// match it.
func doltDataDirPath(cmd string) string {
	return flagValue(cmd, "--data-dir")
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
	return platformPIDAlive(pid)
}

// reapPIDs stops every pid, giving each a cooperative window before the
// forced kill. Before escalating, each pid is re-confirmed to still be a
// dolt sql-server: a process that exited during the grace period may have had
// its pid reused by something unrelated (likelier on a loaded host).
func reapPIDs(pids []int) {
	if len(pids) == 0 {
		return
	}
	for _, pid := range pids {
		requestStop(pid)
	}
	time.Sleep(reapGracePeriod)
	for _, pid := range pids {
		if pidIsDoltSQLServer(pid) {
			forceKill(pid)
		}
	}
}
