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
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// TestSweepProcessTableOrphansLeavesManagedDoltWatchdogAlone runs the real
// process-table scanner and the real orphan sweep over a procfs shaped like
// the incident: a scope watchdog reparented to init with its dolt sql-server
// beneath it, both started from an agent session whose bead has since
// closed. With the environment doltServerEnv now produces, the sweep sees no
// root to attribute to that session and reaps nothing. The same tree with the
// session's environment passed through unchanged is the pre-fix state, and
// there the sweep reaps the watchdog, which is what took Dolt down about 15
// seconds after every agent session that had restarted it.
func TestSweepProcessTableOrphansLeavesManagedDoltWatchdogAlone(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("drives the scanner against a procfs-shaped tree")
	}
	cityPath := t.TempDir()
	sessionEnv := []string{
		"PATH=/usr/bin",
		"GC_CITY_PATH=" + cityPath,
		"GC_SESSION_ID=gc-restarter",
		"GC_SESSION_NAME=gc__worker-gc-restarter",
		"GC_AGENT=worker-1",
		"GC_RUNTIME_EPOCH=1",
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "gc-restarter", Status: "closed"}}, nil)

	for _, tc := range []struct {
		name       string
		serverEnv  []string
		wantReaped int
	}{
		{name: "scrubbed", serverEnv: doltServerEnv(cityPath, sessionEnv), wantReaped: 0},
		{name: "inherited", serverEnv: sessionEnv, wantReaped: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFakeProcEntry(t, root, 4100, 1, "gc", tc.serverEnv)
			writeFakeProcEntry(t, root, 4101, 4100, "dolt", tc.serverEnv)
			t.Cleanup(proctable.SetScanRootForTesting(root))

			sp := &procfsSweepScanner{Fake: runtime.NewFake()}
			var stderr bytes.Buffer
			got := sweepProcessTableOrphans(sp, nil, store, cityPath, &stderr)
			if got != tc.wantReaped || len(sp.terminated) != tc.wantReaped {
				t.Fatalf("sweepProcessTableOrphans() = %d reaped, terminated %v, want %d; stderr=%q", got, sp.terminated, tc.wantReaped, stderr.String())
			}
			if tc.wantReaped == 1 && sp.terminated[0].PID != 4100 {
				t.Fatalf("terminated %v, want the watchdog root pid 4100", sp.terminated)
			}
		})
	}
}

// procfsSweepScanner is the ProcessTableScanner the sweep sees in production,
// backed by the real proctable scan over the injected root, with termination
// recorded instead of signaled.
type procfsSweepScanner struct {
	*runtime.Fake
	terminated []runtime.LiveRuntime
}

func (s *procfsSweepScanner) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	return proctable.ScanBySessionID(id)
}

func (s *procfsSweepScanner) TerminateRuntime(live runtime.LiveRuntime) error { //nolint:unparam // interface compliance; error always nil in the recorder
	s.terminated = append(s.terminated, live)
	return nil
}

// writeFakeProcEntry writes the environ, stat and comm files the Linux scanner
// reads for one process under a procfs-shaped root.
func writeFakeProcEntry(t *testing.T, root string, pid, ppid int, comm string, env []string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	environ := strings.Join(env, "\x00") + "\x00"
	if err := os.WriteFile(filepath.Join(dir, "environ"), []byte(environ), 0o644); err != nil {
		t.Fatalf("write environ: %v", err)
	}
	stat := strconv.Itoa(pid) + " (" + comm + ") S " + strconv.Itoa(ppid) + strings.Repeat(" 0", 48)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatalf("write comm: %v", err)
	}
}

// TestSweepProcessTableOrphansFencesCityInfrastructureByArgv covers what the
// env scrub cannot: a scope watchdog stamped before the upgrade, and bd's
// db-proxy-child, which bd spawns with its own inherited environment. Both
// carry the closed session's GC_SESSION_ID and reparent to init, so the
// scanner reports them as that session's roots; the sweep must recognize them
// by argv and leave them alone, while still reaping a genuine orphaned agent
// runtime of the same session. The dolt child under the watchdog must not be
// promoted to a root in the watchdog's place.
func TestSweepProcessTableOrphansFencesCityInfrastructureByArgv(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("drives the scanner against a procfs-shaped tree")
	}
	cityPath := t.TempDir()
	sessionEnv := []string{
		"PATH=/usr/bin",
		"GC_CITY_PATH=" + cityPath,
		"GC_SESSION_ID=gc-restarter",
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "gc-restarter", Status: "closed"}}, nil)

	root := t.TempDir()
	writeFakeProcEntry(t, root, 4100, 1, "gc", sessionEnv)
	writeFakeProcCmdline(t, root, 4100, "/usr/local/bin/gc", managedDoltScopeWatchdogArg, "/city/.beads/dolt-config.yaml", "/city/.gc/dolt.log", cityPath)
	writeFakeProcEntry(t, root, 4101, 4100, "dolt", sessionEnv)
	writeFakeProcCmdline(t, root, 4101, "dolt", "sql-server", "--config", "/city/.beads/dolt-config.yaml")
	writeFakeProcEntry(t, root, 4200, 1, "bd", sessionEnv)
	writeFakeProcCmdline(t, root, 4200, "/opt/beads/bd-1.3.0", proxyendpoint.ChildVerb, "--root", cityPath+"/.beads/proxy")
	writeFakeProcEntry(t, root, 4300, 1, "claude", sessionEnv)
	writeFakeProcCmdline(t, root, 4300, "claude", "--resume")
	t.Cleanup(proctable.SetScanRootForTesting(root))

	sp := &procfsSweepScanner{Fake: runtime.NewFake()}
	var stderr bytes.Buffer
	got := sweepProcessTableOrphans(sp, nil, store, cityPath, &stderr)
	if got != 1 || len(sp.terminated) != 1 || sp.terminated[0].PID != 4300 {
		t.Fatalf("sweepProcessTableOrphans() = %d reaped, terminated %v, want only the agent pid 4300; stderr=%q", got, sp.terminated, stderr.String())
	}
	for _, pid := range []string{"pid=4100", "pid=4200"} {
		if !strings.Contains(stderr.String(), "leaving process-table root "+pid) {
			t.Errorf("stderr %q does not report leaving %s alone", stderr.String(), pid)
		}
	}
}

// writeFakeProcCmdline writes the cmdline file the kill-path argv fence reads.
func writeFakeProcCmdline(t *testing.T, root string, pid int, argv ...string) {
	t.Helper()
	path := filepath.Join(root, strconv.Itoa(pid), "cmdline")
	if err := os.WriteFile(path, []byte(strings.Join(argv, "\x00")+"\x00"), 0o644); err != nil {
		t.Fatalf("write cmdline: %v", err)
	}
}
