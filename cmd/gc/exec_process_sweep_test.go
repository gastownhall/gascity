package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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
	command, wait := runtimetest.LeftoverCommand(t)
	cfg := runtime.Config{
		WorkDir:              t.TempDir(),
		Command:              command,
		ProcessNames:         []string{"sh", "sleep"},
		AcceptStartupDialogs: new(bool),
		Env:                  map[string]string{"GC_SESSION_ID": sessionID, "GC_SESSION_NAME": name, "GC_CITY_PATH": cityPath},
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
