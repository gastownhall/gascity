//go:build linux

package acp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// snapshotProcRoot copies the scanner-visible /proc files (environ, stat,
// comm) of the given pids into a fresh fake procfs root. The proctable scanner
// refuses the live /proc under go test (gastownhall/gascity#2839) so a test run
// on a host with a live fleet cannot reap real agents; a snapshot restricted to
// the test's own processes keeps that guarantee while still exercising the
// real root-detection rules against real process state. Pids that are already
// gone are omitted, so a rescan after a kill observes the kill.
func snapshotProcRoot(t *testing.T, pids ...int) string {
	t.Helper()
	root := t.TempDir()
	for _, pid := range pids {
		src := filepath.Join("/proc", strconv.Itoa(pid))
		files := make(map[string][]byte, 3)
		complete := true
		for _, name := range []string{"environ", "stat", "comm"} {
			data, err := os.ReadFile(filepath.Join(src, name))
			if err != nil {
				complete = false
				break
			}
			files[name] = data
		}
		if !complete {
			continue
		}
		dst := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dst, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dst, err)
		}
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(dst, name), data, 0o644); err != nil {
				t.Fatalf("write %s/%s: %v", dst, name, err)
			}
		}
	}
	return root
}

// scanSnapshot points the proctable scanner at a snapshot of pids for the
// duration of fn.
func scanSnapshot(t *testing.T, fn func(), pids ...int) {
	t.Helper()
	restore := proctable.SetScanRootForTesting(snapshotProcRoot(t, pids...))
	defer restore()
	fn()
}

// orphaningACPCommand wraps the fake ACP agent so that, before it execs, it
// starts a tool child in its own session (setsid) — the shape of an agent tool
// subprocess that escapes the agent's process group and outlives it. The tool
// child's pid is written to pidFile. Its stdio is detached so it cannot hold
// the agent's pipes open.
func orphaningACPCommand(pidFile string) string {
	return fmt.Sprintf("setsid sleep 300 </dev/null >/dev/null 2>&1 & echo $! > %q; %s", pidFile, fakeACPShellCommand())
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		t.Fatalf("pid file %q holds %q: %v", path, data, err)
	}
	return pid
}

// startOrphaningSession starts a fake ACP agent carrying sessionID on p and
// returns the agent root pid and its setsid'd tool child's pid.
func startOrphaningSession(t *testing.T, p *Provider, name, sessionID, city string) (agentPID, toolPID int) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "tool.pid")
	err := p.Start(context.Background(), name, runtime.Config{
		Command: orphaningACPCommand(pidFile),
		WorkDir: t.TempDir(),
		Env: map[string]string{
			"GC_SESSION_ID": sessionID,
			"GC_CITY_PATH":  city,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })

	p.mu.Lock()
	sc := p.conns[name]
	p.mu.Unlock()
	if sc == nil || sc.cmd == nil || sc.cmd.Process == nil {
		t.Fatalf("no live conn tracked for %q", name)
	}
	agentPID = sc.cmd.Process.Pid
	// The pid file is written before the shell execs the agent, and Start
	// returns only after the agent answered the handshake, so it exists now.
	toolPID = readPIDFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(toolPID, syscall.SIGKILL) })
	return agentPID, toolPID
}

func findOnly(t *testing.T, s runtime.ProcessTableScanner, sessionID string) []runtime.LiveRuntime {
	t.Helper()
	found, err := s.FindRuntimesBySessionID(sessionID)
	if err != nil {
		t.Fatalf("FindRuntimesBySessionID(%q): %v", sessionID, err)
	}
	return found
}

func TestFindRuntimesBySessionIDMarksLiveSessionTracked(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	sessionID := "sid-" + name
	city := t.TempDir()
	agentPID, toolPID := startOrphaningSession(t, p, name, sessionID, city)

	var found []runtime.LiveRuntime
	scanSnapshot(t, func() { found = findOnly(t, p, sessionID) }, agentPID, toolPID)

	// The tool child's parent is the live agent carrying the same session id,
	// so only the agent is a root.
	if len(found) != 1 {
		t.Fatalf("found = %+v, want exactly the agent root", found)
	}
	got := found[0]
	if got.PID != agentPID || !got.IsTracked || got.ProviderName != name || got.City != city {
		t.Fatalf("found[0] = %+v, want tracked pid %d provider name %q city %q", got, agentPID, name, city)
	}
}

func TestFindRuntimesBySessionIDIgnoresOtherSessions(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	agentPID, toolPID := startOrphaningSession(t, p, name, "sid-"+name, t.TempDir())

	var found []runtime.LiveRuntime
	scanSnapshot(t, func() { found = findOnly(t, p, "sid-someone-else") }, agentPID, toolPID)
	if len(found) != 0 {
		t.Fatalf("found = %+v, want none for a different session id", found)
	}
}

// A supervisor death loses the in-process conn table: a fresh Provider on the
// same state directory must report the surviving agent and, once the agent is
// terminated, its escaped tool child as untracked roots, and TerminateRuntime
// must kill both.
func TestProviderRestartReportsAndReapsOrphanedRuntimes(t *testing.T) {
	p1 := newTestProvider(t)
	name := testName()
	sessionID := "sid-" + name
	city := t.TempDir()
	agentPID, toolPID := startOrphaningSession(t, p1, name, sessionID, city)

	p2 := NewProviderWithDir(p1.dir, p1.cfg)

	var found []runtime.LiveRuntime
	scanSnapshot(t, func() { found = findOnly(t, p2, sessionID) }, agentPID, toolPID)
	if len(found) != 1 || found[0].PID != agentPID || found[0].IsTracked || found[0].ProviderName != "" {
		t.Fatalf("after restart found = %+v, want untracked agent root pid %d", found, agentPID)
	}
	if err := p2.TerminateRuntime(found[0]); err != nil {
		t.Fatalf("TerminateRuntime(agent): %v", err)
	}

	// The agent is gone (or a zombie awaiting p1's reaper); its setsid'd tool
	// child has been reparented away from it and is now its own root.
	scanSnapshot(t, func() { found = findOnly(t, p2, sessionID) }, agentPID, toolPID)
	if len(found) != 1 || found[0].PID != toolPID || found[0].IsTracked {
		t.Fatalf("after agent kill found = %+v, want untracked tool root pid %d", found, toolPID)
	}
	if err := p2.TerminateRuntime(found[0]); err != nil {
		t.Fatalf("TerminateRuntime(tool): %v", err)
	}

	scanSnapshot(t, func() { found = findOnly(t, p2, sessionID) }, agentPID, toolPID)
	if len(found) != 0 {
		t.Fatalf("after reaping found = %+v, want no roots", found)
	}
}

// The seam-backed provider is what production wraps; it must expose the
// scanner or callers type-asserting it see no capability.
func TestSeamBackedProviderForwardsProcessTableScanner(t *testing.T) {
	raw := newTestProvider(t)
	name := testName()
	sessionID := "sid-" + name
	agentPID, toolPID := startOrphaningSession(t, raw, name, sessionID, t.TempDir())

	var sp runtime.Provider = seamBack(raw)
	scanner, ok := sp.(runtime.ProcessTableScanner)
	if !ok {
		t.Fatal("seam-backed ACP provider does not implement runtime.ProcessTableScanner")
	}
	var found []runtime.LiveRuntime
	scanSnapshot(t, func() { found = findOnly(t, scanner, sessionID) }, agentPID, toolPID)
	if len(found) != 1 || !found[0].IsTracked || found[0].ProviderName != name {
		t.Fatalf("found = %+v, want the tracked agent root", found)
	}
}

func TestTerminateRuntimeRefusesInitAndInvalidPIDs(t *testing.T) {
	p := newTestProvider(t)
	for _, pid := range []int{-1, 0, 1} {
		if err := p.TerminateRuntime(runtime.LiveRuntime{PID: pid, SessionID: "sid"}); err == nil {
			t.Errorf("TerminateRuntime(PID %d) = nil, want refusal", pid)
		}
	}
}
