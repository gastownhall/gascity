package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
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
			proctable.SetScanRootForTesting(t, root)

			sp := &procfsSweepScanner{Fake: runtime.NewFake()}
			var stderr bytes.Buffer
			got := sweepProcessTableOrphans(sp, newSessionBeadSnapshot(nil), nil, store, cityPath, &stderr)
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
// recorded instead of signaled. Tracking is a provider's: roots of a session
// in tracked are marked tracked, and trackErr stands for a failed session
// listing or per-session GC_SESSION_ID read, returned beside the results.
type procfsSweepScanner struct {
	*runtime.Fake
	tracked    map[string]bool
	trackErr   error
	terminated []runtime.LiveRuntime
}

func (s *procfsSweepScanner) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	found, err := proctable.ScanBySessionID(id)
	for i := range found {
		found[i].IsTracked = s.tracked[found[i].SessionID]
	}
	return found, errors.Join(err, s.trackErr)
}

func (s *procfsSweepScanner) TerminateRuntime(live runtime.LiveRuntime) error { //nolint:unparam // interface compliance; error always nil in the recorder
	s.terminated = append(s.terminated, live)
	return nil
}

// writeFakeProcEntry writes the environ, stat, comm and status files the Linux
// scanner reads for one process under a procfs-shaped root. The status file
// names the test's effective UID: the scanner reports only processes it owns.
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
	uid := strconv.Itoa(os.Geteuid())
	status := "Name:\t" + comm + "\nUid:\t" + uid + "\t" + uid + "\t" + uid + "\t" + uid + "\n"
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
		t.Fatalf("write status: %v", err)
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
	proctable.SetScanRootForTesting(t, root)

	sp := &procfsSweepScanner{Fake: runtime.NewFake()}
	var stderr bytes.Buffer
	got := sweepProcessTableOrphans(sp, newSessionBeadSnapshot(nil), nil, store, cityPath, &stderr)
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

// TestSweepProcessTableOrphansReportsFencedRootOncePerProcess keeps the fence
// from flooding the controller log: a stamped watchdog or bd proxy stays
// fenced for its whole life, and the sweep runs every patrol. Each fenced
// process is reported once; a recycled pid (new start time) is a new process
// and is reported again; a root that is gone is dropped from the set.
func TestSweepProcessTableOrphansReportsFencedRootOncePerProcess(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("drives the scanner against a procfs-shaped tree")
	}
	cityPath := t.TempDir()
	sessionEnv := []string{"PATH=/usr/bin", "GC_CITY_PATH=" + cityPath, "GC_SESSION_ID=gc-restarter"}
	store := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "gc-restarter", Status: "closed"}}, nil)
	root := t.TempDir()
	writeFakeProcEntry(t, root, 4100, 1, "gc", sessionEnv)
	writeFakeProcCmdline(t, root, 4100, "gc", managedDoltScopeWatchdogArg, "cfg", "log", cityPath)
	writeFakeProcStartTime(t, root, 4100, 1, "gc", 1000)
	writeFakeProcEntry(t, root, 4200, 1, "bd", sessionEnv)
	writeFakeProcCmdline(t, root, 4200, "bd", proxyendpoint.ChildVerb, "--root", cityPath)
	writeFakeProcStartTime(t, root, 4200, 1, "bd", 2000)
	proctable.SetScanRootForTesting(t, root)
	t.Cleanup(func() { fencedInfrastructureRoots.put(normalizePathForCompare(cityPath), nil) })

	sweep := func() string {
		t.Helper()
		sp := &procfsSweepScanner{Fake: runtime.NewFake()}
		var stderr bytes.Buffer
		if got := sweepProcessTableOrphans(sp, newSessionBeadSnapshot(nil), nil, store, cityPath, &stderr); got != 0 || len(sp.terminated) != 0 {
			t.Fatalf("sweep reaped %d, terminated %v, want nothing", got, sp.terminated)
		}
		return stderr.String()
	}
	reported := func(out string) []string {
		var pids []string
		for _, pid := range []string{"4100", "4200"} {
			if strings.Contains(out, "leaving process-table root pid="+pid+" ") {
				pids = append(pids, pid)
			}
		}
		return pids
	}

	if got := reported(sweep()); strings.Join(got, ",") != "4100,4200" {
		t.Fatalf("first sweep reported %v, want both fenced roots", got)
	}
	for i := 0; i < 3; i++ {
		if out := sweep(); out != "" {
			t.Fatalf("repeat sweep %d logged %q, want silence for already-reported roots", i, out)
		}
	}
	// pid 4100 is recycled by a new watchdog: a different process, reported anew.
	writeFakeProcStartTime(t, root, 4100, 1, "gc", 3000)
	if got := reported(sweep()); strings.Join(got, ",") != "4100" {
		t.Fatalf("sweep after pid reuse reported %v, want only the new 4100", got)
	}
	// Both roots exit: the remembered set is pruned, not kept forever.
	if err := os.RemoveAll(filepath.Join(root, "4100")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "4200")); err != nil {
		t.Fatal(err)
	}
	sweep()
	if got := fencedInfrastructureRoots.take(normalizePathForCompare(cityPath)); len(got) != 0 {
		t.Fatalf("fenced-root set after the roots exited = %v, want empty", got)
	}
}

// writeFakeProcStartTime rewrites pid's stat with the given start time (field 22).
func writeFakeProcStartTime(t *testing.T, root string, pid, ppid int, comm string, start int) {
	t.Helper()
	fields := make([]string, 48)
	for i := range fields {
		fields[i] = "0"
	}
	fields[17] = strconv.Itoa(start) // fields[0] is field 5; field 22 is fields[17]
	stat := strconv.Itoa(pid) + " (" + comm + ") S " + strconv.Itoa(ppid) + " " + strings.Join(fields, " ")
	if err := os.WriteFile(filepath.Join(root, strconv.Itoa(pid), "stat"), []byte(stat), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}
}

// TestSweepProcessTableOrphansLeavesDetachedUserService: a process that a
// session started as a systemd user service inherits the session's identity
// and reparents to the user manager, but it runs in its own user-manager
// service unit, not in the session. The sweep must leave it running while it
// still reaps the session's own orphaned root.
func TestSweepProcessTableOrphansLeavesDetachedUserService(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("drives the scanner against a procfs-shaped tree")
	}
	cityPath := t.TempDir()
	sessionEnv := []string{"PATH=/usr/bin", "GC_CITY_PATH=" + cityPath, "GC_SESSION_ID=gc-restarter"}
	store := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "gc-restarter", Status: "closed"}}, nil)
	root := t.TempDir()
	uid := strconv.Itoa(os.Geteuid())
	userManager := "0::/user.slice/user-" + uid + ".slice/user@" + uid + ".service"
	writeFakeProcEntry(t, root, 4300, 1, "claude", sessionEnv)
	writeFakeProcCmdline(t, root, 4300, "claude", "--resume")
	writeFakeProcCgroup(t, root, 4300, userManager+"/app.slice/tmux-spawn-1234.scope")
	writeFakeProcEntry(t, root, 4400, 1, "node", sessionEnv)
	writeFakeProcCmdline(t, root, 4400, "node", "server.js")
	writeFakeProcCgroup(t, root, 4400, userManager+"/app.slice/devserver.service")
	proctable.SetScanRootForTesting(t, root)

	sp := &procfsSweepScanner{Fake: runtime.NewFake()}
	var stderr bytes.Buffer
	got := sweepProcessTableOrphans(sp, newSessionBeadSnapshot(nil), nil, store, cityPath, &stderr)
	if got != 1 || len(sp.terminated) != 1 || sp.terminated[0].PID != 4300 {
		t.Fatalf("sweepProcessTableOrphans() = %d reaped, terminated %v, want only the in-session root pid 4300 (the detached user service pid 4400 left running); stderr=%q", got, sp.terminated, stderr.String())
	}
}

// writeFakeProcCgroup writes the cgroup file naming the process's unit.
func writeFakeProcCgroup(t *testing.T, root string, pid int, line string) {
	t.Helper()
	path := filepath.Join(root, strconv.Itoa(pid), "cgroup")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatalf("write cgroup: %v", err)
	}
}

// TestSweepProcessTableOrphansReportsDetachedUserServiceOncePerProcess: a
// detached user service stays exempt for as long as it runs, and the sweep
// runs every patrol. Its exemption is reported once per process, with its
// unit, through the same suppression as fenced infrastructure; a recycled
// pid (new start time) is a new process and is reported again. The ordinary
// orphan of the same session is reaped on the first sweep.
func TestSweepProcessTableOrphansReportsDetachedUserServiceOncePerProcess(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("drives the scanner against a procfs-shaped tree")
	}
	cityPath := t.TempDir()
	sessionEnv := []string{"PATH=/usr/bin", "GC_CITY_PATH=" + cityPath, "GC_SESSION_ID=gc-restarter"}
	store := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "gc-restarter", Status: "closed"}}, nil)
	root := t.TempDir()
	uid := strconv.Itoa(os.Geteuid())
	userManager := "0::/user.slice/user-" + uid + ".slice/user@" + uid + ".service"
	writeFakeProcEntry(t, root, 4300, 1, "claude", sessionEnv)
	writeFakeProcCmdline(t, root, 4300, "claude", "--resume")
	writeFakeProcCgroup(t, root, 4300, userManager+"/app.slice/tmux-spawn-1234.scope")
	writeFakeProcEntry(t, root, 4400, 1, "node", sessionEnv)
	writeFakeProcCmdline(t, root, 4400, "node", "server.js")
	writeFakeProcStartTime(t, root, 4400, 1, "node", 1000)
	writeFakeProcCgroup(t, root, 4400, userManager+"/app.slice/devserver.service")
	proctable.SetScanRootForTesting(t, root)
	t.Cleanup(func() { fencedInfrastructureRoots.put(normalizePathForCompare(cityPath), nil) })

	sweep := func(wantReaped int) string {
		t.Helper()
		sp := &procfsSweepScanner{Fake: runtime.NewFake()}
		var stderr bytes.Buffer
		got := sweepProcessTableOrphans(sp, newSessionBeadSnapshot(nil), nil, store, cityPath, &stderr)
		if got != wantReaped || len(sp.terminated) != wantReaped {
			t.Fatalf("sweep reaped %d, terminated %v, want %d; stderr=%q", got, sp.terminated, wantReaped, stderr.String())
		}
		for _, live := range sp.terminated {
			if live.PID == 4400 {
				t.Fatalf("sweep terminated the detached user service pid 4400; stderr=%q", stderr.String())
			}
		}
		return stderr.String()
	}
	const exempt = `leaving process-table root pid=4400 session=gc-restarter alone: detached user service devserver.service (ppid=1 name="node")`

	if out := sweep(1); strings.Count(out, exempt) != 1 {
		t.Fatalf("first sweep stderr %q, want the service's exemption reported once", out)
	}
	// The reaped orphan exits; the service keeps running.
	if err := os.RemoveAll(filepath.Join(root, "4300")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if out := sweep(0); out != "" {
			t.Fatalf("repeat sweep %d logged %q, want silence for the already-reported service", i, out)
		}
	}
	// pid 4400 is recycled by a new service process: reported anew.
	writeFakeProcStartTime(t, root, 4400, 1, "node", 3000)
	if out := sweep(0); strings.Count(out, exempt) != 1 {
		t.Fatalf("sweep after pid reuse stderr %q, want the new service process reported once", out)
	}
}

// TestSweepProcessTableOrphansReapsOrphansInOwnAndSupervisorUnits: the
// detached user-service fence spares only a foreign unit. An ended session's
// roots in the sweeping process's own unit, or in Gas City's default or
// suffixed supervisor unit (KillMode=process leaves residual processes there),
// are reaped as before, while a foreign service beside them is left running.
func TestSweepProcessTableOrphansReapsOrphansInOwnAndSupervisorUnits(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("drives the scanner against a procfs-shaped tree")
	}
	cityPath := t.TempDir()
	sessionEnv := []string{"PATH=/usr/bin", "GC_CITY_PATH=" + cityPath, "GC_SESSION_ID=gc-restarter"}
	store := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "gc-restarter", Status: "closed"}}, nil)
	root := t.TempDir()
	uid := strconv.Itoa(os.Geteuid())
	userManager := "0::/user.slice/user-" + uid + ".slice/user@" + uid + ".service"
	self := os.Getpid()
	writeFakeProcEntry(t, root, self, 1, "gc", []string{"PATH=/usr/bin"})
	writeFakeProcCgroup(t, root, self, userManager+"/app.slice/gc-controller.service")
	units := map[int]string{
		4500: "gc-controller.service",
		4501: defaultSupervisorSystemdUnit,
		4502: supervisorSystemdUnitPrefix + "3f9a2c.service",
		4400: "devserver.service",
	}
	for pid, unit := range units {
		writeFakeProcEntry(t, root, pid, 1, "claude", sessionEnv)
		writeFakeProcCmdline(t, root, pid, "claude", "--resume")
		writeFakeProcCgroup(t, root, pid, userManager+"/app.slice/"+unit)
	}
	proctable.SetScanRootForTesting(t, root)
	t.Cleanup(func() { fencedInfrastructureRoots.put(normalizePathForCompare(cityPath), nil) })

	sp := &procfsSweepScanner{Fake: runtime.NewFake()}
	var stderr bytes.Buffer
	got := sweepProcessTableOrphans(sp, newSessionBeadSnapshot(nil), nil, store, cityPath, &stderr)
	var pids []int
	for _, live := range sp.terminated {
		pids = append(pids, live.PID)
	}
	slices.Sort(pids)
	if got != 3 || !slices.Equal(pids, []int{4500, 4501, 4502}) {
		t.Fatalf("sweepProcessTableOrphans() = %d reaped, terminated pids %v, want 4500, 4501 and 4502 (own and supervisor units) and never the foreign service 4400; stderr=%q", got, pids, stderr.String())
	}
}

// TestSupervisorSystemdUnitNamesMatchProctableStem pins the supervisor unit
// stem the user-service fence excludes to the names gc installs.
func TestSupervisorSystemdUnitNamesMatchProctableStem(t *testing.T) {
	if want := proctable.SupervisorSystemdUnitStem + ".service"; defaultSupervisorSystemdUnit != want {
		t.Fatalf("defaultSupervisorSystemdUnit = %q, want %q", defaultSupervisorSystemdUnit, want)
	}
	if want := proctable.SupervisorSystemdUnitStem + "-"; supervisorSystemdUnitPrefix != want {
		t.Fatalf("supervisorSystemdUnitPrefix = %q, want %q", supervisorSystemdUnitPrefix, want)
	}
}

// TestSupervisorSystemdDelegationEnvMatchesProctable pins the delegation
// variables the user-service fence reads to the ones gc's supervisor
// lifecycle reads, so the unit an operator delegates the supervisor to stays
// gc's own to the fence.
func TestSupervisorSystemdDelegationEnvMatchesProctable(t *testing.T) {
	if proctable.SupervisorSystemdUnitEnv != supervisorSystemdUnitEnv {
		t.Fatalf("proctable.SupervisorSystemdUnitEnv = %q, want %q", proctable.SupervisorSystemdUnitEnv, supervisorSystemdUnitEnv)
	}
	if proctable.SupervisorSystemdScopeEnv != supervisorSystemdScopeEnv {
		t.Fatalf("proctable.SupervisorSystemdScopeEnv = %q, want %q", proctable.SupervisorSystemdScopeEnv, supervisorSystemdScopeEnv)
	}
}
