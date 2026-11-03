package main

import (
	"bytes"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// TestSweepProcessTableOrphansLeavesDetachedUserService is the closed-session
// half of the systemd-run fence. A transient user service carrying the closed
// session's GC_SESSION_ID is left alone and named once. An in-session child
// that stayed in the session scope, and a system service outside the user
// manager, are still reaped. A leftover in the reaper's own unit is reaped
// and named.
func TestSweepProcessTableOrphansLeavesDetachedUserService(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("drives the scanner against a procfs-shaped tree")
	}
	cityPath := t.TempDir()
	const sessionID = "gc-detached-svc"
	sessionEnv := []string{
		"PATH=/usr/bin",
		"GC_CITY_PATH=" + cityPath,
		"GC_SESSION_ID=" + sessionID,
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{{ID: sessionID, Status: "closed"}}, nil)
	root := t.TempDir()
	const (
		servicePID = 5100
		childPID   = 5101
		scopePID   = 5200
		ownPID     = 5300
		systemPID  = 5400
	)
	writeFakeProcEntry(t, root, servicePID, 1, "sleep", sessionEnv)
	writeFakeProcCgroup(t, root, servicePID, "/user.slice/user-1000.slice/user@1000.service/app.slice/detached-worker.service")
	writeFakeProcEntry(t, root, childPID, servicePID, "sleep", sessionEnv)
	writeFakeProcCgroup(t, root, childPID, "/user.slice/user-1000.slice/user@1000.service/app.slice/detached-worker.service")
	writeFakeProcEntry(t, root, scopePID, 1, "sleep", sessionEnv)
	writeFakeProcCgroup(t, root, scopePID, "/user.slice/user-1000.slice/user@1000.service/app.slice/tmux-spawn-abc.scope")
	writeFakeProcEntry(t, root, ownPID, 1, "claude", sessionEnv)
	writeFakeProcCgroup(t, root, ownPID, "/user.slice/user-1000.slice/user@1000.service/app.slice/agent.service")
	writeFakeProcEntry(t, root, systemPID, 1, "rogue", sessionEnv)
	writeFakeProcCgroup(t, root, systemPID, "/system.slice/rogue.service")
	self := os.Getpid()
	writeFakeProcEntry(t, root, self, 1, "gc", []string{"PATH=/usr/bin"})
	writeFakeProcCgroup(t, root, self, "/user.slice/user-1000.slice/user@1000.service/app.slice/agent.service")
	proctable.SetScanRootForTesting(t, root)
	t.Cleanup(func() { fencedDetachedUserServices.put(normalizePathForCompare(cityPath), nil) })

	sp := &procfsSweepScanner{Fake: runtime.NewFake()}
	var stderr bytes.Buffer
	got := sweepProcessTableOrphans(sp, newSessionBeadSnapshot(nil), nil, store, cityPath, &stderr)
	terminated := map[int]bool{}
	for _, live := range sp.terminated {
		terminated[live.PID] = true
	}
	if terminated[servicePID] || terminated[childPID] {
		t.Fatalf("sweep terminated %v, want the detached service %d and its child %d left alone", terminatedPIDs(sp.terminated), servicePID, childPID)
	}
	for _, pid := range []int{scopePID, ownPID, systemPID} {
		if !terminated[pid] {
			t.Fatalf("sweep reaped %d, terminated %v, want scope %d, own-unit %d, and system %d", got, terminatedPIDs(sp.terminated), scopePID, ownPID, systemPID)
		}
	}
	if len(terminated) != 3 || got != 3 {
		t.Fatalf("sweep reaped %d, terminated %v, want exactly three", got, terminatedPIDs(sp.terminated))
	}
	text := stderr.String()
	if !strings.Contains(text, "user service detached-worker.service") {
		t.Errorf("stderr %q does not name the skipped unit", text)
	}
	if !strings.Contains(text, "unit=agent.service") {
		t.Errorf("stderr %q does not name the reaped session unit", text)
	}

	stderr.Reset()
	sp.terminated = nil
	if again := sweepProcessTableOrphans(sp, newSessionBeadSnapshot(nil), nil, store, cityPath, &stderr); again != 3 {
		t.Fatalf("second sweep reaped %d, want the same three non-service-foreign roots", again)
	}
	if strings.Contains(stderr.String(), "detached-worker.service") {
		t.Errorf("second sweep repeated the detached-service log: %q", stderr.String())
	}
}

func writeFakeProcCgroup(t *testing.T, root string, pid int, path string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, strconv.Itoa(pid), "cgroup"), []byte("0::"+path+"\n"), 0o644); err != nil {
		t.Fatalf("write cgroup: %v", err)
	}
}

func terminatedPIDs(lives []runtime.LiveRuntime) []int {
	out := make([]int, len(lives))
	for i, live := range lives {
		out[i] = live.PID
	}
	return out
}
