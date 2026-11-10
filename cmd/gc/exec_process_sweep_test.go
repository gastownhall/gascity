package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// sweepGoldenPack writes the conformance golden reference, its
// host-filesystem variant (locality local, fs.host) when hostFS is set, with
// an `exec` that runs nothing, so no test drives a host tmux.
func sweepGoldenPack(t *testing.T, hostFS bool) string {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bodies := reference.Bodies()
	if hostFS {
		bodies = reference.HostFSBodies()
	}
	bodies["exec"] = `cat >/dev/null`
	script := filepath.Join(dir, "golden")
	if err := os.WriteFile(script, []byte(reference.Script(stateDir, bodies)), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// startSweepLeftover starts name on sp as session sessionID of cityPath with
// an agent that leaves a process behind, and returns the leftover; stopped
// stops the session first, so only the leftover remains.
func startSweepLeftover(t *testing.T, sp runtime.Provider, name, sessionID, cityPath string, stopped bool) runtimetest.Leftover {
	t.Helper()
	return startSweepLeftoverAt(t, sp, name, sessionID, cityPath, "1", stopped)
}

// startSweepLeftoverAt is startSweepLeftover with the session at runtime
// epoch epoch (GC_RUNTIME_EPOCH).
func startSweepLeftoverAt(t *testing.T, sp runtime.Provider, name, sessionID, cityPath, epoch string, stopped bool) runtimetest.Leftover {
	t.Helper()
	command, wait := runtimetest.LeftoverCommand(t)
	cfg := runtime.Config{
		WorkDir:              t.TempDir(),
		Command:              command,
		ProcessNames:         []string{"sh", "sleep"},
		AcceptStartupDialogs: new(bool),
		Env:                  map[string]string{"GC_SESSION_ID": sessionID, "GC_SESSION_NAME": name, "GC_CITY_PATH": cityPath, "GC_RUNTIME_EPOCH": epoch},
	}
	if err := sp.Start(context.Background(), name, cfg); err != nil {
		t.Fatalf("Start %s: %v", name, err)
	}
	t.Cleanup(func() { _ = sp.Stop(name) })
	l := wait()
	if stopped {
		if err := sp.Stop(name); err != nil {
			t.Fatalf("Stop %s: %v", name, err)
		}
	}
	return l
}

// The orphan sweep over a runtime pack sharing the host reaps exactly the
// leftover process of this city's closed session: a running session's
// leftover (tracked), another city's leftover of a session with the same id,
// and every leftover under a pack whose boxes do not share the host (no
// scanner) are left alone. The scan reads a snapshot of these leftovers
// alone, so the sweep can reach no process the test did not start.
func TestSweepProcessTableOrphansReapsHostFSExecLeftovers(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("reads /proc snapshots, which only linux has")
	}
	cityPath, otherCity := t.TempDir(), t.TempDir()
	sp := sessionexec.NewSeamBackedForCity(sweepGoldenPack(t, true), "sweep-city", cityPath)
	closed := startSweepLeftover(t, sp, "closed", "gc-closed", cityPath, true)
	live := startSweepLeftover(t, sp, "live", "gc-live", cityPath, false)
	foreign := startSweepLeftover(t, sessionexec.NewSeamBackedForCity(sweepGoldenPack(t, true), "other-city", otherCity), "closed", "gc-closed", otherCity, true)
	proctable.SetScanRootForTesting(t, runtimetest.SnapshotProcRoot(t, closed.PID, live.PID, foreign.PID))

	liveRow := beads.Bead{ID: "gc-live", Status: "open", Type: sessionBeadType, Metadata: map[string]string{"session_name": "live", "state": "active", "generation": "1"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "gc-closed", Status: "closed", Type: sessionBeadType}, liveRow}, nil)
	snapshot := newSessionBeadSnapshot([]beads.Bead{liveRow})

	var stderr bytes.Buffer
	offHost := sessionexec.NewSeamBackedForCity(sweepGoldenPack(t, false), "sweep-city", cityPath)
	if got := sweepProcessTableOrphans(offHost, snapshot, nil, store, cityPath, &stderr); got != 0 {
		t.Fatalf("sweep over a pack without fs.host reaped %d; stderr=%q", got, stderr.String())
	}
	if got := sweepProcessTableOrphans(sp, snapshot, nil, store, cityPath, &stderr); got != 1 {
		t.Fatalf("sweep reaped %d, want only the closed session's leftover; stderr=%q", got, stderr.String())
	}
	if pidutil.AliveWithStartTime(closed.PID, closed.Identity) {
		t.Errorf("the closed session's leftover pid %d still runs after the sweep; stderr=%q", closed.PID, stderr.String())
	}
	for label, l := range map[string]runtimetest.Leftover{"the running session's": live, "another city's": foreign} {
		if !pidutil.AliveWithStartTime(l.PID, l.Identity) {
			t.Errorf("the sweep ended %s leftover pid %d; stderr=%q", label, l.PID, stderr.String())
		}
	}
}

// stoppedSweepRow is open session row id, named name, asleep at generation
// 4 since stoppedAt.
func stoppedSweepRow(id, name string, stoppedAt time.Time) beads.Bead {
	return beads.Bead{ID: id, Status: "open", Type: sessionBeadType, Metadata: map[string]string{
		"session_name": name, "state": "asleep", "generation": "4", "slept_at": stoppedAt.Format(time.RFC3339),
	}}
}

// Rule 2 over a runtime pack sharing the host: the leftover of an open row
// that both reads have stopped is reaped once a complete inventory pass
// lists no live box under its name, and only then (no view, nothing
// reaped). A box the pack still runs keeps its leftover through tracking,
// even under a row that reads stopped and a pass that lists no box.
func TestSweepProcessTableOrphansReapsStoppedHostFSExecLeftover(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("reads /proc snapshots, which only linux has")
	}
	cityPath := t.TempDir()
	sp := sessionexec.NewSeamBackedForCity(sweepGoldenPack(t, true), "sweep-city", cityPath)
	stopped := startSweepLeftoverAt(t, sp, "asleep", "gc-asleep", cityPath, "4", true)
	running := startSweepLeftoverAt(t, sp, "running", "gc-running", cityPath, "4", false)
	proctable.SetScanRootForTesting(t, runtimetest.SnapshotProcRoot(t, stopped.PID, running.PID))

	stoppedAt := time.Now().Add(time.Minute)
	rows := []beads.Bead{stoppedSweepRow("gc-asleep", "asleep", stoppedAt), stoppedSweepRow("gc-running", "running", stoppedAt)}
	store := beads.NewMemStoreFrom(0, rows, nil)
	var stderr bytes.Buffer
	if got := sweepProcessTableOrphans(sp, newSessionBeadSnapshot(rows), nil, store, cityPath, &stderr); got != 0 {
		t.Fatalf("sweep without an inventory view reaped %d, want none; stderr=%q", got, stderr.String())
	}
	noBoxes := sweepView(nil, completeBackend("exec"))
	if got := sweepProcessTableOrphans(sp, newSessionBeadSnapshot(rows), noBoxes, store, cityPath, &stderr); got != 1 {
		t.Fatalf("sweep reaped %d, want the stopped row's leftover; stderr=%q", got, stderr.String())
	}
	if pidutil.AliveWithStartTime(stopped.PID, stopped.Identity) {
		t.Errorf("the stopped row's leftover pid %d still runs; stderr=%q", stopped.PID, stderr.String())
	}
	if !pidutil.AliveWithStartTime(running.PID, running.Identity) {
		t.Errorf("the sweep ended the leftover of a box the pack still runs (pid %d); stderr=%q", running.PID, stderr.String())
	}
}

// Under a listing that fails, tracking is unknown, so the sweep keeps only
// rule 1: a closed session's leftover is still reaped, and a stopped open
// row's leftover, which rule 2 would reap, is not.
func TestSweepProcessTableOrphansFailedExecListingReapsOnlyClosedSessions(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("reads /proc snapshots, which only linux has")
	}
	cityPath := t.TempDir()
	sp := sessionexec.NewSeamBackedForCity(sweepGoldenPack(t, true), "sweep-city", cityPath)
	closed := startSweepLeftoverAt(t, sp, "closed", "gc-closed", cityPath, "4", true)
	stopped := startSweepLeftoverAt(t, sp, "asleep", "gc-asleep", cityPath, "4", true)
	proctable.SetScanRootForTesting(t, runtimetest.SnapshotProcRoot(t, closed.PID, stopped.PID))
	t.Setenv("GC_RPP_FAULT", "unavailable")

	asleep := stoppedSweepRow("gc-asleep", "asleep", time.Now().Add(time.Minute))
	store := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "gc-closed", Status: "closed", Type: sessionBeadType}, asleep}, nil)
	var stderr bytes.Buffer
	if got := sweepProcessTableOrphans(sp, newSessionBeadSnapshot([]beads.Bead{asleep}), sweepView(nil, completeBackend("exec")), store, cityPath, &stderr); got != 1 {
		t.Fatalf("sweep reaped %d, want only the closed session's leftover; stderr=%q", got, stderr.String())
	}
	if pidutil.AliveWithStartTime(closed.PID, closed.Identity) {
		t.Errorf("the closed session's leftover pid %d still runs; stderr=%q", closed.PID, stderr.String())
	}
	if !pidutil.AliveWithStartTime(stopped.PID, stopped.Identity) {
		t.Errorf("the sweep reaped the stopped row's leftover pid %d with tracking unknown; stderr=%q", stopped.PID, stderr.String())
	}
}

// Kills two session-identity scrubs that drift: the list gc's detached
// city-infrastructure processes are started without is the one every exec
// op runs without (execenv.SessionScopedEnvKeys), and it covers a hook's
// deadline.
func TestSessionIdentityScrubIsSharedWithExecOps(t *testing.T) {
	if !slices.Equal(managedDoltSessionScopedEnvKeys, execenv.SessionScopedEnvKeys) {
		t.Fatalf("managedDoltSessionScopedEnvKeys = %v, want execenv.SessionScopedEnvKeys %v", managedDoltSessionScopedEnvKeys, execenv.SessionScopedEnvKeys)
	}
	if !slices.Contains(execenv.SessionScopedEnvKeys, hookDeadlineEnv) {
		t.Fatalf("execenv.SessionScopedEnvKeys lacks %s", hookDeadlineEnv)
	}
	env := []string{"PATH=/usr/bin"}
	for _, k := range execenv.SessionScopedEnvKeys {
		env = append(env, k+"=v")
	}
	if got := withoutSessionIdentityEnv(env); !slices.Equal(got, []string{"PATH=/usr/bin"}) {
		t.Fatalf("withoutSessionIdentityEnv = %v, want only PATH", got)
	}
}
