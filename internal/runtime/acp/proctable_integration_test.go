//go:build integration && linux

package acp

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// childPIDs returns pid's direct children from procfs.
func childPIDs(t *testing.T, pid int) []int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "task", strconv.Itoa(pid), "children"))
	if err != nil {
		t.Fatalf("read children of %d: %v", pid, err)
	}
	var out []int
	for _, field := range strings.Fields(string(data)) {
		child, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("parse child pid %q: %v", field, err)
		}
		out = append(out, child)
	}
	return out
}

// TestACPOrphanReapedAfterProviderRestart runs the real fakeacp agent through
// the production (seam-backed) provider, drops the provider's in-process state
// the way a supervisor death does, and proves a fresh provider on the same
// state directory reports the surviving runtime as an untracked orphan and
// terminates its whole process group.
func TestACPOrphanReapedAfterProviderRestart(t *testing.T) {
	var fixture acpConformanceFixture
	if err := prepareACPConformanceFixture(t, &fixture); err != nil {
		t.Fatal(err)
	}
	raw := NewProviderWithDir(fixture.dir, Config{})
	name := testName()
	sessionID := "sid-" + name
	city := t.TempDir()
	if err := seamBack(raw).Start(context.Background(), name, runtime.Config{
		Command: fixture.command,
		WorkDir: t.TempDir(),
		Env:     map[string]string{"GC_SESSION_ID": sessionID, "GC_CITY_PATH": city},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = raw.Stop(name) })

	raw.mu.Lock()
	agentPID := raw.conns[name].cmd.Process.Pid
	raw.mu.Unlock()
	pids := append([]int{agentPID}, childPIDs(t, agentPID)...)

	var found []runtime.LiveRuntime
	scanSnapshot(t, func() { found = findOnly(t, seamBack(raw), sessionID) }, pids...)
	if len(found) != 1 || found[0].PID != agentPID || !found[0].IsTracked || found[0].City != city {
		t.Fatalf("live session found = %+v, want tracked root pid %d in city %s", found, agentPID, city)
	}

	restarted := NewSeamBackedWithDir(fixture.dir, Config{}).(runtime.ProcessTableScanner)
	scanSnapshot(t, func() { found = findOnly(t, restarted, sessionID) }, pids...)
	if len(found) != 1 || found[0].PID != agentPID || found[0].IsTracked {
		t.Fatalf("after restart found = %+v, want untracked root pid %d", found, agentPID)
	}
	if err := restarted.TerminateRuntime(found[0]); err != nil {
		t.Fatalf("TerminateRuntime: %v", err)
	}
	scanSnapshot(t, func() { found = findOnly(t, restarted, sessionID) }, pids...)
	if len(found) != 0 {
		t.Fatalf("after terminate found = %+v, want the whole process group gone", found)
	}
}
